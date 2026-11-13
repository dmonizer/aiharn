package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"aiharn/internal/approval"
	"aiharn/internal/filetools"
	"aiharn/internal/llm"
)

// Text file tool names.
const (
	NameRead  = "read"
	NameWrite = "write"
	NameEdit  = "edit"
)

// FileBackend is the app-owned seam between the file tools and the
// conversation/channel runtime. The app implements it over one conversation's
// filetools session and its live channel selection, so the tools never see
// channel identities, roots, or receipts beyond opaque ids.
type FileBackend interface {
	// ReadFile returns a bounded line range and mints a read receipt.
	ReadFile(ctx context.Context, req filetools.ReadRequest) (filetools.ReadResult, error)
	// ProposeFileWrite validates a creation and returns it unpublished.
	ProposeFileWrite(ctx context.Context, req filetools.WriteRequest) (*filetools.WriteProposal, error)
	// ProposeFileEdit validates an edit and returns it unpublished.
	ProposeFileEdit(ctx context.Context, req filetools.EditRequest) (*filetools.EditProposal, error)
}

// ReadFile returns the read tool. Reads never require approval.
func ReadFile(backend FileBackend, agentID string) Tool {
	return &readFile{backend: backend, agentID: agentID}
}

type readFile struct {
	backend FileBackend
	agentID string
}

func (t *readFile) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name: NameRead,
		Description: "Read lines from a UTF-8 text file in the execution channel's root directory. " +
			"Line numbers are zero-based and end_line is exclusive; omit end_line to read the next chunk (bounded by the configured line limit). " +
			"Returns JSON with the exact content, file_line_count, and a read_id. Do not prefix line numbers yourself: metadata carries the coordinates. " +
			"An edit of an existing file requires a read_id from read for the affected section.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"filename": {"type": "string", "description": "Literal path relative to the channel root (or absolute inside it). No shell expansion, globbing, or URL decoding."},
				"start_line": {"type": "integer", "description": "Zero-based first line. Defaults to 0."},
				"end_line": {"type": "integer", "description": "Exclusive end line. Defaults to start_line plus the configured line limit."}
			},
			"required": ["filename"],
			"additionalProperties": false
		}`),
	}
}

func (t *readFile) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Filename  string `json:"filename"`
		StartLine *int   `json:"start_line"`
		EndLine   *int   `json:"end_line"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	req := filetools.ReadRequest{AgentID: t.agentID, Filename: p.Filename, EndLine: p.EndLine}
	if p.StartLine != nil {
		req.StartLine = *p.StartLine
	}
	res, err := t.backend.ReadFile(ctx, req)
	if err != nil {
		return "", err
	}
	return encodeResult(res)
}

// WriteFile returns the write tool. Creating a file is approval-gated.
func WriteFile(backend FileBackend, gate *approval.Gate, agentID, agentType string) Tool {
	return &writeFile{backend: backend, gate: gate, agentID: agentID, agentType: agentType}
}

type writeFile struct {
	backend   FileBackend
	gate      *approval.Gate
	agentID   string
	agentType string
}

func (t *writeFile) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name: NameWrite,
		Description: "Create a new UTF-8 text file in the execution channel's root directory. " +
			"Fails if any entry already occupies the destination (never overwrites) and never creates parent directories; use edit to change an existing file. " +
			"Requires user approval unless approval is allow-all. A successful write does not permit a later edit, which needs a read.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"filename": {"type": "string", "description": "Literal path relative to the channel root (or absolute inside it)."},
				"content": {"type": "string", "description": "Exact UTF-8 text to write, including line endings."}
			},
			"required": ["filename", "content"],
			"additionalProperties": false
		}`),
	}
}

func (t *writeFile) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Filename string `json:"filename"`
		Content  string `json:"content"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	proposal, err := t.backend.ProposeFileWrite(ctx, filetools.WriteRequest{
		AgentID: t.agentID, Filename: p.Filename, Content: p.Content,
	})
	if err != nil {
		return "", err
	}
	if err := t.approve(ctx, p.Filename, proposal.Preview(), map[string]any{
		"filename": p.Filename, "bytes": len(p.Content), "line_count": filetools.CountLines(p.Content),
	}); err != nil {
		return "", err
	}
	res, err := proposal.Commit(ctx)
	if err != nil {
		return "", err
	}
	return encodeResult(res)
}

