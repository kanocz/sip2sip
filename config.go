package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"
)

type Config struct {
	SIP           SIPConfig           `json:"sip"`
	Uplink        UplinkConfig        `json:"uplink"`
	Users         []UserConfig        `json:"users"`
	MaxCalls      int                 `json:"max_calls"` // concurrent calls in both directions, default 3
	AuthGuard     AuthGuardConfig     `json:"auth_guard"`
	Dialplan      DialplanConfig      `json:"dialplan"`
	Recording     RecordingConfig     `json:"recording"`
	PostCall      PostCallConfig      `json:"post_call"`
	Voicemail     VoicemailConfig     `json:"voicemail"`
	BusinessHours BusinessHoursConfig `json:"business_hours"`
	Vacation      VacationConfig      `json:"vacation"`
}

type SIPConfig struct {
	ListenAddr   string `json:"listen_addr"`
	ListenPort   int    `json:"listen_port"`   // UDP and TCP
	ExternalIP   string `json:"external_ip"`   // public IP for NAT
	ExternalPort int    `json:"external_port"` // public SIP port (default: listen_port)
	TLSPort      int    `json:"tls_port"`      // TLS listener, default 5061; needs tls_cert and tls_key
	TLSCert      string `json:"tls_cert"`      // PEM certificate chain; empty disables the TLS listener
	TLSKey       string `json:"tls_key"`       // PEM private key
	RTPPortMin   int    `json:"rtp_port_min"`
	RTPPortMax   int    `json:"rtp_port_max"`
}

