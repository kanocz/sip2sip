package main

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
)

type Registration struct {
	Username string
	Contact  sip.ContactHeader
	Source   string // ip:port of the registering device
	Expires  time.Time
}

type Registrar struct {
	mu            sync.RWMutex
	users         map[string]string        // username -> password
	registrations map[string]*Registration // username -> registration
	nonces        map[string]string        // nonce -> username (pending auth challenges)
	realm         string
	log           *slog.Logger
}

func NewRegistrar(users []UserConfig, realm string, log *slog.Logger) *Registrar {
	userMap := make(map[string]string, len(users))
	for _, u := range users {
		userMap[u.Username] = u.Password
	}
	return &Registrar{
		users:         userMap,
		registrations: make(map[string]*Registration),
		nonces:        make(map[string]string),
		realm:         realm,
		log:           log,
	}
}

func (r *Registrar) HandleRegister(req *sip.Request, tx sip.ServerTransaction) {
	fromHdr := req.From()
	if fromHdr == nil {
		r.respond(req, tx, 400, "Bad Request")
		return
	}
	username := fromHdr.Address.User

	// Check if user exists
	password, ok := r.users[username]
	if !ok {
		r.log.Warn("REGISTER from unknown user", "user", username)
		r.respond(req, tx, 403, "Forbidden")
		return
	}

	// Check for Authorization header
	authHdr := req.GetHeader("Authorization")
	if authHdr == nil {
		// Send 401 with challenge
		r.sendChallenge(req, tx, username)
		return
	}

	// Verify digest auth
	if !r.verifyAuth(authHdr.Value(), username, password, req.Method.String()) {
		r.log.Warn("REGISTER auth failed", "user", username)
		r.sendChallenge(req, tx, username)
		return
	}

	// Check Expires
	expiry := 3600
	if expiresHdr := req.GetHeader("Expires"); expiresHdr != nil {
		fmt.Sscanf(expiresHdr.Value(), "%d", &expiry)
	}

	contact := req.Contact()
	if contact == nil {
		r.respond(req, tx, 400, "Bad Request - no Contact")
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

func (r *Registrar) sendChallenge(req *sip.Request, tx sip.ServerTransaction, username string) {
	nonce := r.generateNonce()

	r.mu.Lock()
	r.nonces[nonce] = username
	r.mu.Unlock()

	wwwAuth := fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=MD5, qop="auth"`, r.realm, nonce)

	res := sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)
	res.AppendHeader(sip.NewHeader("WWW-Authenticate", wwwAuth))
	if err := tx.Respond(res); err != nil {
		r.log.Error("Failed to respond 401", "err", err)
	}
}

func (r *Registrar) verifyAuth(authValue, username, password, method string) bool {
	params := parseDigestParams(authValue)

	if params["username"] != username {
		return false
	}

	nonce := params["nonce"]
	r.mu.Lock()
	expectedUser, ok := r.nonces[nonce]
	if ok {
		delete(r.nonces, nonce)
	}
	r.mu.Unlock()

	if !ok || expectedUser != username {
		return false
	}

	realm := params["realm"]
	uri := params["uri"]
	nc := params["nc"]
	cnonce := params["cnonce"]
	qop := params["qop"]
	response := params["response"]

	// Calculate expected response
	ha1 := md5hex(username + ":" + realm + ":" + password)
	ha2 := md5hex(method + ":" + uri)

	var expected string
	if qop == "auth" {
		expected = md5hex(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2)
	} else {
		expected = md5hex(ha1 + ":" + nonce + ":" + ha2)
	}

	return response == expected
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

// IsLocalUser checks if username belongs to a registered local user.
func (r *Registrar) IsLocalUser(username string) bool {
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

func (r *Registrar) generateNonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// parseDigestParams parses Digest auth header value into key-value pairs.
func parseDigestParams(value string) map[string]string {
	params := make(map[string]string)

	// Skip "Digest " prefix
	if len(value) > 7 && (value[:7] == "Digest " || value[:7] == "digest ") {
		value = value[7:]
	}

	// Simple parser for key="value" or key=value pairs
	for len(value) > 0 {
		// Skip whitespace
		for len(value) > 0 && (value[0] == ' ' || value[0] == ',') {
			value = value[1:]
		}
		if len(value) == 0 {
			break
		}

		// Find key
		eq := 0
		for eq < len(value) && value[eq] != '=' {
			eq++
		}
		if eq >= len(value) {
			break
		}
		key := value[:eq]
		value = value[eq+1:]

		// Find value
		var val string
		if len(value) > 0 && value[0] == '"' {
			// Quoted value
			value = value[1:]
			end := 0
			for end < len(value) && value[end] != '"' {
				end++
			}
			val = value[:end]
			if end < len(value) {
				value = value[end+1:]
			} else {
				value = ""
			}
		} else {
			// Unquoted value
			end := 0
			for end < len(value) && value[end] != ',' && value[end] != ' ' {
				end++
			}
			val = value[:end]
			value = value[end:]
		}

		params[key] = val
	}

	return params
}
