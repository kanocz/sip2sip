package main

import (
	"log/slog"
	"testing"

	"github.com/emiago/sipgo/sip"
)

// rport names our NAT mapping on UDP, but only the ephemeral connection port on
// TCP: the Contact must keep the listener port there.
func TestUpdateContactFromViaKeepsPortOnTCP(t *testing.T) {
	for _, c := range []struct {
		transport string
		wantPort  int
	}{{"udp", 40000}, {"tcp", 5060}, {"tls", 5061}} {
		port := 5060
		if c.transport == "tls" {
			port = 5061
		}
		cfg := &UplinkConfig{Host: "sip.example", Transport: c.transport}
		r := NewUplinkRegistrar(nil, cfg, sip.ContactHeader{Address: sip.Uri{User: "u", Host: "198.51.100.1", Port: port}}, slog.Default())

		req := sip.NewRequest(sip.REGISTER, sip.Uri{Host: cfg.Host})
		via := &sip.ViaHeader{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: c.transport, Host: "198.51.100.1", Port: port, Params: sip.NewParams()}
		req.AppendHeader(via)
		res := sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)
		res.Via().Params.Add("rport", "40000")
		res.Via().Params.Add("received", "203.0.113.5")

		r.updateContactFromVia(res, req)
		if got := r.contact.Address; got.Port != c.wantPort || got.Host != "203.0.113.5" {
			t.Errorf("%s: contact %s, want host 203.0.113.5 port %d", c.transport, got.String(), c.wantPort)
		}
	}
}

func TestUplinkContactPerTransport(t *testing.T) {
	for _, c := range []struct{ transport, want string }{
		{"udp", "sip:u@198.51.100.1:5060"},
		{"tcp", "sip:u@198.51.100.1:5060;transport=tcp"},
		{"tls", "sips:u@198.51.100.1:5061"},
	} {
		cfg := &Config{SIP: SIPConfig{ExternalIP: "198.51.100.1", ExternalPort: 5060, TLSPort: 5061}, Uplink: UplinkConfig{Username: "u", Transport: c.transport}}
		h := uplinkContact(cfg)
		if got := h.Address.String(); got != c.want {
			t.Errorf("%s: got %s, want %s", c.transport, got, c.want)
		}
	}
}
