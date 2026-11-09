package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

// UplinkRegistrar handles registration on the external SIP server.
// It does the digest exchange itself for full control and debug logging.
type UplinkRegistrar struct {
	client  *sipgo.Client
	cfg     *UplinkConfig
	contact sip.ContactHeader
	log     *slog.Logger
	expiry  time.Duration // what the server granted last time
}

func NewUplinkRegistrar(client *sipgo.Client, cfg *UplinkConfig, contact sip.ContactHeader, log *slog.Logger) *UplinkRegistrar {
	return &UplinkRegistrar{
		client:  client,
		cfg:     cfg,
		contact: contact,
		log:     log,
		expiry:  cfg.ExpiryDuration(),
	}
}

// Run registers and keeps re-registering until ctx is cancelled.
// On cancel, sends unregister (Expires: 0). A provider that is down at startup
// is retried every 5s: local phones can still call each other meanwhile.
func (r *UplinkRegistrar) Run(ctx context.Context) {
	for {
		err := r.register(ctx, r.expiry)
		if err == nil {
			break
		}
		r.log.Error("Registration failed, retrying in 5s", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	r.log.Info("Registered on uplink", "expiry", r.expiry)

	retry := false
	for {
		// 75% of the expiry the server actually granted, recomputed every round;
		// after a failure try again in 5s instead of waiting a full interval.
		wait := max(r.expiry*3/4, 30*time.Second)
		if retry {
			wait = 5 * time.Second
		}

		select {
		case <-ctx.Done():
			unregCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := r.register(unregCtx, 0); err != nil {
				r.log.Error("Unregister failed", "err", err)
			} else {
				r.log.Info("Unregistered")
			}
			return
		case <-time.After(wait):
			err := r.register(ctx, r.expiry)
			retry = err != nil
			if err != nil {
				r.log.Error("Re-registration failed", "err", err)
			} else {
				r.log.Debug("Re-registered", "expiry", r.expiry)
			}
		}
	}
}

// uplinkContact is the Contact we register on the provider: our external
// address on the uplink transport. tcp is named by a parameter; for tls the
// sips: scheme says so by itself, the same form diago puts in dialog Contacts.
func uplinkContact(cfg *Config) sip.ContactHeader {
	u := sip.Uri{Scheme: "sip", User: cfg.Uplink.Username, Host: cfg.SIP.ExternalIP, Port: cfg.SIP.ExternalPort, UriParams: sip.NewParams()}
	switch cfg.Uplink.Transport {
	case "tcp":
		u.UriParams.Add("transport", "tcp")
	case "tls":
		u.Scheme, u.Port = "sips", cfg.SIP.TLSPort
	}
	return sip.ContactHeader{Address: u}
}

// register sends REGISTER with the given expiry (0 unregisters) and answers
// one digest challenge.
func (r *UplinkRegistrar) register(ctx context.Context, expiry time.Duration) error {
	recipient := sip.Uri{Host: r.cfg.Host, Port: r.cfg.Port}
	// Omit default port from URI (some servers strip it in digest computation)
	if recipient.Port == sip.DefaultPort(r.cfg.Transport) {
		recipient.Port = 0
	}

	req := sip.NewRequest(sip.REGISTER, recipient)
	// The transport is set on the request, not as ;transport= in the URI, so the
	// Request-URI and the digest uri= (Addr(), no parameters) stay identical.
	req.SetTransport(strings.ToUpper(r.cfg.Transport))

	// To header must include the username (AOR) — many servers
	// check that To matches the authenticated user and reject with 403 otherwise.
	to := sip.ToHeader{
		Address: sip.Uri{
			Scheme: "sip",
			User:   r.cfg.Username,
			Host:   r.cfg.Host,
		},
	}
	req.AppendHeader(&to)
	contact := r.contact
	req.AppendHeader(&contact)
	expires := sip.ExpiresHeader(uint32(expiry.Seconds()))
	req.AppendHeader(&expires)

	// First attempt (no auth)
	r.log.Debug("Sending REGISTER (no auth)", "expires", expiry)
	res, err := r.doRequest(ctx, req)
	if err != nil {
		return fmt.Errorf("REGISTER request failed: %w", err)
	}

	if res.StatusCode == 200 {
		r.updateExpiry(res, expiry)
		return nil
	}

	authHeader, respHeader := "WWW-Authenticate", "Authorization"
	switch res.StatusCode {
	case 401:
	case 407:
		authHeader, respHeader = "Proxy-Authenticate", "Proxy-Authorization"
	default:
		return fmt.Errorf("unexpected response: %s", res.StartLine())
	}

	challengeHdr := res.GetHeader(authHeader)
	if challengeHdr == nil {
		return fmt.Errorf("no %s header in %d response", authHeader, res.StatusCode)
	}
	chal, err := digest.ParseChallenge(normalizeDigestPrefix(challengeHdr.Value()))
	if err != nil {
		return fmt.Errorf("parse %s: %w", authHeader, err)
	}
	r.log.Debug("Received auth challenge", "realm", chal.Realm, "algorithm", chal.Algorithm, "qop", chal.QOP)

	cred, err := digest.Digest(chal, digest.Options{
		Method:   "REGISTER",
		URI:      req.Recipient.Addr(),
		Username: r.cfg.Username,
		Password: r.cfg.Password,
	})
	if err != nil {
		return fmt.Errorf("digest: %w", err)
	}

	// Update Contact based on rport/received from response Via
	r.updateContactFromVia(res, req)

	// Remove old Via so sipgo generates a new one with a fresh branch ID.
	// Reusing the same branch makes the server treat this as a retransmission
	// of the unauthenticated request, ignoring the Authorization header.
	req.RemoveHeader("Via")
	req.RemoveHeader(respHeader)
	req.AppendHeader(sip.NewHeader(respHeader, cred.String()))

	r.log.Debug("Sending REGISTER (with auth)")
	res, err = r.doRequest(ctx, req)
	if err != nil {
		return fmt.Errorf("authenticated REGISTER failed: %w", err)
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("registration rejected: %s", res.StartLine())
	}
	r.updateExpiry(res, expiry)
	return nil
}

func (r *UplinkRegistrar) doRequest(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	tx, err := r.client.TransactionRequest(ctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return nil, err
	}
	defer tx.Terminate()

	select {
	case res := <-tx.Responses():
		return res, nil
	case <-tx.Done():
		return nil, tx.Err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *UplinkRegistrar) updateContactFromVia(res *sip.Response, req *sip.Request) {
	via := res.Via()
	if via == nil {
		return
	}

	contact := r.contact
	if rport, _ := via.Params.Get("rport"); rport != "" {
		// On UDP rport is the NAT mapping of our listener, which is where the
		// provider has to send INVITEs. On TCP or TLS it is the ephemeral port of
		// this connection: a provider that reuses the connection does not need
		// it, one that opens its own must reach the listener port instead.
		if p, err := strconv.Atoi(rport); err == nil && !sip.IsReliable(r.cfg.Transport) {
			contact.Address.Port = p
		}
		if received, _ := via.Params.Get("received"); received != "" {
			contact.Address.Host = received
		}
		r.contact = contact
		req.ReplaceHeader(&contact)
	}
}

// updateExpiry takes over the expiry the server granted. Skipped for unregister.
func (r *UplinkRegistrar) updateExpiry(res *sip.Response, requested time.Duration) {
	if requested == 0 {
		return
	}
	if h := res.GetHeader("Expires"); h != nil {
		if val, err := strconv.Atoi(h.Value()); err == nil && val > 0 {
			r.expiry = time.Duration(val) * time.Second
		}
	}
}
