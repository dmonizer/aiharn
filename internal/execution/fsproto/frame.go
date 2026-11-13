package fsproto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// frameHeaderSize is the size of the big-endian body length prefix.
const frameHeaderSize = 4

// WriteFrame encodes msg as JSON and writes it as one length-prefixed frame. It
// refuses to emit a body larger than MaxFrameSize, so the helper can never
// produce a frame its peer must reject.
func WriteFrame(w io.Writer, msg any) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("fsproto: encode frame: %w", err)
	}
	if len(body) > MaxFrameSize {
		return fmt.Errorf("%w: body is %d bytes, limit is %d", ErrFrameTooLarge, len(body), MaxFrameSize)
	}
	var header [frameHeaderSize]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, body)
}

// ReadFrame reads one length-prefixed frame and decodes its JSON body into msg.
//
// A clean end of stream before the header is returned as io.EOF so callers can
// distinguish "the peer exited" from corruption. An oversized, empty,
// truncated, or non-JSON frame yields ErrFrameTooLarge or ErrInvalidFrame
// without allocating the announced size, so a hostile peer cannot exhaust
// memory or stall the caller on a huge body.
func ReadFrame(r io.Reader, msg any) error {
	var header [frameHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%w: truncated header", ErrInvalidFrame)
		}
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	switch {
	case size == 0:
		return fmt.Errorf("%w: empty body", ErrInvalidFrame)
	case size > MaxFrameSize:
		return fmt.Errorf("%w: announced body is %d bytes, limit is %d", ErrFrameTooLarge, size, MaxFrameSize)
	}
	body := make([]byte, size)
	if n, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: truncated body (%d of %d bytes)", ErrInvalidFrame, n, size)
		}
		return err
	}
	if err := json.Unmarshal(body, msg); err != nil {
		return fmt.Errorf("%w: invalid JSON: %v", ErrInvalidFrame, err)
	}
	return nil
}

// writeAll writes data completely, matching io.Writer's contract for short
// writes: a writer that reports progress plus an error is still an error.
func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		data = data[n:]
		if err != nil {
			return fmt.Errorf("fsproto: write frame: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("fsproto: write frame: %w", io.ErrShortWrite)
		}
	}
	return nil
}
