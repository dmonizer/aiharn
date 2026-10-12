package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"aiharn/internal/tools"
)

type fakeTitleBackend struct {
	title string
	err   error
}

func (f *fakeTitleBackend) SetSessionTitle(title string) error {
	f.title = title
	return f.err
}

func TestSetSessionTitleTool(t *testing.T) {
	backend := &fakeTitleBackend{}
	r := newRegistry(t, tools.SetSessionTitle(backend))
	out, err := r.Run(context.Background(), tools.NameSetSessionTitle, json.RawMessage(`{"title":"  Fix the widget  "}`))
	if err != nil {
		t.Fatal(err)
	}
	if backend.title != "Fix the widget" {
		t.Fatalf("backend title = %q, want trimmed", backend.title)
	}
	if !strings.Contains(out, "Fix the widget") {
		t.Fatalf("output = %q", out)
	}
}

func TestSetSessionTitleRejectsEmpty(t *testing.T) {
	backend := &fakeTitleBackend{}
	r := newRegistry(t, tools.SetSessionTitle(backend))
	if _, err := r.Run(context.Background(), tools.NameSetSessionTitle, json.RawMessage(`{"title":"   "}`)); err == nil {
		t.Fatal("expected error for whitespace title")
	}
}

func TestSetSessionTitlePropagatesBackendError(t *testing.T) {
	backend := &fakeTitleBackend{err: errors.New("nope")}
	r := newRegistry(t, tools.SetSessionTitle(backend))
	if _, err := r.Run(context.Background(), tools.NameSetSessionTitle, json.RawMessage(`{"title":"x"}`)); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}
