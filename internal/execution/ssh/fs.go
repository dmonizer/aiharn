package ssh

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"aiharn/internal/execution"
	"aiharn/internal/execution/fsproto"
	"aiharn/internal/logging"
)

// Filesystem helper settings.
const (
	// defaultFSHelper is the remote command that starts the filesystem helper
	// when Options.FSHelper is empty.
	defaultFSHelper = "aiharn fs-helper"

	// maxHelperFileBytes bounds the file content exchanged with the helper. It
	// matches the default [file_tools] max_file_bytes (10 MiB) and still fits one
	// fsproto frame: base64 expands it to ~13.4 MiB, leaving room for the JSON
	// envelope inside the 16 MiB frame cap.
	maxHelperFileBytes = 10 << 20

	// helperExitGrace is how long the init handshake waits for a final frame
	// after the helper process exits, so its protocol answer (if any) wins over
	// the exit.
	helperExitGrace = 200 * time.Millisecond

	// fsInitTimeout bounds starting the helper and completing the init
	// handshake, so a helper that never answers cannot stall NewFilesystem.
	fsInitTimeout = 15 * time.Second

	// fsCloseTimeout bounds the best-effort close frame sent by Close.
	fsCloseTimeout = 2 * time.Second
)

// errFilesystemClosed reports a request after (or during) Close.
var errFilesystemClosed = errors.New("ssh: filesystem is closed")

// compile-time assertions
var (
	_ execution.FilesystemProvider = (*Transport)(nil)
	_ execution.Filesystem         = (*filesystem)(nil)
)

// filesystem implements execution.Filesystem on a remote host by running the
// fsproto helper on a dedicated SSH client connection. Requests are serialized:
// one frame is outstanding at a time, so request/response pairing never depends
// on correlation ids.
type filesystem struct {
	client  *ssh.Client
	session *ssh.Session
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	target  string

	mu        sync.Mutex // serializes requests (one outstanding frame)
	closeOnce sync.Once
	connOnce  sync.Once
	waitOnce  sync.Once
	exited    chan struct{} // closed once the helper process has exited
	closed    atomic.Bool
}

// NewFilesystem implements execution.FilesystemProvider. It opens a dedicated
// SSH client connection (a session's shared client is never reused or
// disturbed), starts Options.FSHelper on a new session channel, and performs the
// protocol init handshake for root.
//
// Errors are *execution.FSError values. A helper that cannot be started or that
// does not speak this protocol version yields FSUnsupportedFilesystem with
// instructions for installing and selecting the matching helper; a dial or I/O
// failure yields FSIOError; a well-formed refusal from the helper (for example a
// missing root) is returned with its own code.
func (t *Transport) NewFilesystem(ctx context.Context, root string) (execution.Filesystem, error) {
	canonical, err := canonicalRoot(root)
	if err != nil {
		return nil, err
	}
	client, err := t.dialDedicated(ctx)
	if err != nil {
		return nil, err
	}
	fs, err := startFilesystem(ctx, client, t.opts, canonical)
	if err != nil {
		client.Close()
		return nil, err
	}
	return fs, nil
}

// canonicalRoot validates and canonicalizes a remote root path. It uses slash
// semantics because the path is interpreted on the execution host, not locally.
func canonicalRoot(root string) (string, error) {
	if root == "" || !strings.HasPrefix(root, "/") {
		return "", execution.NewFSError(execution.FSOutsideRoot, root, "root must be an absolute path on the execution host")
	}
	for _, part := range strings.Split(root, "/") {
		if part == ".." {
			return "", execution.NewFSError(execution.FSOutsideRoot, root, "root must not contain traversal components")
		}
	}
	return path.Clean(root), nil
}

// dialDedicated opens a client connection owned by one filesystem, so closing
// the filesystem cannot affect a shared session client.
func (t *Transport) dialDedicated(ctx context.Context) (*ssh.Client, error) {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return nil, execution.NewFSError(execution.FSIOError, "", "ssh: transport is closed")
	}
	client, err := dial(ctx, t.opts)
	if err != nil {
		return nil, execution.NewFSError(execution.FSIOError, "", fmt.Sprintf("ssh: dial %s for filesystem: %v", t.opts.Host, err))
	}
	logging.Debug("ssh: NewFilesystem dial ok", slog.String("component", "ssh"), slog.String("host", t.opts.Host))
	return client, nil
}

