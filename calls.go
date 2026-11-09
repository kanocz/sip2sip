package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
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
	cfg       atomic.Pointer[Config]
	log       *slog.Logger
	uplinkIPs atomic.Pointer[[]net.IP] // resolved uplink server IPs: only they may send us incoming calls
	active    atomic.Int32             // calls in progress, both directions
}

// uplinkResolveInterval re-resolves the uplink host: providers rotate DNS and
// an INVITE from a server that appeared later must not be rejected.
const uplinkResolveInterval = time.Hour

func NewCallHandler(dg *diago.Diago, reg *Registrar, cfg *Config, log *slog.Logger) *CallHandler {
	h := &CallHandler{
		dg:        dg,
		registrar: reg,
		log:       log,
	}
	h.cfg.Store(cfg)
	h.uplinkIPs.Store(&[]net.IP{})
	h.resolveUplink()
	go func() {
		for {
			// While the list is empty every incoming call is rejected, so do not wait an hour.
			wait := uplinkResolveInterval
			if len(*h.uplinkIPs.Load()) == 0 {
				wait = time.Minute
			}
			time.Sleep(wait)
			h.resolveUplink()
		}
	}()
	return h
}

// resolveUplink refreshes the uplink IP list. On DNS failure the old list stays.
func (h *CallHandler) resolveUplink() {
	host := h.getConfig().Uplink.Host
	ips, err := net.LookupIP(host)
	if err != nil {
		h.log.Error("Failed to resolve uplink host", "host", host, "err", err)
		return
	}
	h.uplinkIPs.Store(&ips)
	h.log.Info("Resolved uplink IPs", "host", host, "ips", ips)
}

func (h *CallHandler) getConfig() *Config {
	return h.cfg.Load()
}

func (h *CallHandler) UpdateConfig(c *Config) {
	h.cfg.Store(c)
	h.resolveUplink()
	h.log.Info("Call handler config updated")
}

// acquireCall claims a slot under max_calls. The caller releases it with releaseCall.
func (h *CallHandler) acquireCall() bool {
	if h.active.Add(1) > int32(h.getConfig().MaxCalls) {
		h.active.Add(-1)
		return false
	}
	return true
}

func (h *CallHandler) releaseCall() { h.active.Add(-1) }

func (h *CallHandler) HandleInvite(inDialog *diago.DialogServerSession) {
	cfg := h.getConfig()
	fromUser := inDialog.FromUser()
	toUser := inDialog.ToUser()
	source := inDialog.InviteRequest.Source()
	fromUplink := h.isFromUplink(inDialog.InviteRequest)

	// uplink=false with the provider's address here means the provider sends
	// INVITEs from a host other than uplink.host.
	h.log.Info("Incoming INVITE", "from", fromUser, "to", toUser, "source", source, "uplink", fromUplink)

	if fromUplink {
		if cfg.Uplink.FilterCalledNo && toUser != cfg.Uplink.Username {
			h.log.Warn("Rejected INVITE: wrong called number", "from", fromUser, "to", toUser, "expected", cfg.Uplink.Username)
			inDialog.Respond(sip.StatusNotFound, "Not Found", nil)
			return
		}
		if h.registrar.IsLocalUser(fromUser) {
			h.log.Info("Caller number matches a local extension, treating as an uplink call", "from", fromUser)
		}
		if !h.acquireCall() {
			h.log.Warn("Rejected INVITE: max_calls reached", "from", fromUser, "max", cfg.MaxCalls)
			inDialog.Respond(sip.StatusBusyHere, "Busy Here", nil)
			return
		}
		defer h.releaseCall()
		h.handleIncoming(inDialog, fromUser, toUser)
		return
	}

	// Anyone else must be one of our phones and prove it with digest auth;
	// otherwise the whole internet could place uplink calls on our account.
	// Unknown From users get the same challenge, so the response does not
	// reveal which extensions exist.
	if res := h.registrar.Authorize(inDialog.InviteRequest, fromUser); res != nil {
		var hdrs []sip.Header
		if wa := res.GetHeader("WWW-Authenticate"); wa != nil {
			hdrs = append(hdrs, wa)
		}
		inDialog.Respond(res.StatusCode, res.Reason, nil, hdrs...)
		return
	}
	if !h.acquireCall() {
		h.log.Warn("Rejected INVITE: max_calls reached", "from", fromUser, "max", cfg.MaxCalls)
		inDialog.Respond(sip.StatusBusyHere, "Busy Here", nil)
		return
	}
	defer h.releaseCall()
	h.handleOutgoing(inDialog, fromUser, toUser)
}

