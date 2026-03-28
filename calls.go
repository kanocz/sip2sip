package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
)

type CallHandler struct {
	dg        *diago.Diago
	registrar *Registrar
	cfg       *Config
	log       *slog.Logger
}

func NewCallHandler(dg *diago.Diago, reg *Registrar, cfg *Config, log *slog.Logger) *CallHandler {
	return &CallHandler{
		dg:        dg,
		registrar: reg,
		cfg:       cfg,
		log:       log,
	}
}

func (h *CallHandler) HandleInvite(inDialog *diago.DialogServerSession) {
	fromUser := inDialog.FromUser()
	toUser := inDialog.ToUser()

	h.log.Info("Incoming INVITE", "from", fromUser, "to", toUser)

	if h.registrar.IsLocalUser(fromUser) {
		h.handleOutgoing(inDialog, fromUser, toUser)
	} else {
		h.handleIncoming(inDialog, fromUser, toUser)
	}
}

// handleIncoming handles calls from external (uplink) - fork to all registered phones.
func (h *CallHandler) handleIncoming(inDialog *diago.DialogServerSession, callerNum, calledNum string) {
	callID := uuid.New().String()[:8]
	log := h.log.With("call_id", callID, "direction", "incoming", "caller", callerNum)

	cdr := &CDR{
		CallID:       callID,
		Direction:    "incoming",
		CallerNumber: callerNum,
		CalledNumber: calledNum,
		StartTime:    time.Now(),
	}

	defer func() {
		cdr.EndTime = time.Now()
		cdr.ComputeDurations()
		h.finishCall(cdr, log)
	}()

	inDialog.Trying()
	inDialog.Ringing()

	regs := h.registrar.GetAllRegistrations()
	if len(regs) == 0 {
		log.Warn("No registered phones")
		inDialog.Respond(sip.StatusTemporarilyUnavailable, "No phones registered", nil)
		cdr.HangupBy = "timeout"
		return
	}

	log.Info("Forking to registered phones", "count", len(regs))

	// Fork call to all registered phones
	winner, err := h.forkCall(inDialog, regs, log)
	if err != nil || winner == nil {
		log.Warn("No phone answered", "err", err)
		inDialog.Respond(sip.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil)
		cdr.HangupBy = "timeout"
		return
	}
	defer winner.Close()

	cdr.Answered = true
	cdr.AnswerTime = time.Now()
	cdr.AnsweredBy = winner.ToUser()

	// Answer the incoming side
	if err := inDialog.Answer(); err != nil {
		log.Error("Failed to answer incoming call", "err", err)
		winner.Hangup(context.Background())
		cdr.HangupBy = "caller"
		return
	}

	log.Info("Call connected", "answered_by", winner.ToUser())

	// Bridge with recording
	h.bridgeWithRecording(inDialog, winner, cdr, log)
}

// handleOutgoing handles calls from local phones - route to uplink or internal.
func (h *CallHandler) handleOutgoing(inDialog *diago.DialogServerSession, callerNum, calledNum string) {
	callID := uuid.New().String()[:8]
	log := h.log.With("call_id", callID, "direction", "outgoing", "caller", callerNum, "called", calledNum)

	cdr := &CDR{
		CallID:       callID,
		Direction:    "outgoing",
		CallerNumber: callerNum,
		CalledNumber: calledNum,
		StartTime:    time.Now(),
	}

	defer func() {
		cdr.EndTime = time.Now()
		cdr.ComputeDurations()
		h.finishCall(cdr, log)
	}()

	inDialog.Trying()

	if len(calledNum) <= h.cfg.Dialplan.InternalMaxDigits {
		// Internal call
		h.handleInternalCall(inDialog, callerNum, calledNum, cdr, log)
	} else {
		// External call via uplink
		h.handleUplinkCall(inDialog, callerNum, calledNum, cdr, log)
	}
}

