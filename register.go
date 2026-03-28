package main

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// UplinkRegistrar handles registration on the external SIP server.
// It implements digest auth manually for full control and debug logging.
type UplinkRegistrar struct {
	client   *sipgo.Client
	cfg      *UplinkConfig
	contact  sip.ContactHeader
	log      *slog.Logger
	expiry   time.Duration
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
// On cancel, sends unregister (Expires: 0).
func (r *UplinkRegistrar) Run(ctx context.Context) error {
	if err := r.register(ctx); err != nil {
		return err
	}
	r.log.Info("Registered on uplink", "expiry", r.expiry)

	// Re-register loop at 75% of expiry
	reregInterval := r.expiry * 3 / 4
	if reregInterval < 30*time.Second {
		reregInterval = 30 * time.Second
	}

	for {
		select {
		case <-ctx.Done():
			// Unregister
			unregCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r.unregister(unregCtx)
			return nil
		case <-time.After(reregInterval):
			if err := r.register(ctx); err != nil {
				r.log.Error("Re-registration failed", "err", err)
				// Retry sooner
				time.Sleep(5 * time.Second)
			} else {
				r.log.Debug("Re-registered", "expiry", r.expiry)
			}
		}
	}
}

func (r *UplinkRegistrar) register(ctx context.Context) error {
	recipient := sip.Uri{
		Host: r.cfg.Host,
	}
	// Omit default port from URI (some servers strip it in digest computation)
	if r.cfg.Port != 0 && r.cfg.Port != 5060 {
		recipient.Port = r.cfg.Port
	}

	req := sip.NewRequest(sip.REGISTER, recipient)

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
	req.AppendHeader(&r.contact)
	expires := sip.ExpiresHeader(uint32(r.expiry.Seconds()))
	req.AppendHeader(&expires)

	// First attempt (no auth)
	r.log.Debug("Sending REGISTER (no auth)")
	res, err := r.doRequest(ctx, req)
	if err != nil {
		return fmt.Errorf("REGISTER request failed: %w", err)
	}

	if res.StatusCode == 200 {
		r.updateExpiry(res)
		return nil
	}

	if res.StatusCode != 401 && res.StatusCode != 407 {
		return fmt.Errorf("unexpected response: %s", res.StartLine())
	}

	// Parse challenge
	authHeader := "WWW-Authenticate"
	if res.StatusCode == 407 {
		authHeader = "Proxy-Authenticate"
	}
	challengeHdr := res.GetHeader(authHeader)
	if challengeHdr == nil {
		return fmt.Errorf("no %s header in %d response", authHeader, res.StatusCode)
	}

	challenge := parseDigestParams(challengeHdr.Value())
	realm := challenge["realm"]
	nonce := challenge["nonce"]
	algorithm := challenge["algorithm"]

	r.log.Debug("Received auth challenge",
		"realm", realm,
		"nonce", nonce,
		"algorithm", algorithm,
	)

	// Compute digest response
	username := r.cfg.Username
	password := r.cfg.Password
	uri := req.Recipient.Addr()

	ha1 := md5hex(username + ":" + realm + ":" + password)
	ha2 := md5hex("REGISTER:" + uri)
	response := md5hex(ha1 + ":" + nonce + ":" + ha2)

	r.log.Debug("Digest computation",
		"username", username,
		"realm", realm,
		"uri", uri,
		"ha1", ha1,
		"ha2", ha2,
		"response", response,
	)

	// Build Authorization header
	authValue := fmt.Sprintf(
		`Digest username="%s", realm="%s", nonce="%s", uri="%s", algorithm=MD5, response="%s"`,
		username, realm, nonce, uri, response,
	)

	respHeader := "Authorization"
	if res.StatusCode == 407 {
		respHeader = "Proxy-Authorization"
	}

	// Update Contact based on rport/received from response Via
	r.updateContactFromVia(res, req)

	// Remove old Via so sipgo generates a new one with a fresh branch ID.
	// Reusing the same branch makes the server treat this as a retransmission
	// of the unauthenticated request, ignoring the Authorization header.
	req.RemoveHeader("Via")

	// Send authenticated request
	req.RemoveHeader(respHeader)
	req.AppendHeader(sip.NewHeader(respHeader, authValue))

	r.log.Debug("Sending REGISTER (with auth)")
	res, err = r.doRequest(ctx, req)
	if err != nil {
		return fmt.Errorf("authenticated REGISTER failed: %w", err)
	}

	if res.StatusCode == 200 {
		r.updateExpiry(res)
		return nil
	}

	return fmt.Errorf("registration rejected: %s", res.StartLine())
}

func (r *UplinkRegistrar) unregister(ctx context.Context) {
	recipient := sip.Uri{Host: r.cfg.Host}
	if r.cfg.Port != 0 && r.cfg.Port != 5060 {
		recipient.Port = r.cfg.Port
	}

	req := sip.NewRequest(sip.REGISTER, recipient)
	contact := r.contact
	req.AppendHeader(&contact)
	expires := sip.ExpiresHeader(0)
	req.AppendHeader(&expires)

	res, err := r.doRequest(ctx, req)
	if err != nil {
		r.log.Error("Unregister failed", "err", err)
		return
	}

	if res.StatusCode == 401 || res.StatusCode == 407 {
		// Need auth for unregister too
		authHeader := "WWW-Authenticate"
		respHeader := "Authorization"
		if res.StatusCode == 407 {
			authHeader = "Proxy-Authenticate"
			respHeader = "Proxy-Authorization"
		}
		challengeHdr := res.GetHeader(authHeader)
		if challengeHdr != nil {
			challenge := parseDigestParams(challengeHdr.Value())
			uri := req.Recipient.Addr()
			ha1 := md5hex(r.cfg.Username + ":" + challenge["realm"] + ":" + r.cfg.Password)
			ha2 := md5hex("REGISTER:" + uri)
			response := md5hex(ha1 + ":" + challenge["nonce"] + ":" + ha2)
			authValue := fmt.Sprintf(
				`Digest username="%s", realm="%s", nonce="%s", uri="%s", algorithm=MD5, response="%s"`,
				r.cfg.Username, challenge["realm"], challenge["nonce"], uri, response,
			)
			r.updateContactFromVia(res, req)
			req.RemoveHeader(respHeader)
			req.AppendHeader(sip.NewHeader(respHeader, authValue))
			res, err = r.doRequest(ctx, req)
		}
	}

	if err != nil {
		r.log.Error("Unregister auth failed", "err", err)
	} else {
		r.log.Info("Unregistered", "status", res.StatusCode)
	}
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
		if p, err := strconv.Atoi(rport); err == nil {
			contact.Address.Port = p
		}
		if received, _ := via.Params.Get("received"); received != "" {
			contact.Address.Host = received
		}
		r.contact = contact
		req.ReplaceHeader(&contact)
	}
}

func (r *UplinkRegistrar) updateExpiry(res *sip.Response) {
	if h := res.GetHeader("Expires"); h != nil {
		if val, err := strconv.Atoi(h.Value()); err == nil {
			r.expiry = time.Duration(val) * time.Second
		}
	}
}
