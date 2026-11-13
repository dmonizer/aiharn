package filetools

import (
	"context"
	"fmt"
)

// ReadRequest is one read invocation.
type ReadRequest struct {
	// AgentID identifies the calling agent instance; receipts are bound to it.
	AgentID string
	// Filename is a literal path, relative to the channel root (or absolute
	// inside it). It is never shell-expanded or decoded.
	Filename string
	// StartLine is the zero-based first line.
	StartLine int
	// EndLine is the exclusive end line; nil means "start_line + max_read_lines".
	EndLine *int
	// MaxResultBytes, when positive, bounds the encoded size of the whole
	// result. A range whose envelope would exceed it is refused before a receipt
	// is minted, so no receipt ever authorizes content the caller truncated.
	MaxResultBytes int64
}

// ReadResult is the structured, model-facing read result. Content carries no
// line-number prefixes; the metadata supplies the coordinates.
type ReadResult struct {
	Filename      string `json:"filename"`
	StartLine     int    `json:"start_line"`
	EndLine       int    `json:"end_line"`
	FileLineCount int    `json:"file_line_count"`
	Content       string `json:"content"`
	ReadID        string `json:"read_id"`
	HasMore       bool   `json:"has_more"`
	NextStartLine *int   `json:"next_start_line,omitempty"`
}

// Read returns a bounded line range of a text file and mints a read receipt for
// the returned range. Reads never require approval.
func (s *Session) Read(ctx context.Context, req ReadRequest) (ReadResult, error) {
	t, err := s.resolve(ctx)
	if err != nil {
		return ReadResult{}, err
	}
	if err := checkTarget(ctx, t); err != nil {
		return ReadResult{}, err
	}
	snapshot, err := t.FS.ReadFile(ctx, req.Filename, s.limits.MaxFileBytes)
	if err != nil {
		return ReadResult{}, WrapBackendError(err)
	}
	if err := validateText(string(snapshot.Content)); err != nil {
		return ReadResult{}, err
	}
	doc := parseDocument(string(snapshot.Content))
	start, end, err := s.readRange(doc.lineCount(), req, snapshot.Stat.Name)
	if err != nil {
		return ReadResult{}, err
	}
	content := doc.segment(start, end)
	if int64(len(content)) > s.limits.MaxReadBytes {
		return ReadResult{}, Errorf(CodeReadLimitExceeded,
			fmt.Sprintf("the requested range returns %d bytes, above the %d-byte read limit; request a smaller range", len(content), s.limits.MaxReadBytes),
			fmt.Sprintf("effective limits: max_read_lines=%d, max_read_bytes=%d", s.limits.MaxReadLines, s.limits.MaxReadBytes))
	}

	path := snapshot.Stat.Name
	if req.MaxResultBytes > 0 {
		if bound := envelopeBound(path, content); int64(bound) > req.MaxResultBytes {
			return ReadResult{}, Errorf(CodeReadLimitExceeded,
				fmt.Sprintf("the requested range would return about %d encoded bytes, above the %d-byte result budget", bound, req.MaxResultBytes),
				"request a smaller range with start_line/end_line")
		}
	}
	var readID string
	err = s.locked(t, func(st *targetState) error {
		if !st.recordVersion(path, snapshot.Stat.Version) {
			// This read observed a change the tools did not make. Every receipt
			// for the file is untrustworthy, wherever it covered.
			st.forgetPath(path)
			st.recordVersion(path, snapshot.Stat.Version)
		}
		id, err := st.mintReceipt(s.runtimeID, req.AgentID, t, path, start, end)
		if err != nil {
			return err
		}
		readID = id
		return nil
	})
	if err != nil {
		return ReadResult{}, err
	}
	return newReadResult(path, start, end, doc.lineCount(), content, readID), nil
}

// readRange validates the requested coordinates and applies the line-cap
// default. An explicit end is never clipped to the line cap or to EOF.
func (s *Session) readRange(fileLines int, req ReadRequest, filename string) (int, int, error) {
	start := req.StartLine
	end := fileLines
	if req.EndLine != nil {
		end = *req.EndLine
	} else if start >= 0 && start+s.limits.MaxReadLines < fileLines {
		end = start + s.limits.MaxReadLines
	}
	switch {
	case start < 0:
		return 0, 0, invalidRange(filename, start, end, fileLines)
	case start > fileLines:
		return 0, 0, invalidRange(filename, start, end, fileLines)
	case end < start || end > fileLines:
		return 0, 0, invalidRange(filename, start, end, fileLines)
	case end-start > s.limits.MaxReadLines:
		return 0, 0, Errorf(CodeReadLimitExceeded,
			fmt.Sprintf("the requested range covers %d lines, above the %d-line limit", end-start, s.limits.MaxReadLines),
			fmt.Sprintf("effective limits: max_read_lines=%d, max_read_bytes=%d", s.limits.MaxReadLines, s.limits.MaxReadBytes))
	}
	return start, end, nil
}

func invalidRange(filename string, start, end, fileLines int) error {
	return Errorf(CodeInvalidRange,
		fmt.Sprintf("invalid range [%d, %d) for %q with %d lines", start, end, filename, fileLines),
		hintFor(CodeInvalidRange))
}

func newReadResult(filename string, start, end, fileLines int, content, readID string) ReadResult {
	res := ReadResult{
		Filename:      filename,
		StartLine:     start,
		EndLine:       end,
		FileLineCount: fileLines,
		Content:       content,
		ReadID:        readID,
		HasMore:       end < fileLines,
	}
	if res.HasMore {
		next := end
		res.NextStartLine = &next
	}
	return res
}
