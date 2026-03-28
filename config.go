package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type Config struct {
	SIP       SIPConfig       `json:"sip"`
	Uplink    UplinkConfig    `json:"uplink"`
	Users     []UserConfig    `json:"users"`
	Dialplan  DialplanConfig  `json:"dialplan"`
	Recording RecordingConfig `json:"recording"`
	PostCall  PostCallConfig  `json:"post_call"`
}

type SIPConfig struct {
	ListenAddr   string `json:"listen_addr"`
	ListenPort   int    `json:"listen_port"`
	ExternalIP   string `json:"external_ip"`   // public IP for NAT
	ExternalPort int    `json:"external_port"` // public SIP port (default: listen_port)
	RTPPortMin   int    `json:"rtp_port_min"`
	RTPPortMax   int    `json:"rtp_port_max"`
}

type UplinkConfig struct {
	Host           string `json:"host"`
	Port           int    `json:"port"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	Expiry         int    `json:"expiry"`            // seconds
	FilterCalledNo bool   `json:"filter_called_no"`  // only accept calls to our registered number
	FilterSourceIP bool   `json:"filter_source_ip"`  // only accept calls from uplink server IP
}

type UserConfig struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type DialplanConfig struct {
	InternalMaxDigits int `json:"internal_max_digits"` // <= this many digits = internal call
}

type RecordingConfig struct {
	Enabled      bool   `json:"enabled"`
	Dir          string `json:"dir"`
	Announcement string `json:"announcement"`  // WAV file to play before recording (EU compliance)
	AnswerFirst  bool   `json:"answer_first"`  // answer caller before ringing phones (plays announcement first)
}

type PostCallConfig struct {
	Script string `json:"script"` // path to external script
}

func (c *UplinkConfig) ExpiryDuration() time.Duration {
	if c.Expiry <= 0 {
		return 300 * time.Second
	}
	return time.Duration(c.Expiry) * time.Second
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := &Config{
		SIP: SIPConfig{
			ListenAddr: "0.0.0.0",
			ListenPort: 5060,
			RTPPortMin: 10000,
			RTPPortMax: 10200,
		},
		Dialplan: DialplanConfig{
			InternalMaxDigits: 3,
		},
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Uplink.Host == "" {
		return nil, fmt.Errorf("uplink.host is required")
	}
	if cfg.Uplink.Username == "" {
		return nil, fmt.Errorf("uplink.username is required")
	}
	if len(cfg.Users) == 0 {
		return nil, fmt.Errorf("at least one user is required")
	}
	if cfg.Recording.Enabled && cfg.Recording.Dir == "" {
		return nil, fmt.Errorf("recording.dir is required when recording is enabled")
	}

	if cfg.SIP.ExternalPort == 0 {
		cfg.SIP.ExternalPort = cfg.SIP.ListenPort
	}

	return cfg, nil
}
