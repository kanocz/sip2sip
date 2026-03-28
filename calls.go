package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	uplinkIPs []net.IP // resolved uplink server IPs for filtering
}

func NewCallHandler(dg *diago.Diago, reg *Registrar, cfg *Config, log *slog.Logger) *CallHandler {
	h := &CallHandler{
		dg:        dg,
		registrar: reg,
		cfg:       cfg,
		log:       log,
	}
	// Resolve uplink IPs for incoming call filtering
	if ips, err := net.LookupIP(cfg.Uplink.Host); err == nil {
		h.uplinkIPs = ips
		log.Info("Resolved uplink IPs", "host", cfg.Uplink.Host, "ips", ips)
	} else {
		log.Error("Failed to resolve uplink host", "host", cfg.Uplink.Host, "err", err)
	}
	return h
}

func (h *CallHandler) HandleInvite(inDialog *diago.DialogServerSession) {
	fromUser := inDialog.FromUser()
	toUser := inDialog.ToUser()

	h.log.Info("Incoming INVITE", "from", fromUser, "to", toUser)

	if h.registrar.IsLocalUser(fromUser) {
		h.handleOutgoing(inDialog, fromUser, toUser)
		return
	}

	// Filter incoming calls based on config
	if h.cfg.Uplink.FilterCalledNo && toUser != h.cfg.Uplink.Username {
		h.log.Warn("Rejected INVITE: wrong called number", "from", fromUser, "to", toUser, "expected", h.cfg.Uplink.Username)
		inDialog.Respond(sip.StatusNotFound, "Not Found", nil)
		return
	}
	if h.cfg.Uplink.FilterSourceIP && !h.isFromUplink(inDialog) {
		source := inDialog.InviteRequest.Source()
		h.log.Warn("Rejected INVITE: unknown source IP", "from", fromUser, "to", toUser, "source", source)
		inDialog.Respond(sip.StatusForbidden, "Forbidden", nil)
		return
	}

	h.handleIncoming(inDialog, fromUser, toUser)
}

// isFromUplink checks if the INVITE came from the uplink server's IP.
func (h *CallHandler) isFromUplink(inDialog *diago.DialogServerSession) bool {
	source := inDialog.InviteRequest.Source()
	sourceHost, _, err := net.SplitHostPort(source)
	if err != nil {
		return false
	}
	sourceIP := net.ParseIP(sourceHost)
	if sourceIP == nil {
		return false
	}
	for _, ip := range h.uplinkIPs {
		if ip.Equal(sourceIP) {
			return true
		}
	}
	return false
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

	regs := h.registrar.GetAllRegistrations()
	if len(regs) == 0 {
		log.Warn("No registered phones")
		inDialog.Respond(sip.StatusTemporarilyUnavailable, "No phones registered", nil)
		cdr.HangupBy = "timeout"
		return
	}

	// answer_first mode: answer caller → announcement → ringback + fork → bridge
	if h.cfg.Recording.AnswerFirst && h.cfg.Recording.Announcement != "" {
		h.handleIncomingAnswerFirst(inDialog, regs, cdr, log)
		return
	}

	// Normal mode: ring phones first → answer after pickup
	inDialog.Ringing()

	log.Info("Forking to registered phones", "count", len(regs))

	winner, err := h.forkCall(inDialog, regs, log)
	if err != nil || winner == nil {
		log.Warn("No phone answered", "err", err)
		inDialog.Respond(sip.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil)
		cdr.HangupBy = "timeout"
		return
	}
	defer winner.Close()

	h.enableRTPNAT(winner)


	cdr.Answered = true
	cdr.AnswerTime = time.Now()
	cdr.AnsweredBy = winner.ToUser()

	if err := inDialog.Answer(); err != nil {
		log.Error("Failed to answer incoming call", "err", err)
		winner.Hangup(context.Background())
		cdr.HangupBy = "caller"
		return
	}


	log.Info("Call connected", "answered_by", winner.ToUser())
	h.bridgeWithRecording(inDialog, winner, cdr, log)
}

