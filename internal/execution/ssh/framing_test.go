package ssh

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSanitizeSSHError(t *testing.T) {
	opts := Options{Password: "hunter2secret"}
	got := sanitizeSSHError(errors.New("dial: password hunter2secret rejected"), opts)
	if strings.Contains(got.Error(), "hunter2secret") {
		t.Fatalf("password leaked: %q", got)
	}
	if !strings.Contains(got.Error(), "[REDACTED]") {
		t.Fatalf("expected redaction marker: %q", got)
	}
}

func TestReadToMarker(t *testing.T) {
	marker := "AIHARN-END-abc123"
	r := bufio.NewReader(strings.NewReader("hello " + marker + " world"))
	var out bytes.Buffer
	trunc, err := readToMarker(r, marker, &out, nil, 0)
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
	_, err := readToMarker(r, marker, &out, nil, 0)
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
