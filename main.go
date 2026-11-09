package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

func main() {
	configPath := flag.String("config", "config.json", "path to config file")
	debug := flag.Bool("debug", false, "enable SIP message tracing and debug logging")
	flag.Parse()

	logLevel := slog.LevelInfo
	if *debug {
		logLevel = slog.LevelDebug
		sip.SIPDebug = true
	}
	log := slog.New(newFilterHandler(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})))
	// sipgo's SIPDebug uses sip.DefaultLogger() which falls back to slog.Default()
	sip.SetDefaultLogger(log)
	slog.SetDefault(log)

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Error("Failed to load config", "err", err)
		os.Exit(1)
	}

	log.Info("Config loaded",
		"listen", cfg.SIP.ListenAddr,
		"port", cfg.SIP.ListenPort,
		"uplink", cfg.Uplink.Host,
		"users", len(cfg.Users),
	)

	// Set RTP port range
	if cfg.SIP.RTPPortMin > 0 && cfg.SIP.RTPPortMax > 0 {
		media.RTPPortStart = cfg.SIP.RTPPortMin
		media.RTPPortEnd = cfg.SIP.RTPPortMax
	}

	// Recordings, voicemail and CDRs all go here, so it is needed regardless of recording.enabled
	if err := os.MkdirAll(cfg.Recording.Dir, dirPerm); err != nil {
		log.Error("Failed to create recording dir", "err", err)
		os.Exit(1)
	}

	// Create SIP User Agent
	// UA name is used as the From/Contact user in SIP messages,
	// so it must match the uplink SIP username for registration to work.
	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(cfg.Uplink.Username),
		sipgo.WithUserAgentHostname(cfg.Uplink.Host),
	)
	if err != nil {
		log.Error("Failed to create UA", "err", err)
		os.Exit(1)
	}

	// Create a global client with NAT support.
	// Using a separate client avoids the port conflict where diago's per-transport
	// client tries to bind the same port as the server listener.
	// Contact headers are still built from the transport config (external IP/port).
	clientOpts := []sipgo.ClientOption{sipgo.WithClientNAT()}
	if cfg.SIP.ExternalIP != "" {
		clientOpts = append(clientOpts,
			sipgo.WithClientHostname(cfg.SIP.ExternalIP),
			sipgo.WithClientPort(cfg.SIP.ExternalPort),
		)
	}
	client, err := sipgo.NewClient(ua, clientOpts...)
	if err != nil {
		log.Error("Failed to create client", "err", err)
		os.Exit(1)
	}

	// Registrar for local phones
	realm := "sip2sip"
	registrar := NewRegistrar(cfg, realm, log.With("component", "registrar"))

	// Create sipgo Server with REGISTER and OPTIONS handlers.
	// These must be registered directly on the server because diago's middleware
	// only wraps the INVITE handler — other methods get 405 by default.
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		log.Error("Failed to create server", "err", err)
		os.Exit(1)
	}
	srv.OnRegister(registrar.HandleRegister)
	srv.OnOptions(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		tx.Respond(res)
	})

	// UDP and TCP share listen_port; TLS is on when a certificate is configured.
	// Contact headers use the external IP/port of each transport.
	transports := []diago.Transport{
		{Transport: "udp", BindHost: cfg.SIP.ListenAddr, BindPort: cfg.SIP.ListenPort, ExternalHost: cfg.SIP.ExternalIP, ExternalPort: cfg.SIP.ExternalPort},
		{Transport: "tcp", BindHost: cfg.SIP.ListenAddr, BindPort: cfg.SIP.ListenPort, ExternalHost: cfg.SIP.ExternalIP, ExternalPort: cfg.SIP.ExternalPort},
	}
	var certs *certWatcher
	if cfg.SIP.TLSCert != "" {
		certs, err = newCertWatcher(cfg.SIP.TLSCert, cfg.SIP.TLSKey, log.With("component", "tls"))
		if err != nil {
			log.Error("Failed to load TLS certificate", "err", err)
			os.Exit(1)
		}
		transports = append(transports, diago.Transport{
			Transport: "tls", BindHost: cfg.SIP.ListenAddr, BindPort: cfg.SIP.TLSPort, ExternalHost: cfg.SIP.ExternalIP, ExternalPort: cfg.SIP.TLSPort,
			// Contacts on this transport are sips: URIs. diago adds ;transport= only
			// when listen_addr equals external_ip, and a plain sip:host:5061 without
			// it means UDP to the peer, where nothing listens.
			TLSConf: &tls.Config{GetCertificate: certs.get, MinVersion: tls.VersionTLS12},
		})
	}

	// Create Diago instance with our server and client
	opts := []diago.DiagoOption{
		diago.WithServer(srv),
		diago.WithClient(client),
		diago.WithLogger(log.With("component", "diago")),
	}
	for _, t := range transports {
		opts = append(opts, diago.WithTransport(t))
	}
	dg := diago.NewDiago(ua, opts...)

	// Create call handler
	handler := NewCallHandler(dg, registrar, cfg, log.With("component", "calls"))

	// Context with signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range sigCh {
			if sig == syscall.SIGHUP {
				log.Info("Reloading configuration", "signal", sig)
				newCfg, err := LoadConfig(*configPath)
				if err != nil {
					log.Error("Failed to reload config", "err", err)
					continue
				}
				// The uplink registration and the SIP transport were built from the
				// startup config; outgoing calls must keep using the same account.
				if newCfg.Uplink.Host != cfg.Uplink.Host || newCfg.Uplink.Port != cfg.Uplink.Port || newCfg.Uplink.Transport != cfg.Uplink.Transport ||
					newCfg.Uplink.Username != cfg.Uplink.Username || newCfg.Uplink.Password != cfg.Uplink.Password {
					log.Warn("uplink host/port/transport/credentials changed: restart required, keeping the old values")
					newCfg.Uplink.Host, newCfg.Uplink.Port, newCfg.Uplink.Transport = cfg.Uplink.Host, cfg.Uplink.Port, cfg.Uplink.Transport
					newCfg.Uplink.Username, newCfg.Uplink.Password = cfg.Uplink.Username, cfg.Uplink.Password
				}
				if newCfg.SIP != cfg.SIP {
					log.Warn("sip section changed: restart required, keeping the old values")
					newCfg.SIP = cfg.SIP
				}
				handler.UpdateConfig(newCfg)
				registrar.UpdateConfig(newCfg)
				if certs != nil {
					certs.reload()
				}
				log.Info("Configuration reloaded", "users", len(newCfg.Users))
				continue
			}
			log.Info("Shutting down", "signal", sig)
			cancel()
			return
		}
	}()

	// Start serving
	log.Info("Starting SIP server", "addr", cfg.SIP.ListenAddr, "port", cfg.SIP.ListenPort)
	if err := dg.ServeBackground(ctx, handler.HandleInvite); err != nil {
		log.Error("Failed to start server", "err", err)
		os.Exit(1)
	}
	if certs != nil {
		go certs.watch(ctx)
	}

	// Register on uplink using our own registration handler.
	// This gives us full control over digest auth and debug output.
	uplinkReg := NewUplinkRegistrar(client, &cfg.Uplink, uplinkContact(cfg), log.With("component", "uplink"))

	log.Info("Registering on uplink", "host", cfg.Uplink.Host, "user", cfg.Uplink.Username)
	uplinkReg.Run(ctx)

	log.Info("Shutdown complete")
}
