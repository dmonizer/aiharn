package filetools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONSizeOfIsAnUpperBound(t *testing.T) {
	cases := []string{
		"",
		"plain ascii text\n",
		"quotes \" everywhere \" and backslashes \\\\",
		"tabs\tand\x01control\x1f",
		"multibyte é ü 日本",
		"line separators \u2028 and \u2029",
		strings.Repeat("x\"\\\n", 100),
	}
	for _, content := range cases {
		encoded, err := json.Marshal(content)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if got, want := jsonSizeOf(content), len(encoded); got < want {
			t.Fatalf("jsonSizeOf(%q) = %d, encoded length = %d", content, got, want)
		}
	}
}

func TestEnvelopeBoundCoversResult(t *testing.T) {
	res := ReadResult{
		Filename: "src/example.go", StartLine: 0, EndLine: 3, FileLineCount: 740,
		Content: "package main\n\nfunc main() {}\n", ReadID: strings.Repeat("a", 32),
		HasMore: true,
	}
	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if bound := envelopeBound(res.Filename, res.Content); bound < len(encoded)+32 {
		t.Fatalf("envelopeBound = %d, encoded = %d: no room for a receipt id", bound, len(encoded))
	}
}

func TestReadRefusesResultAboveBudget(t *testing.T) {
	f := newFixture(t, testLimits())
	s := f.session("conv-1")
	f.fs.Set("a.txt", strings.Repeat("abcdefgh", 100)+"\n")
	req := ReadRequest{AgentID: "agent-1", Filename: "a.txt", MaxResultBytes: 128}
	if _, err := s.Read(context.Background(), req); !HasCode(err, CodeReadLimitExceeded) {
		t.Fatalf("budget: %v", err)
	}
	// No receipt was minted, so an edit cannot authorize anything.
	if _, err := editOnce(t, s, EditRequest{AgentID: "agent-1", Filename: "a.txt", ReadID: "", StartLine: 0, EndLine: 1, Content: "x\n"}); !HasCode(err, CodeReadRequired) {
		t.Fatalf("receipt minted for a refused read: %v", err)
	}
	roomy := ReadRequest{AgentID: "agent-1", Filename: "a.txt", MaxResultBytes: 1 << 16}
	if _, err := s.Read(context.Background(), roomy); err != nil {
		t.Fatalf("roomy budget refused: %v", err)
	}
}
