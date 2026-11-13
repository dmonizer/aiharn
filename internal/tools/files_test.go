package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aiharn/internal/approval"
	"aiharn/internal/filetools"
	"aiharn/internal/testutil/memfs"
	"aiharn/internal/tools"
)

// fileBackend adapts one conversation's filetools session to tools.FileBackend.
type fileBackend struct {
	session *filetools.Session
}

func (b *fileBackend) ReadFile(ctx context.Context, req filetools.ReadRequest) (filetools.ReadResult, error) {
	return b.session.Read(ctx, req)
}

func (b *fileBackend) ProposeFileWrite(ctx context.Context, req filetools.WriteRequest) (*filetools.WriteProposal, error) {
	return b.session.ProposeWrite(ctx, req)
}

func (b *fileBackend) ProposeFileEdit(ctx context.Context, req filetools.EditRequest) (*filetools.EditProposal, error) {
	return b.session.ProposeEdit(ctx, req)
}

// fileFixture wires the file tools to an in-memory filesystem.
func fileFixture(t *testing.T) (*fileBackend, *memfs.FS) {
	t.Helper()
	fs := memfs.New()
	limits := filetools.Limits{
		MaxReadLines: 100, MaxReadBytes: 1 << 16, MaxWriteBytes: 1 << 16,
		MaxFileBytes: 1 << 16, MaxEditResultBytes: 1 << 16,
	}
	session := filetools.NewRegistry().NewSession("conv-1", limits, func(context.Context) (filetools.Target, error) {
		return filetools.Target{Channel: "local", Root: "/root", FS: fs}, nil
	})
	return &fileBackend{session: session}, fs
}

// runGated runs a tool in the background and decides its approval request.
func runGated(t *testing.T, gate *approval.Gate, run func() (string, error), decision approval.Decision) (string, error, approval.Request) {
	t.Helper()
	type outcome struct {
		out string
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		out, err := run()
		done <- outcome{out: out, err: err}
	}()
	var req approval.Request
	select {
	case req = <-gate.Pending():
	case <-time.After(2 * time.Second):
		t.Fatal("no approval request surfaced")
	}
	if err := gate.Decide(req.ID, decision); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-done:
		return o.out, o.err, req
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not return after the decision")
		return "", nil, req
	}
}

func TestFileToolNamesRegistered(t *testing.T) {
	names := tools.Names()
	for _, want := range []string{tools.NameRead, tools.NameWrite, tools.NameEdit} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%q missing from tools.Names(): %v", want, names)
		}
	}
}

func TestReadToolReturnsStructuredResult(t *testing.T) {
	backend, fs := fileFixture(t)
	fs.Set("src/a.go", "package main\n\nfunc main() {}\n")
	reg := newRegistry(t, tools.ReadFile(backend, "agent-1"))

	out, err := reg.Run(context.Background(), tools.NameRead, json.RawMessage(`{"filename":"src/a.go","start_line":0,"end_line":2}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var res struct {
		Filename      string `json:"filename"`
		StartLine     int    `json:"start_line"`
		EndLine       int    `json:"end_line"`
		FileLineCount int    `json:"file_line_count"`
		Content       string `json:"content"`
		ReadID        string `json:"read_id"`
		HasMore       bool   `json:"has_more"`
		NextStartLine *int   `json:"next_start_line"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("result is not JSON: %v (%q)", err, out)
	}
	if res.Filename != "src/a.go" || res.Content != "package main\n\n" {
		t.Fatalf("result = %+v", res)
	}
	if res.FileLineCount != 3 || res.ReadID == "" || !res.HasMore || res.NextStartLine == nil || *res.NextStartLine != 2 {
		t.Fatalf("result = %+v", res)
	}
}

func TestReadToolReportsStableCodes(t *testing.T) {
	backend, _ := fileFixture(t)
	reg := newRegistry(t, tools.ReadFile(backend, "agent-1"))
	_, err := reg.Run(context.Background(), tools.NameRead, json.RawMessage(`{"filename":"missing.txt"}`))
	if err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("err = %v, want a not_found failure", err)
	}
}

