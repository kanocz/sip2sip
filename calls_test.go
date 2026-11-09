package main

import (
	"log/slog"
	"net"
	"testing"

	"github.com/emiago/sipgo/sip"
)

// The uplink is trusted on its configured transport only: a UDP packet can
// carry any source address, a TCP connection cannot.
func TestIsFromUplinkRequiresConfiguredTransport(t *testing.T) {
	h := &CallHandler{log: slog.Default()}
	h.cfg.Store(&Config{Uplink: UplinkConfig{Transport: "tcp"}})
	h.uplinkIPs.Store(&[]net.IP{net.ParseIP("203.0.113.1")})

	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "x", Host: "pbx"})
	req.SetSource("203.0.113.1:5060")
	req.SetTransport("UDP")
	if h.isFromUplink(req) {
		t.Fatal("UDP INVITE from the uplink IP must not be trusted when uplink.transport is tcp")
	}
	req.SetTransport("TCP")
	if !h.isFromUplink(req) {
		t.Fatal("TCP INVITE from the uplink IP must be trusted")
	}
	req.SetSource("198.51.100.7:5060")
	if h.isFromUplink(req) {
		t.Fatal("INVITE from another IP must not be trusted")
	}

	// A UDP uplink that switches a large INVITE to TCP is still the uplink.
	h.cfg.Store(&Config{Uplink: UplinkConfig{Transport: "udp"}})
	req.SetSource("203.0.113.1:5060")
	req.SetTransport("TCP")
	if !h.isFromUplink(req) {
		t.Fatal("TCP INVITE from the uplink IP must be trusted when uplink.transport is udp")
	}
}
