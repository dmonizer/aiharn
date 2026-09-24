package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/config"
	"aiharn/internal/execution"
	"aiharn/internal/sessions"
	testexec "aiharn/internal/testutil/execution"
	testllm "aiharn/internal/testutil/llm"
	"aiharn/internal/tools"
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

	none, err := buildRegistry(sess, gate, 0, "", 0, config.ToolSelection{Mode: config.ToolModeNone}, nil, "a1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Names()) != 0 {
		t.Fatalf("none: got %v", none.Names())
	}

	all, err := buildRegistry(sess, gate, 0, "", 0, config.ToolSelection{Mode: config.ToolModeAll}, nil, "a1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Names()) != len(tools.Names()) {
		t.Fatalf("all: got %v, want %v", all.Names(), tools.Names())
	}
	if !all.Has(tools.NameListSubagentTypes) {
		t.Fatal("all must include subagent type discovery")
	}

	list, err := buildRegistry(sess, gate, 0, "", 0, config.ToolSelection{
		Mode:  config.ToolModeList,
		Names: []string{"execute_command"},
	}, nil, "a1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Names()) != 1 || list.Names()[0] != "execute_command" {
		t.Fatalf("list: got %v", list.Names())
	}
	discovery, err := buildRegistry(sess, gate, 0, "", 0, config.ToolSelection{
		Mode: config.ToolModeList, Names: []string{tools.NameListSubagentTypes},
	}, nil, "a1", "main")
	if err != nil || !discovery.Has(tools.NameListSubagentTypes) || discovery.Has(tools.NameSpawnSubagent) {
		t.Fatalf("discovery-only registry: names=%v err=%v", discovery.Names(), err)
	}
	if _, err := buildRegistry(sess, gate, 0, "", 0, config.ToolSelection{Mode: config.ToolModeList, Names: []string{"not_a_tool"}}, nil, "a1", "main"); err == nil {
		t.Fatal("expected unknown tool error")
	}
}

func TestConfiguredSubagentTypes(t *testing.T) {
	cfg := &config.Config{
		Channels: []config.ChannelConfig{{Name: "first"}, {Name: "other"}},
		Agents: map[string]config.AgentConfig{
			"zeta":  {Model: "model-z", Description: "Delegator", AllowSubagents: true},
			"alpha": {Model: "model-a", Channel: "other"},
		},
	}
	// A subagent with no explicit channel inherits the main agent's channel;
	// one with an explicit channel keeps it.
	types := configuredSubagentTypes(cfg, "ssh-main")
	if len(types) != 2 || types[0].Name != "alpha" || types[0].Channel != "other" ||
		types[1].Name != "zeta" || types[1].Channel != "ssh-main" ||
		types[1].Description != "Delegator" || !types[1].AllowSubagents {
		t.Fatalf("configured types = %+v", types)
	}
}

func TestResolveDefaultCwd(t *testing.T) {
	t.Run("no working dir", func(t *testing.T) {
		sess := testexec.NewSession(nil)
		got, err := resolveDefaultCwd(context.Background(), sess, config.AgentConfig{}, config.ChannelConfig{}, "main", "main")
		if err != nil || got != "" {
			t.Fatalf("got %q, %v", got, err)
		}
		if len(sess.Calls()) != 0 {
			t.Fatalf("unexpected exec calls: %d", len(sess.Calls()))
		}
	})

	t.Run("agent override with home", func(t *testing.T) {
		sess := testexec.NewSession(func(ctx context.Context, cmd string, opts execution.ExecOptions) (execution.Result, error) {
			if strings.HasPrefix(cmd, "printf") {
				return execution.Result{Stdout: "/home/ubuntu\n"}, nil
			}
			return execution.Result{}, nil
		})
		wd, _ := config.ParseWorkingDir("$HOME/work/${agent.type}-${agent.id}")
		got, err := resolveDefaultCwd(context.Background(), sess,
			config.AgentConfig{WorkingDir: &wd}, config.ChannelConfig{}, "coder", "coder-1")
		if err != nil {
			t.Fatal(err)
		}
		if got != "/home/ubuntu/work/coder-coder-1" {
			t.Fatalf("got %q", got)
		}
		if calls := sess.Calls(); len(calls) != 2 || !strings.HasPrefix(calls[1].Cmd, "mkdir -p") {
			t.Fatalf("expected $HOME query then mkdir, got %+v", calls)
		}
	})

	t.Run("channel default without home", func(t *testing.T) {
		sess := testexec.NewSession(nil)
		wd, _ := config.ParseWorkingDir("/srv/${agent.type}")
		got, err := resolveDefaultCwd(context.Background(), sess,
			config.AgentConfig{}, config.ChannelConfig{WorkingDir: wd}, "coder", "coder-1")
		if err != nil {
			t.Fatal(err)
		}
		if got != "/srv/coder" {
			t.Fatalf("got %q", got)
		}
		calls := sess.Calls()
		if len(calls) != 1 || !strings.HasPrefix(calls[0].Cmd, "mkdir -p") || !strings.Contains(calls[0].Cmd, "/srv/coder") {
			t.Fatalf("expected a single mkdir -p for the working dir, got %+v", calls)
		}
	})
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

	got, err := readSystemPrompt(cfgPrompt, "", dir)
	if err != nil || got != "from config" {
		t.Fatalf("config path: %q, %v", got, err)
	}
	got, err = readSystemPrompt(cfgPrompt, overridePrompt, dir)
	if err != nil || got != "from override" {
		t.Fatalf("override path: %q, %v", got, err)
	}
	if _, err := readSystemPrompt(filepath.Join(dir, "missing.md"), "", dir); err == nil {
		t.Fatal("expected error for missing prompt")
	}
}

func TestReadSystemPromptExpandsSkillsIndex(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "index.md"), []byte("index contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(prompt, []byte("before ${SKILLS_INDEX} after"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readSystemPrompt(prompt, "", dir)
	if err != nil {
		t.Fatalf("readSystemPrompt: %v", err)
	}
	if want := "before index contents after"; got != want {
		t.Fatalf("readSystemPrompt = %q, want %q", got, want)
	}
}

func TestChannelControllerSwitch(t *testing.T) {
	firstSess := testexec.NewSession(nil)
	secondSess := testexec.NewSession(nil)
	tc := &transportCache{transports: map[string]execution.Transport{
		"first":  testexec.NewTransport(firstSess),
		"second": testexec.NewTransport(secondSess),
	}}
	cfg := &config.Config{Channels: []config.ChannelConfig{
		{Name: "first", Type: config.ChannelTypeLocal},
		{Name: "second", Type: config.ChannelTypeLocal},
	}}
	c, err := newChannelController(context.Background(), cfg, tc, config.AgentConfig{}, "main", "first")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if got := c.Name(); got != "first" {
		t.Fatalf("initial name = %q, want first", got)
	}
	if firstSess.Closed() {
		t.Fatal("first session closed before switch")
	}

	if err := c.Set(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	if got := c.Name(); got != "second" {
		t.Fatalf("name after switch = %q, want second", got)
	}
	if !firstSess.Closed() {
		t.Fatal("first session not closed after switch")
	}
	if secondSess.Closed() {
		t.Fatal("second session closed before use")
	}

	if err := c.Set(context.Background(), "missing"); !errors.Is(err, sessions.ErrChannelNotFound) {
		t.Fatalf("Set(unknown) error = %v, want ErrChannelNotFound", err)
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
