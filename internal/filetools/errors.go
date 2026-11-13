// Package filetools implements the domain logic behind the read, write, and
// edit text-file tools: the shared line model, bounded reads, read receipts
// (read-before-edit), atomic publications, and the stable error vocabulary that
// reaches the model. It knows nothing about tool schemas, approval prompts, the
// agent loop, or channel selection; the app supplies a channel-bound
// execution.Filesystem per operation.
package filetools

import (
	"context"
	"errors"
	"fmt"

	"aiharn/internal/execution"
)

// Stable error codes. Everything a caller can observe is one of these; the
// codes are part of the model-facing contract and must stay stable.
const (
	// CodeRootNotConfigured reports a channel without a configured root.
	CodeRootNotConfigured = "root_not_configured"
	// CodeInvalidText reports invalid UTF-8 or NUL bytes.
	CodeInvalidText = "invalid_text"
	// CodeMixedNewlines reports a file mixing LF and CRLF line endings.
	CodeMixedNewlines = "mixed_newlines"
	// CodeInvalidRange reports zero-based, end-exclusive coordinates outside the
	// file or inverted.
	CodeInvalidRange = "invalid_range"
	// CodeReadLimitExceeded reports a read that exceeds the line or byte limit.
	CodeReadLimitExceeded = "read_limit_exceeded"
	// CodeReadRequired reports a mutation without a qualifying read receipt.
	CodeReadRequired = "read_required"
	// CodeReadScopeMismatch reports a receipt that does not cover what is being
	// replaced, or that belongs to another scope.
	CodeReadScopeMismatch = "read_scope_mismatch"
	// CodeChannelChanged reports a channel switch between approval and commit.
	CodeChannelChanged = "channel_changed"
	// CodeApprovalDenied reports a denied approval. No publication occurred.
	CodeApprovalDenied = "approval_denied"
	// CodeCancelled reports cancellation before publication.
	CodeCancelled = "cancelled"
)

// Codes reused from the execution filesystem contract.
const (
	CodeOutsideRoot         = execution.FSOutsideRoot
	CodeSymlinkNotAllowed   = execution.FSSymlinkNotAllowed
	CodeNotRegularFile      = execution.FSNotRegularFile
	CodeNotFound            = execution.FSNotFound
	CodeParentNotFound      = execution.FSParentNotFound
	CodeAlreadyExists       = execution.FSAlreadyExists
	CodeFileLimitExceeded   = execution.FSFileLimitExceeded
	CodeStaleRead           = execution.FSStaleRead
	CodeHardLinksNotAllowed = execution.FSHardLinksNotAllowed
	CodePermissionDenied    = execution.FSPermissionDenied
	CodeIOError             = execution.FSIOError
	CodeUnsupported         = execution.FSUnsupportedFilesystem
	CodeOutcomeUnknown      = execution.FSOutcomeUnknown
)

// Error is a file-tool failure with a stable code, a concise message, and an
// actionable recovery hint. Messages never include unrelated file content.
type Error struct {
	Code    string
	Message string
	Hint    string
}

// ApprovalDenied returns the model-facing failure for a denied change.
func ApprovalDenied(filename string) error {
	return &Error{
		Code:    CodeApprovalDenied,
		Message: fmt.Sprintf("the user denied the change to %q", filename),
		Hint:    hintFor(CodeApprovalDenied),
	}
}

// Cancelled returns the model-facing failure for an operation that was
// cancelled before publication.
func Cancelled(filename string) error {
	return &Error{
		Code:    CodeCancelled,
		Message: fmt.Sprintf("the operation on %q was cancelled before publication", filename),
		Hint:    hintFor(CodeCancelled),
	}
}

// Errorf builds an *Error.
func Errorf(code, message, hint string) *Error {
	return &Error{Code: code, Message: message, Hint: hint}
}

// Error implements error as "code: message (hint)".
func (e *Error) Error() string {
	s := e.Code + ": " + e.Message
	if e.Hint != "" {
		s += " (" + e.Hint + ")"
	}
	return s
}

// Code returns the stable code of err: a file-tool code, the code of an
// underlying filesystem error, "cancelled" for context cancellation, or
// io_error for anything else. A nil error yields "".
func Code(err error) string {
	if err == nil {
		return ""
	}
	var fe *Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	if code := execution.FSECode(err); code != "" {
		return code
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return CodeCancelled
	}
	return CodeIOError
}

// HasCode reports whether err carries code.
func HasCode(err error, code string) bool { return Code(err) == code }
