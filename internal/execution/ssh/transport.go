// Package ssh implements the execution.Transport/Session interfaces over SSH
// with a persistent, stateful, non-PTY shell per session. It owns all SSH
// session creation, host-key verification, shared/dedicated client handling,
// and (framing.go) the delimiter protocol that separates command output.
package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"aiharn/internal/execution"
)

// Options are the channel settings needed to dial and authenticate an SSH
// transport. It deliberately duplicates config.ChannelConfig rather than
// importing it, keeping execution independent of the config package.
type Options struct {
	Host         string
	Port         int
	User         string
	KeyFile      string // local path to a private key (exclusive with Password)
	Password     string // exclusive with KeyFile
	KnownHosts   string // local path to an OpenSSH known_hosts file
	Insecure     bool   // if true, skip host-key verification (warned opt-out)
	KeepAlive    bool   // true = sessions share one client; false = one client per session
	DefaultShell string // e.g. "/bin/bash"

	// SSHConfigAlias, when true, treats Host as an OpenSSH ~/.ssh/config alias
	// and resolves HostName, Port, User, IdentityFile, and KnownHosts from it.
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
	if opts.SSHConfigAlias {
		configPath, err := defaultSSHConfigPath()
		if err != nil {
			return nil, err
		}
		opts, err = resolveAlias(opts, configPath)
		if err != nil {
			return nil, err
		}
	}
	if opts.Port == 0 {
		opts.Port = 22
	}
	if opts.User == "" {
		return nil, errors.New("ssh: user is required")
	}
	if opts.KeyFile == "" && opts.Password == "" {
		return nil, errors.New("ssh: key_file or password is required")
	}
	if opts.KeyFile != "" && opts.Password != "" {
		return nil, errors.New("ssh: key_file and password are mutually exclusive")
	}
	if !opts.Insecure && opts.KnownHosts == "" {
		return nil, errors.New("ssh: known_hosts is required (or set insecure)")
	}
	if opts.DefaultShell == "" {
		opts.DefaultShell = "/bin/bash"
	}
	return &Transport{opts: opts}, nil
}

// NewSession opens a persistent shell. With KeepAlive it reuses one shared
// client; otherwise it dials a dedicated client owned by the returned session.
func (t *Transport) NewSession(ctx context.Context) (execution.Session, error) {
	client, dedicated, err := t.acquireClient()
	if err != nil {
		return nil, err
	}
	var onDead func()
	if !dedicated {
		onDead = t.dropShared
	}
	s, err := openShellSession(client, t.opts, dedicated, onDead)
	if err != nil {
		if dedicated {
			client.Close()
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
		return err
	}
	return nil
}

func (t *Transport) acquireClient() (*ssh.Client, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, false, errors.New("ssh: transport closed")
	}
	if t.opts.KeepAlive {
		if t.shared == nil {
			c, err := dial(t.opts)
			if err != nil {
				return nil, false, err
			}
			t.shared = c
		}
		return t.shared, false, nil
	}
	c, err := dial(t.opts)
	if err != nil {
		return nil, false, err
	}
	return c, true, nil
}

// dropShared closes the shared client so the next NewSession redials. Called by
// a shared-client session when the connection is discovered to be dead.
func (t *Transport) dropShared() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.shared != nil {
		t.shared.Close()
		t.shared = nil
	}
}

func dial(opts Options) (*ssh.Client, error) {
	cfg, err := buildClientConfig(opts)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(opts.Host, strconv.Itoa(opts.Port))
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, sanitizeSSHError(err, opts)
	}
	return client, nil
}

func buildClientConfig(opts Options) (*ssh.ClientConfig, error) {
	var auth ssh.AuthMethod
	if opts.Password != "" {
		auth = ssh.Password(opts.Password)
	} else {
		key, err := os.ReadFile(opts.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("ssh: read key_file: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("ssh: parse key_file %s: %w", opts.KeyFile, err)
		}
		auth = ssh.PublicKeys(signer)
	}

	cfg := &ssh.ClientConfig{
		User: opts.User,
		Auth: []ssh.AuthMethod{auth},
	}
	if opts.Insecure {
		cfg.HostKeyCallback = ssh.InsecureIgnoreHostKey()
	} else {
		cb, err := knownhosts.New(opts.KnownHosts)
		if err != nil {
			return nil, fmt.Errorf("ssh: load known_hosts: %w", err)
		}
		cfg.HostKeyCallback = cb
	}
	return cfg, nil
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