func (h *CallHandler) handleInternalCall(inDialog *diago.DialogServerSession, callerNum, calledNum string, cdr *CDR, log *slog.Logger) {
	reg := h.registrar.GetRegistration(calledNum)
	if reg == nil {
		log.Warn("Internal call: user not registered", "called", calledNum)
		inDialog.Respond(sip.StatusNotFound, "Not Found", nil)
		cdr.HangupBy = "timeout"
		return
	}

	inDialog.Ringing()

	uri := h.registrar.ContactURI(reg)
	log.Info("Internal call", "target_uri", uri.String())

	ctx, cancel := context.WithTimeout(inDialog.Context(), 60*time.Second)
	defer cancel()

	outDialog, err := h.dg.Invite(ctx, uri, diago.InviteOptions{
		Originator: inDialog,
	})
	if err != nil {
		log.Warn("Internal call: invite failed", "err", err)
		inDialog.Respond(sip.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil)
		cdr.HangupBy = "timeout"
		return
	}
	defer outDialog.Close()

	cdr.Answered = true
	cdr.AnswerTime = time.Now()
	cdr.AnsweredBy = calledNum

	if err := inDialog.Answer(); err != nil {
		log.Error("Failed to answer", "err", err)
		outDialog.Hangup(context.Background())
		return
	}

	log.Info("Internal call connected")
	h.bridgeWithRecording(inDialog, outDialog, cdr, log)
}

func (h *CallHandler) handleUplinkCall(inDialog *diago.DialogServerSession, callerNum, calledNum string, cdr *CDR, log *slog.Logger) {
	inDialog.Ringing()

	uplinkURI := sip.Uri{
		User: calledNum,
		Host: h.cfg.Uplink.Host,
		Port: h.cfg.Uplink.Port,
	}

	log.Info("Outgoing call via uplink", "uri", uplinkURI.String())

	ctx, cancel := context.WithTimeout(inDialog.Context(), 60*time.Second)
	defer cancel()

	outDialog, err := h.dg.Invite(ctx, uplinkURI, diago.InviteOptions{
		Originator: inDialog,
		Username:   h.cfg.Uplink.Username,
		Password:   h.cfg.Uplink.Password,
	})
	if err != nil {
		log.Warn("Uplink call failed", "err", err)
		inDialog.Respond(sip.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil)
		cdr.HangupBy = "timeout"
		return
	}
	defer outDialog.Close()

	cdr.Answered = true
	cdr.AnswerTime = time.Now()

	if err := inDialog.Answer(); err != nil {
		log.Error("Failed to answer", "err", err)
		outDialog.Hangup(context.Background())
		return
	}

	log.Info("Outgoing call connected")
	h.bridgeWithRecording(inDialog, outDialog, cdr, log)
}

// forkCall invites all registered phones in parallel, returns the first to answer.
func (h *CallHandler) forkCall(inDialog *diago.DialogServerSession, regs []*Registration, log *slog.Logger) (*diago.DialogClientSession, error) {
	ctx, cancel := context.WithTimeout(inDialog.Context(), 30*time.Second)
	defer cancel()

	var (
		mu     sync.Mutex
		winner *diago.DialogClientSession
		wg     sync.WaitGroup
	)

	for _, reg := range regs {
		uri := h.registrar.ContactURI(reg)
		wg.Add(1)
		go func(u sip.Uri, username string) {
			defer wg.Done()

			log.Info("Inviting phone", "user", username, "uri", u.String())

			d, err := h.dg.Invite(ctx, u, diago.InviteOptions{
				Originator: inDialog,
			})
			if err != nil {
				log.Debug("Phone didn't answer", "user", username, "err", err)
				return
			}

			mu.Lock()
			if winner == nil {
				winner = d
				cancel() // Cancel remaining invites
				mu.Unlock()
				log.Info("Phone answered", "user", username)
			} else {
				mu.Unlock()
				// Another phone answered after winner - hang up
				d.Hangup(context.Background())
				d.Close()
			}
		}(uri, reg.Username)
	}

	wg.Wait()

	if winner == nil {
		return nil, fmt.Errorf("no phones answered")
	}
	return winner, nil
}

