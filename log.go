package main

import (
	"context"
	"log/slog"
	"strings"
)

// filterHandler wraps a slog.Handler and suppresses noisy messages
// from third-party libraries (e.g. diago's RTCP unmarshal errors).
type filterHandler struct {
	inner slog.Handler
}

func newFilterHandler(inner slog.Handler) *filterHandler {
	return &filterHandler{inner: inner}
}

func (h *filterHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *filterHandler) Handle(ctx context.Context, r slog.Record) error {
	// Suppress diago/sipgo RTCP errors — harmless packet parsing noise
	if r.Level == slog.LevelError && strings.Contains(r.Message, "RTCP") {
		r.Level = slog.LevelDebug
		if !h.inner.Enabled(ctx, slog.LevelDebug) {
			return nil
		}
	}
	return h.inner.Handle(ctx, r)
}

func (h *filterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &filterHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *filterHandler) WithGroup(name string) slog.Handler {
	return &filterHandler{inner: h.inner.WithGroup(name)}
}
