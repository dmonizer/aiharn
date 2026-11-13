package fshelper

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"aiharn/internal/execution"
	"aiharn/internal/execution/fsproto"
	"aiharn/internal/logging"
)

// Serve runs one helper session: it reads framed requests from r, writes framed
// responses to w, and returns when the client sends close, the stream ends, or
// a protocol or transport failure makes further progress impossible.
//
// Serve writes frames and nothing else to w; diagnostics go to the process's
// stderr (through the logging package), so stdout stays a clean protocol
// stream. A non-nil error means the session ended abnormally and the helper
// process should exit non-zero.
func Serve(r io.Reader, w io.Writer, open Factory) error {
	req, err := readInit(r)
	if err != nil {
		logf("init handshake failed: %v", err)
		return err
	}
	if req.Protocol != fsproto.ProtocolVersion {
		// A version mismatch is the peer's only chance to learn what to do, so
		// answer it as a well-formed unsupported response before ending.
		msg := fmt.Sprintf("fs-helper speaks protocol %d, client requested %d; install the aiharn binary matching version %s and select it with the channel's fs_helper option",
			fsproto.ProtocolVersion, req.Protocol, fsproto.HelperVersion)
		logf("%s", msg)
		_ = fsproto.WriteFrame(w, fsproto.NewErrorResponse(req.Root, execution.NewFSError(execution.FSUnsupportedFilesystem, req.Root, msg)))
		return nil
	}
	backend, err := open(req.Root)
	if err != nil {
		logf("open root %q: %v", req.Root, err)
		_ = fsproto.WriteFrame(w, fsproto.NewErrorResponse(req.Root, err))
		return nil
	}
	defer func() {
		if err := backend.Close(); err != nil {
			logf("close backend: %v", err)
		}
	}()

	s := &session{backend: backend, w: w, maxContent: req.MaxFileBytes}
	if err := s.send(fsproto.Response{
		OK:            true,
		Protocol:      fsproto.ProtocolVersion,
		HelperVersion: fsproto.HelperVersion,
		Root:          req.Root,
	}); err != nil {
		logf("write init response: %v", err)
		return err
	}
	logging.Debug("fshelper: session started",
		slog.String("component", "fshelper"),
		slog.String("root", req.Root),
		slog.String("target", backend.Target()),
		slog.Int64("max_file_bytes", req.MaxFileBytes),
	)
	return s.run(r)
}

// readInit reads the first frame and requires it to be an init request.
func readInit(r io.Reader) (fsproto.Request, error) {
	var req fsproto.Request
	if err := fsproto.ReadFrame(r, &req); err != nil {
		if errors.Is(err, io.EOF) {
			return req, fmt.Errorf("%w: stream ended before init", fsproto.ErrProtocol)
		}
		return req, err
	}
	if req.Op != fsproto.OpInit {
		return req, &fsproto.ProtocolError{Op: string(req.Op), Message: "first request must be init"}
	}
	if req.Root == "" {
		return req, &fsproto.ProtocolError{Op: string(req.Op), Message: "init carries no root"}
	}
	return req, nil
}

// session is the state of one accepted helper session.
type session struct {
	backend    Backend
	w          io.Writer
	maxContent int64 // 0 means no protocol-level content bound
}

// run consumes requests until close or a failure. A clean end of stream (the
// client hung up without close) is not an error.
func (s *session) run(r io.Reader) error {
	for {
		var req fsproto.Request
		if err := fsproto.ReadFrame(r, &req); err != nil {
			if errors.Is(err, io.EOF) {
				logging.Debug("fshelper: stream closed by client", slog.String("component", "fshelper"))
				return nil
			}
			logf("read request: %v", err)
			return err
		}
		done, err := s.handle(req)
		if err != nil {
			logf("handle %s: %v", req.Op, err)
			return err
		}
		if done {
			logging.Debug("fshelper: close requested", slog.String("component", "fshelper"))
			return nil
		}
	}
}

// handle dispatches one request and reports whether it ended the session.
func (s *session) handle(req fsproto.Request) (done bool, err error) {
	op, err := fsproto.ParseOp(string(req.Op))
	if err != nil {
		return false, err
	}
	switch op {
	case fsproto.OpInit:
		return false, &fsproto.ProtocolError{Op: string(op), Message: "session already initialized"}
	case fsproto.OpPing:
		return false, s.send(fsproto.Response{OK: true})
	case fsproto.OpClose:
		return true, s.send(fsproto.Response{OK: true})
	case fsproto.OpRead:
		return false, s.handleRead(req)
	case fsproto.OpCreate:
		return false, s.handleCreate(req)
	case fsproto.OpReplace:
		return false, s.handleReplace(req)
	default:
		return false, &fsproto.ProtocolError{Op: string(op)}
	}
}

func (s *session) handleRead(req fsproto.Request) error {
	snap, err := s.backend.Read(req.Name, req.MaxBytes)
	if err != nil {
		return s.fail(req.Name, err)
	}
	if limit := s.maxContent; limit > 0 && int64(len(snap.Content)) > limit {
		return s.fail(req.Name, execution.NewFSError(execution.FSFileLimitExceeded, req.Name,
			fmt.Sprintf("helper session bounds file content at %d bytes, file would return %d", limit, len(snap.Content))))
	}
	stat := snap.Stat
	return s.send(fsproto.Response{OK: true, Stat: &stat, Content: snap.Content})
}

func (s *session) handleCreate(req fsproto.Request) error {
	if err := s.checkContent(req); err != nil {
		return s.fail(req.Name, err)
	}
	stat, err := s.backend.Create(req.Name, req.Content)
	if err != nil {
		return s.fail(req.Name, err)
	}
	return s.send(fsproto.Response{OK: true, Stat: &stat})
}

func (s *session) handleReplace(req fsproto.Request) error {
	if req.Expected == nil {
		return &fsproto.ProtocolError{Op: string(fsproto.OpReplace), Message: "replace carries no expected version"}
	}
	if err := s.checkContent(req); err != nil {
		return s.fail(req.Name, err)
	}
	stat, err := s.backend.Replace(req.Name, req.Content, *req.Expected)
	if err != nil {
		return s.fail(req.Name, err)
	}
	return s.send(fsproto.Response{OK: true, Stat: &stat})
}

// checkContent enforces the init-time content bound before touching the
// backend, so an oversized body is refused without any side effect.
func (s *session) checkContent(req fsproto.Request) error {
	if limit := s.maxContent; limit > 0 && int64(len(req.Content)) > limit {
		return execution.NewFSError(execution.FSFileLimitExceeded, req.Name,
			fmt.Sprintf("content is %d bytes, helper session bounds it at %d", len(req.Content), limit))
	}
	return nil
}

// send writes a response frame. A write failure ends the session: the client is
// gone, so there is no peer left to answer.
func (s *session) send(resp fsproto.Response) error {
	return fsproto.WriteFrame(s.w, resp)
}

// fail reports a backend error as a response with its stable code.
func (s *session) fail(name string, err error) error {
	logging.Debug("fshelper: backend failure",
		slog.String("component", "fshelper"),
		slog.String("name", name),
		slog.String("code", execution.FSECode(err)),
	)
	return s.send(fsproto.NewErrorResponse(name, err))
}

// logf writes a diagnostic to stderr. The helper owns stdout exclusively for
// protocol frames, so every human-readable message must come through here.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fshelper: "+format+"\n", args...)
}
