package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

// nonceLifetime is how long a client has to answer a challenge. It also bounds
// how long a failing IP+user keeps the same nonce (see challenge), so it should
// cover the auth guard window.
const nonceLifetime = 5 * time.Minute

// maxRegisterExpiry caps the registration lifetime we grant. Phones behind
// NAT lose their UDP binding after 30-60 s of silence, and the periodic
// REGISTER is what keeps it open, so a phone asking for 3600 s gets 120 s back.
const maxRegisterExpiry = 120

type Registration struct {
	Username string
	Contact  sip.ContactHeader
	Source   string // ip:port of the registering device
	Expires  time.Time
}

type nonceEntry struct {
	user    string
	created time.Time
}

type Registrar struct {
	mu            sync.RWMutex
	users         map[string]string        // username -> password
	registrations map[string]*Registration // username -> registration
	nonces        map[string]nonceEntry    // nonce -> pending challenge
	pending       map[string]string        // ip|user -> nonce reissued while that client keeps failing
	guard         authGuard
	realm         string
	log           *slog.Logger
}

func NewRegistrar(cfg *Config, realm string, log *slog.Logger) *Registrar {
	r := &Registrar{
		registrations: make(map[string]*Registration),
		nonces:        make(map[string]nonceEntry),
		pending:       make(map[string]string),
		guard:         authGuard{ips: make(map[string]*ipFailures)},
		realm:         realm,
		log:           log,
	}
	r.UpdateConfig(cfg)
	return r
}

func (r *Registrar) HandleRegister(req *sip.Request, tx sip.ServerTransaction) {
	fromHdr := req.From()
	if fromHdr == nil {
		r.respond(req, tx, 400, "Bad Request")
		return
	}
	username := fromHdr.Address.User

	// Unknown users are challenged like everyone else: a 403 here would tell
	// a scanner which extensions exist. They fail inside verifyAuth instead.
	if res := r.Authorize(req, username); res != nil {
		if err := tx.Respond(res); err != nil {
			r.log.Error("Failed to respond", "code", res.StatusCode, "err", err)
		}
		return
	}

	contact := req.Contact()
	if contact == nil {
		r.respond(req, tx, 400, "Bad Request - no Contact")
		return
	}

	expiry, ok := registerExpiry(req, contact)
	if !ok {
		r.respond(req, tx, 400, "Bad Request - invalid Expires")
		return
	}

	r.mu.Lock()
	if expiry == 0 {
		// Unregister
		delete(r.registrations, username)
		r.log.Info("User unregistered", "user", username)
	} else {
		r.registrations[username] = &Registration{
			Username: username,
			Contact:  *contact,
			Source:   req.Source(),
			Expires:  time.Now().Add(time.Duration(expiry) * time.Second),
		}
		r.log.Info("User registered", "user", username, "contact", contact.Address.String(), "source", req.Source(), "expires", expiry)
	}
	r.mu.Unlock()

	// 200 OK
	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	expiresH := sip.ExpiresHeader(uint32(expiry))
	res.AppendHeader(&expiresH)
	if err := tx.Respond(res); err != nil {
		r.log.Error("Failed to respond 200", "err", err)
	}
}

