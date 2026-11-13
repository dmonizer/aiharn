package fsproto_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"aiharn/internal/execution"
	"aiharn/internal/execution/fsproto"
)

func TestFrameRoundTrip(t *testing.T) {
	stat := execution.FileStat{
		Name:      "a/b.txt",
		Version:   execution.FileVersion{Device: 1, Inode: 2, Size: 5, ModTimeUnixNano: 3, ChangeTimeUnixNano: 4, Digest: "abcd"},
		Mode:      0o644,
		LinkCount: 1,
		UID:       1000,
		GID:       1000,
	}
	resp := fsproto.Response{OK: true, Stat: &stat, Content: []byte("hello")}
	var buf bytes.Buffer
	if err := fsproto.WriteFrame(&buf, resp); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	var got fsproto.Response
	if err := fsproto.ReadFrame(&buf, &got); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !got.OK || got.Stat == nil || !got.Stat.Version.Equal(stat.Version) {
		t.Fatalf("decoded response = %+v", got)
	}
	if string(got.Content) != "hello" {
		t.Fatalf("content = %q, want hello", got.Content)
	}
}

func TestFrameRoundTripPreservesEmptyContent(t *testing.T) {
	var buf bytes.Buffer
	if err := fsproto.WriteFrame(&buf, fsproto.Request{Op: fsproto.OpCreate, Name: "empty.txt"}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	var got fsproto.Request
	if err := fsproto.ReadFrame(&buf, &got); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if got.Op != fsproto.OpCreate || got.Name != "empty.txt" || len(got.Content) != 0 {
		t.Fatalf("decoded request = %+v", got)
	}
}

func frameHeader(size uint32) []byte {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], size)
	return header[:]
}

func TestReadFrameRejectsOversizedAnnouncement(t *testing.T) {
	// A hostile peer announces a 1 GiB body; ReadFrame must reject it from the
	// header alone instead of allocating the announced size.
	r := bytes.NewReader(frameHeader(1 << 30))
	var req fsproto.Request
	err := fsproto.ReadFrame(r, &req)
	if !errors.Is(err, fsproto.ErrFrameTooLarge) {
		t.Fatalf("error = %v, want ErrFrameTooLarge", err)
	}
}

func TestReadFrameRejectsTruncatedHeader(t *testing.T) {
	r := bytes.NewReader([]byte{0x00, 0x01})
	var req fsproto.Request
	if err := fsproto.ReadFrame(r, &req); !errors.Is(err, fsproto.ErrInvalidFrame) {
		t.Fatalf("error = %v, want ErrInvalidFrame", err)
	}
}

func TestReadFrameRejectsTruncatedBody(t *testing.T) {
	r := bytes.NewReader(append(frameHeader(64), []byte(`{"op":"ping"`)...))
	var req fsproto.Request
	if err := fsproto.ReadFrame(r, &req); !errors.Is(err, fsproto.ErrInvalidFrame) {
		t.Fatalf("error = %v, want ErrInvalidFrame", err)
	}
}

func TestReadFrameRejectsEmptyBody(t *testing.T) {
	r := bytes.NewReader(frameHeader(0))
	var req fsproto.Request
	if err := fsproto.ReadFrame(r, &req); !errors.Is(err, fsproto.ErrInvalidFrame) {
		t.Fatalf("error = %v, want ErrInvalidFrame", err)
	}
}

func TestReadFrameRejectsInvalidJSON(t *testing.T) {
	body := []byte("this is not json")
	r := bytes.NewReader(append(frameHeader(uint32(len(body))), body...))
	var req fsproto.Request
	if err := fsproto.ReadFrame(r, &req); !errors.Is(err, fsproto.ErrInvalidFrame) {
		t.Fatalf("error = %v, want ErrInvalidFrame", err)
	}
}

func TestReadFramePropagatesCleanEOF(t *testing.T) {
	var req fsproto.Request
	if err := fsproto.ReadFrame(bytes.NewReader(nil), &req); !errors.Is(err, io.EOF) {
		t.Fatalf("error = %v, want io.EOF", err)
	}
}

