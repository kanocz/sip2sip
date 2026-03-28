package main

import (
	"context"
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

	// Create recording dir if needed
	if cfg.Recording.Enabled {
		if err := os.MkdirAll(cfg.Recording.Dir, 0755); err != nil {
			log.Error("Failed to create recording dir", "err", err)
			os.Exit(1)
		}
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
	registrar := NewRegistrar(cfg.Users, realm, log.With("component", "registrar"))

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

	// Single transport — server listens here, Contact headers use external IP/port
	transport := diago.Transport{
		Transport: "udp",
		BindHost:  cfg.SIP.ListenAddr,
		BindPort:  cfg.SIP.ListenPort,
	}
	if cfg.SIP.ExternalIP != "" {
		transport.ExternalHost = cfg.SIP.ExternalIP
	}
	if cfg.SIP.ExternalPort > 0 {
		transport.ExternalPort = cfg.SIP.ExternalPort
	}

	// Create Diago instance with our server and client
	dg := diago.NewDiago(ua,
		diago.WithServer(srv),
		diago.WithClient(client),
		diago.WithTransport(transport),
		diago.WithLogger(log.With("component", "diago")),
	)

	// Create call handler
	handler := NewCallHandler(dg, registrar, cfg, log.With("component", "calls"))

	// Context with signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info("Shutting down", "signal", sig)
		cancel()
	}()

	// Start serving
	log.Info("Starting SIP server", "addr", cfg.SIP.ListenAddr, "port", cfg.SIP.ListenPort)
	if err := dg.ServeBackground(ctx, handler.HandleInvite); err != nil {
		log.Error("Failed to start server", "err", err)
		os.Exit(1)
	}

	// Register on uplink using our own registration handler.
	// This gives us full control over digest auth and debug output.
	uplinkContact := sip.ContactHeader{
		Address: sip.Uri{
			Scheme: "sip",
			User:   cfg.Uplink.Username,
			Host:   cfg.SIP.ExternalIP,
			Port:   cfg.SIP.ExternalPort,
		},
	}

	uplinkReg := NewUplinkRegistrar(client, &cfg.Uplink, uplinkContact, log.With("component", "uplink"))

	log.Info("Registering on uplink", "host", cfg.Uplink.Host, "user", cfg.Uplink.Username)
	err = uplinkReg.Run(ctx)
	if err != nil && ctx.Err() == nil {
		log.Error("Registration failed", "err", err)
		os.Exit(1)
	}

	log.Info("Shutdown complete")
}