// isFromUplink reports whether req came from the uplink server: its IP, over
// the transport uplink.transport names. The transport matters because a UDP
// source address can be forged from anywhere, a TCP or TLS connection cannot.
func (h *CallHandler) isFromUplink(req *sip.Request) bool {
	sourceHost, _, err := net.SplitHostPort(req.Source())
	if err != nil {
		return false
	}
	sourceIP := net.ParseIP(sourceHost)
	if sourceIP == nil {
		return false
	}
	match := false
	for _, ip := range *h.uplinkIPs.Load() {
		if ip.Equal(sourceIP) {
			match = true
			break
		}
	}
	if !match {
		return false
	}
	// Only UDP is forgeable, so only UDP is refused when the uplink is on tcp or
	// tls. A provider on UDP that switches a large INVITE to TCP (RFC 3261 18.1.1)
	// is still the provider.
	want := h.getConfig().Uplink.Transport
	if got := strings.ToLower(req.Transport()); got == "udp" && want != "udp" {
		h.log.Warn("INVITE from the uplink IP over UDP while uplink.transport is not udp, challenged like a phone", "source", req.Source(), "uplink_transport", want)
		return false
	}
	return true
}

// handleIncoming handles calls from external (uplink) - fork to all registered phones.
func (h *CallHandler) handleIncoming(inDialog *diago.DialogServerSession, callerNum, calledNum string) {
	cfg := h.getConfig()
	callID := uuid.New().String()[:8]
	log := h.log.With("call_id", callID, "direction", "incoming", "caller", callerNum)

	cdr := &CDR{
		CallID:       callID,
		Direction:    "incoming",
		CallerNumber: cleanNumber(callerNum),
		CalledNumber: cleanNumber(calledNum),
		StartTime:    time.Now(),
	}

	defer func() {
		cdr.EndTime = time.Now()
		cdr.ComputeDurations()
		h.finishCall(cdr, log)
	}()

	inDialog.Trying()

	if p := CurrentVacation(&cfg.Vacation, cfg.BusinessHours.Timezone); p != nil {
		announcement := cmp.Or(p.Announcement, cfg.Vacation.Announcement)
		log.Info("Vacation mode", "from", p.From, "to", p.To, "announcement", announcement)
		h.handleVoicemail(inDialog, "vacation", announcement, cdr, log)
		return
	}

	// Check business hours
	if !IsWithinBusinessHours(&cfg.BusinessHours) {
		log.Info("Outside business hours")
		h.handleVoicemail(inDialog, "outside_hours", cfg.BusinessHours.OutsideHoursAnnouncement, cdr, log)
		return
	}

	regs := h.registrar.GetAllRegistrations()
	if len(regs) == 0 {
		log.Warn("No registered phones")
		if cfg.Voicemail.UnavailableAnnouncement != "" || cfg.Voicemail.Enabled {
			h.handleVoicemail(inDialog, "unavailable", cfg.Voicemail.UnavailableAnnouncement, cdr, log)
		} else {
			inDialog.Respond(sip.StatusTemporarilyUnavailable, "No phones registered", nil)
			cdr.HangupBy = "timeout"
		}
		return
	}

	// answer_first mode: answer caller → announcement → ringback + fork → bridge
	if cfg.Recording.AnswerFirst && cfg.Recording.Announcement != "" {
		h.handleIncomingAnswerFirst(inDialog, regs, cdr, log)
		return
	}

	// Normal mode: ring phones first → answer after pickup
	inDialog.Ringing()

	log.Info("Forking to registered phones", "count", len(regs))

	winner, winMed, err := h.forkCall(inDialog, regs, log)
	if err != nil || winner == nil {
		log.Warn("No phone answered", "err", err)
		inDialog.Respond(sip.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil)
		cdr.HangupBy = "timeout"
		return
	}
	defer winner.Close()
	defer winMed.Close()

	inMed, err := inDialog.Answer(diago.AnswerOptions{})
	if err != nil {
		log.Error("Failed to answer incoming call", "err", err)
		winner.Hangup(context.Background())
		cdr.HangupBy = "system"
		return
	}
	defer inMed.Close()

	cdr.Answered = true
	cdr.AnswerTime = time.Now()
	cdr.AnsweredBy = winner.ToUser()

	log.Info("Call connected", "answered_by", winner.ToUser())
	h.bridgeWithRecording(inDialog, inMed, winner, winMed, cdr, log, nil)
}

