// Package ssh implements the execution.Transport/Session interfaces over SSH
// with a long-lived non-PTY shell process per session. It owns all SSH
// session creation, host-key verification, shared/dedicated client handling,
// and (framing.go) the delimiter protocol that separates command output.
package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"aiharn/internal/execution"
	"aiharn/internal/logging"
)

// Options are the channel settings needed to dial and authenticate an SSH
// transport. It deliberately duplicates config.ChannelConfig rather than
// importing it, keeping execution independent of the config package.
type Options struct {
	Host          string
	Port          int
	User          string
	KeyFile       string // local path to a private key (exclusive with Password)
	Password      string // exclusive with KeyFile
	KnownHosts    string // local path to an OpenSSH known_hosts file
	Insecure      bool   // if true, skip host-key verification (warned opt-out)
	KeepAlive     bool   // true = sessions share one client; false = one client per session
	DefaultShell  string // e.g. "/bin/bash"
	RemoteCommand string // optional command run verbatim in place of DefaultShell

	// Env holds "NAME=value" pairs sent to the remote before the shell starts,
	// mirroring OpenSSH's SetEnv directive (read from ~/.ssh/config during alias
	// resolution). A rejected or malformed entry is ignored, matching OpenSSH.
	Env []string

	// SSHConfigAlias, when true, resolves Host through ~/.ssh/config whenever
	// Host is not an IP literal: a matching alias is applied if present, and the
	// explicit fields are used otherwise. IP literals always use the explicit
	// fields directly.
	SSHConfigAlias bool
}

// Transport implements execution.Transport.
type Transport struct {
	opts Options

	mu     sync.Mutex
	shared *ssh.Client
	closed bool
}

// compile-time assertion
var _ execution.Transport = (*Transport)(nil)

// NewTransport validates options and returns a Transport. It does not dial;
// connections are established lazily on the first NewSession.
func NewTransport(opts Options) (*Transport, error) {
	if opts.Host == "" {
		return nil, errors.New("ssh: host is required")
	}
	if opts.SSHConfigAlias && !isIPLiteral(opts.Host) {
		configPath, err := defaultSSHConfigPath()
		if err != nil {
			return nil, err
		}
		if aliasOnly(opts) {
			opts, err = resolveAlias(opts, configPath)
			if err != nil {
				return nil, err
			}
		} else {
			resolved, found, err := tryResolveAlias(opts, configPath)
			if err != nil {
				return nil, err
			}
			if found {
				opts = resolved
			}
		}
	}
	if opts.Port == 0 {
		opts.Port = 22
	}
	if opts.Port < 1 || opts.Port > 65535 {
		return nil, fmt.Errorf("ssh: port must be between 1 and 65535, got %d", opts.Port)
	}
	if opts.User == "" {
		return nil, errors.New("ssh: user is required")
	}
	if opts.KeyFile != "" && opts.Password != "" {
		return nil, errors.New("ssh: key_file and password are mutually exclusive")
	}
	if opts.KeyFile != "" {
		if err := validateLocalFile(opts.KeyFile, 1<<20, "key_file"); err != nil {
			return nil, err
		}
	}
	if !opts.Insecure && opts.KnownHosts == "" {
		return nil, errors.New("ssh: known_hosts is required (or set insecure)")
	}
	if opts.KnownHosts != "" {
		if err := validateLocalFile(opts.KnownHosts, 64<<20, "known_hosts"); err != nil {
			return nil, err
		}
	}
	if opts.DefaultShell == "" {
		opts.DefaultShell = "/bin/bash"
	}
	return &Transport{opts: opts}, nil
}

func validateLocalFile(path string, maxBytes int64, name string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("ssh: %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("ssh: %s %s is not a regular file", name, path)
	}
	if info.Size() > maxBytes {
		return fmt.Errorf("ssh: %s %s exceeds %d-byte limit", name, path, maxBytes)
	}
	return nil
}

// NewSession opens a persistent shell. With KeepAlive it reuses one shared
// client; otherwise it dials a dedicated client owned by the returned session.
func (t *Transport) NewSession(ctx context.Context) (execution.Session, error) {
	logging.Debug("ssh: NewSession",
		slog.String("component", "ssh"),
		slog.String("host", t.opts.Host),
		slog.Int("port", t.opts.Port),
		slog.String("user", t.opts.User),
		slog.Bool("keep_alive", t.opts.KeepAlive),
	)
	client, dedicated, err := t.acquireClient(ctx)
	if err != nil {
		return nil, err
	}
	var onDead func()
	if !dedicated {
		onDead = func() { t.dropShared(client) }
	}
	s, err := openShellSession(ctx, client, t.opts, dedicated, onDead)
	if err != nil {
		if dedicated {
			client.Close()
		} else {
			t.dropShared(client)
		}
		return nil, err
	}
	return s, nil
}

