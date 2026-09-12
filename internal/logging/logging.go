// Package logging provides process-wide debug logging for Aiharn. Debug logs
// are off by default and are enabled either by setting the AIHARN_DEBUG
// environment variable to a truthy value or by calling SetDebug(true) (used by
// the -debug CLI flag). All output goes to stderr so it never interferes with
// the Bubbletea TUI, which renders on stdout.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
)

var (
	enabled atomic.Bool
	logger  = newLogger(os.Stderr)
)

// newLogger builds a text handler that accepts debug records. The default
// slog handler level is Info, which silently discards LevelDebug records; the
// explicit Level is what makes Debug() actually emit.
func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func init() {
	enabled.Store(envTruthy(os.Getenv("AIHARN_DEBUG")))
}

// SetDebug enables debug logging. It is called by the -debug CLI flag. Enabling
// is sticky: it never overrides an enablement already in effect from the
// environment, so a flag default of false does not silence AIHARN_DEBUG.
func SetDebug(on bool) {
	if on {
		enabled.Store(true)
	}
}

// Enabled reports whether debug logging is active.
func Enabled() bool {
	return enabled.Load()
}

// Debug emits a debug log line. It is a no-op when debug logging is disabled.
// Callers must never pass secrets (API keys, passwords, or key-file contents):
// for auth, log only the method name, never the value.
func Debug(msg string, attrs ...slog.Attr) {
	if !enabled.Load() {
		return
	}
	logger.LogAttrs(context.Background(), slog.LevelDebug, msg, attrs...)
}

func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}