// handleIncomingAnswerFirst answers the caller first, plays announcement,
// then forks to phones with ringback tone for the caller.
func (h *CallHandler) handleIncomingAnswerFirst(inDialog *diago.DialogServerSession, regs []*Registration, cdr *CDR, log *slog.Logger) {
	cfg := h.getConfig()

	// Answer the caller immediately
	inMed, err := inDialog.Answer(diago.AnswerOptions{})
	if err != nil {
		log.Error("Failed to answer incoming call", "err", err)
		cdr.HangupBy = "system"
		return
	}
	defer inMed.Close()

	cdr.Answered = true
	cdr.AnswerTime = time.Now()

	// Set up recording BEFORE announcement so everything is captured:
	// - Left channel: caller's audio from the moment we answer
	// - Right channel: announcement + ringback + then callee's voice
	var rec *recordingState
	if cfg.Recording.Enabled {
		wavPath, closeFn, err := h.setupRecording(inMed, cdr, log)
		if err != nil {
			log.Error("Failed to setup recording, continuing without", "err", err)
			cdr.Error = "recording setup failed: " + err.Error()
		} else {
			rec = &recordingState{wavPath: wavPath, closeFn: closeFn}
		}
	}

	// Drain caller's RTP through the recording-wrapped reader.
	// This captures caller audio on the left channel during announcement + ringtone.
	// Also prevents buffer accumulation that causes audio desync.
	stopDrain := drainAudio(inMed)

	// Brief pause after answer — caller's audio path needs a moment to establish,
	// otherwise the first second of the announcement gets clipped.
	time.Sleep(1000 * time.Millisecond)

	// Play announcement to caller (goes through recording-wrapped writer → right channel)
	log.Info("Playing announcement to caller", "file", cfg.Recording.Announcement)
	pbCaller, err := inMed.PlaybackCreate()
	if err != nil {
		log.Error("Failed to create playback for announcement", "err", err)
	} else {
		if _, err := pbCaller.PlayFile(cfg.Recording.Announcement); err != nil {
			log.Error("Announcement playback failed", "err", err)
		}
	}

	log.Info("Forking to registered phones", "count", len(regs))

	// Play ringback tone to caller while phones are ringing
	var stopRingtone func() error
	ringtone, err := inMed.PlaybackRingtoneCreate()
	if err == nil {
		stopRingtone, err = ringtone.PlayBackground()
		if err != nil {
			log.Error("Failed to start ringtone", "err", err)
		}
	} else {
		log.Error("Failed to create ringtone", "err", err)
	}

	// Fork call to all registered phones
	winner, winMed, forkErr := h.forkCall(inDialog, regs, log)

	// Stop ringback
	if stopRingtone != nil {
		if err := stopRingtone(); err != nil {
			log.Debug("Ringtone stop error", "err", err)
		}
	}

	// Stop drain before bridge takes over the reader
	stopDrain()

	if forkErr != nil || winner == nil {
		log.Warn("No phone answered", "err", forkErr)
		if rec != nil {
			rec.close(cdr, log)
		}
		inDialog.Hangup(context.Background())
		cdr.HangupBy = "timeout"
		return
	}
	defer winner.Close()
	defer winMed.Close()

	cdr.AnsweredBy = winner.ToUser()

	log.Info("Call connected", "answered_by", winner.ToUser())
	h.bridgeWithRecording(inDialog, inMed, winner, winMed, cdr, log, rec)
}

