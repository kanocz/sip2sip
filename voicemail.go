package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/emiago/diago"
	diagoaudio "github.com/emiago/diago/audio"
	"github.com/emiago/diago/media"
)

// handleVoicemail answers the call, plays announcement, and optionally records a voicemail.
// Recording is done as mono WAV by decoding G.711 directly, avoiding stereo sync issues.
func (h *CallHandler) handleVoicemail(inDialog *diago.DialogServerSession, reason, announcement string, cdr *CDR, log *slog.Logger) {
	cfg := h.getConfig()

	// Answer the call
	med, err := inDialog.Answer(diago.AnswerOptions{})
	if err != nil {
		log.Error("Failed to answer for voicemail", "err", err)
		cdr.HangupBy = "system"
		return
	}
	defer med.Close()

	cdr.Answered = true
	cdr.AnswerTime = time.Now()
	cdr.AnsweredBy = "voicemail"
	cdr.Voicemail = true
	cdr.VoicemailReason = reason

	// Drain caller's audio during announcement to prevent buffer buildup
	stopDrain := drainAudio(med)

	// Brief pause for media path to establish
	time.Sleep(1 * time.Second)

	// Play announcement
	if announcement != "" {
		log.Info("Playing voicemail announcement", "file", announcement)
		if pb, err := med.PlaybackCreate(); err == nil {
			if _, err := pb.PlayFile(announcement); err != nil {
				log.Error("Announcement playback failed", "err", err)
			}
		} else {
			log.Error("Failed to create playback", "err", err)
		}
	}

	// Check if caller hung up during announcement
	if inDialog.Context().Err() != nil {
		stopDrain()
		cdr.HangupBy = "caller"
		return
	}

	// If voicemail recording not enabled, hang up after announcement
	if !cfg.Voicemail.Enabled {
		stopDrain()
		time.Sleep(500 * time.Millisecond)
		inDialog.Hangup(context.Background())
		cdr.HangupBy = "system"
		return
	}

	// Play beep tone to signal recording start
	h.playBeepTone(med, log)

	// Stop drain before recording takes over the reader
	stopDrain()

	// Get audio reader with codec info for proper decoding
	var mprops diago.MediaProps
	reader, err := med.AudioReader(diago.WithAudioReaderMediaProps(&mprops))
	if err != nil {
		log.Error("Failed to get audio reader for voicemail", "err", err)
		inDialog.Hangup(context.Background())
		cdr.HangupBy = "system"
		return
	}

	decodeFn, err := g711Decoder(mprops.Codec.PayloadType)
	if err != nil {
		log.Error("Unsupported codec for voicemail", "codec", mprops.Codec.Name, "err", err)
		inDialog.Hangup(context.Background())
		cdr.HangupBy = "system"
		return
	}

	log.Info("Voicemail codec detected", "name", mprops.Codec.Name)

	// Create mono WAV file for the voicemail message
	recDir := cfg.Recording.Dir
	os.MkdirAll(recDir, dirPerm)

	filename := fmt.Sprintf("vm_%s_%s.wav", cdr.StartTime.Format("20060102_150405"), cdr.CallID)
	wavPath := filepath.Join(recDir, filename)
	wavFile, err := os.OpenFile(wavPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, filePerm)
	if err != nil {
		log.Error("Failed to create voicemail file", "err", err)
		inDialog.Hangup(context.Background())
		cdr.HangupBy = "system"
		return
	}

	// Write placeholder WAV header (will update with actual size later)
	writeMonoWavHeader(wavFile, 0)

	silenceTimeout := time.Duration(cfg.Voicemail.SilenceTimeout) * time.Second
	maxDuration := time.Duration(cfg.Voicemail.MaxDuration) * time.Second

	log.Info("Recording voicemail", "file", wavPath, "silence_timeout", silenceTimeout, "max_duration", maxDuration)

	// Record: read G.711 → decode to PCM → write to WAV + silence detection
	messageStart := time.Now()
	dataSize, lastSound, err := recordVoicemail(reader, wavFile, decodeFn, silenceTimeout, maxDuration, inDialog.Context(), log)
	// The trailing silence that stopped the recording is not part of the message.
	cdr.MessageDuration = lastSound.Sub(messageStart).Seconds()

	// Update WAV header with actual data size
	if err == nil {
		_, err = wavFile.Seek(0, io.SeekStart)
	}
	if err == nil {
		err = writeMonoWavHeader(wavFile, uint32(dataSize))
	}
	if closeErr := wavFile.Close(); err == nil {
		err = closeErr
	}

	switch {
	case err != nil:
		// A half-written file would be sent out as if it were the message.
		os.Remove(wavPath)
		log.Error("Voicemail recording failed", "file", wavPath, "err", err)
		cdr.Error = "voicemail recording failed: " + err.Error()
	case dataSize > 0:
		cdr.RecordingFile = wavPath
		log.Info("Voicemail saved", "file", wavPath, "message_duration", cdr.MessageDuration)
	default:
		os.Remove(wavPath)
		log.Info("No voicemail recorded (empty)")
	}

	inDialog.Hangup(context.Background())
	cdr.HangupBy = "voicemail"
}

// g711DecoderFunc decodes encoded audio to 16-bit PCM.
type g711DecoderFunc func(lpcm []byte, encoded []byte) (int, error)

