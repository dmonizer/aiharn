package ssh

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
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

func TestBuildClientConfigHostKeyAlgorithms(t *testing.T) {
	cfg, release, err := buildClientConfigContext(context.Background(), Options{User: "u", Password: "p", Insecure: true})
	if err != nil {
		t.Fatalf("buildClientConfigContext: %v", err)
	}
	if cfg == nil {
		t.Fatal("cfg is nil")
	}
	release()

	algos := cfg.HostKeyAlgorithms
	if len(algos) == 0 {
		t.Fatal("HostKeyAlgorithms is empty")
	}

	indexOf := func(want string) int {
		for i, a := range algos {
			if a == want {
				return i
			}
		}
		return -1
	}

	ed := indexOf(ssh.KeyAlgoED25519)
	rsa256 := indexOf(ssh.KeyAlgoRSASHA256)
	rsa := indexOf(ssh.KeyAlgoRSA)
	if ed < 0 {
		t.Fatalf("HostKeyAlgorithms missing %s", ssh.KeyAlgoED25519)
	}
	if rsa256 < 0 {
		t.Fatalf("HostKeyAlgorithms missing %s", ssh.KeyAlgoRSASHA256)
	}
	if rsa < 0 {
		t.Fatalf("HostKeyAlgorithms missing %s", ssh.KeyAlgoRSA)
	}
	if ed >= rsa256 {
		t.Fatalf("%s (index %d) must come before %s (index %d)", ssh.KeyAlgoED25519, ed, ssh.KeyAlgoRSASHA256, rsa256)
	}
	if ed >= rsa {
		t.Fatalf("%s (index %d) must come before %s (index %d)", ssh.KeyAlgoED25519, ed, ssh.KeyAlgoRSA, rsa)
	}
	if rsa256 >= rsa {
		t.Fatalf("%s (index %d) must come before %s (index %d)", ssh.KeyAlgoRSASHA256, rsa256, ssh.KeyAlgoRSA, rsa)
	}

	rsa512 := indexOf(ssh.KeyAlgoRSASHA512)
	if rsa512 < 0 {
		t.Fatalf("HostKeyAlgorithms missing %s", ssh.KeyAlgoRSASHA512)
	}
	if rsa512 >= rsa256 {
		t.Fatalf("%s (index %d) must come before %s (index %d)", ssh.KeyAlgoRSASHA512, rsa512, ssh.KeyAlgoRSASHA256, rsa256)
	}

	for _, cert := range []string{
		ssh.CertAlgoED25519v01,
		ssh.CertAlgoECDSA256v01,
		ssh.CertAlgoECDSA384v01,
		ssh.CertAlgoECDSA521v01,
		ssh.CertAlgoRSASHA512v01,
		ssh.CertAlgoRSASHA256v01,
	} {
		if indexOf(cert) < 0 {
			t.Fatalf("HostKeyAlgorithms missing %s", cert)
		}
	}
}