// handleOutgoing handles calls from local phones - route to uplink or internal.
func (h *CallHandler) handleOutgoing(inDialog *diago.DialogServerSession, callerNum, calledNum string) {
	cfg := h.getConfig()
	callID := uuid.New().String()[:8]
	log := h.log.With("call_id", callID, "direction", "outgoing", "caller", callerNum, "called", calledNum)

	cdr := &CDR{
		CallID:       callID,
		Direction:    "outgoing",
		CallerNumber: cleanNumber(callerNum),
		CalledNumber: cleanNumber(calledNum),
		StartTime:    time.Now(),
	}

	defer func() {
		cdr.EndTime = time.Now()
		cdr.ComputeDurations()
		h.finishCall(cdr, log)
	}()

	inDialog.Trying()

	if len(calledNum) <= cfg.Dialplan.InternalMaxDigits {
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

	outDialog, outMed, err := h.dg.Invite(ctx, uri, diago.InviteOptions{
		Originator: inDialog,
		Transport:  reg.Transport,
		RTPNAT:     media.RTPNATSymetric,
	})
	if err != nil {
		log.Warn("Internal call: invite failed", "err", err)
		inDialog.Respond(sip.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil)
		cdr.HangupBy = "timeout"
		return
	}
	defer outDialog.Close()
	defer outMed.Close()

	inMed, err := inDialog.Answer(diago.AnswerOptions{RTPNAT: media.RTPNATSymetric})
	if err != nil {
		log.Error("Failed to answer", "err", err)
		outDialog.Hangup(context.Background())
		cdr.HangupBy = "system"
		return
	}
	defer inMed.Close()

	cdr.Answered = true
	cdr.AnswerTime = time.Now()
	cdr.AnsweredBy = calledNum

	log.Info("Internal call connected")
	h.bridgeWithRecording(inDialog, inMed, outDialog, outMed, cdr, log, nil)
}

func (h *CallHandler) handleUplinkCall(inDialog *diago.DialogServerSession, callerNum, calledNum string, cdr *CDR, log *slog.Logger) {
	cfg := h.getConfig()
	inDialog.Ringing()

	// No ;transport= in the URI: the transport goes through InviteOptions, and
	// the digest uri= sipgo computes has no parameters, so the Request-URI must not either.
	target := sip.Uri{User: calledNum, Host: cfg.Uplink.Host, Port: cfg.Uplink.Port}

	log.Info("Outgoing call via uplink", "uri", target.String(), "transport", cfg.Uplink.Transport)

	ctx, cancel := context.WithTimeout(inDialog.Context(), 60*time.Second)
	defer cancel()

	// No Originator here: with it diago offers the uplink only the first codec
	// from the phone's list, and the call fails if the provider doesn't support it.
	// Without it the uplink gets our full codec list and picks one itself.
	// Keep the phone's From, as Originator would, but with our own dialog tag.
	origFrom := inDialog.InviteRequest.From()
	from := &sip.FromHeader{
		DisplayName: origFrom.DisplayName,
		Address:     *origFrom.Address.Clone(),
		Params:      origFrom.Params.Clone(),
	}
	from.Params.Remove("tag")

	outDialog, outMed, err := h.dg.Invite(ctx, target, diago.InviteOptions{
		Transport: cfg.Uplink.Transport,
		Username:  cfg.Uplink.Username,
		Password:  cfg.Uplink.Password,
		Headers:   []sip.Header{from},
	})
	if err != nil {
		log.Warn("Uplink call failed", "err", err)
		inDialog.Respond(sip.StatusTemporarilyUnavailable, "Temporarily Unavailable", nil)
		cdr.HangupBy = "timeout"
		return
	}
	defer outDialog.Close()
	defer outMed.Close()

	// The bridge copies RTP without transcoding, so the phone must use
	// the same codec the uplink chose.
	codec := media.CodecAudioFromSession(outMed.MediaSession())
	log.Info("Uplink codec negotiated", "codec", codec.Name)

	inMed, err := inDialog.Answer(diago.AnswerOptions{
		Codecs: []media.Codec{codec, media.CodecTelephoneEvent8000},
		RTPNAT: media.RTPNATSymetric,
	})
	if err != nil {
		log.Error("Failed to answer phone with uplink codec", "codec", codec.Name, "err", err)
		outDialog.Hangup(context.Background())
		cdr.HangupBy = "system"
		return
	}
	defer inMed.Close()

	cdr.Answered = true
	cdr.AnswerTime = time.Now()

	log.Info("Outgoing call connected")
	h.bridgeWithRecording(inDialog, inMed, outDialog, outMed, cdr, log, nil)
}

// forkCall invites all registered phones in parallel, returns the first to answer.
func (h *CallHandler) forkCall(inDialog *diago.DialogServerSession, regs []*Registration, log *slog.Logger) (*diago.DialogClientSession, *diago.DialogMedia, error) {
	ctx, cancel := context.WithTimeout(inDialog.Context(), 30*time.Second)
	defer cancel()

	var (
		mu     sync.Mutex
		winner *diago.DialogClientSession
		winMed *diago.DialogMedia
		wg     sync.WaitGroup
	)

	for _, reg := range regs {
		uri := h.registrar.ContactURI(reg)
		wg.Add(1)
		go func(u sip.Uri, username, transport string) {
			defer wg.Done()

			log.Info("Inviting phone", "user", username, "uri", u.String())

			// Symmetric RTP on every phone leg: a LAN phone and a mobile behind
			// carrier NAT both show private addresses in SDP, and learning the
			// real source from the first packet is harmless for the LAN one.
			d, med, err := h.dg.Invite(ctx, u, diago.InviteOptions{
				Originator: inDialog,
				Transport:  transport,
				RTPNAT:     media.RTPNATSymetric,
			})
			if err != nil {
				log.Debug("Phone didn't answer", "user", username, "err", err)
				return
			}

			mu.Lock()
			if winner == nil {
				winner, winMed = d, med
				cancel() // Cancel remaining invites
				mu.Unlock()
				log.Info("Phone answered", "user", username)
			} else {
				mu.Unlock()
				d.Hangup(context.Background())
				med.Close()
				d.Close()
			}
		}(uri, reg.Username, reg.Transport)
	}

	wg.Wait()

	if winner == nil {
		return nil, nil, fmt.Errorf("no phones answered")
	}
	return winner, winMed, nil
}

// drainAudio keeps reading med's audio in the background until stop is called:
// the recording stays fed and the socket buffer does not pile up while the
// caller only hears playback. stop unblocks a Read that is waiting for a packet
// that never comes, so no stale reader is left to steal the bridge's first packet.
func drainAudio(med *diago.DialogMedia) (stop func()) {
	reader, err := med.AudioReader()
	if err != nil {
		return func() {}
	}
	var stopped atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, media.RTPBufSize)
		for !stopped.Load() {
			if _, err := reader.Read(buf); err != nil {
				return
			}
		}
	}()
	return func() {
		stopped.Store(true)
		med.StopRTP(1, 0)
		<-done
		med.StartRTP(1, 0)
	}
}