// startFilesystem starts the helper command on a new session channel of client
// and completes the init handshake. The caller owns client and closes it on
// error.
func startFilesystem(ctx context.Context, client *ssh.Client, opts Options, root string) (*filesystem, error) {
	helper := opts.FSHelper
	if helper == "" {
		helper = defaultFSHelper
	}
	sess, err := newSSHSession(ctx, client)
	if err != nil {
		return nil, helperUnavailable(helper, root, fmt.Errorf("open session: %w", err), "")
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return nil, closeAndUnavailable(sess, helper, root, fmt.Errorf("stdin pipe: %w", err), "")
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return nil, closeAndUnavailable(sess, helper, root, fmt.Errorf("stdout pipe: %w", err), "")
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		return nil, closeAndUnavailable(sess, helper, root, fmt.Errorf("stderr pipe: %w", err), "")
	}
	if err := sess.Start(helper); err != nil {
		return nil, closeAndUnavailable(sess, helper, root, fmt.Errorf("start %q: %w", helper, err), "")
	}
	// Drain helper stderr into a bounded tail so startup diagnostics (for
	// example "command not found") can explain an unsupported helper, and so a
	// chatty helper cannot backpressure on a full stderr pipe.
	tail := newTailBuffer(4 << 10)
	go func() { _, _ = io.Copy(tail, stderr) }()

	fs := &filesystem{
		client:  client,
		session: sess,
		stdin:   stdin,
		stdout:  bufio.NewReader(stdout),
		target:  filesystemTarget(opts, root),
	}
	initCtx, cancel := context.WithTimeout(ctx, fsInitTimeout)
	defer cancel()
	if err := fs.handshake(initCtx, root); err != nil {
		fs.closeConnection()
		var fe *execution.FSError
		if errors.As(err, &fe) && fe.Code != "" && fe.Code != execution.FSUnsupportedFilesystem {
			// The helper answered with a real filesystem error (for example a
			// missing root): surface it instead of blaming the helper binary.
			return nil, err
		}
		return nil, helperUnavailable(helper, root, err, tail.String())
	}
	logging.Debug("ssh: NewFilesystem ready",
		slog.String("component", "ssh"),
		slog.String("target", fs.target),
		slog.String("helper", helper),
	)
	return fs, nil
}

// closeAndUnavailable starts an unsupported error and closes a half-open
// session on the way out.
func closeAndUnavailable(sess *ssh.Session, helper, root string, cause error, stderr string) error {
	_ = sess.Close()
	return helperUnavailable(helper, root, cause, stderr)
}

// helperUnavailable builds the operator-facing FSUnsupportedFilesystem error.
// detail explains the immediate cause; stderr is the helper's bounded stderr
// tail, which usually contains the remote shell's "command not found".
func helperUnavailable(helper, root string, detail error, stderr string) error {
	message := fmt.Sprintf("remote filesystem helper %q is unavailable: %v", helper, detail)
	if stderr != "" {
		message += " (helper stderr: " + stderr + ")"
	}
	message += fmt.Sprintf("; install the aiharn binary matching helper version %s on the execution host (it provides `aiharn fs-helper`) and select it with the channel's fs_helper option", fsproto.HelperVersion)
	return execution.NewFSError(execution.FSUnsupportedFilesystem, root, message)
}

// filesystemTarget identifies the backend by channel kind, user, host, port,
// and canonical root.
func filesystemTarget(opts Options, root string) string {
	return fmt.Sprintf("ssh:%s@%s:%d|%s", opts.User, opts.Host, opts.Port, root)
}

