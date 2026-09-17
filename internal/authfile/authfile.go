// Package authfile manages the bcrypt password file used by the HTTP API.
package authfile

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const maxFileBytes = 1 << 20

// Load reads a user:hash file. A missing, empty, or malformed file is an error.
func Load(path string) (map[string]string, error) {
	users := make(map[string]string)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open user file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat user file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("user file must be a regular file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("user file must be readable only by its owner (mode 0600)")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read user file: %w", err)
	}
	if len(data) > maxFileBytes {
		return nil, errors.New("user file is too large")
	}
	scan := bufio.NewScanner(bytes.NewReader(data))
	for scan.Scan() {
		line := scan.Text()
		name, hash, ok := strings.Cut(line, ":")
		if !ok || !ValidUsername(name) || !strings.HasPrefix(hash, "$2") || strings.ContainsAny(hash, "\r\n:") {
			return nil, errors.New("user file contains an invalid entry")
		}
		if _, err := bcrypt.Cost([]byte(hash)); err != nil {
			return nil, errors.New("user file contains an invalid bcrypt hash")
		}
		if _, exists := users[name]; exists {
			return nil, errors.New("user file contains a duplicate username")
		}
		users[name] = hash
	}
	if err := scan.Err(); err != nil {
		return nil, fmt.Errorf("scan user file: %w", err)
	}
	if len(users) == 0 {
		return nil, errors.New("user file contains no users")
	}
	return users, nil
}

// ValidUsername accepts a small, predictable set of characters for file entries.
func ValidUsername(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

// Set hashes a password and atomically adds or replaces one user. The file is
// created with mode 0600 and must remain outside any publicly served directory.
func Set(path, name string, password []byte) error {
	if !ValidUsername(name) {
		return errors.New("username must be 1-64 letters, digits, dots, dashes, or underscores")
	}
	if len(password) == 0 {
		return errors.New("password must not be empty")
	}
	if len(password) > 72 {
		return errors.New("password must be at most 72 bytes for bcrypt")
	}
	hash, err := bcrypt.GenerateFromPassword(password, bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	users := map[string]string{}
	if _, err := os.Lstat(path); err == nil {
		users, err = Load(path)
		if err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	users[name] = string(hash)
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".aiharn-users-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	for user, encoded := range users {
		if _, err := fmt.Fprintf(tmp, "%s:%s\n", user, encoded); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