type recordingState struct {
	wavPath string
	closeFn func() error
}

// close flushes the recording and records the file, or the failure, in the CDR.
func (r *recordingState) close(cdr *CDR, log *slog.Logger) {
	if err := r.closeFn(); err != nil {
		log.Error("Failed to close recording", "file", r.wavPath, "err", err)
		cdr.Error = "recording failed: " + err.Error()
	}
	cdr.RecordingFile = r.wavPath
}

// bridgeWithRecording proxies audio between two dialog sessions with optional stereo recording.
// If rec is non-nil, recording is already set up (answer_first mode).
func (h *CallHandler) bridgeWithRecording(inDialog *diago.DialogServerSession, inMed *diago.DialogMedia, outDialog *diago.DialogClientSession, outMed *diago.DialogMedia, cdr *CDR, log *slog.Logger, rec *recordingState) {
	cfg := h.getConfig()

	// Set up recording if not already done (answer_first sets it up earlier)
	if rec == nil && cfg.Recording.Enabled {
		wavPath, closeFn, err := h.setupRecording(inMed, cdr, log)
		if err != nil {
			log.Error("Failed to setup recording, continuing without", "err", err)
			cdr.Error = "recording setup failed: " + err.Error()
		} else {
			rec = &recordingState{wavPath: wavPath, closeFn: closeFn}
		}
	}

	if rec != nil {
		defer rec.close(cdr, log)
	}

	// Play announcement to both sides if configured and not in answer_first mode
	// (answer_first plays announcement earlier in the flow)
	if cfg.Recording.Announcement != "" && cfg.Recording.Enabled && !cfg.Recording.AnswerFirst {
		h.playAnnouncement(inMed, outMed, log)
	}

	// Audio readers/writers wrapped with RFC 4733 DTMF handling. They wrap the
	// current reader/writer, so recording (if set) stays in the chain. A leg
	// without telephone-event falls back to plain audio.
	aReader, aWriter := audioEndpoints(inMed, "caller", log)
	bReader, bWriter := audioEndpoints(outMed, "callee", log)

	// DTMF is regenerated on the other leg after the digit ends, not passed through.
	// Raw event packets would go out with the audio payload type and turn into noise.
	forwardDTMF := func(from string, to *diago.DTMFWriter) func(rune) error {
		return func(d rune) error {
			log.Info("DTMF", "from", from, "digit", string(d), "forwarded", to != nil)
			if to != nil {
				if err := to.WriteDTMF(d); err != nil {
					log.Warn("DTMF forward failed", "err", err)
				}
			}
			return nil
		}
	}
	if r, ok := aReader.(*diago.DTMFReader); ok {
		r.OnDTMF(forwardDTMF("caller", bWriter))
	}
	if r, ok := bReader.(*diago.DTMFReader); ok {
		r.OnDTMF(forwardDTMF("callee", aWriter))
	}

	// Log media session addresses for NAT debugging
	if inMS := inMed.MediaSession(); inMS != nil {
		log.Debug("inDialog media", "local", inMS.Laddr.String(), "remote", inMS.Raddr.String(), "rtpnat", inMS.RTPNAT)
	}
	if outMS := outMed.MediaSession(); outMS != nil {
		log.Debug("outDialog media", "local", outMS.Laddr.String(), "remote", outMS.Raddr.String(), "rtpnat", outMS.RTPNAT)
	}

	// Bridge: proxy audio between both sides. A read error means that side
	// hung up, a write error means the other side did.
	done := make(chan string, 2)

	go func() {
		err := copyAudio(aReader, inMed.RTPPacketReader, writerOf(bWriter, outMed)) // A → B
		done <- pick(errors.Is(err, errWrite), "callee", "caller")
	}()
	go func() {
		err := copyAudio(bReader, outMed.RTPPacketReader, writerOf(aWriter, inMed)) // B → A
		done <- pick(errors.Is(err, errWrite), "caller", "callee")
	}()

	// Wait for one side to hang up
	hangupBy := <-done
	cdr.HangupBy = hangupBy
	log.Info("Call ended", "hangup_by", hangupBy)

	outDialog.Hangup(context.Background())
	inDialog.Hangup(context.Background())
}

