package app

import "testing"

func TestSessionTitleBinding(t *testing.T) {
	rt := &Runtime{title: &titleBackend{}}
	s := newSession("s1", "S1", rt, nil)
	defer s.Close()

	if err := rt.title.SetSessionTitle("Hello"); err != nil {
		t.Fatal(err)
	}
	if got := s.Title(); got != "Hello" {
		t.Fatalf("Title() = %q, want Hello", got)
	}
}

func TestSessionTitleUnavailable(t *testing.T) {
	b := &titleBackend{}
	if err := b.SetSessionTitle("x"); err == nil {
		t.Fatal("expected error before a session binds the setter")
	}
}
