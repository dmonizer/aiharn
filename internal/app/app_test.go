package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/config"
	"aiharn/internal/execution"
	"aiharn/internal/tools"
	testexec "aiharn/internal/testutil/execution"
	testllm "aiharn/internal/testutil/llm"
)

func TestSelectChannel(t *testing.T) {
	cfg := &config.Config{
		Channels: []config.ChannelConfig{
			{Name: "first"},
			{Name: "second"},
		},
	}

	t.Run("override wins", func(t *testing.T) {
		c, err := selectChannel(cfg, "first", "second")
		if err != nil || c.Name != "second" {
			t.Fatalf("got %q, %v", c.Name, err)
		}
	})
	t.Run("agent channel", func(t *testing.T) {
		c, err := selectChannel(cfg, "second", "")
		if err != nil || c.Name != "second" {
			t.Fatalf("got %q, %v", c.Name, err)
		}
	})
	t.Run("first by default", func(t *testing.T) {
		c, err := selectChannel(cfg, "", "")
		if err != nil || c.Name != "first" {
			t.Fatalf("got %q, %v", c.Name, err)
		}
	})
	t.Run("missing override", func(t *testing.T) {
		if _, err := selectChannel(cfg, "", "nope"); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("no channels", func(t *testing.T) {
		if _, err := selectChannel(&config.Config{}, "", ""); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestBuildGate(t *testing.T) {
	g, err := buildGate(config.ApprovalModeAsk)
	if err != nil || g.Mode() != approval.ModeAsk {
		t.Fatalf("ask: %v %v", g, err)
	}
	g, err = buildGate(config.ApprovalModeAllowAll)
	if err != nil || g.Mode() != approval.ModeAllowAll {
		t.Fatalf("allow-all: %v %v", g, err)
	}
	if _, err := buildGate("bogus"); err == nil {
		t.Fatal("expected error for bogus mode")
	}
}

func TestBuildRegistrySelection(t *testing.T) {
	sess := testexec.NewSession(nil)
	gate := approval.NewGate(approval.ModeAllowAll)

	none := buildRegistry(sess, gate, 0, config.ToolSelection{Mode: config.ToolModeNone}, nil, "a1")
	if len(none.Names()) != 0 {
		t.Fatalf("none: got %v", none.Names())
	}

	all := buildRegistry(sess, gate, 0, config.ToolSelection{Mode: config.ToolModeAll}, nil, "a1")
	if len(all.Names()) != len(tools.Names()) {
		t.Fatalf("all: got %v, want %v", all.Names(), tools.Names())
	}

	list := buildRegistry(sess, gate, 0, config.ToolSelection{
		Mode:  config.ToolModeList,
		Names: []string{"execute_command", "not_a_tool"},
	}, nil, "a1")
	if len(list.Names()) != 1 || list.Names()[0] != "execute_command" {
		t.Fatalf("list: got %v", list.Names())
	}
}

func TestReadSystemPrompt(t *testing.T) {
	dir := t.TempDir()
	cfgPrompt := filepath.Join(dir, "cfg.md")
	overridePrompt := filepath.Join(dir, "override.md")
	if err := os.WriteFile(cfgPrompt, []byte("from config"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overridePrompt, []byte("from override"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readSystemPrompt(cfgPrompt, "")
	if err != nil || got != "from config" {
		t.Fatalf("config path: %q, %v", got, err)
	}
	got, err = readSystemPrompt(cfgPrompt, overridePrompt)
	if err != nil || got != "from override" {
		t.Fatalf("override path: %q, %v", got, err)
	}
	if _, err := readSystemPrompt(filepath.Join(dir, "missing.md"), ""); err == nil {
		t.Fatal("expected error for missing prompt")
	}
}

func TestRuntimeCloseIdempotent(t *testing.T) {
	tr := testexec.NewTransport()
	tc := &transportCache{transports: map[string]execution.Transport{"devbox": tr}}
	gate := approval.NewGate(approval.ModeAsk)

	mgr := agent.NewManager(agent.ManagerOptions{
		MaxAgents: 8,
		Builder: func(ctx context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
			return agent.New(agent.Spec{ID: spec.ID, Type: spec.Type, Client: &testllm.FakeClient{}}), nil
		},
	})
	if err := mgr.RegisterTop(agent.New(agent.Spec{ID: "main", Type: "main", Client: &testllm.FakeClient{}})); err != nil {
		t.Fatal(err)
	}

	rt := &Runtime{Manager: mgr, Gate: gate, transports: tc}
	if err := rt.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if !tr.Closed() {
		t.Fatal("transport not closed")
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestRuntimeCloseNilSafe(t *testing.T) {
	var rt Runtime
	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