// handshake performs the init exchange.
func (f *filesystem) handshake(ctx context.Context, root string) error {
	req := fsproto.Request{
		Op:           fsproto.OpInit,
		Protocol:     fsproto.ProtocolVersion,
		Root:         root,
		MaxFileBytes: maxHelperFileBytes,
	}
	if err := fsproto.WriteFrame(f.stdin, req); err != nil {
		return fmt.Errorf("send init: %w", err)
	}
	resp, err := f.readInitResponse(ctx)
	if err != nil {
		return fmt.Errorf("read init response: %w", err)
	}
	if err := resp.Err(); err != nil {
		return err
	}
	if resp.Protocol != fsproto.ProtocolVersion {
		return fmt.Errorf("helper speaks protocol %d, want %d", resp.Protocol, fsproto.ProtocolVersion)
	}
	logging.Debug("ssh: filesystem handshake ok",
		slog.String("component", "ssh"),
		slog.String("root", resp.Root),
		slog.String("helper_version", resp.HelperVersion),
	)
	return nil
}

// readInitResponse waits for the helper's init answer, giving up as soon as the
// helper process exits (a missing binary or a crashed helper must not cost the
// full init timeout) or ctx is done. A frame that arrives just before the exit
// still wins, within helperExitGrace.
func (f *filesystem) readInitResponse(ctx context.Context) (fsproto.Response, error) {
	type result struct {
		resp fsproto.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var resp fsproto.Response
		err := fsproto.ReadFrame(f.stdout, &resp)
		done <- result{resp: resp, err: err}
	}()
	exited := f.waitChannel()
	select {
	case got := <-done:
		return got.resp, got.err
	case <-exited:
		select {
		case got := <-done:
			return got.resp, got.err
		case <-time.After(helperExitGrace):
			return fsproto.Response{}, errors.New("helper exited before answering the init handshake")
		}
	case <-ctx.Done():
		return fsproto.Response{}, ctx.Err()
	}
}

// waitChannel returns a channel that is closed once the helper process exits.
// The wait is started once and never repeated.
func (f *filesystem) waitChannel() <-chan struct{} {
	f.waitOnce.Do(func() {
		f.exited = make(chan struct{})
		go func() {
			defer close(f.exited)
			_ = f.session.Wait()
		}()
	})
	return f.exited
}

// Target implements execution.Filesystem.
func (f *filesystem) Target() string { return f.target }

// ReadFile implements execution.Filesystem. A lost connection is an IO error:
// a read has no side effect to be uncertain about.
func (f *filesystem) ReadFile(ctx context.Context, name string, maxBytes int64) (execution.FileSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	resp, err := f.roundTrip(ctx, fsproto.Request{Op: fsproto.OpRead, Name: name, MaxBytes: maxBytes})
	if err != nil {
		return execution.FileSnapshot{}, execution.NewFSError(execution.FSIOError, name,
			"read aborted before a reply arrived: "+err.Error())
	}
	if err := resp.Err(); err != nil {
		return execution.FileSnapshot{}, err
	}
	if resp.Stat == nil {
		return execution.FileSnapshot{}, execution.NewFSError(execution.FSIOError, name, "helper reported success without file metadata")
	}
	if maxBytes > 0 && int64(len(resp.Content)) > maxBytes {
		return execution.FileSnapshot{}, execution.NewFSError(execution.FSFileLimitExceeded, name,
			fmt.Sprintf("helper returned %d bytes, caller bound is %d", len(resp.Content), maxBytes))
	}
	return execution.FileSnapshot{Stat: *resp.Stat, Content: resp.Content}, nil
}