func g711Decoder(payloadType uint8) (g711DecoderFunc, error) {
	switch payloadType {
	case 0: // PCMU (G.711 u-law)
		return diagoaudio.DecodeUlawTo, nil
	case 8: // PCMA (G.711 a-law)
		return diagoaudio.DecodeAlawTo, nil
	default:
		return nil, fmt.Errorf("unsupported payload type %d", payloadType)
	}
}

// recordVoicemail reads encoded audio, decodes to PCM, writes to the WAV file,
// and monitors for silence. Returns total PCM bytes written, when the caller
// was last heard, and the first write error (disk full, for one).
func recordVoicemail(reader io.Reader, writer io.Writer, decode g711DecoderFunc, silenceTimeout, maxDuration time.Duration, ctx context.Context, log *slog.Logger) (int, time.Time, error) {
	encodedBuf := make([]byte, media.RTPBufSize)
	pcmBuf := make([]byte, media.RTPBufSize*2) // G.711 decodes 1:2
	totalWritten := 0
	startTime := time.Now()
	lastSoundTime := startTime

	const silenceThreshold = 500.0 // RMS amplitude threshold for 16-bit PCM

	for {
		if ctx.Err() != nil {
			log.Debug("Voicemail: caller hung up")
			return totalWritten, lastSoundTime, nil
		}
		if time.Since(startTime) >= maxDuration {
			log.Debug("Voicemail: max duration reached")
			return totalWritten, lastSoundTime, nil
		}
		if time.Since(lastSoundTime) >= silenceTimeout {
			log.Debug("Voicemail: silence timeout")
			return totalWritten, lastSoundTime, nil
		}

		n, err := reader.Read(encodedBuf)
		if err != nil {
			log.Debug("Voicemail: read ended", "err", err)
			return totalWritten, lastSoundTime, nil
		}
		if n == 0 {
			continue
		}

		// Decode G.711 to 16-bit PCM
		pcmN, err := decode(pcmBuf, encodedBuf[:n])
		if err != nil {
			log.Debug("Voicemail: decode error", "err", err)
			continue
		}
		if pcmN == 0 {
			continue
		}

		if _, err := writer.Write(pcmBuf[:pcmN]); err != nil {
			return totalWritten, lastSoundTime, err
		}
		totalWritten += pcmN

		if rmsLevel(pcmBuf[:pcmN]) > silenceThreshold {
			lastSoundTime = time.Now()
		}
	}
}

// playBeepTone generates and plays a short beep tone to signal recording start.
func (h *CallHandler) playBeepTone(med *diago.DialogMedia, log *slog.Logger) {
	// Generate 0.5s of 1kHz sine wave at 8000Hz, 16-bit mono PCM
	const sampleRate = 8000
	const durationSec = 0.5
	const freq = 1000.0
	const amplitude = 8000.0

	numSamples := int(sampleRate * durationSec)
	pcmData := make([]byte, numSamples*2)

	for i := 0; i < numSamples; i++ {
		t := float64(i) / float64(sampleRate)
		sample := int16(amplitude * math.Sin(2*math.Pi*freq*t))
		binary.LittleEndian.PutUint16(pcmData[i*2:], uint16(sample))
	}

	// Write to temp WAV file for playback
	tmpFile, err := os.CreateTemp("", "sip2sip-beep-*.wav")
	if err != nil {
		log.Error("Failed to create temp beep file", "err", err)
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if err := writeMonoWavHeader(tmpFile, uint32(len(pcmData))); err != nil {
		log.Error("Failed to write beep WAV header", "err", err)
		tmpFile.Close()
		return
	}
	tmpFile.Write(pcmData)
	tmpFile.Close()

	// Play the beep
	pb, err := med.PlaybackCreate()
	if err != nil {
		log.Error("Failed to create playback for beep", "err", err)
		return
	}
	if _, err := pb.PlayFile(tmpPath); err != nil {
		log.Error("Beep playback failed", "err", err)
	}
}

// rmsLevel calculates the RMS amplitude of 16-bit PCM audio data.
func rmsLevel(pcmData []byte) float64 {
	numSamples := len(pcmData) / 2
	if numSamples == 0 {
		return 0
	}

	var sumSquares float64
	for i := 0; i < numSamples; i++ {
		sample := int16(binary.LittleEndian.Uint16(pcmData[i*2:]))
		sumSquares += float64(sample) * float64(sample)
	}

	return math.Sqrt(sumSquares / float64(numSamples))
}

// writeMonoWavHeader writes a WAV file header for 8kHz 16-bit mono PCM.
func writeMonoWavHeader(w io.Writer, dataSize uint32) error {
	var sampleRate uint32 = 8000
	var numChannels uint16 = 1
	var bitsPerSample uint16 = 16
	blockAlign := numChannels * (bitsPerSample / 8)
	byteRate := sampleRate * uint32(blockAlign)

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+dataSize))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16)) // chunk size
	binary.Write(&buf, binary.LittleEndian, uint16(1))  // PCM format
	binary.Write(&buf, binary.LittleEndian, numChannels)
	binary.Write(&buf, binary.LittleEndian, sampleRate)
	binary.Write(&buf, binary.LittleEndian, byteRate)
	binary.Write(&buf, binary.LittleEndian, blockAlign)
	binary.Write(&buf, binary.LittleEndian, bitsPerSample)
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, dataSize)

	_, err := w.Write(buf.Bytes())
	return err
}
