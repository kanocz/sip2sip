package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// certReloadInterval is how often the certificate and key files are checked for a change.
const certReloadInterval = time.Hour

// certWatcher serves the TLS certificate from certFile and keyFile and picks up
// a renewed pair without a restart. A pair that fails to load is ignored and
// the previous one stays in use.
type certWatcher struct {
	certFile, keyFile string
	cert              atomic.Pointer[tls.Certificate]
	log               *slog.Logger

	mu      sync.Mutex // serialises reload from the ticker and from SIGHUP
	modTime time.Time  // newest mtime of the two files at the last successful load
}

func newCertWatcher(certFile, keyFile string, log *slog.Logger) (*certWatcher, error) {
	w := &certWatcher{certFile: certFile, keyFile: keyFile, log: log}
	if err := w.load(); err != nil {
		return nil, err
	}
	return w, nil
}

// get is tls.Config.GetCertificate.
func (w *certWatcher) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return w.cert.Load(), nil
}

func (w *certWatcher) load() error {
	// mtime first: a file rewritten between the two calls is loaded again next time.
	mt := w.newestModTime()
	cert, err := tls.LoadX509KeyPair(w.certFile, w.keyFile)
	if err != nil {
		return err
	}
	w.cert.Store(&cert)
	w.modTime = mt
	return nil
}

func (w *certWatcher) newestModTime() time.Time {
	var t time.Time
	for _, f := range []string{w.certFile, w.keyFile} {
		if st, err := os.Stat(f); err == nil && st.ModTime().After(t) {
			t = st.ModTime()
		}
	}
	return t
}

// reload loads the files again if either changed since the last successful load.
func (w *certWatcher) reload() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.newestModTime().After(w.modTime) {
		return
	}
	if err := w.load(); err != nil {
		w.log.Error("TLS certificate changed but failed to load, keeping the previous one", "cert", w.certFile, "err", err)
		return
	}
	w.log.Info("TLS certificate reloaded", "cert", w.certFile)
}

// watch calls reload every certReloadInterval until ctx is done.
func (w *certWatcher) watch(ctx context.Context) {
	t := time.NewTicker(certReloadInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.reload()
		}
	}
}
