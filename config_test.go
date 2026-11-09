package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadConfigString(t *testing.T, users string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := `{"sip": {"external_ip": "198.51.100.1"}, "uplink": {"host": "sip.example", "username": "u"}, "users": ` + users + `}`
	if err := os.WriteFile(path, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	return err
}

func TestLoadConfigUsers(t *testing.T) {
	cases := []struct{ users, wantErr string }{
		{`[{"username": "100", "password": "x"}]`, ""},
		{`[{"username": "100", "password": ""}]`, "users[0]"},
		{`[{"username": "", "password": "x"}]`, "users[0]"},
		{`[{"username": "100", "password": "x"}, {"username": "100", "password": "y"}]`, "duplicate"},
	}
	for _, c := range cases {
		err := loadConfigString(t, c.users)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.users, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: want error containing %q, got %v", c.users, c.wantErr, err)
		}
	}
}

func TestLoadConfigTLSUplinkNeedsCert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := `{"sip": {"external_ip": "198.51.100.1"}, "uplink": {"host": "sip.example", "username": "u", "transport": "tls"}, "users": [{"username": "100", "password": "x"}]}`
	if err := os.WriteFile(path, []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "tls_cert") {
		t.Fatalf("uplink.transport tls without a certificate must be rejected, got %v", err)
	}
}
