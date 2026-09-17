package authfile

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestSetAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users")
	if err := Set(path, "alice", []byte("first password")); err != nil {
		t.Fatal(err)
	}
	if err := Set(path, "bob", []byte("second password")); err != nil {
		t.Fatal(err)
	}
	if err := Set(path, "alice", []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("file mode = %o", info.Mode().Perm())
	}
	users, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("user count = %d", len(users))
	}
	if bcrypt.CompareHashAndPassword([]byte(users["alice"]), []byte("replacement")) != nil ||
		bcrypt.CompareHashAndPassword([]byte(users["bob"]), []byte("second password")) != nil {
		t.Fatal("stored hashes do not match passwords")
	}
	if bcrypt.CompareHashAndPassword([]byte(users["alice"]), []byte("first password")) == nil {
		t.Fatal("replaced password still works")
	}
}

func TestRejectInsecureFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users")
	if err := os.WriteFile(path, []byte("alice:invalid\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected mode error")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected malformed entry error")
	}
}
