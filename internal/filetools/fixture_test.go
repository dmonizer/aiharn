package filetools

import (
	"context"
	"strings"
	"testing"

	"aiharn/internal/testutil/memfs"
)

// testLimits keeps every bound small enough to hit limits in unit tests.
func testLimits() Limits {
	return Limits{
		MaxReadLines:       100,
		MaxReadBytes:       1024,
		MaxWriteBytes:      512,
		MaxFileBytes:       2048,
		MaxEditResultBytes: 4096,
	}
}

// fixture wires a Session to an in-memory backend with a mutable channel
// selection, so tests can simulate switches and generations.
type fixture struct {
	fs       *memfs.FS
	registry *Registry
	channel  string
	root     string
	gen      uint64
	limits   Limits
}

func newFixture(t *testing.T, limits Limits) *fixture {
	t.Helper()
	return &fixture{
		fs:       memfs.New(),
		registry: NewRegistry(),
		channel:  "local",
		root:     "/root",
		limits:   limits,
	}
}

func (f *fixture) resolve(context.Context) (Target, error) {
	return Target{Channel: f.channel, Root: f.root, Generation: f.gen, FS: f.fs}, nil
}

func (f *fixture) session(runtimeID string) *Session {
	return f.registry.NewSession(runtimeID, f.limits, f.resolve)
}

// switchChannel simulates a successful channel switch.
func (f *fixture) switchChannel(name string) {
	f.gen++
	f.channel = name
}

// readRange issues a read with an explicit end line.
func readRange(t *testing.T, s *Session, agent, name string, start, end int) (ReadResult, error) {
	t.Helper()
	return s.Read(context.Background(), ReadRequest{AgentID: agent, Filename: name, StartLine: start, EndLine: &end})
}

// mustReadRange reads an explicit range and fails the test on error.
func mustReadRange(t *testing.T, s *Session, agent, name string, start, end int) ReadResult {
	t.Helper()
	res, err := readRange(t, s, agent, name, start, end)
	if err != nil {
		t.Fatalf("read %s [%d,%d): %v", name, start, end, err)
	}
	return res
}

// openEndedRead issues a read without an end line.
func openEndedRead(t *testing.T, s *Session, agent, name string, start int) (ReadResult, error) {
	t.Helper()
	return s.Read(context.Background(), ReadRequest{AgentID: agent, Filename: name, StartLine: start})
}

// editOnce proposes and commits an edit, simulating an approved prompt.
func editOnce(t *testing.T, s *Session, req EditRequest) (EditResult, error) {
	t.Helper()
	proposal, err := s.ProposeEdit(context.Background(), req)
	if err != nil {
		return EditResult{}, err
	}
	return proposal.Commit(context.Background())
}

// mustEdit proposes and commits an edit and fails the test on error.
func mustEdit(t *testing.T, s *Session, req EditRequest) EditResult {
	t.Helper()
	res, err := editOnce(t, s, req)
	if err != nil {
		t.Fatalf("edit %s [%d,%d): %v", req.Filename, req.StartLine, req.EndLine, err)
	}
	return res
}

// wantCode fails unless err carries code.
func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil", code)
	}
	if got := Code(err); got != code {
		t.Fatalf("code = %q, want %q (err: %v)", got, code, err)
	}
}

// wantNoCode fails when err is nil.
func wantErr(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
}

// lines builds test content with n numbered lines.
func lines(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("line")
		b.WriteString(itoa(i))
		b.WriteByte('\n')
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
