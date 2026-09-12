package ssh

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"

	"aiharn/internal/execution"
	"aiharn/internal/logging"
)

// Marker prefixes. Each Exec generates a fresh random nonce so a command whose
// output merely resembles a delimiter cannot collide with a real one (a true
// collision requires the command to emit the exact 128-bit nonce).
const (
	beginPrefix = "AIHARN-BEGIN-"
	endPrefix   = "AIHARN-END-"
	errPrefix   = "AIHARN-ERR-"
)

// markers are the unique delimiters for one Exec.
type markers struct {
	begin string
	end   string
	err   string
}

func newMarkers() (markers, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return markers{}, err
	}
	n := hex.EncodeToString(b[:])
	logging.Debug("ssh: newMarkers", slog.String("component", "ssh"), slog.String("nonce_prefix", n[:8]))
	return markers{
		begin: beginPrefix + n,
		end:   endPrefix + n,
		err:   errPrefix + n,
	}, nil
}

// buildWrapperScript returns the shell script that runs cmd (with optional cwd)
// inside the persistent shell, framing stdout, stderr, and the exit code with
// the given markers. The command is base64-encoded on the client and decoded
// remotely, so its contents never pass through shell quoting.
//
// Framing: the command runs in a setsid'd subshell so it owns its process
// group; the client can then kill just the command (kill -TERM -- -pid) without
// harming the persistent shell. Command stderr is captured to a temp file and
// re-emitted between two err markers, keeping stdout and stderr un-interleaved.
func buildWrapperScript(m markers, cwd, cmd string) string {
	cmdB64 := base64.StdEncoding.EncodeToString([]byte(cmd))
	cwdB64 := base64.StdEncoding.EncodeToString([]byte(cwd))

	var b strings.Builder
	b.WriteString(`__a_f="$(mktemp)"` + "\n")
	b.WriteString(`__a_cd="$(printf '%s' '` + cwdB64 + `' | base64 -d 2>/dev/null)"` + "\n")
	b.WriteString(`__a_cmd="$(printf '%s' '` + cmdB64 + `' | base64 -d 2>/dev/null)"` + "\n")
	b.WriteString(`setsid bash -c 'if [ -n "$1" ]; then cd -- "$1" 2>/dev/null || exit 1; fi; eval "$2"' _ "$__a_cd" "$__a_cmd" 0</dev/null 2>"$__a_f" &` + "\n")
	b.WriteString(`__a_pid=$!` + "\n")
	b.WriteString(`printf '%s pid=%d\n' '` + m.begin + `' "$__a_pid"` + "\n")
	b.WriteString(`wait $__a_pid` + "\n")
	b.WriteString(`__a_rc=$?` + "\n")
	b.WriteString(`printf '%s rc=%d\n' '` + m.end + `' "$__a_rc"` + "\n")
	b.WriteString(`printf '%s\n' '` + m.err + `'` + "\n")
	b.WriteString(`cat "$__a_f"` + "\n")
	b.WriteString(`printf '%s\n' '` + m.err + `'` + "\n")
	b.WriteString(`rm -f "$__a_f"` + "\n")
	return b.String()
}

// execFrame is the decoded result of one Exec's framed output.
type execFrame struct {
	stdout    []byte
	stderr    []byte
	exitCode  int
	truncated bool
}

type streamWriteError struct{ err error }

func (e *streamWriteError) Error() string { return "ssh: stream writer: " + e.err.Error() }
func (e *streamWriteError) Unwrap() error { return e.err }

// readBegin consumes the begin marker and returns the remote process-group id of
// the running command. It is called synchronously (before the command can
// block) so the caller can kill the command on cancellation.
func readBegin(r *bufio.Reader, m markers) (int, error) {
	if _, err := readToMarker(r, m.begin, nil, nil, -1); err != nil {
		return 0, wrapFrameErr(err)
	}
	line, err := readLine(r)
	if err != nil {
		return 0, wrapFrameErr(err)
	}
	pid, err := parseField(line, "pid")
	if err != nil {
		return 0, err
	}
	if pid <= 0 {
		return 0, fmt.Errorf("ssh framing: invalid process-group id %d", pid)
	}
	logging.Debug("ssh: readBegin", slog.String("component", "ssh"), slog.Int("pid", pid), slog.Int("pgid", pid))
	return pid, nil
}