// Close releases the shared client, if any.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	if t.shared != nil {
		err := t.shared.Close()
		t.shared = nil
		logging.Debug("ssh: Transport.Close", slog.String("component", "ssh"), slog.Bool("closed_shared", true), slog.Any("err", err))
		return err
	}
	logging.Debug("ssh: Transport.Close", slog.String("component", "ssh"), slog.Bool("closed_shared", false))
	return nil
}

func (t *Transport) acquireClient(ctx context.Context) (*ssh.Client, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, false, errors.New("ssh: transport closed")
	}
	if t.opts.KeepAlive {
		reused := t.shared != nil
		if t.shared == nil {
			c, err := dial(ctx, t.opts)
			if err != nil {
				logging.Debug("ssh: acquireClient",
					slog.String("component", "ssh"),
					slog.Bool("shared", true),
					slog.Bool("reused", false),
					slog.Any("err", err),
				)
				return nil, false, err
			}
			t.shared = c
		}
		logging.Debug("ssh: acquireClient",
			slog.String("component", "ssh"),
			slog.Bool("shared", true),
			slog.Bool("reused", reused),
		)
		return t.shared, false, nil
	}
	c, err := dial(ctx, t.opts)
	if err != nil {
		logging.Debug("ssh: acquireClient",
			slog.String("component", "ssh"),
			slog.Bool("shared", false),
			slog.Any("err", err),
		)
		return nil, false, err
	}
	logging.Debug("ssh: acquireClient",
		slog.String("component", "ssh"),
		slog.Bool("shared", false),
	)
	return c, true, nil
}

// dropShared closes the shared client so the next NewSession redials. Called by
// a shared-client session when the connection is discovered to be dead.
func (t *Transport) dropShared(expected *ssh.Client) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.shared == expected {
		t.shared.Close()
		t.shared = nil
		logging.Debug("ssh: dropShared", slog.String("component", "ssh"))
		return
	}
	logging.Debug("ssh: dropShared", slog.String("component", "ssh"), slog.Bool("matched", false))
}

func dial(ctx context.Context, opts Options) (*ssh.Client, error) {
	addr := net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port))
	logging.Debug("ssh: dial start", slog.String("component", "ssh"), slog.String("addr", addr))
	cfg, release, err := buildClientConfigContext(ctx, opts)
	if err != nil {
		logging.Debug("ssh: dial fail", slog.String("component", "ssh"), slog.String("addr", addr), slog.Any("err", err))
		return nil, err
	}
	defer release()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		logging.Debug("ssh: dial fail", slog.String("component", "ssh"), slog.String("addr", addr), slog.Any("err", err))
		return nil, sanitizeSSHError(err, opts)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			raw.Close()
		case <-done:
		}
	}()
	conn, chans, reqs, err := ssh.NewClientConn(raw, addr, cfg)
	close(done)
	if err != nil {
		raw.Close()
		logging.Debug("ssh: dial fail", slog.String("component", "ssh"), slog.String("addr", addr), slog.Any("err", err))
		return nil, sanitizeSSHError(err, opts)
	}
	if err := ctx.Err(); err != nil {
		conn.Close()
		logging.Debug("ssh: dial fail", slog.String("component", "ssh"), slog.String("addr", addr), slog.Any("err", err))
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{})
	logging.Debug("ssh: dial ok", slog.String("component", "ssh"), slog.String("addr", addr))
	return ssh.NewClient(conn, chans, reqs), nil
}

// preferredHostKeyAlgorithms mirrors OpenSSH's default hostkeyalgorithms
// order (see `ssh -Q key` / `ssh -G host | grep hostkeyalgorithms`).
// x/crypto's default puts Ed25519 last among plain keys, so a server that
// offers both RSA and Ed25519 would negotiate RSA and then fail against an
// Ed25519-only known_hosts entry ("knownhosts: key mismatch").
var preferredHostKeyAlgorithms = []string{
	ssh.CertAlgoED25519v01,
	ssh.CertAlgoECDSA256v01,
	ssh.CertAlgoECDSA384v01,
	ssh.CertAlgoECDSA521v01,
	ssh.CertAlgoRSASHA512v01,
	ssh.CertAlgoRSASHA256v01,
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoECDSA256,
	ssh.KeyAlgoECDSA384,
	ssh.KeyAlgoECDSA521,
	ssh.KeyAlgoRSASHA512,
	ssh.KeyAlgoRSASHA256,
	// Legacy ssh-rsa-only servers; OpenSSH disables this by default, but
	// x/crypto supports it. Keep it last as a compatibility fallback.
	ssh.KeyAlgoRSA,
}