func TestWriteFrameRejectsOversizedBody(t *testing.T) {
	// Content above MaxFrameSize cannot fit in one frame, even before base64
	// and JSON overhead; WriteFrame must refuse rather than emit it.
	var sink bytes.Buffer
	req := fsproto.Request{Op: fsproto.OpCreate, Name: "big.txt", Content: bytes.Repeat([]byte("a"), fsproto.MaxFrameSize)}
	if err := fsproto.WriteFrame(&sink, req); !errors.Is(err, fsproto.ErrFrameTooLarge) {
		t.Fatalf("error = %v, want ErrFrameTooLarge", err)
	}
	if sink.Len() != 0 {
		t.Fatalf("WriteFrame wrote %d bytes before refusing, want none", sink.Len())
	}
}

func TestWriteFrameWritesOneCompleteFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := fsproto.WriteFrame(&buf, fsproto.Request{Op: fsproto.OpPing}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	header := buf.Next(4)
	size := binary.BigEndian.Uint32(header)
	if int(size) != buf.Len() {
		t.Fatalf("announced size %d != body %d", size, buf.Len())
	}
	if !strings.Contains(buf.String(), `"op":"ping"`) {
		t.Fatalf("body = %q", buf.String())
	}
}

func TestParseOp(t *testing.T) {
	for _, op := range []fsproto.Op{fsproto.OpInit, fsproto.OpRead, fsproto.OpCreate, fsproto.OpReplace, fsproto.OpPing, fsproto.OpClose} {
		got, err := fsproto.ParseOp(string(op))
		if err != nil || got != op {
			t.Fatalf("ParseOp(%q) = (%q, %v)", op, got, err)
		}
	}
	if _, err := fsproto.ParseOp("teleport"); !errors.Is(err, fsproto.ErrProtocol) {
		t.Fatalf("ParseOp(teleport) error = %v, want ErrProtocol", err)
	}
}

func TestResponseErr(t *testing.T) {
	if err := (fsproto.Response{OK: true}).Err(); err != nil {
		t.Fatalf("successful response Err() = %v, want nil", err)
	}
	resp := fsproto.Response{Error: &fsproto.ErrorPayload{Code: execution.FSStaleRead, Message: "changed", Path: "a.txt"}}
	err := resp.Err()
	if execution.FSECode(err) != execution.FSStaleRead {
		t.Fatalf("code = %q, want %q", execution.FSECode(err), execution.FSStaleRead)
	}
	var fe *execution.FSError
	if !errors.As(err, &fe) || fe.Path != "a.txt" || fe.Message != "changed" {
		t.Fatalf("error = %#v", err)
	}
	if code := execution.FSECode(fsproto.Response{}.Err()); code != execution.FSIOError {
		t.Fatalf("failure without body code = %q, want %q", code, execution.FSIOError)
	}
	if code := execution.FSECode((fsproto.Response{Error: &fsproto.ErrorPayload{}}).Err()); code != execution.FSIOError {
		t.Fatalf("empty code mapped to %q, want %q", code, execution.FSIOError)
	}
}

func TestNewErrorResponse(t *testing.T) {
	fsErr := fsproto.NewErrorResponse("a.txt", execution.NewFSError(execution.FSNotFound, "", "gone"))
	if fsErr.OK || fsErr.Error == nil || fsErr.Error.Code != execution.FSNotFound {
		t.Fatalf("response = %+v", fsErr)
	}
	if fsErr.Error.Path != "a.txt" {
		t.Fatalf("path = %q, want fallback a.txt", fsErr.Error.Path)
	}
	generic := fsproto.NewErrorResponse("b.txt", errors.New("boom"))
	if generic.Error.Code != execution.FSIOError || generic.Error.Message != "boom" {
		t.Fatalf("generic response = %+v", generic.Error)
	}
}