// handleIncomingAnswerFirst answers the caller first, plays announcement,
// then forks to phones with ringback tone for the caller.
func (h *CallHandler) handleIncomingAnswerFirst(inDialog *diago.DialogServerSession, regs []*Registration, cdr *CDR, log *slog.Logger) {
	// Answer the caller immediately
	if err := inDialog.Answer(); err != nil {
		log.Error("Failed to answer incoming call", "err", err)
		cdr.HangupBy = "caller"
		return
	}


	// Set up recording BEFORE announcement so everything is captured:
	// - Left channel: caller's audio from the moment we answer
	// - Right channel: announcement + ringback + then callee's voice
	var rec *recordingState
	if h.cfg.Recording.Enabled {
		wavPath, closeFn, err := h.setupRecording(inDialog, cdr, log)
		if err != nil {
			log.Error("Failed to setup recording, continuing without", "err", err)
		} else {
			rec = &recordingState{wavPath: wavPath, closeFn: closeFn}
		}
	}

	// Drain caller's RTP through the recording-wrapped reader.
	// This captures caller audio on the left channel during announcement + ringtone.
	// Also prevents buffer accumulation that causes audio desync.
	var drainStop atomic.Bool
	drainDone := make(chan struct{})
	if callerReader, err := inDialog.AudioReader(); err == nil {
		go func() {
			defer close(drainDone)
			buf := make([]byte, media.RTPBufSize)
			for !drainStop.Load() {
				if _, err := callerReader.Read(buf); err != nil {
					return
				}
			}
		}()
	} else {
		close(drainDone)
	}

	// Brief pause after answer — caller's audio path needs a moment to establish,
	// otherwise the first second of the announcement gets clipped.
	time.Sleep(1000 * time.Millisecond)

	// Play announcement to caller (goes through recording-wrapped writer → right channel)
	log.Info("Playing announcement to caller", "file", h.cfg.Recording.Announcement)
	pbCaller, err := inDialog.PlaybackCreate()
	if err != nil {
		log.Error("Failed to create playback for announcement", "err", err)
	} else {
		if _, err := pbCaller.PlayFile(h.cfg.Recording.Announcement); err != nil {
			log.Error("Announcement playback failed", "err", err)
		}
	}

	log.Info("Forking to registered phones", "count", len(regs))

	// Play ringback tone to caller while phones are ringing
	var stopRingtone func() error
	ringtone, err := inDialog.PlaybackRingtoneCreate()
	if err == nil {
		stopRingtone, err = ringtone.PlayBackground()
		if err != nil {
			log.Error("Failed to start ringtone", "err", err)
		}
	} else {
		log.Error("Failed to create ringtone", "err", err)
	}

	// Fork call to all registered phones
	winner, forkErr := h.forkCall(inDialog, regs, log)

	// Stop ringback
	if stopRingtone != nil {
		if err := stopRingtone(); err != nil {
			log.Debug("Ringtone stop error", "err", err)
		}
	}

	// Stop drain before bridge takes over the reader
	drainStop.Store(true)
	select {
	case <-drainDone:
	case <-time.After(100 * time.Millisecond):
	}

	if forkErr != nil || winner == nil {
		log.Warn("No phone answered", "err", forkErr)
		if rec != nil && rec.closeFn != nil {
			rec.closeFn()
			cdr.RecordingFile = rec.wavPath
		}
		inDialog.Hangup(context.Background())
		cdr.HangupBy = "timeout"
		return
	}
	defer winner.Close()

	h.enableRTPNAT(winner)


	cdr.Answered = true
	cdr.AnswerTime = time.Now()
	cdr.AnsweredBy = winner.ToUser()

	log.Info("Call connected", "answered_by", winner.ToUser())
	h.bridgeWithRecordingState(inDialog, winner, cdr, log, rec)
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
		h.handleInternalCall(inDialog, callerNum, calledNum, cdr, log)
	} else {
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

	h.enableRTPNAT(outDialog)


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

// enableRTPNAT enables symmetric RTP on the outgoing dialog.
// Always enabled because we can't distinguish LAN private IPs (192.168.x.x)
// from carrier CGNAT private IPs (10.x.x.x) — both appear as private.
// Symmetric RTP is safe for LAN phones too (learns correct source on first packet).
func (h *CallHandler) enableRTPNAT(d *diago.DialogClientSession) {
	if ms := d.MediaSession(); ms != nil {
		ms.RTPNAT = media.RTPNATSymetric
	}
}


// bridgeWithRecording proxies audio between two dialog sessions with optional stereo recording.
// If recState is non-nil, recording is already set up (answer_first mode).
func (h *CallHandler) bridgeWithRecording(inDialog *diago.DialogServerSession, outDialog *diago.DialogClientSession, cdr *CDR, log *slog.Logger) {
	h.bridgeWithRecordingState(inDialog, outDialog, cdr, log, nil)
}

type recordingState struct {
	wavPath  string
	closeFn  func() error
}

func (h *CallHandler) bridgeWithRecordingState(inDialog *diago.DialogServerSession, outDialog *diago.DialogClientSession, cdr *CDR, log *slog.Logger, rec *recordingState) {
	// Set up recording if not already done (answer_first sets it up earlier)
	if rec == nil && h.cfg.Recording.Enabled {
		wavPath, closeFn, err := h.setupRecording(inDialog, cdr, log)
		if err != nil {
			log.Error("Failed to setup recording, continuing without", "err", err)
		} else {
			rec = &recordingState{wavPath: wavPath, closeFn: closeFn}
		}
	}

	if rec != nil && rec.closeFn != nil {
		defer func() {
			if err := rec.closeFn(); err != nil {
				log.Error("Failed to close recording", "err", err)
			}
			cdr.RecordingFile = rec.wavPath
		}()
	}

	// Play announcement to both sides if configured and not in answer_first mode
	// (answer_first plays announcement earlier in the flow)
	if h.cfg.Recording.Announcement != "" && h.cfg.Recording.Enabled && !h.cfg.Recording.AnswerFirst {
		h.playAnnouncement(inDialog, outDialog, log)
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

	// Log media session addresses for NAT debugging
	if inMS := inDialog.MediaSession(); inMS != nil {
		log.Debug("inDialog media", "local", inMS.Laddr.String(), "remote", inMS.Raddr.String(), "rtpnat", inMS.RTPNAT)
	}
	if outMS := outDialog.MediaSession(); outMS != nil {
		log.Debug("outDialog media", "local", outMS.Laddr.String(), "remote", outMS.Raddr.String(), "rtpnat", outMS.RTPNAT)
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

	outDialog.Hangup(context.Background())
	inDialog.Hangup(context.Background())
}

// playAnnouncement plays a WAV file to both call parties simultaneously.
func (h *CallHandler) playAnnouncement(inDialog *diago.DialogServerSession, outDialog *diago.DialogClientSession, log *slog.Logger) {
	log.Info("Playing announcement", "file", h.cfg.Recording.Announcement)

	pbCaller, err1 := inDialog.PlaybackCreate()
	pbCallee, err2 := outDialog.PlaybackCreate()
	if err1 != nil || err2 != nil {
		log.Error("Failed to create playback for announcement", "err_in", err1, "err_out", err2)
		return
	}

	// Drain caller's audio during announcement so it gets recorded on the left channel
	var drainStop atomic.Bool
	drainDone := make(chan struct{})
	callerReader, err := inDialog.AudioReader()
	if err == nil {
		go func() {
			defer close(drainDone)
			buf := make([]byte, media.RTPBufSize)
			for !drainStop.Load() {
				if _, err := callerReader.Read(buf); err != nil {
					return
				}
			}
		}()
	} else {
		close(drainDone)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := pbCaller.PlayFile(h.cfg.Recording.Announcement); err != nil {
			log.Error("Announcement playback failed (caller side)", "err", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := pbCallee.PlayFile(h.cfg.Recording.Announcement); err != nil {
			log.Error("Announcement playback failed (callee side)", "err", err)
		}
	}()
	wg.Wait()

	drainStop.Store(true)
	select {
	case <-drainDone:
	case <-time.After(100 * time.Millisecond):
	}

	log.Debug("Announcement finished")
}

// setupRecording sets up stereo WAV recording on the incoming dialog.
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

	mon := rec.AudioReader()
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

	cdrDir := h.cfg.Recording.Dir
	if cdrDir == "" {
		cdrDir = "."
	}
	jsonPath, err := SaveCDR(cdr, cdrDir)
	if err != nil {
		log.Error("Failed to save CDR", "err", err)
		return
	}

	wavPath := cdr.RecordingFile
	if wavPath == "" {
		wavPath = "none"
	}
	go RunPostCallScript(h.cfg.PostCall.Script, jsonPath, wavPath, log)
}