// buildClientConfig assembles the SSH client config for one of three auth
// methods: password, key file, or (when neither is set) the SSH agent. The
// returned release func closes any agent connection and must be called after
// the handshake completes.
func buildClientConfigContext(ctx context.Context, opts Options) (*ssh.ClientConfig, func(), error) {
	release := func() {}
	var auth ssh.AuthMethod
	authMethod := "ssh-agent"
	switch {
	case opts.Password != "":
		authMethod = "password"
		auth = ssh.Password(opts.Password)
	case opts.KeyFile != "":
		authMethod = "key-file"
		const maxPrivateKeyBytes = 1 << 20
		f, err := os.Open(opts.KeyFile)
		if err != nil {
			return nil, release, fmt.Errorf("ssh: read key_file: %w", err)
		}
		key, err := io.ReadAll(io.LimitReader(f, maxPrivateKeyBytes+1))
		closeErr := f.Close()
		if err != nil {
			return nil, release, fmt.Errorf("ssh: read key_file: %w", err)
		}
		if closeErr != nil {
			return nil, release, fmt.Errorf("ssh: close key_file: %w", closeErr)
		}
		if len(key) > maxPrivateKeyBytes {
			return nil, release, fmt.Errorf("ssh: key_file exceeds %d-byte limit", maxPrivateKeyBytes)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, release, fmt.Errorf("ssh: parse key_file %s: %w", opts.KeyFile, err)
		}
		auth = ssh.PublicKeys(signer)
	default:
		authMethod = "ssh-agent"
		var err error
		auth, release, err = agentAuthContext(ctx)
		if err != nil {
			return nil, release, err
		}
	}

	authAttrs := []slog.Attr{
		slog.String("component", "ssh"),
		slog.String("auth", authMethod),
		slog.String("user", opts.User),
		slog.Bool("insecure", opts.Insecure),
		slog.Bool("has_known_hosts", opts.KnownHosts != ""),
	}
	if authMethod == "key-file" {
		authAttrs = append(authAttrs, slog.String("key_file", opts.KeyFile))
	}
	logging.Debug("ssh: buildClientConfig", authAttrs...)

	cfg := &ssh.ClientConfig{
		User: opts.User,
		Auth: []ssh.AuthMethod{auth},
		// Prefer OpenSSH's host-key algorithm order so Ed25519 beats RSA/ECDSA
		// when a server offers both.
		HostKeyAlgorithms: preferredHostKeyAlgorithms,
	}
	if opts.Insecure {
		cfg.HostKeyCallback = ssh.InsecureIgnoreHostKey()
	} else {
		cb, err := knownhosts.New(opts.KnownHosts)
		if err != nil {
			release()
			return nil, release, fmt.Errorf("ssh: load known_hosts: %w", err)
		}
		cfg.HostKeyCallback = cb
	}
	return cfg, release, nil
}

// agentAuth returns an auth method backed by the SSH agent at $SSH_AUTH_SOCK,
// plus a release func that closes the agent connection.
func agentAuthContext(ctx context.Context) (ssh.AuthMethod, func(), error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, func() {}, errors.New("ssh: no key_file, password, or SSH agent (SSH_AUTH_SOCK unset)")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, func() {}, fmt.Errorf("ssh: connect to SSH agent %s: %w", sock, err)
	}
	client := agent.NewClient(conn)
	signers, err := client.Signers()
	if err != nil {
		conn.Close()
		return nil, func() {}, fmt.Errorf("ssh: list SSH agent keys: %w", err)
	}
	if len(signers) == 0 {
		conn.Close()
		return nil, func() {}, errors.New("ssh: SSH agent has no keys")
	}
	return ssh.PublicKeys(signers...), func() { conn.Close() }, nil
}

// sanitizeSSHError removes secret-bearing material (password) from an error, as
// defense in depth; x/crypto/ssh errors do not normally include credentials.
func sanitizeSSHError(err error, opts Options) error {
	msg := err.Error()
	if opts.Password != "" {
		msg = strings.ReplaceAll(msg, opts.Password, "[REDACTED]")
	}
	return errors.New(msg)
}