// registerExpiry returns the lifetime to grant: the Expires header, else the
// Contact ;expires= parameter, else 3600, capped at maxRegisterExpiry.
// ok is false for a value that is not a non-negative integer.
func registerExpiry(req *sip.Request, contact *sip.ContactHeader) (expiry int, ok bool) {
	value := ""
	if h := req.GetHeader("Expires"); h != nil {
		value = h.Value()
	} else if contact.Params != nil {
		value, _ = contact.Params.Get("expires")
	}
	if value == "" {
		return maxRegisterExpiry, true
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n < 0 {
		return 0, false
	}
	return min(n, maxRegisterExpiry), true
}

// Authorize checks the digest credentials of req for user username.
// It returns nil when the request is authorized, otherwise the response to send:
// 401 with a fresh challenge, or 403 when the source IP is blocked by the auth guard.
// Used for REGISTER and for INVITE from local extensions.
func (r *Registrar) Authorize(req *sip.Request, username string) *sip.Response {
	ip := sourceIP(req)
	if r.guard.blocked(ip) {
		return sip.NewResponseFromRequest(req, sip.StatusForbidden, "Forbidden", nil)
	}

	if authHdr := req.GetHeader("Authorization"); authHdr != nil {
		ok, guess := r.verifyAuth(req, authHdr.Value(), username)
		if ok {
			return nil
		}
		reason := "bad credentials"
		if !r.IsLocalUser(username) {
			reason = "unknown user"
		}
		// One line per failure with the source IP, so fail2ban can match it.
		r.log.Warn("Auth failed", "method", req.Method, "user", username, "source", req.Source(), "reason", reason)
		if guess != "" && r.recordFailure(req, guess) {
			return sip.NewResponseFromRequest(req, sip.StatusForbidden, "Forbidden", nil)
		}
	}

	return r.challenge(req, username)
}

// recordFailure counts one distinct wrong guess against the source IP and reports
// whether it is now blocked. Repeating the same guess does not add up: a phone
// configured with a wrong password retries for minutes without getting its owner blocked.
func (r *Registrar) recordFailure(req *sip.Request, guess string) bool {
	ip := sourceIP(req)
	until, blocked := r.guard.fail(ip, guess)
	if blocked {
		r.log.Warn("Source IP blocked after repeated auth failures", "source", ip, "until", until.Format(time.RFC3339))
	}
	return blocked
}

// challenge issues a 401. The same IP+user gets the same nonce until it either
// authenticates or the nonce expires, and the challenge carries no qop, so the
// response depends only on the password: that is what lets the auth guard tell
// "same wrong password again" from "another guess". Replay of a successful
// response is still blocked because a nonce is deleted on success.
func (r *Registrar) challenge(req *sip.Request, username string) *sip.Response {
	now := time.Now()
	key := sourceIP(req) + "|" + username

	r.mu.Lock()
	// ponytail: full sweep on every challenge, O(pending nonces); fine for a handful of phones.
	// A flood of INVITEs with random From users grows both maps for nonceLifetime;
	// sipgo's per-request transactions cost more than these entries, so no cap here.
	for n, e := range r.nonces {
		if now.Sub(e.created) > nonceLifetime {
			delete(r.nonces, n)
		}
	}
	for k, n := range r.pending {
		if _, alive := r.nonces[n]; !alive {
			delete(r.pending, k)
		}
	}
	nonce, ok := r.pending[key]
	if !ok {
		nonce = generateNonce()
		r.nonces[nonce] = nonceEntry{user: username, created: now}
		r.pending[key] = nonce
	}
	r.mu.Unlock()

	chal := digest.Challenge{Realm: r.realm, Nonce: nonce, Algorithm: "MD5"}
	res := sip.NewResponseFromRequest(req, sip.StatusUnauthorized, "Unauthorized", nil)
	res.AppendHeader(sip.NewHeader("WWW-Authenticate", chal.String()))
	return res
}

// verifyAuth checks the credentials. On failure it also returns a key that
// identifies the guess: the client's response, which for a fixed nonce is a
// function of the password alone. The key is empty when the request did not
// answer a live nonce of ours: whoever sent it never saw our challenge, and
// counting it would let spoofed UDP packets get a real phone's IP blocked.
func (r *Registrar) verifyAuth(req *sip.Request, authValue, username string) (bool, string) {
	cred, err := digest.ParseCredentials(normalizeDigestPrefix(authValue))
	if err != nil || cred.Username != username || cred.Realm != r.realm {
		return false, ""
	}
	// The response signs the uri= the client chose, so it has to be the Request-URI
	// (RFC 3261 22.4). Otherwise anyone on the path can take a signed INVITE and
	// redial it to another number by rewriting the request line alone.
	if !sameURI(cred.URI, req.Recipient) {
		return false, ""
	}

	// Lookup, check and delete under one lock: two copies of the same answer
	// arriving together must not both pass.
	r.mu.Lock()
	defer r.mu.Unlock()
	password, known := r.users[username]
	entry, ok := r.nonces[cred.Nonce]
	if !ok || entry.user != username || time.Since(entry.created) > nonceLifetime {
		return false, ""
	}
	guess := username + ":" + cred.Response
	if !known {
		return false, guess
	}

	// Recompute the response with whatever qop/cnonce/nc the client sent.
	chal := &digest.Challenge{Realm: r.realm, Nonce: cred.Nonce, Algorithm: cred.Algorithm}
	if cred.QOP != "" {
		chal.QOP = []string{cred.QOP}
	}
	expected, err := digest.Digest(chal, digest.Options{
		Method:   req.Method.String(),
		URI:      cred.URI,
		Username: username,
		Password: password,
		Cnonce:   cred.Cnonce,
		Count:    cred.Nc,
	})
	if err != nil {
		return false, guess
	}
	if subtle.ConstantTimeCompare([]byte(expected.Response), []byte(cred.Response)) != 1 {
		return false, guess
	}

	// Single use: the nonce dies with the successful answer.
	delete(r.nonces, cred.Nonce)
	return true, ""
}

// sameURI reports whether the digest uri= names the same user and host as the
// Request-URI. Port and params are left out: phones differ in whether they copy
// them, and neither changes who gets called.
func sameURI(digestURI string, requestURI sip.Uri) bool {
	var u sip.Uri
	if err := sip.ParseUri(digestURI, &u); err != nil {
		return false
	}
	return u.User == requestURI.User && strings.EqualFold(u.Host, requestURI.Host)
}

// normalizeDigestPrefix makes the scheme name match what icholy/digest expects:
// the scheme is case-insensitive per RFC 7235, the parser is not.
func normalizeDigestPrefix(s string) string {
	if len(s) >= len(digest.Prefix) && strings.EqualFold(s[:len(digest.Prefix)], digest.Prefix) {
		return digest.Prefix + s[len(digest.Prefix):]
	}
	return s
}

// GetAllRegistrations returns all currently valid registrations.
func (r *Registrar) GetAllRegistrations() []*Registration {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()
	var result []*Registration
	for _, reg := range r.registrations {
		if reg.Expires.After(now) {
			result = append(result, reg)
		}
	}
	return result
}

// GetRegistration returns registration for a specific user.
func (r *Registrar) GetRegistration(username string) *Registration {
	r.mu.RLock()
	defer r.mu.RUnlock()

	reg, ok := r.registrations[username]
	if !ok || reg.Expires.Before(time.Now()) {
		return nil
	}
	return reg
}

// UpdateConfig replaces the user list and auth guard settings (for config reload).
func (r *Registrar) UpdateConfig(cfg *Config) {
	r.guard.setConfig(cfg.AuthGuard)

	r.mu.Lock()
	defer r.mu.Unlock()

	newUsers := make(map[string]string, len(cfg.Users))
	for _, u := range cfg.Users {
		newUsers[u.Username] = u.Password
	}
	r.users = newUsers

	// Remove registrations for deleted users
	for username := range r.registrations {
		if _, ok := newUsers[username]; !ok {
			delete(r.registrations, username)
			r.log.Info("Removed registration for deleted user", "user", username)
		}
	}
}

// IsLocalUser checks if username belongs to a configured local user.
func (r *Registrar) IsLocalUser(username string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.users[username]
	return ok
}

// ContactURI builds a SIP URI for reaching a registered user.
func (r *Registrar) ContactURI(reg *Registration) sip.Uri {
	host, port, _ := net.SplitHostPort(reg.Source)
	p := 0
	fmt.Sscanf(port, "%d", &p)
	return sip.Uri{
		User: reg.Username,
		Host: host,
		Port: p,
	}
}

func (r *Registrar) respond(req *sip.Request, tx sip.ServerTransaction, code int, reason string) {
	res := sip.NewResponseFromRequest(req, code, reason, nil)
	if err := tx.Respond(res); err != nil {
		r.log.Error("Failed to respond", "code", code, "err", err)
	}
}

func generateNonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func sourceIP(req *sip.Request) string {
	host, _, err := net.SplitHostPort(req.Source())
	if err != nil {
		return req.Source()
	}
	return host
}

// authGuard blocks a source IP after too many distinct wrong guesses in a window.
type authGuard struct {
	mu  sync.Mutex
	cfg AuthGuardConfig
	ips map[string]*ipFailures
}

type ipFailures struct {
	guesses      map[string]struct{}
	windowStart  time.Time
	blockedUntil time.Time
}

func (g *authGuard) setConfig(cfg AuthGuardConfig) {
	g.mu.Lock()
	g.cfg = cfg
	g.mu.Unlock()
}

func (g *authGuard) blocked(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.cfg.Enabled {
		return false
	}
	f, ok := g.ips[ip]
	return ok && f.blockedUntil.After(time.Now())
}

// fail records one wrong guess and reports whether ip is now blocked.
func (g *authGuard) fail(ip, guess string) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.cfg.Enabled {
		return time.Time{}, false
	}
	now := time.Now()
	window := time.Duration(g.cfg.Window) * time.Second

	// ponytail: sweep stale entries on every failure; attackers are few, honest phones fewer.
	for k, f := range g.ips {
		if now.Sub(f.windowStart) > window && !f.blockedUntil.After(now) {
			delete(g.ips, k)
		}
	}

	f, ok := g.ips[ip]
	if !ok || now.Sub(f.windowStart) > window {
		f = &ipFailures{windowStart: now, guesses: make(map[string]struct{})}
		g.ips[ip] = f
	}
	f.guesses[guess] = struct{}{}
	if len(f.guesses) >= g.cfg.MaxGuesses {
		f.blockedUntil = now.Add(time.Duration(g.cfg.BlockTime) * time.Second)
		return f.blockedUntil, true
	}
	return time.Time{}, false
}
