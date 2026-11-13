package filetools

import (
	"context"
	"errors"

	"aiharn/internal/execution"
)

// hints gives every stable code an actionable recovery hint. Codes reach the
// model inside the tool failure text, so the hint must say what to do next.
var hints = map[string]string{
	CodeRootNotConfigured:   "configure `root` on the selected execution channel",
	CodeOutsideRoot:         "use a path relative to the channel root; absolute paths outside the root, parent traversal, and empty names are rejected",
	CodeSymlinkNotAllowed:   "use a direct regular-file path; symbolic links are not followed",
	CodeNotRegularFile:      "select a regular text file, not a directory, pipe, socket, or device",
	CodeNotFound:            "check the path, or read the parent directory listing first",
	CodeParentNotFound:      "the parent directory must already exist; write never creates directories",
	CodeAlreadyExists:       "read and edit the existing file instead of overwriting it",
	CodeInvalidText:         "supply UTF-8 text without NUL bytes; binary files are unsupported",
	CodeMixedNewlines:       "the file mixes CRLF and LF line endings; edit a file with consistent line endings",
	CodeInvalidRange:        "use zero-based, end-exclusive coordinates with 0 <= start_line <= end_line <= file_line_count",
	CodeReadLimitExceeded:   "request a smaller range with start_line/end_line",
	CodeFileLimitExceeded:   "the file or the proposed result exceeds the configured limit",
	CodeReadRequired:        "read the file section with `read` first, then pass that read_id to `edit`",
	CodeReadScopeMismatch:   "read exactly the section this edit touches with `read`, then pass that read_id",
	CodeStaleRead:           "the file changed; read it again before editing",
	CodeChannelChanged:      "the execution channel changed while approval was pending; read the file again on the intended channel",
	CodeHardLinksNotAllowed: "editing a file with multiple hard links is unsupported",
	CodeApprovalDenied:      "the user denied the change; no file was modified",
	CodeCancelled:           "the operation was cancelled; no file was modified",
	CodePermissionDenied:    "the execution host denied access to the path",
	CodeIOError:             "inspect the path on the execution host and retry",
	CodeUnsupported:         "the backend cannot uphold the file-tool safety contract",
	CodeOutcomeUnknown:      "the change may or may not have been published; read the file to check its current state before retrying",
}

// hintFor returns the recovery hint for code, or a generic one.
func hintFor(code string) string {
	if h, ok := hints[code]; ok {
		return h
	}
	return "inspect the path on the execution host and retry"
}

// wrapBackendError converts a backend failure into a *Error carrying a stable
// code, the backend's message, and a recovery hint. Content never appears in
// diagnostics.
func WrapBackendError(err error) error {
	if err == nil {
		return nil
	}
	var fe *execution.FSError
	if errors.As(err, &fe) {
		return &Error{Code: fe.Code, Message: fe.Message, Hint: hintFor(fe.Code)}
	}
	code := Code(err)
	return &Error{Code: code, Message: err.Error(), Hint: hintFor(code)}
}

// checkTarget validates the resolved target before any filesystem access.
func checkTarget(ctx context.Context, t Target) error {
	if err := ctx.Err(); err != nil {
		return Errorf(CodeCancelled, "operation cancelled", hintFor(CodeCancelled))
	}
	if t.Root == "" {
		return Errorf(CodeRootNotConfigured, "the selected execution channel has no root directory", hintFor(CodeRootNotConfigured))
	}
	if t.FS == nil {
		return Errorf(CodeUnsupported, "no confined filesystem backend is available for this channel", hintFor(CodeUnsupported))
	}
	return nil
}
