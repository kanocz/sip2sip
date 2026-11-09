package main

import (
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

func testRegistrar(t *testing.T) *Registrar {
	t.Helper()
	cfg := &Config{
		Users:     []UserConfig{{Username: "100", Password: "secret"}},
		AuthGuard: AuthGuardConfig{Enabled: true, MaxGuesses: 3, Window: 300, BlockTime: 3600},
	}
	return NewRegistrar(cfg, "sip2sip", slog.Default())
}

func inviteAs(user, source string) *sip.Request {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "+420123", Host: "pbx"})
	req.AppendHeader(&sip.FromHeader{Address: sip.Uri{User: user, Host: "pbx"}})
	req.SetSource(source)
	return req
}

func inviteFrom(source string) *sip.Request {
	return inviteAs("100", source)
}

// answerAs builds an Authorization header for the challenge in res.
func answerAs(t *testing.T, res *sip.Response, user, password string) string {
	t.Helper()
	chal, err := digest.ParseChallenge(res.GetHeader("WWW-Authenticate").Value())
	if err != nil {
		t.Fatal(err)
	}
	cred, err := digest.Digest(chal, digest.Options{Method: "INVITE", URI: "sip:+420123@pbx", Username: user, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	return cred.String()
}

func answer(t *testing.T, res *sip.Response, password string) string {
	return answerAs(t, res, "100", password)
}

func TestAuthorizeDigest(t *testing.T) {
	r := testRegistrar(t)
	src := "10.0.0.5:5060"

	res := r.Authorize(inviteFrom(src), "100")
	if res == nil || res.StatusCode != 401 {
		t.Fatalf("want 401 challenge, got %v", res)
	}

	// wrong password: new challenge, not authorized
	req := inviteFrom(src)
	req.AppendHeader(sip.NewHeader("Authorization", answer(t, res, "wrong")))
	if res2 := r.Authorize(req, "100"); res2 == nil || res2.StatusCode != 401 {
		t.Fatalf("wrong password must get 401, got %v", res2)
	}

	// right password against a fresh challenge
	res = r.Authorize(inviteFrom(src), "100")
	req = inviteFrom(src)
	req.AppendHeader(sip.NewHeader("Authorization", answer(t, res, "secret")))
	if got := r.Authorize(req, "100"); got != nil {
		t.Fatalf("valid credentials rejected: %v", got)
	}

	// nonce is single use; the retry is told it was stale, not wrong
	got := r.Authorize(req, "100")
	if got == nil {
		t.Fatal("replayed credentials accepted")
	}
	if chal, err := digest.ParseChallenge(got.GetHeader("WWW-Authenticate").Value()); err != nil || !chal.Stale {
		t.Fatalf("answer to a used nonce must get stale=true, got %v %v", got.GetHeader("WWW-Authenticate"), err)
	}
}

// tryPassword answers a fresh challenge for user with password and returns the status code.
func tryPassword(t *testing.T, r *Registrar, src, user, password string) int {
	t.Helper()
	res := r.Authorize(inviteAs(user, src), user)
	if res == nil || res.StatusCode != 401 {
		t.Fatalf("want 401 challenge, got %v", res)
	}
	req := inviteAs(user, src)
	req.AppendHeader(sip.NewHeader("Authorization", answerAs(t, res, user, password)))
	got := r.Authorize(req, user)
	if got == nil {
		return 200
	}
	return got.StatusCode
}

func TestSameWrongPasswordIsOneGuess(t *testing.T) {
	r := testRegistrar(t)
	src := "203.0.113.8:5060"
	for i := 0; i < 10; i++ {
		if code := tryPassword(t, r, src, "100", "typo"); code != 401 {
			t.Fatalf("retry %d of the same wrong password: want 401, got %d", i, code)
		}
	}
	if code := tryPassword(t, r, src, "100", "secret"); code != 200 {
		t.Fatalf("correct password after retries must succeed, got %d", code)
	}
}

func TestAuthGuardBlocks(t *testing.T) {
	r := testRegistrar(t)
	src := "203.0.113.9:5060"

	for i, pw := range []string{"guess1", "guess2"} {
		if code := tryPassword(t, r, src, "100", pw); code != 401 {
			t.Fatalf("attempt %d: want 401, got %d", i, code)
		}
	}
	// third distinct guess trips the guard
	if code := tryPassword(t, r, src, "100", "guess3"); code != 403 {
		t.Fatalf("want 403 after 3 distinct guesses, got %d", code)
	}
	// blocked even without credentials, other IPs unaffected
	if got := r.Authorize(inviteFrom(src), "100"); got.StatusCode != 403 {
		t.Fatalf("blocked IP must get 403, got %d", got.StatusCode)
	}
	if got := r.Authorize(inviteFrom("198.51.100.1:5060"), "100"); got.StatusCode != 401 {
		t.Fatalf("other IP must get 401, got %d", got.StatusCode)
	}

	// disabling the guard on reload lifts the block
	r.UpdateConfig(&Config{Users: []UserConfig{{Username: "100", Password: "secret"}}})
	if got := r.Authorize(inviteFrom(src), "100"); got.StatusCode != 401 {
		t.Fatalf("guard disabled, want 401, got %d", got.StatusCode)
	}
}

// An unknown login gets the same 401 as a real one, and answering it counts
// as a guess: enumerating logins is how extensions get found.
func TestUnknownUserIsChallengedAndCounted(t *testing.T) {
	r := testRegistrar(t)
	src := "203.0.113.7:5060"

	for i := 0; i < 5; i++ {
		if code := tryPassword(t, r, src, "999", "x"); code != 401 {
			t.Fatalf("one unknown login retried must stay at 401, got %d", code)
		}
	}
	tryPassword(t, r, src, "998", "x")
	if code := tryPassword(t, r, src, "997", "x"); code != 403 {
		t.Fatalf("IP enumerating 3 logins must be blocked, got %d", code)
	}
}

// Requests that never answered one of our nonces are not counted: with UDP the
// source address can be forged, and the forger does not see our challenge.
func TestSpoofedFailuresAreNotCounted(t *testing.T) {
	r := testRegistrar(t)
	src := "203.0.113.6:5060"

	for i := 0; i < 10; i++ {
		chal := &digest.Challenge{Realm: "sip2sip", Nonce: fmt.Sprintf("bogus%d", i), Algorithm: "MD5"}
		cred, err := digest.Digest(chal, digest.Options{Method: "INVITE", URI: "sip:+420123@pbx", Username: "100", Password: fmt.Sprintf("pw%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		req := inviteFrom(src)
		req.AppendHeader(sip.NewHeader("Authorization", cred.String()))
		if got := r.Authorize(req, "100"); got == nil || got.StatusCode != 401 {
			t.Fatalf("made-up nonce %d: want 401, got %v", i, got)
		}
	}
	for i := 0; i < 10; i++ {
		user := fmt.Sprintf("u%d", i)
		if got := r.Authorize(inviteAs(user, src), user); got == nil || got.StatusCode != 401 {
			t.Fatalf("unanswered challenge for %s: want 401, got %v", user, got)
		}
	}
	if got := r.Authorize(inviteFrom(src), "100"); got.StatusCode != 401 {
		t.Fatalf("IP must not be blocked by unanswered or forged requests, got %d", got.StatusCode)
	}
}

func TestNonceExpiry(t *testing.T) {
	r := testRegistrar(t)
	res := r.Authorize(inviteFrom("10.0.0.5:5060"), "100")
	chal, _ := digest.ParseChallenge(res.GetHeader("WWW-Authenticate").Value())
	r.mu.Lock()
	r.nonces[chal.Nonce] = nonceEntry{user: "100", created: time.Now().Add(-2 * nonceLifetime)}
	r.mu.Unlock()
	req := inviteFrom("10.0.0.5:5060")
	req.AppendHeader(sip.NewHeader("Authorization", answer(t, res, "secret")))
	if got := r.Authorize(req, "100"); got == nil {
		t.Fatal("expired nonce accepted")
	}
}

func TestPendingIsSweptWithNonces(t *testing.T) {
	r := testRegistrar(t)
	r.Authorize(inviteFrom("10.0.0.1:5060"), "100")
	r.Authorize(inviteFrom("10.0.0.2:5060"), "100")

	r.mu.Lock()
	for n, e := range r.nonces {
		e.created = e.created.Add(-2 * nonceLifetime)
		r.nonces[n] = e
	}
	r.mu.Unlock()

	r.Authorize(inviteFrom("10.0.0.3:5060"), "100")
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.nonces) != 1 || len(r.pending) != 1 {
		t.Fatalf("want 1 live nonce and 1 pending key, got %d and %d", len(r.nonces), len(r.pending))
	}
}

func TestRegisterExpiry(t *testing.T) {
	cases := []struct {
		header, param string
		want          int
		ok            bool
	}{
		{"3600", "", maxRegisterExpiry, true},
		{"60", "", 60, true},
		{"0", "", 0, true},
		{"", "", maxRegisterExpiry, true},
		{"", "600", maxRegisterExpiry, true},
		{"", "30", 30, true},
		{"-1", "", 0, false},
		{"abc", "", 0, false},
	}
	for _, c := range cases {
		req := sip.NewRequest(sip.REGISTER, sip.Uri{Host: "pbx"})
		if c.header != "" {
			req.AppendHeader(sip.NewHeader("Expires", c.header))
		}
		contact := &sip.ContactHeader{Address: sip.Uri{User: "100", Host: "10.0.0.5"}, Params: sip.NewParams()}
		if c.param != "" {
			contact.Params.Add("expires", c.param)
		}
		got, ok := registerExpiry(req, contact)
		if got != c.want || ok != c.ok {
			t.Errorf("Expires=%q contact expires=%q: got (%d, %v), want (%d, %v)", c.header, c.param, got, ok, c.want, c.ok)
		}
	}
}

func TestCleanNumber(t *testing.T) {
	for in, want := range map[string]string{
		"+420 123-456 (7)": "+420123-456(7)",
		"</etc/passwd":     "",
		"anonymous":        "",
		"100":              "100",
	} {
		if got := cleanNumber(in); got != want {
			t.Errorf("cleanNumber(%q) = %q, want %q", in, got, want)
		}
	}
}

// A signed INVITE redialled to another number must not pass: the digest uri=
// still names the original callee, the request line does not.
func TestAuthorizeRejectsRewrittenRequestURI(t *testing.T) {
	r := testRegistrar(t)
	src := "10.0.0.6:5060"

	res := r.Authorize(inviteFrom(src), "100")
	req := inviteFrom(src)
	req.Recipient.User = "+900premium"
	req.AppendHeader(sip.NewHeader("Authorization", answer(t, res, "secret")))
	if got := r.Authorize(req, "100"); got == nil || got.StatusCode != 401 {
		t.Fatalf("rewritten Request-URI must be rejected with 401, got %v", got)
	}
}

func TestContactURICarriesTransport(t *testing.T) {
	r := testRegistrar(t)
	reg := &Registration{Username: "100", Source: "192.0.2.9:49152", Transport: "tcp"}
	u := r.ContactURI(reg)
	if got := u.String(); got != "sip:100@192.0.2.9:49152;transport=tcp" {
		t.Fatalf("tcp registration: got %s", got)
	}
	reg.Transport = "udp"
	u = r.ContactURI(reg)
	if got := u.String(); got != "sip:100@192.0.2.9:49152" {
		t.Fatalf("udp registration: got %s", got)
	}
}
