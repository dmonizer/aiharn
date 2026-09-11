package ssh

import (
	"strings"
	"testing"
)

func TestAgentAuthNoSocket(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	_, release, err := agentAuth()
	if err == nil || !strings.Contains(err.Error(), "SSH_AUTH_SOCK") {
		t.Fatalf("agentAuth error = %v, want SSH_AUTH_SOCK", err)
	}
	release() // no-op, must not panic
}

func TestBuildClientConfigAgentFallback(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	cfg, release, err := buildClientConfig(Options{Host: "h", User: "u", Insecure: true})
	if err == nil || !strings.Contains(err.Error(), "SSH_AUTH_SOCK") {
		t.Fatalf("buildClientConfig error = %v, want agent fallback error", err)
	}
	if cfg != nil {
		t.Fatal("cfg should be nil on error")
	}
	release() // no-op, must not panic
}