// readBody consumes the remainder of one Exec's framed output: stdout until the
// end marker, then stderr until the closing err marker. maxBytes (0 = unlimited)
// caps the retained stdout+stderr combined; bytes beyond the cap are still
// consumed (to keep framing aligned) but dropped. sink, if non-nil, receives
// retained stdout bytes as they are read.
func readBody(r *bufio.Reader, m markers, maxBytes int64, sink io.Writer) (execFrame, error) {
	var f execFrame
	var out bytes.Buffer
	outputCap := maxBytes
	if outputCap == 0 {
		outputCap = -1
	}
	truncOut, err := readToMarker(r, m.end, &out, sink, outputCap)
	if err != nil {
		var sinkErr *streamWriteError
		if !errors.As(err, &sinkErr) {
			return f, wrapFrameErr(err)
		}
	}
	streamErr := err
	f.stdout = out.Bytes()

	line, err := readLine(r)
	if err != nil {
		return f, wrapFrameErr(err)
	}
	rc, err := parseField(line, "rc")
	if err != nil {
		return f, err
	}
	f.exitCode = rc

	if _, err := readToMarker(r, m.err, nil, nil, -1); err != nil {
		return f, wrapFrameErr(err)
	}
	// The first err marker is a complete line ("<marker>\n"); consume its
	// terminator so it is not captured as stderr content.
	if _, err := readLine(r); err != nil {
		return f, wrapFrameErr(err)
	}

	var errb bytes.Buffer
	remaining := int64(-1)
	if maxBytes > 0 {
		remaining = maxBytes - int64(out.Len())
	}
	truncErr, err := readToMarker(r, m.err, &errb, nil, remaining)
	if err != nil {
		return f, wrapFrameErr(err)
	}
	f.stderr = errb.Bytes()
	f.truncated = truncOut || truncErr
	logging.Debug("ssh: readBody",
		slog.String("component", "ssh"),
		slog.Int("exit_code", f.exitCode),
		slog.Int("stdout_bytes", len(f.stdout)),
		slog.Int("stderr_bytes", len(f.stderr)),
		slog.Bool("truncated", f.truncated),
	)
	return f, streamErr
}

func wrapFrameErr(err error) error {
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		logging.Debug("ssh: shell stream ended before marker",
			slog.String("component", "ssh"),
			slog.Any("err", err),
		)
		return fmt.Errorf("%w: shell stream ended before marker", execution.ErrSessionReset)
	}
	return err
}

// readToMarker reads from r until the byte sequence marker is found, consuming
// it. Bytes before the marker are appended to out and (if non-nil) sink, up to
// cap bytes (a negative cap means unlimited). It reports whether the cap was exceeded.
// The bytes are written to out/sink only as they are confirmed not to be part of
// a marker, so no framing bytes leak into the result.
func readToMarker(r *bufio.Reader, marker string, out *bytes.Buffer, sink io.Writer, cap int64) (truncated bool, err error) {
	m := []byte(marker)
	var carry []byte
	var written int64
	var writeErr error

	commit := func(b []byte) {
		if len(b) == 0 {
			return
		}
		if cap < 0 {
			if out != nil {
				_, _ = out.Write(b)
			}
			if sink != nil && writeErr == nil {
				if n, err := sink.Write(b); err != nil {
					writeErr = err
				} else if n != len(b) {
					writeErr = io.ErrShortWrite
				}
			}
			return
		}
		if written >= cap {
			truncated = true
			return
		}
		take := b
		if int64(len(b)) > cap-written {
			truncated = true
			take = b[:cap-written]
		}
		written += int64(len(take))
		if out != nil {
			_, _ = out.Write(take)
		}
		if sink != nil && writeErr == nil {
			if n, err := sink.Write(take); err != nil {
				writeErr = err
			} else if n != len(take) {
				writeErr = io.ErrShortWrite
			}
		}
	}

	for {
		c, err := r.ReadByte()
		if err != nil {
			if len(carry) > 0 {
				commit(carry)
			}
			logging.Debug("ssh: readToMarker error", slog.String("component", "ssh"), slog.Any("err", err))
			return truncated, err
		}
		carry = append(carry, c)
		if bytes.HasSuffix(carry, m) {
			commit(carry[:len(carry)-len(m)])
			if writeErr != nil {
				return truncated, &streamWriteError{err: writeErr}
			}
			return truncated, nil
		}
		if len(carry) >= len(m) {
			n := len(carry) - (len(m) - 1)
			commit(carry[:n])
			carry = append(carry[:0], carry[n:]...)
		}
	}
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	return strings.TrimRight(line, "\r\n"), err
}

func parseField(line, key string) (int, error) {
	line = strings.TrimSpace(line)
	prefix := key + "="
	if !strings.HasPrefix(line, prefix) {
		return 0, fmt.Errorf("ssh framing: unexpected marker payload %q", line)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(line, prefix))
	if err != nil {
		return 0, fmt.Errorf("ssh framing: bad %s in %q: %w", key, line, err)
	}
	return n, nil
}