// audioEndpoints returns the DTMF-aware reader and writer of a leg, or the plain
// audio reader and a nil writer when telephone-event was not negotiated on it.
func audioEndpoints(med *diago.DialogMedia, side string, log *slog.Logger) (io.Reader, *diago.DTMFWriter) {
	var r io.Reader
	dr, err := med.AudioReaderDTMF()
	if err == nil {
		r = dr
	} else {
		log.Debug("No DTMF on leg, plain audio", "side", side, "err", err)
		if r, err = med.AudioReader(); err != nil {
			log.Error("No audio reader on leg", "side", side, "err", err)
			r = eofReader{}
		}
	}
	w, err := med.AudioWriterDTMF()
	if err != nil {
		return r, nil
	}
	return r, w
}

// writerOf picks the DTMF writer when the leg has one, else the plain audio writer.
func writerOf(w *diago.DTMFWriter, med *diago.DialogMedia) io.Writer {
	if w != nil {
		return w
	}
	aw, err := med.AudioWriter()
	if err != nil {
		return io.Discard
	}
	return aw
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

var errWrite = errors.New("write failed")

func pick[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}

// copyAudio is media.Copy that skips telephone-event packets: the reader has already
// turned them into an OnDTMF callback, and writing them on would send them as audio.
// Write errors are wrapped in errWrite so the caller can tell the sides apart.
func copyAudio(r io.Reader, pr *media.RTPPacketReader, w io.Writer) error {
	buf := make([]byte, media.RTPBufSize)
	for {
		n, err := r.Read(buf)
		if err != nil {
			return err
		}
		if n == 0 || pr.PacketHeader.PayloadType == media.CodecTelephoneEvent8000.PayloadType {
			continue
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return fmt.Errorf("%w: %w", errWrite, err)
		}
	}
}

// playAnnouncement plays a WAV file to both call parties simultaneously.
func (h *CallHandler) playAnnouncement(inMed, outMed *diago.DialogMedia, log *slog.Logger) {
	cfg := h.getConfig()
	log.Info("Playing announcement", "file", cfg.Recording.Announcement)

	pbCaller, err1 := inMed.PlaybackCreate()
	pbCallee, err2 := outMed.PlaybackCreate()
	if err1 != nil || err2 != nil {
		log.Error("Failed to create playback for announcement", "err_in", err1, "err_out", err2)
		return
	}

	// Drain caller's audio during announcement so it gets recorded on the left channel
	stopDrain := drainAudio(inMed)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if _, err := pbCaller.PlayFile(cfg.Recording.Announcement); err != nil {
			log.Error("Announcement playback failed (caller side)", "err", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := pbCallee.PlayFile(cfg.Recording.Announcement); err != nil {
			log.Error("Announcement playback failed (callee side)", "err", err)
		}
	}()
	wg.Wait()

	stopDrain()

	log.Debug("Announcement finished")
}

// setupRecording sets up stereo WAV recording on the incoming leg.
func (h *CallHandler) setupRecording(inMed *diago.DialogMedia, cdr *CDR, log *slog.Logger) (string, func() error, error) {
	cfg := h.getConfig()

	if err := os.MkdirAll(cfg.Recording.Dir, dirPerm); err != nil {
		return "", nil, fmt.Errorf("create recording dir: %w", err)
	}

	filename := fmt.Sprintf("%s_%s.wav", cdr.StartTime.Format("20060102_150405"), cdr.CallID)
	wavPath := filepath.Join(cfg.Recording.Dir, filename)

	wavFile, err := os.OpenFile(wavPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, filePerm)
	if err != nil {
		return "", nil, fmt.Errorf("create wav file: %w", err)
	}

	rec, err := inMed.AudioStereoRecordingCreate(wavFile)
	if err != nil {
		wavFile.Close()
		os.Remove(wavPath)
		return "", nil, fmt.Errorf("create recording: %w", err)
	}

	mon := rec.AudioReader()
	inMed.SetAudioReader(mon)
	inMed.SetAudioWriter(mon)

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
	cfg := h.getConfig()

	log.Info("Call finished",
		"answered", cdr.Answered,
		"duration", cdr.Duration,
		"talk_time", cdr.TalkTime,
		"error", cdr.Error,
	)

	jsonPath, err := SaveCDR(cdr, cfg.Recording.Dir)
	if err != nil {
		log.Error("Failed to save CDR", "err", err)
		return
	}

	wavPath := cdr.RecordingFile
	if wavPath == "" {
		wavPath = "none"
	}
	go RunPostCallScript(cfg.PostCall.Script, jsonPath, wavPath, log)
}
