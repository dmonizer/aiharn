package shell

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadToMarker(t *testing.T) {
	marker := "AIHARN-END-abc123"
	r := bufio.NewReader(strings.NewReader("hello " + marker + " world"))
	var out bytes.Buffer
	trunc, err := readToMarker(r, marker, &out, nil, -1)
	if err != nil {
		t.Fatal(err)
	}
	if trunc {
		t.Fatal("unexpected truncation")
	}
	if out.String() != "hello " {
		t.Fatalf("got %q", out.String())
	}
	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != " world" {
		t.Fatalf("remaining = %q", rest)
	}
}

func TestReadToMarkerDelimiterLike(t *testing.T) {
	marker := "AIHARN-END-" + strings.Repeat("0", 32)
	payload := "pre AIHARN-END-" + strings.Repeat("1", 32) + " post"
	r := bufio.NewReader(strings.NewReader(payload + "\n" + marker + "\n"))
	var out bytes.Buffer
	_, err := readToMarker(r, marker, &out, nil, -1)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != payload+"\n" {
		t.Fatalf("got %q", out.String())
	}
}

func TestReadToMarkerTruncation(t *testing.T) {
	marker := "END"
	data := strings.Repeat("x", 1000) + marker
	r := bufio.NewReader(strings.NewReader(data))
	var out bytes.Buffer
	trunc, err := readToMarker(r, marker, &out, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !trunc {
		t.Fatal("expected truncation")
	}
	if out.Len() != 10 {
		t.Fatalf("got %d bytes, want 10", out.Len())
	}
}

func TestReadBodyCombinedCapWhenStdoutFillsBudget(t *testing.T) {
	m := Markers{End: "END", Err: "ERR"}
	framed := "12345END rc=0\nERR\nstderr must be droppedERR"
	f, err := ReadBody(bufio.NewReader(strings.NewReader(framed)), m, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(f.Stdout); got != "12345" {
		t.Fatalf("stdout = %q", got)
	}
	if got := string(f.Stderr); got != "" {
		t.Fatalf("stderr exceeded combined cap: %q", got)
	}
	if !f.Truncated {
		t.Fatal("expected truncation")
	}
}

func TestReadBeginRejectsNonPositivePID(t *testing.T) {
	for _, pid := range []string{"0", "-1"} {
		m := Markers{Begin: "BEGIN"}
		if _, err := ReadBegin(bufio.NewReader(strings.NewReader("BEGIN pid="+pid+"\n")), m); err == nil {
			t.Fatalf("pid %s was accepted", pid)
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("sink failed") }

func TestReadToMarkerReportsSinkErrorAfterConsumingFrame(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("payloadENDafter"))
	var out bytes.Buffer
	_, err := readToMarker(r, "END", &out, failingWriter{}, -1)
	if err == nil || !strings.Contains(err.Error(), "sink failed") {
		t.Fatalf("err = %v", err)
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != "after" {
		t.Fatalf("frame was not consumed: %q", rest)
	}
}

func TestShellCommand(t *testing.T) {
	if got := ShellCommand("/bin/bash"); got != "'/bin/bash' --noprofile --norc -s" {
		t.Fatalf("ShellCommand bash = %q", got)
	}
	if got := ShellCommand("/bin/sh"); got != "'/bin/sh' -s" {
		t.Fatalf("ShellCommand sh = %q", got)
	}
}

func TestShellArgs(t *testing.T) {
	if got := ShellArgs("/bin/bash"); len(got) != 3 || got[0] != "--noprofile" || got[1] != "--norc" || got[2] != "-s" {
		t.Fatalf("ShellArgs bash = %v", got)
	}
	if got := ShellArgs("/bin/sh"); len(got) != 1 || got[0] != "-s" {
		t.Fatalf("ShellArgs sh = %v", got)
	}
}
