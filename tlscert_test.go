package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSelfSigned(t *testing.T, certFile, keyFile string, serial int64) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
}

// touch moves the files' mtime forward so the watcher sees a change regardless of fs granularity.
func touch(t *testing.T, when time.Time, files ...string) {
	t.Helper()
	for _, f := range files {
		if err := os.Chtimes(f, when, when); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCertWatcherReload(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	writeSelfSigned(t, certFile, keyFile, 1)

	w, err := newCertWatcher(certFile, keyFile, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	first, _ := w.get(nil)

	// Unchanged files: nothing happens.
	w.reload()
	if got, _ := w.get(nil); got != first {
		t.Fatal("reload without a change replaced the certificate")
	}

	// A broken key with a newer mtime: the loaded pair stays.
	os.WriteFile(keyFile, []byte("garbage"), 0600)
	touch(t, time.Now().Add(time.Minute), keyFile)
	w.reload()
	if got, _ := w.get(nil); got != first {
		t.Fatal("a pair that fails to load replaced the certificate")
	}

	// A valid new pair is picked up.
	writeSelfSigned(t, certFile, keyFile, 2)
	touch(t, time.Now().Add(2*time.Minute), certFile, keyFile)
	w.reload()
	got, _ := w.get(nil)
	if got == first {
		t.Fatal("renewed certificate was not loaded")
	}
	if got.Leaf == nil || got.Leaf.SerialNumber.Int64() != 2 {
		t.Fatalf("loaded certificate has serial %v, want 2", got.Leaf)
	}
}