type UplinkConfig struct {
	Host           string `json:"host"`
	Port           int    `json:"port"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	Transport      string `json:"transport"`        // udp (default), tcp or tls
	Expiry         int    `json:"expiry"`           // seconds
	FilterCalledNo bool   `json:"filter_called_no"` // only accept calls to our registered number
}

type UserConfig struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// AuthGuardConfig blocks a source IP after MaxGuesses distinct wrong
// login/password guesses (REGISTER or INVITE) within Window seconds, for
// BlockTime seconds. Repeating the same wrong guess counts once.
type AuthGuardConfig struct {
	Enabled    bool `json:"enabled"`     // default true
	MaxGuesses int  `json:"max_guesses"` // default 10
	Window     int  `json:"window"`      // seconds, default 300
	BlockTime  int  `json:"block_time"`  // seconds, default 3600
}

type DialplanConfig struct {
	InternalMaxDigits int `json:"internal_max_digits"` // <= this many digits = internal call
}

type RecordingConfig struct {
	Enabled      bool   `json:"enabled"`
	Dir          string `json:"dir"`          // recordings, voicemail and CDR files (default ./recordings)
	Announcement string `json:"announcement"` // WAV file to play before recording (EU compliance)
	AnswerFirst  bool   `json:"answer_first"` // answer caller before ringing phones (plays announcement first)
}

type PostCallConfig struct {
	Script string `json:"script"` // path to external script
}

type VoicemailConfig struct {
	Enabled                 bool   `json:"enabled"`
	UnavailableAnnouncement string `json:"unavailable_announcement"` // WAV file for "no agents available"
	SilenceTimeout          int    `json:"silence_timeout"`          // seconds of silence to stop recording (default 10)
	MaxDuration             int    `json:"max_duration"`             // max recording seconds (default 120)
}

type BusinessHoursConfig struct {
	Enabled                  bool     `json:"enabled"`
	Timezone                 string   `json:"timezone"`                   // IANA timezone (e.g. "Europe/Prague")
	Days                     []string `json:"days"`                       // e.g. ["mon","tue","wed","thu","fri"]
	StartTime                string   `json:"start_time"`                 // "HH:MM"
	EndTime                  string   `json:"end_time"`                   // "HH:MM"
	OutsideHoursAnnouncement string   `json:"outside_hours_announcement"` // WAV file for outside hours
}

type VacationConfig struct {
	Announcement string           `json:"announcement"` // default WAV file played during vacation
	Periods      []VacationPeriod `json:"periods"`
}

// VacationPeriod is an inclusive date range in YYYY-MM-DD, in the business_hours timezone.
type VacationPeriod struct {
	From         string `json:"from"`
	To           string `json:"to"`
	Announcement string `json:"announcement"` // optional, overrides vacation.announcement
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
			TLSPort:    5061,
			RTPPortMin: 10000,
			RTPPortMax: 10200,
		},
		AuthGuard: AuthGuardConfig{
			Enabled:    true,
			MaxGuesses: 10,
			Window:     300,
			BlockTime:  3600,
		},
		Uplink:   UplinkConfig{Transport: "udp"},
		MaxCalls: 3,
		Dialplan: DialplanConfig{
			InternalMaxDigits: 3,
		},
		Recording: RecordingConfig{
			Dir: "./recordings",
		},
		Voicemail: VoicemailConfig{
			SilenceTimeout: 10,
			MaxDuration:    120,
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
	cfg.Uplink.Transport = strings.ToLower(cfg.Uplink.Transport)
	switch cfg.Uplink.Transport {
	case "":
		cfg.Uplink.Transport = "udp"
	case "udp", "tcp", "tls":
	default:
		return nil, fmt.Errorf("uplink.transport must be udp, tcp or tls, got %q", cfg.Uplink.Transport)
	}
	if (cfg.SIP.TLSCert == "") != (cfg.SIP.TLSKey == "") {
		return nil, fmt.Errorf("sip.tls_cert and sip.tls_key must be set together")
	}
	if cfg.SIP.TLSCert != "" && (cfg.SIP.TLSPort <= 0 || cfg.SIP.TLSPort == cfg.SIP.ListenPort) {
		return nil, fmt.Errorf("sip.tls_port must be positive and differ from sip.listen_port")
	}
	// Outgoing INVITEs go through diago, which has a TLS transport only when it
	// also listens on it; REGISTER alone would work and the failure would show
	// up on the first outgoing call.
	if cfg.Uplink.Transport == "tls" && cfg.SIP.TLSCert == "" {
		return nil, fmt.Errorf("uplink.transport tls needs sip.tls_cert and sip.tls_key")
	}
	// The example config carries port 5060; a provider's TLS usually lives on 5061.
	if cfg.Uplink.Transport == "tls" && cfg.Uplink.Port == 5060 {
		slog.Warn("uplink.port is 5060 with transport tls; providers usually serve TLS on 5061, check if registration fails")
	}
	if len(cfg.Users) == 0 {
		return nil, fmt.Errorf("at least one user is required")
	}
	seen := make(map[string]bool, len(cfg.Users))
	for i, u := range cfg.Users {
		if u.Username == "" || u.Password == "" {
			return nil, fmt.Errorf("users[%d]: username and password are required", i)
		}
		if seen[u.Username] {
			return nil, fmt.Errorf("users[%d]: duplicate username %q", i, u.Username)
		}
		seen[u.Username] = true
	}
	if cfg.SIP.RTPPortMin <= 0 || cfg.SIP.RTPPortMax < cfg.SIP.RTPPortMin {
		return nil, fmt.Errorf("sip.rtp_port_min/rtp_port_max: invalid range %d-%d", cfg.SIP.RTPPortMin, cfg.SIP.RTPPortMax)
	}
	if cfg.MaxCalls <= 0 {
		return nil, fmt.Errorf("max_calls must be positive")
	}
	if cfg.SIP.ExternalIP == "" {
		// The uplink Contact and every SDP carry this address, so it has to be a real one.
		if ip := net.ParseIP(cfg.SIP.ListenAddr); ip == nil || ip.IsUnspecified() {
			return nil, fmt.Errorf("sip.external_ip is required when sip.listen_addr is %q", cfg.SIP.ListenAddr)
		}
		cfg.SIP.ExternalIP = cfg.SIP.ListenAddr
	}
	if cfg.Recording.Dir == "" {
		return nil, fmt.Errorf("recording.dir is required (CDR and voicemail files are stored there too)")
	}
	if cfg.Voicemail.SilenceTimeout <= 0 || cfg.Voicemail.MaxDuration <= 0 {
		return nil, fmt.Errorf("voicemail.silence_timeout and max_duration must be positive")
	}
	if g := cfg.AuthGuard; g.MaxGuesses <= 0 || g.Window <= 0 || g.BlockTime <= 0 {
		return nil, fmt.Errorf("auth_guard.max_guesses, window and block_time must be positive")
	}

	for i, p := range cfg.Vacation.Periods {
		from, errFrom := time.Parse(dateLayout, p.From)
		to, errTo := time.Parse(dateLayout, p.To)
		if errFrom != nil || errTo != nil {
			return nil, fmt.Errorf("vacation.periods[%d]: dates must be YYYY-MM-DD", i)
		}
		if to.Before(from) {
			return nil, fmt.Errorf("vacation.periods[%d]: to (%s) is before from (%s)", i, p.To, p.From)
		}
	}

	// A wrong timezone or schedule must not silently send every call to voicemail:
	// fall back to UTC / disable the schedule and say so.
	if tz := cfg.BusinessHours.Timezone; tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			slog.Warn("Invalid business_hours.timezone, using UTC", "timezone", tz, "err", err)
			cfg.BusinessHours.Timezone = "UTC"
		}
	}
	if cfg.BusinessHours.Enabled {
		if err := cfg.BusinessHours.validate(); err != nil {
			slog.Warn("Invalid business_hours, schedule disabled", "err", err)
			cfg.BusinessHours.Enabled = false
		}
	}

	announcements := []string{
		cfg.Recording.Announcement,
		cfg.Voicemail.UnavailableAnnouncement,
		cfg.BusinessHours.OutsideHoursAnnouncement,
		cfg.Vacation.Announcement,
	}
	for _, p := range cfg.Vacation.Periods {
		announcements = append(announcements, p.Announcement)
	}
	for _, f := range announcements {
		if f == "" {
			continue
		}
		if _, err := os.Stat(f); err != nil {
			slog.Warn("Announcement file not accessible", "file", f, "err", err)
		}
	}

	if cfg.SIP.ExternalPort == 0 {
		cfg.SIP.ExternalPort = cfg.SIP.ListenPort
	}

	return cfg, nil
}
