//go:build linux

package fscore

import (
	"math"

	"golang.org/x/sys/unix"

	"aiharn/internal/execution"
)

// readChunkSize is the buffer used to stream content out of a file descriptor.
const readChunkSize = 32 << 10

// Read returns the whole content of name plus its metadata. Content larger than
// maxBytes yields file_limit_exceeded and no partial data.
func (s *Service) Read(name string, maxBytes int64) (execution.FileSnapshot, error) {
	release, err := s.begin()
	if err != nil {
		return execution.FileSnapshot{}, err
	}
	defer release()

	target, err := s.resolve(name)
	if err != nil {
		return execution.FileSnapshot{}, err
	}
	defer target.close()

	fd, stat, err := openRegular(target)
	if err != nil {
		return execution.FileSnapshot{}, err
	}
	defer func() { _ = unix.Close(fd) }()

	content, err := readBounded(fd, maxBytes, target.name)
	if err != nil {
		return execution.FileSnapshot{}, err
	}
	return execution.FileSnapshot{
		Stat:    fileStatOf(target.name, stat, digestHex(content)),
		Content: content,
	}, nil
}

// readBounded reads at most maxBytes+1 bytes, so oversized content is detected
// without trusting a pre-read size and without ever returning partial data.
func readBounded(fd int, maxBytes int64, name string) ([]byte, error) {
	if maxBytes < 0 {
		return nil, limitError(name)
	}
	limit := maxBytes
	if limit < math.MaxInt64 {
		limit++
	}
	content := make([]byte, 0, int(min(limit, readChunkSize)))
	buffer := make([]byte, readChunkSize)
	for read := int64(0); read < limit; {
		want := int64(len(buffer))
		if remaining := limit - read; remaining < want {
			want = remaining
		}
		n, err := unix.Read(fd, buffer[:want])
		if n > 0 {
			content = append(content, buffer[:n]...)
			read += int64(n)
		}
		if err != nil {
			if errnoIs(err, unix.EINTR) {
				continue
			}
			return nil, ioError(name, "read file", err)
		}
		if n == 0 {
			break // end of file
		}
	}
	if int64(len(content)) > maxBytes {
		return nil, limitError(name)
	}
	return content, nil
}
