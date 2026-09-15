package ssh

import (
	"errors"
	"strings"
	"testing"
)

func TestSanitizeSSHError(t *testing.T) {
	opts := Options{Password: "hunter2secret"}
	got := sanitizeSSHError(errors.New("dial: password hunter2secret rejected"), opts)
	if strings.Contains(got.Error(), "hunter2secret") {
		t.Fatalf("password leaked: %q", got)
	}
	if !strings.Contains(got.Error(), "[REDACTED]") {
		t.Fatalf("expected redaction marker: %q", got)
	}
}
