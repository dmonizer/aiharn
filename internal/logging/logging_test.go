package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestNewLoggerEmitsDebug guards the handler-level regression: the default slog
// handler level (Info) silently drops LevelDebug records, which made -debug
// produce no output at all.
func TestNewLoggerEmitsDebug(t *testing.T) {
	var buf bytes.Buffer
	l := newLogger(&buf)
	l.LogAttrs(context.Background(), slog.LevelDebug, "probe", slog.String("k", "v"))
	if !strings.Contains(buf.String(), "msg=probe") {
		t.Fatalf("debug record was dropped: %q", buf.String())
	}
}

func TestEnvTruthy(t *testing.T) {
	truthy := []string{"1", "true", "on", "yes", "TRUE", " On "}
	for _, v := range truthy {
		if !envTruthy(v) {
			t.Errorf("envTruthy(%q) = false, want true", v)
		}
	}
	falsy := []string{"", "0", "false", "off", "no", "debug", "2"}
	for _, v := range falsy {
		if envTruthy(v) {
			t.Errorf("envTruthy(%q) = true, want false", v)
		}
	}
}
