package filetools

import (
	"context"
	"fmt"

	"aiharn/internal/execution"
)

// EditRequest is one edit invocation: replace the complete lines in
// [StartLine, EndLine) with Content. Equal endpoints insert.
type EditRequest struct {
	AgentID   string
	Filename  string
	ReadID    string
	StartLine int
	EndLine   int
	Content   string
}

// EditResult is the structured result of a successful edit.
type EditResult struct {
	Filename  string `json:"filename"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Bytes     int    `json:"bytes"`
	LineCount int    `json:"line_count"`
}

// EditProposal is a fully validated edit that has not been published yet. It
// carries the exact result the user approves, so publication can never apply a
// different change than the previewed one.
type EditProposal struct {
	session     *Session
	target      Target
	req         EditRequest
	path        string
	baseVersion execution.FileVersion
	newContent  string
	preview     string
}

// Preview returns the human-readable diff shown in the approval prompt.
func (p *EditProposal) Preview() string { return p.preview }

// ProposeEdit validates coordinates, the read receipt, and the resulting text
// and size, and returns the exact proposed result.
func (s *Session) ProposeEdit(ctx context.Context, req EditRequest) (*EditProposal, error) {
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
	if req.StartLine < 0 || req.EndLine < req.StartLine {
		return nil, invalidRange(req.Filename, req.StartLine, req.EndLine, 0)
	}
	proposal := &EditProposal{session: s, target: t, req: req}
	err = s.locked(t, func(st *targetState) error {
		snapshot, err := t.FS.ReadFile(ctx, req.Filename, s.limits.MaxFileBytes)
		if err != nil {
			if execution.FSECode(err) == execution.FSNotFound {
				st.forgetPath(req.Filename)
			}
			return WrapBackendError(err)
		}
		return proposal.prepare(st, t, snapshot)
	})
	if err != nil {
		return nil, err
	}
	return proposal, nil
}

// prepare validates one snapshot and stores the exact proposed result. The
// caller holds the target lock.
func (p *EditProposal) prepare(st *targetState, t Target, snapshot execution.FileSnapshot) error {
	if err := validateText(string(snapshot.Content)); err != nil {
		return err
	}
	doc := parseDocument(string(snapshot.Content))
	if doc.style == styleMixed {
		return Errorf(CodeMixedNewlines, fmt.Sprintf("%q mixes CRLF and LF line endings", snapshot.Stat.Name), hintFor(CodeMixedNewlines))
	}
	fileLines := doc.lineCount()
	if p.req.StartLine > fileLines || p.req.EndLine > fileLines {
		return invalidRange(snapshot.Stat.Name, p.req.StartLine, p.req.EndLine, fileLines)
	}
	if !st.recordVersion(snapshot.Stat.Name, snapshot.Stat.Version) {
		st.forgetPath(snapshot.Stat.Name)
		st.recordVersion(snapshot.Stat.Name, snapshot.Stat.Version)
		return staleRead(snapshot.Stat.Name)
	}
	if err := authorizeEdit(st, p.session, t, p.req, snapshot.Stat.Name, fileLines); err != nil {
		return err
	}
	result, err := applyEdit(doc, p.req.StartLine, p.req.EndLine, p.req.Content)
	if err != nil {
		return err
	}
	if int64(len(result)) > p.session.limits.MaxEditResultBytes {
		return Errorf(CodeFileLimitExceeded,
			fmt.Sprintf("the edit would produce %d bytes, above the %d-byte result limit", len(result), p.session.limits.MaxEditResultBytes),
			fmt.Sprintf("keep the resulting file at or below %d bytes", p.session.limits.MaxEditResultBytes))
	}
	p.path = snapshot.Stat.Name
	p.baseVersion = snapshot.Stat.Version
	p.newContent = result
	p.preview = editPreview(snapshot.Stat.Name, p.req, doc, result)
	return nil
}

// authorizeEdit checks that the supplied receipt is a valid read receipt for
// this agent, channel, root, and file, and that it covers the read context the
// edit requires.
func authorizeEdit(st *targetState, s *Session, t Target, req EditRequest, path string, fileLines int) error {
	rec, ok := st.receipts[req.ReadID]
	if !ok {
		return Errorf(CodeReadRequired, fmt.Sprintf("no read receipt %q for %q", req.ReadID, path), hintFor(CodeReadRequired))
	}
	if rec.runtime != s.runtimeID || rec.agent != req.AgentID || rec.channel != t.Channel || rec.root != t.Root || rec.path != path {
		return Errorf(CodeReadScopeMismatch,
			fmt.Sprintf("read receipt %q was not issued to this agent for %q on this channel and root", req.ReadID, path),
			hintFor(CodeReadScopeMismatch))
	}
	need := requiredReadContext(req.StartLine, req.EndLine, fileLines)
	if !rec.satisfies(need) {
		return Errorf(CodeReadScopeMismatch,
			fmt.Sprintf("read receipt %q covers lines [%d, %d); this edit needs %s", req.ReadID, rec.start, rec.end, describeRequirement(need)),
			hintFor(CodeReadScopeMismatch))
	}
	return nil
}

// requiredReadContext returns the read context an edit of [start, end) needs.
// One receipt must cover that context; receipts are never combined.
func requiredReadContext(start, end, fileLines int) receiptRequirement {
	if start < end {
		return receiptRequirement{kind: requireCoverage, start: start, end: end}
	}
	switch {
	case fileLines == 0:
		return receiptRequirement{kind: requireAny}
	case start == 0:
		return receiptRequirement{kind: requireCoverage, start: 0, end: 1}
	case start >= fileLines:
		return receiptRequirement{kind: requireCoverage, start: fileLines - 1, end: fileLines}
	default:
		return receiptRequirement{kind: requireCoverage, start: start - 1, end: start + 1}
	}
}

func describeRequirement(r receiptRequirement) string {
	switch {
	case r.kind == requireAny:
		return "a read of this (empty) file"
	case r.end-r.start == 1:
		return fmt.Sprintf("a receipt covering line %d", r.start)
	default:
		return fmt.Sprintf("a receipt covering both adjacent lines [%d, %d)", r.start, r.end)
	}
}

// Commit revalidates the file and receipt and atomically publishes the approved
// result. A change made while approval was pending is rejected, never merged.
func (p *EditProposal) Commit(ctx context.Context) (EditResult, error) {
	t, err := p.session.resolve(ctx)
	if err != nil {
		return EditResult{}, err
	}
	if err := checkTarget(ctx, t); err != nil {
		return EditResult{}, err
	}
	if !p.target.sameSelection(t) {
		return EditResult{}, channelChanged(p.req.Filename)
	}
	var result EditResult
	err = p.session.locked(t, func(st *targetState) error {
		return p.publish(ctx, st, t, &result)
	})
	if err != nil {
		return EditResult{}, err
	}
	return result, nil
}

// publish re-reads the file, revalidates the receipt and version, replaces the
// file atomically, and updates the mutation ledger. The caller holds the target
// lock.
func (p *EditProposal) publish(ctx context.Context, st *targetState, t Target, result *EditResult) error {
	snapshot, err := t.FS.ReadFile(ctx, p.path, p.session.limits.MaxFileBytes)
	if err != nil {
		return WrapBackendError(err)
	}
	if !snapshot.Stat.Version.Equal(p.baseVersion) {
		st.forgetPath(p.path)
		st.recordVersion(p.path, snapshot.Stat.Version)
		return staleRead(p.path)
	}
	doc := parseDocument(string(snapshot.Content))
	if err := authorizeEdit(st, p.session, t, p.req, p.path, doc.lineCount()); err != nil {
		return err
	}
	rebuilt, err := applyEdit(doc, p.req.StartLine, p.req.EndLine, p.req.Content)
	if err != nil || rebuilt != p.newContent {
		return staleRead(p.path)
	}
	stat, err := t.FS.ReplaceFile(ctx, p.path, []byte(p.newContent), p.baseVersion)
	if err != nil {
		return p.publishFailure(st, err)
	}
	mutations := append(st.mutations(stat.Name), mutation{start: p.req.StartLine, end: p.req.EndLine})
	st.files[stat.Name] = &fileRecord{version: stat.Version, mutations: mutations}
	st.invalidateFrom(stat.Name, p.req.StartLine)
	*result = EditResult{
		Filename:  stat.Name,
		StartLine: p.req.StartLine,
		EndLine:   p.req.EndLine,
		Bytes:     len(p.newContent),
		LineCount: parseDocument(p.newContent).lineCount(),
	}
	return nil
}

// publishFailure classifies a failed publication. A stale version or an unknown
// outcome invalidates every receipt for the file: the caller must re-read.
func (p *EditProposal) publishFailure(st *targetState, err error) error {
	switch execution.FSECode(err) {
	case execution.FSStaleRead, execution.FSOutcomeUnknown:
		st.forgetPath(p.path)
	}
	return WrapBackendError(err)
}

// staleRead reports that the file changed since it was read.
func staleRead(path string) error {
	return Errorf(CodeStaleRead, fmt.Sprintf("%q changed since it was read", path), hintFor(CodeStaleRead))
}

// invalidateFrom drops every receipt whose covered section ends at or after the
// mutation's start line. A receipt is kept only when the mutation starts
// strictly after its end. Coordinates of surviving receipts are not shifted.
func (st *targetState) invalidateFrom(path string, start int) {
	for id, rec := range st.receipts {
		if rec.path == path && rec.end >= start {
			delete(st.receipts, id)
		}
	}
}

// mutations returns the tracked mutation history recorded for path.
func (st *targetState) mutations(path string) []mutation {
	rec, ok := st.files[path]
	if !ok {
		return nil
	}
	return append([]mutation(nil), rec.mutations...)
}
