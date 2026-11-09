package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type CDR struct {
	CallID          string    `json:"call_id"`
	Direction       string    `json:"direction"` // "incoming" or "outgoing"
	CallerNumber    string    `json:"caller_number"`
	CalledNumber    string    `json:"called_number"`
	AnsweredBy      string    `json:"answered_by,omitempty"` // local extension that answered
	StartTime       time.Time `json:"start_time"`
	AnswerTime      time.Time `json:"answer_time,omitempty"`
	EndTime         time.Time `json:"end_time"`
	Duration        float64   `json:"duration_seconds"`  // total duration from start
	TalkTime        float64   `json:"talk_time_seconds"` // time after answer
	Answered        bool      `json:"answered"`
	Voicemail       bool      `json:"voicemail"`
	VoicemailReason string    `json:"voicemail_reason,omitempty"`         // "vacation", "outside_hours", "unavailable"
	MessageDuration float64   `json:"message_duration_seconds,omitempty"` // voicemail message duration (after beep, without the trailing silence)
	HangupBy        string    `json:"hangup_by"`                          // "caller", "callee", "timeout", "voicemail", "system"
	RecordingFile   string    `json:"recording_file,omitempty"`
	Error           string    `json:"error,omitempty"` // recording or voicemail failure the operator should hear about
}

func (c *CDR) ComputeDurations() {
	c.Duration = c.EndTime.Sub(c.StartTime).Seconds()
	if c.Answered && !c.AnswerTime.IsZero() {
		c.TalkTime = c.EndTime.Sub(c.AnswerTime).Seconds()
	}
}

// cleanNumber keeps digits, "+", "-" and brackets. The From/To user comes from
// the network and ends up in post-call shell scripts, so nothing else gets through.
func cleanNumber(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || strings.ContainsRune("+-()", r) {
			return r
		}
		return -1
	}, s)
}

// Recordings and CDRs hold call audio and phone numbers: owner-only permissions.
const (
	dirPerm  = 0700
	filePerm = 0600
)

func SaveCDR(cdr *CDR, dir string) (string, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return "", fmt.Errorf("create CDR dir: %w", err)
	}

	filename := fmt.Sprintf("%s_%s.json", cdr.StartTime.Format("20060102_150405"), cdr.CallID)
	path := filepath.Join(dir, filename)

	data, err := json.MarshalIndent(cdr, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal CDR: %w", err)
	}

	if err := os.WriteFile(path, data, filePerm); err != nil {
		return "", fmt.Errorf("write CDR: %w", err)
	}

	return path, nil
}

// postCallTimeout bounds the script run so a hung upload does not pile up goroutines.
const postCallTimeout = 5 * time.Minute

func RunPostCallScript(script string, jsonPath string, wavPath string, log *slog.Logger) {
	if script == "" {
		return
	}

	log.Info("Running post-call script", "script", script, "json", jsonPath, "wav", wavPath)

	ctx, cancel := context.WithTimeout(context.Background(), postCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, script, jsonPath, wavPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		log.Error("Post-call script failed", "err", err)
	}
}
