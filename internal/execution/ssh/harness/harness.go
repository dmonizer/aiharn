// Package harness provides an in-process SSH server for testing the ssh
// transport against a real SSH handshake and channel, without a system sshd. It
// implements host-key exchange, permissive authentication, session channels,
// exec/shell requests, exit-status, signals, and process-group cleanup.
package harness

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/crypto/ssh"
)

// Server is a running in-process SSH server.
type Server struct {
	shell    string
	listener net.Listener
	config   *ssh.ServerConfig
	hostKey  ssh.PublicKey
	host     string

	mu    sync.Mutex
	conns map[net.Conn]struct{}
	procs map[*exec.Cmd]struct{}
	closed bool
	wg    sync.WaitGroup

	errs chan error
}

// New starts a server on 127.0.0.1:0 accepting any client and running shell for
// remote commands. Call Close when done.
func New(shell string) (*Server, error) {
	if shell == "" {
		shell = "/bin/bash"
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}

	config := &ssh.ServerConfig{
		NoClientAuth: true,
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			return nil, nil
		},
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	config.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	s := &Server{
		shell:    shell,
		listener: ln,
		config:   config,
		hostKey:  signer.PublicKey(),
		host:     "127.0.0.1",
		conns:    make(map[net.Conn]struct{}),
		procs:    make(map[*exec.Cmd]struct{}),
		errs:     make(chan error, 64),
	}

	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Addr returns the host:port the server listens on.
func (s *Server) Addr() string { return s.listener.Addr().String() }

// Port returns the server's TCP port.
func (s *Server) Port() int {
	_, portStr, _ := net.SplitHostPort(s.listener.Addr().String())
	p, _ := strconv.Atoi(portStr)
	return p
}

// KnownHostsLine returns a single known_hosts entry for this server's host key.
func (s *Server) KnownHostsLine() string {
	key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.hostKey)))
	return fmt.Sprintf("[%s]:%d %s", s.host, s.Port(), key)
}

// WriteKnownHosts writes the server's known_hosts entry to path.
func (s *Server) WriteKnownHosts(path string) error {
	return os.WriteFile(path, []byte(s.KnownHostsLine()+"\n"), 0o600)
}

// Errors returns server-side errors (buffered; non-blocking to publish).
func (s *Server) Errors() <-chan error { return s.errs }

func (s *Server) publishErr(err error) {
	select {
	case s.errs <- err:
	default:
	}
}

func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		s.mu.Unlock()

		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
	}()

	sconn, chans, reqs, err := ssh.NewServerConn(conn, s.config)
	if err != nil {
		s.publishErr(fmt.Errorf("handshake: %w", err))
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		s.wg.Add(1)
		go s.handleChannel(newCh)
	}
}

func (s *Server) handleChannel(newCh ssh.NewChannel) {
	defer s.wg.Done()
	if newCh.ChannelType() != "session" {
		newCh.Reject(ssh.UnknownChannelType, "unsupported channel type")
		return
	}
	ch, reqs, err := newCh.Accept()
	if err != nil {
		return
	}

	var (
		mu   sync.Mutex
		cmd  *exec.Cmd
		done bool
	)

	reap := func(c *exec.Cmd) {
		c.Wait()
		mu.Lock()
		done = true
		mu.Unlock()

		status := uint32(0)
		if c.ProcessState != nil {
			if ee, ok := c.ProcessState.Sys().(interface{ ExitCode() int }); ok {
				status = uint32(ee.ExitCode())
			} else if !c.ProcessState.Success() {
				status = 1
			}
		}
		ch.SendRequest("exit-status", false, ssh.Marshal(struct{ ExitStatus uint32 }{status}))
		ch.Close()
	}

	kill := func() {
		mu.Lock()
		c := cmd
		reaped := done
		mu.Unlock()
		if c != nil && !reaped && c.Process != nil {
			_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		}
	}

	start := func(name string, args ...string) *exec.Cmd {
		c := exec.Command(name, args...)
		c.Stdin = ch
		c.Stdout = ch
		c.Stderr = ch.Stderr()
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil {
			s.publishErr(fmt.Errorf("start %s: %w", name, err))
			return nil
		}
		s.mu.Lock()
		s.procs[c] = struct{}{}
		s.mu.Unlock()
		return c
	}

	go func() {
		for req := range reqs {
			switch req.Type {
			case "exec":
				var p struct{ Command string }
				if err := ssh.Unmarshal(req.Payload, &p); err != nil {
					req.Reply(false, nil)
					continue
				}
				c := start(s.shell, "-c", p.Command)
				mu.Lock()
				cmd = c
				mu.Unlock()
				req.Reply(true, nil)
				if c != nil {
					go reap(c)
				} else {
					ch.Close()
				}
			case "shell":
				c := start(s.shell, "--noprofile", "--norc", "-s")
				mu.Lock()
				cmd = c
				mu.Unlock()
				req.Reply(true, nil)
				if c != nil {
					go reap(c)
				} else {
					ch.Close()
				}
			case "signal":
				var p struct{ Signal string }
				ssh.Unmarshal(req.Payload, &p)
				req.Reply(true, nil)
				mu.Lock()
				c := cmd
				mu.Unlock()
				if c != nil && c.Process != nil {
					_ = c.Process.Signal(signalByName(p.Signal))
				}
			default:
				req.Reply(false, nil)
			}
		}
		kill()
	}()
}

// Close stops the server, closes connections, and kills any live processes.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.listener.Close()
	for conn := range s.conns {
		conn.Close()
	}
	for c := range s.procs {
		if c.Process != nil {
			_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		}
	}
	s.mu.Unlock()

	s.wg.Wait()
	return nil
}

func signalByName(name string) syscall.Signal {
	switch strings.ToUpper(name) {
	case "TERM", "SIGTERM":
		return syscall.SIGTERM
	case "KILL", "SIGKILL":
		return syscall.SIGKILL
	case "INT", "SIGINT":
		return syscall.SIGINT
	case "HUP", "SIGHUP":
		return syscall.SIGHUP
	default:
		return syscall.SIGTERM
	}
}