// approve runs the approval gate for a mutation and maps denial and
// cancellation to model-facing failures.
func (t *writeFile) approve(ctx context.Context, filename, preview string, args any) error {
	return requestApproval(ctx, t.gate, approval.Request{
		AgentID:   t.agentID,
		AgentType: t.agentType,
		ToolName:  NameWrite,
		Command:   preview,
		Args:      encodeArgs(args),
	}, filename)
}

// EditFile returns the edit tool. Editing is approval-gated and requires a prior
// read of the affected section by the same agent.
func EditFile(backend FileBackend, gate *approval.Gate, agentID, agentType string) Tool {
	return &editFile{backend: backend, gate: gate, agentID: agentID, agentType: agentType}
}

type editFile struct {
	backend   FileBackend
	gate      *approval.Gate
	agentID   string
	agentType string
}

func (t *editFile) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Name: NameEdit,
		Description: "Replace the complete lines [start_line, end_line) of an existing UTF-8 text file in the execution channel's root directory with content. " +
			"Equal endpoints insert lines; empty content deletes the selection. Requires a read_id from read covering the edited section and, for insertions, the adjacent line(s). " +
			"Line endings are normalized to the file's existing style. Requires user approval unless approval is allow-all.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"filename": {"type": "string", "description": "Literal path relative to the channel root (or absolute inside it)."},
				"read_id": {"type": "string", "description": "Receipt id returned by read for the section this edit touches."},
				"start_line": {"type": "integer", "description": "Zero-based first line of the replaced range."},
				"end_line": {"type": "integer", "description": "Exclusive end line of the replaced range; equal to start_line to insert."},
				"content": {"type": "string", "description": "Replacement text, including the line endings you want between inserted lines."}
			},
			"required": ["filename", "read_id", "start_line", "end_line", "content"],
			"additionalProperties": false
		}`),
	}
}

func (t *editFile) Run(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Filename  string `json:"filename"`
		ReadID    string `json:"read_id"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
		Content   string `json:"content"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	proposal, err := t.backend.ProposeFileEdit(ctx, filetools.EditRequest{
		AgentID: t.agentID, Filename: p.Filename, ReadID: p.ReadID,
		StartLine: p.StartLine, EndLine: p.EndLine, Content: p.Content,
	})
	if err != nil {
		return "", err
	}
	err = requestApproval(ctx, t.gate, approval.Request{
		AgentID:   t.agentID,
		AgentType: t.agentType,
		ToolName:  NameEdit,
		Command:   proposal.Preview(),
		Args: encodeArgs(map[string]any{
			"filename": p.Filename, "start_line": p.StartLine, "end_line": p.EndLine,
			"bytes": len(p.Content), "line_count": filetools.CountLines(p.Content),
		}),
	}, p.Filename)
	if err != nil {
		return "", err
	}
	res, err := proposal.Commit(ctx)
	if err != nil {
		return "", err
	}
	return encodeResult(res)
}

// requestApproval asks the gate and converts a denial or cancellation into a
// model-facing failure. Nothing is published on either path.
func requestApproval(ctx context.Context, gate *approval.Gate, req approval.Request, filename string) error {
	decision, err := gate.Check(ctx, req)
	if err != nil {
		if filetools.Code(err) == filetools.CodeCancelled {
			return filetools.Cancelled(filename)
		}
		return err
	}
	if decision == approval.DecisionDenied {
		return filetools.ApprovalDenied(filename)
	}
	return nil
}

// encodeResult renders a structured tool result as compact JSON.
func encodeResult(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode result: %w", err)
	}
	return string(data), nil
}

// encodeArgs renders compact approval-request arguments without the file
// content.
func encodeArgs(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(data)
}
