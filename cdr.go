package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type CDR struct {
	CallID       string    `json:"call_id"`
	Direction    string    `json:"direction"` // "incoming" or "outgoing"
	CallerNumber string    `json:"caller_number"`
	CalledNumber string    `json:"called_number"`
	AnsweredBy   string    `json:"answered_by,omitempty"` // local extension that answered
	StartTime    time.Time `json:"start_time"`
	AnswerTime   time.Time `json:"answer_time,omitempty"`
	EndTime      time.Time `json:"end_time"`
	Duration     float64   `json:"duration_seconds"`    // total duration from start
	TalkTime     float64   `json:"talk_time_seconds"`   // time after answer
	Answered        bool    `json:"answered"`
	Voicemail       bool    `json:"voicemail"`
	MessageDuration float64 `json:"message_duration_seconds,omitempty"` // voicemail message duration (after beep)
	HangupBy        string  `json:"hangup_by"`                         // "caller", "callee", "timeout", "voicemail"
	RecordingFile   string  `json:"recording_file,omitempty"`
}

func (c *CDR) ComputeDurations() {
	c.Duration = c.EndTime.Sub(c.StartTime).Seconds()
	if c.Answered && !c.AnswerTime.IsZero() {
		c.TalkTime = c.EndTime.Sub(c.AnswerTime).Seconds()
	}
}

func SaveCDR(cdr *CDR, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create CDR dir: %w", err)
	}

	filename := fmt.Sprintf("%s_%s.json", cdr.StartTime.Format("20060102_150405"), cdr.CallID)
	path := filepath.Join(dir, filename)

	data, err := json.MarshalIndent(cdr, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal CDR: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return "", fmt.Errorf("write CDR: %w", err)
	}

	return path, nil
}

func RunPostCallScript(script string, jsonPath string, wavPath string, log *slog.Logger) {
	if script == "" {
		return
	}

	log.Info("Running post-call script", "script", script, "json", jsonPath, "wav", wavPath)

	cmd := exec.Command(script, jsonPath, wavPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		log.Error("Post-call script failed", "err", err)
	}
}