func TestWriteToolAllowAll(t *testing.T) {
	backend, fs := fileFixture(t)
	gate := approval.NewGate(approval.ModeAllowAll)
	reg := newRegistry(t, tools.WriteFile(backend, gate, "agent-1", "main"))

	out, err := reg.Run(context.Background(), tools.NameWrite, json.RawMessage(`{"filename":"new.txt","content":"a\nb\n"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var res struct {
		Filename string `json:"filename"`
		Bytes    int    `json:"bytes_written"`
		Lines    int    `json:"line_count"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	if res.Filename != "new.txt" || res.Bytes != 4 || res.Lines != 2 {
		t.Fatalf("result = %+v", res)
	}
	if content, _ := fs.Content("new.txt"); content != "a\nb\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestWriteToolApprovalPreviewAndCommit(t *testing.T) {
	backend, fs := fileFixture(t)
	gate := approval.NewGate(approval.ModeAsk)
	reg := newRegistry(t, tools.WriteFile(backend, gate, "agent-1", "main"))

	args := json.RawMessage(`{"filename":"new.txt","content":"secret payload\n"}`)
	out, err, req := runGated(t, gate, func() (string, error) {
		return reg.Run(context.Background(), tools.NameWrite, args)
	}, approval.DecisionApproved)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, `"bytes_written":15`) {
		t.Fatalf("out = %q", out)
	}
	if !strings.Contains(req.Command, "create") || !strings.Contains(req.Command, "+ secret payload") {
		t.Fatalf("preview = %q", req.Command)
	}
	if strings.Contains(req.Args, "secret payload") {
		t.Fatalf("approval args leaked content: %q", req.Args)
	}
	if req.ToolName != tools.NameWrite || req.AgentID != "agent-1" || req.AgentType != "main" {
		t.Fatalf("request = %+v", req)
	}
	if content, _ := fs.Content("new.txt"); content != "secret payload\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestWriteToolDeniedPublishesNothing(t *testing.T) {
	backend, fs := fileFixture(t)
	gate := approval.NewGate(approval.ModeAsk)
	reg := newRegistry(t, tools.WriteFile(backend, gate, "agent-1", "main"))

	_, err, _ := runGated(t, gate, func() (string, error) {
		return reg.Run(context.Background(), tools.NameWrite, json.RawMessage(`{"filename":"new.txt","content":"x"}`))
	}, approval.DecisionDenied)
	if err == nil || !strings.Contains(err.Error(), filetools.CodeApprovalDenied) {
		t.Fatalf("err = %v, want approval_denied", err)
	}
	if _, ok := fs.Content("new.txt"); ok {
		t.Fatal("denied write created a file")
	}
}

func TestEditToolFlow(t *testing.T) {
	backend, fs := fileFixture(t)
	gate := approval.NewGate(approval.ModeAllowAll)
	reg := newRegistry(t, tools.ReadFile(backend, "agent-1"), tools.EditFile(backend, gate, "agent-1", "main"))
	fs.Set("a.txt", "one\ntwo\n")

	readOut, err := reg.Run(context.Background(), tools.NameRead, json.RawMessage(`{"filename":"a.txt"}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var read struct {
		ReadID string `json:"read_id"`
	}
	if err := json.Unmarshal([]byte(readOut), &read); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{
		"filename": "a.txt", "read_id": read.ReadID,
		"start_line": 1, "end_line": 2, "content": "TWO\n",
	})
	if _, err := reg.Run(context.Background(), tools.NameEdit, args); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if content, _ := fs.Content("a.txt"); content != "one\nTWO\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestEditToolDeniedLeavesFileUnchanged(t *testing.T) {
	backend, fs := fileFixture(t)
	readReg := newRegistry(t, tools.ReadFile(backend, "agent-1"))
	fs.Set("a.txt", "one\ntwo\n")
	readOut, err := readReg.Run(context.Background(), tools.NameRead, json.RawMessage(`{"filename":"a.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	var read struct {
		ReadID string `json:"read_id"`
	}
	if err := json.Unmarshal([]byte(readOut), &read); err != nil {
		t.Fatal(err)
	}

	askGate := approval.NewGate(approval.ModeAsk)
	reg := newRegistry(t, tools.EditFile(backend, askGate, "agent-1", "main"))
	args, _ := json.Marshal(map[string]any{
		"filename": "a.txt", "read_id": read.ReadID,
		"start_line": 0, "end_line": 1, "content": "ONE\n",
	})
	_, err, req := runGated(t, askGate, func() (string, error) {
		return reg.Run(context.Background(), tools.NameEdit, args)
	}, approval.DecisionDenied)
	if err == nil || !strings.Contains(err.Error(), filetools.CodeApprovalDenied) {
		t.Fatalf("err = %v, want approval_denied", err)
	}
	if !strings.Contains(req.Command, "- one") || !strings.Contains(req.Command, "+ ONE") {
		t.Fatalf("diff preview = %q", req.Command)
	}
	if content, _ := fs.Content("a.txt"); content != "one\ntwo\n" {
		t.Fatalf("denied edit changed content: %q", content)
	}
}

func TestMutationCancelledWhileApproving(t *testing.T) {
	backend, fs := fileFixture(t)
	gate := approval.NewGate(approval.ModeAsk)
	reg := newRegistry(t, tools.WriteFile(backend, gate, "agent-1", "main"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := reg.Run(ctx, tools.NameWrite, json.RawMessage(`{"filename":"new.txt","content":"x"}`))
		done <- err
	}()
	select {
	case <-gate.Pending():
	case <-time.After(2 * time.Second):
		t.Fatal("no approval request surfaced")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), filetools.CodeCancelled) {
			t.Fatalf("err = %v, want cancelled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not return after cancellation")
	}
	if _, ok := fs.Content("new.txt"); ok {
		t.Fatal("cancelled write created a file")
	}
}

func TestEditToolSchemaRequiresReadID(t *testing.T) {
	backend, _ := fileFixture(t)
	reg := newRegistry(t, tools.EditFile(backend, approval.NewGate(approval.ModeAllowAll), "agent-1", "main"))
	if _, err := reg.Run(context.Background(), tools.NameEdit, json.RawMessage(`{"filename":"a.txt","start_line":0,"end_line":0,"content":"x"}`)); err == nil {
		t.Fatal("schema accepted an edit without read_id")
	}
}
