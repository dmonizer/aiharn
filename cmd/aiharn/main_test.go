package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aiharn/internal/agent"
	"aiharn/internal/app"
	"aiharn/internal/approval"
	"aiharn/internal/config"
	"aiharn/internal/sessions"
	testllm "aiharn/internal/testutil/llm"
)

// transcriptFiles returns the files in <home>/transcripts.
func transcriptFiles(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "transcripts"))
	if os.IsNotExist(err) {
		// No per-session transcripts were written at all.
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestTranscriptFactoryDisabled(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	factory, closeAll, err := transcriptFactory(home, "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()

	// A nil factory leaves app.Options.NewTranscript unset, which the session
	// layer reads as "this session keeps no transcript". Returning a factory
	// that yields nil instead would make NewSessionManager fail at startup.
	if factory != nil {
		t.Fatal("disabled transcripts returned a factory")
	}
	if _, err := os.Stat(filepath.Join(home, "transcripts")); !os.IsNotExist(err) {
		t.Fatalf("disabled transcripts still created a directory: %v", err)
	}
}

func TestTranscriptFactorySharedPath(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	custom := filepath.Join(t.TempDir(), "explicit.jsonl")
	factory, closeAll, err := transcriptFactory(home, custom, true)
	if err != nil {
		t.Fatal(err)
	}

	first, err := factory("default", "Default")
	if err != nil || first == nil {
		t.Fatalf("first shared transcript = %v, err=%v", first, err)
	}
	second, err := factory("session-1", "Session 1")
	if err != nil || second == nil {
		t.Fatalf("second shared transcript = %v, err=%v", second, err)
	}
	// An explicit path pins one file for every session, so the factory must
	// hand out the same recorder rather than opening the file twice.
	if first != second {
		t.Fatal("explicit --log did not share one recorder between sessions")
	}
	// The shared recorder takes the header of whichever session writes first.
	first.SetMeta(sessions.TranscriptMeta{ID: "default", Name: "Default"})
	closeAll()
	if _, err := os.Stat(custom); err != nil {
		t.Fatalf("explicit transcript was not created: %v", err)
	}
	if names := transcriptFiles(t, home); len(names) != 0 {
		t.Fatalf("explicit path also wrote per-session transcripts: %v", names)
	}
}

func TestTranscriptFactoryPerSession(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	factory, closeAll, err := transcriptFactory(home, "", false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()

	first, err := factory("default", "Default")
	if err != nil || first == nil {
		t.Fatalf("default transcript = %v, err=%v", first, err)
	}
	second, err := factory("session-1", "Session 1")
	if err != nil || second == nil {
		t.Fatalf("second transcript = %v, err=%v", second, err)
	}
	if first == second {
		t.Fatal("per-session transcripts share one recorder")
	}
	// Each session owns its file, so the factory's close function must not
	// close them: the sessions do that themselves.
	first.Close()
	second.Close()

	names := transcriptFiles(t, home)
	if len(names) != 2 {
		t.Fatalf("per-session transcripts = %v, want 2", names)
	}
	for _, name := range names {
		info, err := os.Stat(filepath.Join(home, "transcripts", name))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("transcript %s mode = %o, want 600", name, perm)
		}
	}
}

func TestSessionLimit(t *testing.T) {
	for _, tc := range []struct{ configured, want int }{
		{0, 8}, {-1, 8}, {1, 1}, {16, 16},
	} {
		if got := sessionLimit(tc.configured); got != tc.want {
			t.Fatalf("sessionLimit(%d) = %d, want %d", tc.configured, got, tc.want)
		}
	}
}

func TestEnsureConfigSpecified(t *testing.T) {
	path := "already-set"
	code, done := ensureConfig(map[string]bool{"config": true}, &path)
	if done || code != 0 {
		t.Fatalf("ensureConfig(specified) = (%d, %v), want (0, false)", code, done)
	}
	if path != "already-set" {
		t.Fatalf("config path changed to %q", path)
	}
}

func TestEnsureConfigCreatesDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var path string
	code, done := ensureConfig(map[string]bool{}, &path)
	if done || code != 0 {
		t.Fatalf("ensureConfig() = (%d, %v), want (0, false)", code, done)
	}
	if path == "" {
		t.Fatal("config path was not resolved")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default config was not created: %v", err)
	}
}

func TestApplyAPIFlags(t *testing.T) {
	t.Setenv("AIHARN_AUTH_FILE", "/tmp/auth-from-env")

	api := config.APIConfig{AuthFile: "original", Listen: "original", AllowOrigins: []string{"a"}}
	applyAPIFlags(&api, map[string]bool{
		"api-listen":       true,
		"api-auth-file":    true,
		"api-allow-origin": true,
		"api-only":         true,
	}, "127.0.0.1:7331", "/tmp/auth-from-flag", "https://a.example,https://b.example", true)

	if api.Listen != "127.0.0.1:7331" {
		t.Fatalf("Listen = %q", api.Listen)
	}
	if api.AuthFile != "/tmp/auth-from-flag" {
		t.Fatalf("AuthFile = %q", api.AuthFile)
	}
	if api.Only != true {
		t.Fatalf("Only = %v", api.Only)
	}
	want := []string{"https://a.example", "https://b.example"}
	if len(api.AllowOrigins) != len(want) || api.AllowOrigins[0] != want[0] || api.AllowOrigins[1] != want[1] {
		t.Fatalf("AllowOrigins = %v", api.AllowOrigins)
	}
}

func TestApplyAPIFlagsEnvOnly(t *testing.T) {
	t.Setenv("AIHARN_AUTH_FILE", "/tmp/auth-from-env")

	api := config.APIConfig{AuthFile: "original"}
	applyAPIFlags(&api, map[string]bool{}, "", "", "", false)

	if api.AuthFile != "/tmp/auth-from-env" {
		t.Fatalf("AuthFile = %q, want env override", api.AuthFile)
	}
	if api.Listen != "" || api.Only {
		t.Fatalf("unexpected override: %+v", api)
	}
}

// newTestSessionManager builds a SessionManager backed by a scripted in-memory
// top-level agent, avoiding real transports and model providers.
func newTestSessionManager(t *testing.T) *app.SessionManager {
	t.Helper()
	cfg := &config.Config{
		Agents:   map[string]config.AgentConfig{"main": {Model: "m", Channel: "local"}},
		Models:   map[string]config.ModelConfig{"m": {Provider: "openai", Model: "m"}},
		Channels: []config.ChannelConfig{{Name: "local", Type: "local"}},
	}
	m, err := app.NewSessionManager(context.Background(), app.SessionManagerOptions{
		Config: cfg,
		BuildRuntime: func(_ context.Context, _ *config.Config, _ app.Options) (*app.Runtime, error) {
			mgr := agent.NewManager(agent.ManagerOptions{})
			top := agent.New(agent.Spec{ID: "main", Type: "main", Client: &testllm.FakeClient{}})
			if err := mgr.RegisterTop(top); err != nil {
				return nil, err
			}
			return &app.Runtime{
				Manager: mgr,
				Agent:   top,
				Gate:    approval.NewGate(approval.ModeAsk),
				Summary: app.Summary{AgentType: "main", Model: "m", Channel: "local", Approval: "ask"},
			}, nil
		},
		ID: func() string { return "s1" },
	})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	t.Cleanup(func() { _ = m.Shutdown() })
	return m
}

func TestStartAPIInvalidListen(t *testing.T) {
	mgr := newTestSessionManager(t)
	cfg := &config.Config{API: config.APIConfig{Listen: "invalid-listen"}}
	if _, err := startAPI(cfg, mgr); err == nil {
		t.Fatal("startAPI() error = nil, want invalid listen error")
	}
}

func TestClearedRuntime(t *testing.T) {
	mgr := newTestSessionManager(t)
	res, err := clearedRuntime(context.Background(), mgr)
	if err != nil {
		t.Fatalf("clearedRuntime: %v", err)
	}
	if res.Agent == nil || res.Manager == nil || res.Gate == nil {
		t.Fatalf("clearedRuntime returned incomplete result: %+v", res)
	}
}
