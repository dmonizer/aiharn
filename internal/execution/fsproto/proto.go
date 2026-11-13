// Package fsproto defines the framed request/response protocol that an
// execution host's filesystem helper speaks over one bidirectional byte stream
// (the stdio of an SSH session channel). The client side lives in the ssh
// transport; the helper side lives in fshelper.
//
// The protocol is deliberately small and strictly bounded: every frame is a
// 4-byte big-endian body length followed by a JSON body, and no body may exceed
// MaxFrameSize, so a corrupt or hostile peer cannot exhaust client memory.
package fsproto

import (
	"errors"
	"strings"

	"aiharn/internal/execution"
)

// ProtocolVersion is the wire protocol version implemented by this build. A
// client and helper must agree exactly; any difference is reported as an
// unsupported filesystem so the operator can install a matching helper.
const ProtocolVersion = 1

// MaxFrameSize is the hard upper bound on one frame body in bytes (16 MiB).
// A peer that announces a larger body is a protocol error and the stream is
// closed without allocating the announced size.
const MaxFrameSize = 16 << 20

// HelperVersion identifies the helper implementation. It is reported in the
// init response and by `aiharn fs-helper --version` for capability checks.
const HelperVersion = "1"

// Op identifies one protocol operation.
type Op string

// Protocol operations. init must be the first request of a session; close ends
// it. read, create, and replace map onto the corresponding execution.Filesystem
// methods.
const (
	OpInit    Op = "init"
	OpRead    Op = "read"
	OpCreate  Op = "create"
	OpReplace Op = "replace"
	OpPing    Op = "ping"
	OpClose   Op = "close"
)

// ErrInvalidFrame reports a frame that is empty, truncated, or not valid JSON.
// It is a protocol error: the peer must close the stream rather than guess.
var ErrInvalidFrame = errors.New("fsproto: invalid frame")

// ErrFrameTooLarge reports a frame body that exceeds MaxFrameSize, either
// announced by the peer or produced by this side.
var ErrFrameTooLarge = errors.New("fsproto: frame exceeds maximum size")

// ErrProtocol reports a well-formed frame that violates the protocol sequence,
// such as an unknown op or a read request before init.
var ErrProtocol = errors.New("fsproto: protocol violation")

// Request is one client-to-helper message. Fields are populated per Op:
//
//	init:    Protocol, Root, MaxFileBytes
//	read:    Name, MaxBytes
//	create:  Name, Content
//	replace: Name, Content, Expected
//	ping:    (none)
//	close:   (none)
type Request struct {
	Op       Op     `json:"op"`
	Protocol int    `json:"protocol,omitempty"`
	Root     string `json:"root,omitempty"`
	// MaxFileBytes is the largest file content the session may exchange. The
	// client picks a value whose base64 encoding plus JSON overhead still fits
	// in one MaxFrameSize frame.
	MaxFileBytes int64                  `json:"max_file_bytes,omitempty"`
	Name         string                 `json:"name,omitempty"`
	MaxBytes     int64                  `json:"max_bytes,omitempty"`
	Content      []byte                 `json:"content,omitempty"`
	Expected     *execution.FileVersion `json:"expected,omitempty"`
}

// Response is one helper-to-client message. OK reports success; otherwise Error
// carries the stable execution.FS* code that the client rebuilds as an
// *execution.FSError. Content is base64 encoded by encoding/json.
type Response struct {
	OK            bool                `json:"ok"`
	Protocol      int                 `json:"protocol,omitempty"`
	HelperVersion string              `json:"helper_version,omitempty"`
	Root          string              `json:"root,omitempty"`
	Stat          *execution.FileStat `json:"stat,omitempty"`
	Content       []byte              `json:"content,omitempty"`
	Error         *ErrorPayload       `json:"error,omitempty"`
}

// ErrorPayload is a failed response body. It mirrors execution.FSError so the
// client can rebuild the original code, message, and path.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path,omitempty"`
}

// NewErrorResponse builds a failed response for err. An *execution.FSError keeps
// its code, message, and path (path falls back to the supplied name when the
// error carries none); any other error becomes io_error, because the helper
// cannot prove that a generic failure left the destination untouched.
func NewErrorResponse(name string, err error) Response {
	var fe *execution.FSError
	if errors.As(err, &fe) && fe.Code != "" {
		path := fe.Path
		if path == "" {
			path = name
		}
		return Response{Error: &ErrorPayload{Code: fe.Code, Message: fe.Message, Path: path}}
	}
	message := "unknown error"
	if err != nil {
		message = err.Error()
	}
	return Response{Error: &ErrorPayload{Code: execution.FSIOError, Message: message, Path: name}}
}

// Err returns the failure described by the response as an *execution.FSError,
// or nil when the response reports success. A failure without an error body is
// reported as io_error rather than silently treated as success.
func (r Response) Err() error {
	if r.OK {
		return nil
	}
	if r.Error == nil {
		return execution.NewFSError(execution.FSIOError, "", "helper reported failure without an error body")
	}
	code := r.Error.Code
	if code == "" {
		code = execution.FSIOError
	}
	return execution.NewFSError(code, r.Error.Path, r.Error.Message)
}

// IsKnownOp reports whether op is part of the protocol.
func IsKnownOp(op Op) bool {
	switch op {
	case OpInit, OpRead, OpCreate, OpReplace, OpPing, OpClose:
		return true
	default:
		return false
	}
}

// ParseOp validates a wire op string, returning ErrProtocol for anything this
// version does not implement. Unknown ops close the stream instead of being
// answered, so a client with a newer protocol cannot mistake a refusal for a
// successful operation.
func ParseOp(op string) (Op, error) {
	o := Op(op)
	if !IsKnownOp(o) {
		return "", &ProtocolError{Op: op}
	}
	return o, nil
}

// ProtocolError reports an unsupported or out-of-sequence operation.
type ProtocolError struct {
	Op      string
	Message string
}

// Error implements error.
func (e *ProtocolError) Error() string {
	if e.Message != "" {
		return "fsproto: protocol violation: " + e.Message
	}
	return "fsproto: protocol violation: unsupported op " + strings.TrimSpace(e.Op)
}

// Unwrap makes errors.Is(err, ErrProtocol) hold for protocol errors.
func (e *ProtocolError) Unwrap() error { return ErrProtocol }