// bridgeWithRecording proxies audio between two dialog sessions with optional stereo recording.
func (h *CallHandler) bridgeWithRecording(inDialog *diago.DialogServerSession, outDialog *diago.DialogClientSession, cdr *CDR, log *slog.Logger) {
	var wavPath string
	var recCloser func() error

	if h.cfg.Recording.Enabled {
		var err error
		wavPath, recCloser, err = h.setupRecording(inDialog, cdr, log)
		if err != nil {
			log.Error("Failed to setup recording, continuing without", "err", err)
		}
	}

	if recCloser != nil {
		// Recording is set up - use the recording's monitor as the audio pipeline
		// The SetAudioReader/SetAudioWriter has already been called in setupRecording
		defer func() {
			if err := recCloser(); err != nil {
				log.Error("Failed to close recording", "err", err)
			}
			cdr.RecordingFile = wavPath
		}()
	}

	// Get audio readers/writers
	aReader, err := inDialog.AudioReader()
	if err != nil {
		log.Error("Failed to get audio reader for incoming side", "err", err)
		return
	}
	aWriter, err := inDialog.AudioWriter()
	if err != nil {
		log.Error("Failed to get audio writer for incoming side", "err", err)
		return
	}

	bReader, err := outDialog.AudioReader()
	if err != nil {
		log.Error("Failed to get audio reader for outgoing side", "err", err)
		return
	}
	bWriter, err := outDialog.AudioWriter()
	if err != nil {
		log.Error("Failed to get audio writer for outgoing side", "err", err)
		return
	}

	// Bridge: proxy audio between both sides
	done := make(chan string, 2)

	go func() {
		media.Copy(aReader, bWriter) // A → B
		done <- "caller"
	}()
	go func() {
		media.Copy(bReader, aWriter) // B → A
		done <- "callee"
	}()

	// Wait for one side to hang up
	hangupBy := <-done
	cdr.HangupBy = hangupBy
	log.Info("Call ended", "hangup_by", hangupBy)

	// Hang up the other side
	outDialog.Hangup(context.Background())
	inDialog.Hangup(context.Background())
}

// setupRecording sets up stereo WAV recording on the incoming dialog.
// It uses AudioStereoRecordingCreate which wraps the dialog's audio reader/writer.
// After this call, inDialog.AudioReader() and AudioWriter() will go through the recording pipeline.
func (h *CallHandler) setupRecording(inDialog *diago.DialogServerSession, cdr *CDR, log *slog.Logger) (string, func() error, error) {
	if err := os.MkdirAll(h.cfg.Recording.Dir, 0755); err != nil {
		return "", nil, fmt.Errorf("create recording dir: %w", err)
	}

	filename := fmt.Sprintf("%s_%s.wav", cdr.StartTime.Format("20060102_150405"), cdr.CallID)
	wavPath := filepath.Join(h.cfg.Recording.Dir, filename)

	wavFile, err := os.Create(wavPath)
	if err != nil {
		return "", nil, fmt.Errorf("create wav file: %w", err)
	}

	rec, err := inDialog.AudioStereoRecordingCreate(wavFile)
	if err != nil {
		wavFile.Close()
		os.Remove(wavPath)
		return "", nil, fmt.Errorf("create recording: %w", err)
	}

	// Replace the dialog's audio reader and writer with the recording monitors
	// This way, any subsequent AudioReader()/AudioWriter() calls will go through recording
	mon := rec.AudioReader() // same object as rec.AudioWriter()
	inDialog.SetAudioReader(mon)
	inDialog.SetAudioWriter(mon)

	log.Info("Recording started", "file", wavPath)

	closer := func() error {
		err1 := rec.Close()
		err2 := wavFile.Close()
		if err1 != nil {
			return err1
		}
		return err2
	}

	return wavPath, closer, nil
}

func (h *CallHandler) finishCall(cdr *CDR, log *slog.Logger) {
	log.Info("Call finished",
		"answered", cdr.Answered,
		"duration", cdr.Duration,
		"talk_time", cdr.TalkTime,
	)

	// Save CDR
	cdrDir := h.cfg.Recording.Dir
	if cdrDir == "" {
		cdrDir = "."
	}
	jsonPath, err := SaveCDR(cdr, cdrDir)
	if err != nil {
		log.Error("Failed to save CDR", "err", err)
		return
	}

	// Run post-call script in background
	wavPath := cdr.RecordingFile
	if wavPath == "" {
		wavPath = "none"
	}
	go RunPostCallScript(h.cfg.PostCall.Script, jsonPath, wavPath, log)
}