// CreateFile implements execution.Filesystem. Once the request is on the wire a
// lost connection yields FSOutcomeUnknown, never a claim of failure or rollback.
func (f *filesystem) CreateFile(ctx context.Context, name string, content []byte) (execution.FileStat, error) {
	if err := checkHelperContent(name, content); err != nil {
		return execution.FileStat{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	resp, err := f.roundTrip(ctx, fsproto.Request{Op: fsproto.OpCreate, Name: name, Content: content})
	return mutationResult(name, "create", resp, err)
}

// ReplaceFile implements execution.Filesystem. Version checks run on the host,
// so a concurrent change is reported as stale_read by the helper; a lost
// connection yields FSOutcomeUnknown.
func (f *filesystem) ReplaceFile(ctx context.Context, name string, content []byte, expected execution.FileVersion) (execution.FileStat, error) {
	if err := checkHelperContent(name, content); err != nil {
		return execution.FileStat{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	resp, err := f.roundTrip(ctx, fsproto.Request{
		Op:       fsproto.OpReplace,
		Name:     name,
		Content:  content,
		Expected: &expected,
	})
	return mutationResult(name, "replace", resp, err)
}

// mutationResult maps one mutation exchange onto a stat or an error. A
// transport failure after the request was sent is FSOutcomeUnknown: the helper
// may already have published the file, and no rollback was attempted.
func mutationResult(name, op string, resp fsproto.Response, err error) (execution.FileStat, error) {
	if err != nil {
		if errors.Is(err, errFilesystemClosed) {
			// Close was called before the request reached the helper.
			return execution.FileStat{}, execution.NewFSError(execution.FSIOError, name, "filesystem is closed; "+op+" was not sent")
		}
		return execution.FileStat{}, execution.NewFSError(execution.FSOutcomeUnknown, name,
			fmt.Sprintf("%s failed after the request was sent (%v); the file may or may not have been published, no rollback was attempted: read the file again before retrying", op, err))
	}
	if err := resp.Err(); err != nil {
		return execution.FileStat{}, err
	}
	if resp.Stat == nil {
		return execution.FileStat{}, execution.NewFSError(execution.FSOutcomeUnknown, name,
			op+" reported success without file metadata; read the file again before relying on it")
	}
	return *resp.Stat, nil
}

// checkHelperContent refuses content that could not fit in one protocol frame.
func checkHelperContent(name string, content []byte) error {
	if int64(len(content)) > maxHelperFileBytes {
		return execution.NewFSError(execution.FSFileLimitExceeded, name,
			fmt.Sprintf("content is %d bytes, the remote helper accepts at most %d", len(content), maxHelperFileBytes))
	}
	return nil
}

// roundTrip sends one request and reads its response. The caller must hold mu.
// It checks ctx before sending so a canceled caller never starts a mutation.
func (f *filesystem) roundTrip(ctx context.Context, req fsproto.Request) (fsproto.Response, error) {
	var resp fsproto.Response
	if err := ctx.Err(); err != nil {
		return resp, err
	}
	if f.closed.Load() {
		return resp, errFilesystemClosed
	}
	if err := fsproto.WriteFrame(f.stdin, req); err != nil {
		return resp, err
	}
	err := readFrameContext(ctx, f.stdout, &resp)
	if err != nil && ctx.Err() != nil {
		// The request may already be running on the host and the stream can no
		// longer be trusted to stay aligned, so drop the connection.
		f.closeConnection()
	}
	return resp, err
}

// readFrameContext reads one frame, giving up when ctx is done. The pending
// read goroutine exits once the caller closes the connection, so a canceled
// handshake or request must close it.
func readFrameContext(ctx context.Context, r io.Reader, msg any) error {
	done := make(chan error, 1)
	go func() { done <- fsproto.ReadFrame(r, msg) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close releases the helper channel and the dedicated client. It sends a
// best-effort close frame when no request is in flight, never blocks on an
// in-flight request, and is idempotent.
func (f *filesystem) Close() error {
	f.closeOnce.Do(func() {
		f.closed.Store(true)
		if f.mu.TryLock() {
			f.sendCloseFrame()
			f.mu.Unlock()
		}
		f.closeConnection()
	})
	return nil
}

// sendCloseFrame tells the helper to end the session, with a bounded wait so a
// wedged peer cannot block Close.
func (f *filesystem) sendCloseFrame() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = fsproto.WriteFrame(f.stdin, fsproto.Request{Op: fsproto.OpClose})
	}()
	select {
	case <-done:
	case <-time.After(fsCloseTimeout):
	}
}

// closeConnection tears down the channel and client exactly once; it also
// unblocks any pending frame read.
func (f *filesystem) closeConnection() {
	f.connOnce.Do(func() {
		f.waitChannel() // reap the helper process
		if f.session != nil {
			_ = f.session.Close()
		}
		if f.client != nil {
			_ = f.client.Close()
		}
	})
}

// tailBuffer keeps the last max bytes written to it. It never blocks the
// writer, so draining a child's stderr cannot backpressure the child.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

// Write implements io.Writer.
func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-b.max:]...)
	}
	return len(p), nil
}

// String returns the retained tail without trailing whitespace.
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}
