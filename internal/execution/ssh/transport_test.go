package ssh

import (
	"context"
	"strings"
	"testing"
)

func TestAgentAuthNoSocket(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	_, release, err := agentAuthContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "SSH_AUTH_SOCK") {
		t.Fatalf("agentAuth error = %v, want SSH_AUTH_SOCK", err)
	}
	release() // no-op, must not panic
}

func TestBuildClientConfigAgentFallback(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	cfg, release, err := buildClientConfigContext(context.Background(), Options{Host: "h", User: "u", Insecure: true})
	if err == nil || !strings.Contains(err.Error(), "SSH_AUTH_SOCK") {
		t.Fatalf("buildClientConfig error = %v, want agent fallback error", err)
	}
	if cfg != nil {
		t.Fatal("cfg should be nil on error")
	}
	release() // no-op, must not panic
}
