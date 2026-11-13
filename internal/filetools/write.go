package filetools

import (
	"context"
	"fmt"
	"path"

	"aiharn/internal/execution"
)

// WriteRequest is one write invocation.
type WriteRequest struct {
	AgentID  string
	Filename string
	Content  string
}

// WriteResult is the structured result of a successful write. A write never
// mints a receipt and never satisfies read-before-edit.
type WriteResult struct {
	Filename string `json:"filename"`
	Bytes    int    `json:"bytes_written"`
	Lines    int    `json:"line_count"`
}

// WriteProposal is a validated, not yet published file creation. The approval
// gate runs between ProposeWrite and Commit; Commit never publishes a different
// content than the one previewed.
type WriteProposal struct {
	session  *Session
	target   Target
	filename string
	content  string
	lines    int
	preview  string
}

// Preview returns the human-readable creation preview shown in the approval
// prompt.
func (p *WriteProposal) Preview() string { return p.preview }

// ProposeWrite validates the destination and content and returns a proposal
// that can be shown to the user and committed afterwards.
func (s *Session) ProposeWrite(ctx context.Context, req WriteRequest) (*WriteProposal, error) {
	t, err := s.resolve(ctx)
	if err != nil {
		return nil, err
	}
	if err := checkTarget(ctx, t); err != nil {
		return nil, err
	}
	if err := validateText(req.Content); err != nil {
		return nil, err
	}
	if int64(len(req.Content)) > s.limits.MaxWriteBytes {
		return nil, Errorf(CodeFileLimitExceeded,
			fmt.Sprintf("content is %d bytes, above the %d-byte write limit", len(req.Content), s.limits.MaxWriteBytes),
			fmt.Sprintf("write at most %d bytes per call", s.limits.MaxWriteBytes))
	}
	if err := s.precheckDestination(ctx, t, req.Filename); err != nil {
		return nil, err
	}
	lines := parseDocument(req.Content).lineCount()
	return &WriteProposal{
		session:  s,
		target:   t,
		filename: req.Filename,
		content:  req.Content,
		lines:    lines,
		preview:  creationPreview(req.Filename, req.Content, lines),
	}, nil
}

// precheckDestination fails fast when the destination is already occupied or its
// parent is missing. Publication re-checks atomically, so this is a courtesy
// check that avoids asking for approval for a write that cannot succeed.
func (s *Session) precheckDestination(ctx context.Context, t Target, name string) error {
	_, err := t.FS.ReadFile(ctx, name, s.limits.MaxFileBytes)
	if err == nil {
		return Errorf(CodeAlreadyExists, fmt.Sprintf("%q already exists", name), hintFor(CodeAlreadyExists))
	}
	switch execution.FSECode(err) {
	case execution.FSNotFound:
		return s.checkParentExists(ctx, t, name)
	case execution.FSParentNotFound:
		return WrapBackendError(err)
	default:
		// A symlinked destination or parent is reported by publication, which can
		// tell "occupied" from "not a direct regular-file path".
		return nil
	}
}

// checkParentExists reports parent_not_found when the destination's parent
// directory is missing.
func (s *Session) checkParentExists(ctx context.Context, t Target, name string) error {
	parent := path.Dir(path.Clean(name))
	if parent == "." || parent == "/" {
		return nil
	}
	_, err := t.FS.ReadFile(ctx, parent, 1)
	if err == nil {
		return Errorf(CodeNotRegularFile, fmt.Sprintf("%q is a file, not a directory", parent), hintFor(CodeNotRegularFile))
	}
	switch execution.FSECode(err) {
	case execution.FSNotFound, execution.FSParentNotFound:
		return Errorf(CodeParentNotFound, fmt.Sprintf("parent directory %q does not exist", parent), hintFor(CodeParentNotFound))
	default:
		return nil
	}
}

// Commit publishes the approved creation atomically. It rejects a channel switch
// that happened while approval was pending and revalidates everything it can
// before publication.
func (p *WriteProposal) Commit(ctx context.Context) (WriteResult, error) {
	t, err := p.session.resolve(ctx)
	if err != nil {
		return WriteResult{}, err
	}
	if err := checkTarget(ctx, t); err != nil {
		return WriteResult{}, err
	}
	if !p.target.sameSelection(t) {
		return WriteResult{}, channelChanged(p.filename)
	}
	var stat execution.FileStat
	err = p.session.locked(t, func(st *targetState) error {
		created, err := t.FS.CreateFile(ctx, p.filename, []byte(p.content))
		if err != nil {
			return WrapBackendError(err)
		}
		stat = created
		st.files[stat.Name] = &fileRecord{version: stat.Version}
		st.dropReceipts(stat.Name)
		return nil
	})
	if err != nil {
		return WriteResult{}, err
	}
	return WriteResult{Filename: stat.Name, Bytes: len(p.content), Lines: p.lines}, nil
}

// sameSelection reports whether t is still the selection the proposal was built
// against. Any switch, even one that returns to the same channel, changes the
// generation and therefore rejects the proposal.
func (t Target) sameSelection(o Target) bool {
	return t.Channel == o.Channel && t.Root == o.Root && t.Generation == o.Generation && t.targetKey() == o.targetKey()
}

func channelChanged(filename string) error {
	return Errorf(CodeChannelChanged,
		fmt.Sprintf("the execution channel changed while the change to %q awaited approval", filename),
		hintFor(CodeChannelChanged))
}
