// Package app assembles a running harness from configuration. It is the single
// place where the config, llm, execution, approval, tools, and agent layers
// meet; cmd/aiharn stays a thin flag parser and TUI bootstrap.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/config"
	"aiharn/internal/execution"
	execssh "aiharn/internal/execution/ssh"
	"aiharn/internal/llm/responses"
	"aiharn/internal/tools"
)

// Options are CLI-level overrides applied on top of the loaded configuration.
// Empty fields mean "use the configuration value".
type Options struct {
	Agent      string // agent type name (default "main")
	PromptFile string // override system prompt file
	Channel    string // override execution channel
	Model      string // override model config name
	Approval   string // override approval mode
}

// Summary is the startup banner content. It never carries secret values.
type Summary struct {
	AgentType string
	Model     string // model config name (e.g. "gpt5_via_openai")
	ModelName string // provider model string (e.g. "gpt-5")
	Channel   string
	Approval  string
}

// Runtime holds the assembled pieces and owns their cleanup.
type Runtime struct {
	Manager    *agent.Manager
	Agent      *agent.Agent // the top-level agent
	Gate       *approval.Gate
	Summary    Summary
	transports *transportCache
}

// Close releases all agents (and their sessions), the transports, and the gate.
// It is idempotent and safe to call with a partially-built Runtime.
func (r *Runtime) Close() error {
	var errs []error
	if r.Manager != nil {
		if err := r.Manager.Shutdown(); err != nil {
			errs = append(errs, err)
		}
	}
	if r.transports != nil {
		if err := r.transports.closeAll(); err != nil {
			errs = append(errs, err)
		}
	}
	if r.Gate != nil {
		r.Gate.Close()
	}
	return errors.Join(errs...)
}

// Build resolves the selected agent, model, and channel; builds the Manager
// (with a builder that can materialize subagents), the top-level agent, the
// approval gate, and the tool registry; and returns a Runtime ready to run. It
// makes no model calls and runs no commands, but it does open the top-level
// agent's SSH session so connection problems surface at startup.
func Build(ctx context.Context, cfg *config.Config, opts Options) (rt *Runtime, err error) {
	agentType := opts.Agent
	if agentType == "" {
		agentType = "main"
	}
	agentCfg, ok := cfg.Agents[agentType]
	if !ok {
		return nil, fmt.Errorf("app: agent type %q is not defined", agentType)
	}

	modelName := agentCfg.Model
	if opts.Model != "" {
		modelName = opts.Model
	}
	modelCfg, ok := cfg.Models[modelName]
	if !ok {
		return nil, fmt.Errorf("app: model %q is not defined", modelName)
	}

	channelCfg, err := selectChannel(cfg, agentCfg.Channel, opts.Channel)
	if err != nil {
		return nil, err
	}

	approvalMode := cfg.Approval.Mode
	if opts.Approval != "" {
		approvalMode = opts.Approval
	}
	gate, err := buildGate(approvalMode)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			gate.Close()
		}
	}()

	tc := &transportCache{transports: map[string]execution.Transport{}}
	defer func() {
		if err != nil {
			tc.closeAll()
		}
	}()

	var mgr *agent.Manager
	mgr = agent.NewManager(agent.ManagerOptions{
		MaxDepth:      cfg.Limits.MaxAgentDepth,
		MaxAgents:     cfg.Limits.MaxOpenAgents,
		InboxCapacity: cfg.Limits.InboxDepth,
		EventCapacity: cfg.Limits.EventCapacity,
		Builder: func(ctx context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
			return buildAgent(ctx, cfg, tc, mgr, gate, spec, agentOverrides{})
		},
	})

	fmt.Fprintf(os.Stderr, "aiharn: connecting to channel %q (host %q)...\n", channelCfg.Name, channelCfg.Host)

	top, err := buildAgent(ctx, cfg, tc, mgr, gate, agent.SpawnSpec{
		ID:       agentType,
		Type:     agentType,
		Depth:    0,
		CallerID: "",
	}, agentOverrides{PromptFile: opts.PromptFile, Model: opts.Model, Channel: opts.Channel})
	if err != nil {
		return nil, err
	}
	if err := mgr.RegisterTop(top); err != nil {
		top.Close()
		return nil, err
	}

	return &Runtime{
		Manager: mgr,
		Agent:   top,
		Gate:    gate,
		Summary: Summary{
			AgentType: agentType,
			Model:     modelName,
			ModelName: modelCfg.Model,
			Channel:   channelCfg.Name,
			Approval:  approvalMode,
		},
		transports: tc,
	}, nil
}

// agentOverrides are CLI-level values applied to the top-level agent only.
type agentOverrides struct {
	PromptFile string
	Model      string
	Channel    string
}

// buildAgent resolves an agent type, model, and channel; opens a dedicated
// session; reads the system prompt; and constructs a fully-wired Agent whose
// cleanup closes its session.
func buildAgent(ctx context.Context, cfg *config.Config, tc *transportCache, mgr *agent.Manager, gate *approval.Gate, spec agent.SpawnSpec, o agentOverrides) (*agent.Agent, error) {
	agentCfg, ok := cfg.Agents[spec.Type]
	if !ok {
		return nil, fmt.Errorf("app: agent type %q is not defined", spec.Type)
	}

	modelName := agentCfg.Model
	if o.Model != "" {
		modelName = o.Model
	}
	modelCfg, ok := cfg.Models[modelName]
	if !ok {
		return nil, fmt.Errorf("app: model %q is not defined", modelName)
	}

	channelCfg, err := selectChannel(cfg, agentCfg.Channel, o.Channel)
	if err != nil {
		return nil, err
	}

	transport, err := tc.get(channelCfg)
	if err != nil {
		return nil, err
	}
	session, err := transport.NewSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("app: open channel %q session (host %q): %w", channelCfg.Name, channelCfg.Host, err)
	}

	system, err := readSystemPrompt(agentCfg.SystemPrompt, o.PromptFile)
	if err != nil {
		session.Close()
		return nil, err
	}

	defaultCwd, err := resolveDefaultCwd(ctx, session, agentCfg, channelCfg, spec.Type, spec.ID)
	if err != nil {
		session.Close()
		return nil, err
	}

	reg := buildRegistry(session, gate, cfg.Limits.CommandOutputBytes, defaultCwd, agentCfg.Tools, mgr, spec.ID)

	a := agent.New(agent.Spec{
		ID:              spec.ID,
		Type:            spec.Type,
		Model:           modelCfg.Model,
		System:          system,
		Client:          responses.NewAdapter(modelCfg.BaseURL, modelCfg.APIKey),
		Tools:           reg,
		Depth:           spec.Depth,
		CallerID:        spec.CallerID,
		AllowSubagents:  agentCfg.AllowSubagents,
		EventCapacity:   cfg.Limits.EventCapacity,
		InboxCapacity:   cfg.Limits.InboxDepth,
		RequestTimeout:  cfg.Limits.RequestTimeout.Std(),
		ToolResultBytes: cfg.Limits.ToolResultBytes,
		Cleanup:         func() { session.Close() },
	})
	return a, nil
}

// resolveDefaultCwd renders the working_dir template (agent override, else the
// channel's) into a literal remote path. It queries the remote home lazily —
// only when the template references "~" or "$HOME" — and returns "" when no
// working_dir is configured.
func resolveDefaultCwd(ctx context.Context, session execution.Session, agentCfg config.AgentConfig, channelCfg config.ChannelConfig, agentType, agentID string) (string, error) {
	tmpl := channelCfg.WorkingDir
	if agentCfg.WorkingDir != nil {
		tmpl = *agentCfg.WorkingDir
	}
	if tmpl.Raw == "" {
		return "", nil
	}

	home := ""
	if strings.Contains(tmpl.Raw, "~") || strings.Contains(tmpl.Raw, "$HOME") {
		r, err := session.Exec(ctx, `printf '%s' "$HOME"`, execution.ExecOptions{})
		if err != nil {
			return "", fmt.Errorf("app: resolve remote $HOME on channel %q for working_dir %q: %w", channelCfg.Name, tmpl.Raw, err)
		}
		home = strings.TrimSpace(r.Stdout)
	}

	return execution.RenderWorkingDir(tmpl.Raw, agentType, agentID, home)
}

// transportCache returns one Transport per channel name, created lazily and
// shared by every agent on that channel.
type transportCache struct {
	mu         sync.Mutex
	transports map[string]execution.Transport
}

func (tc *transportCache) get(c config.ChannelConfig) (execution.Transport, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if t, ok := tc.transports[c.Name]; ok {
		return t, nil
	}
	t, err := buildTransport(c)
	if err != nil {
		return nil, err
	}
	tc.transports[c.Name] = t
	return t, nil
}

func (tc *transportCache) closeAll() error {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	var errs []error
	for _, t := range tc.transports {
		if err := t.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// selectChannel resolves the execution channel with precedence: override, then
// the agent's channel, then the first configured channel.
func selectChannel(cfg *config.Config, agentChannel, override string) (config.ChannelConfig, error) {
	name := override
	if name == "" {
		name = agentChannel
	}
	if name == "" {
		if len(cfg.Channels) == 0 {
			return config.ChannelConfig{}, errors.New("app: no channels configured")
		}
		return cfg.Channels[0], nil
	}
	for _, c := range cfg.Channels {
		if c.Name == name {
			return c, nil
		}
	}
	return config.ChannelConfig{}, fmt.Errorf("app: channel %q is not defined", name)
}

func buildTransport(c config.ChannelConfig) (execution.Transport, error) {
	if c.Type != config.ChannelTypeSSH {
		return nil, fmt.Errorf("app: channel type %q is not implemented", c.Type)
	}
	return execssh.NewTransport(execssh.Options{
		Host:           c.Host,
		Port:           c.Port,
		User:           c.User,
		KeyFile:        c.Auth.KeyFile,
		Password:       c.Auth.Password,
		KnownHosts:     c.KnownHosts,
		Insecure:       c.Insecure,
		KeepAlive:      c.KeepAliveEnabled(),
		DefaultShell:   c.DefaultShell,
		SSHConfigAlias: c.IsSSHConfigAlias(),
	})
}

func buildGate(mode string) (*approval.Gate, error) {
	switch mode {
	case config.ApprovalModeAsk:
		return approval.NewGate(approval.ModeAsk), nil
	case config.ApprovalModeAllowAll:
		return approval.NewGate(approval.ModeAllowAll), nil
	default:
		return nil, fmt.Errorf("app: invalid approval mode %q", mode)
	}
}

// buildRegistry attaches the built-in tools selected by sel to a fresh registry.
// The session and gate are shared by all built-ins; backend (the Manager) and
// callerID wire the subagent tools to the runtime. defaultCwd is the directory
// execute_command falls back to when the model omits one.
func buildRegistry(session execution.Session, gate *approval.Gate, maxOutput int64, defaultCwd string, sel config.ToolSelection, backend tools.SubagentBackend, callerID string) *tools.Registry {
	reg := tools.New()
	all := map[string]tools.Tool{
		tools.NameExecuteCommand:      tools.ExecuteCommand(session, gate, maxOutput, defaultCwd),
		tools.NameSpawnSubagent:       tools.SpawnSubagent(backend, gate, callerID),
		tools.NameSendSubagentMessage: tools.SendSubagentMessage(backend, callerID),
		tools.NameCheckSubagent:       tools.CheckSubagent(backend, callerID),
		tools.NameListSubagents:       tools.ListSubagents(backend, callerID),
		tools.NameCloseSubagent:       tools.CloseSubagent(backend, callerID),
		tools.NameSetApproval:         tools.SetApproval(gate),
	}
	switch sel.Mode {
	case config.ToolModeNone:
		return reg
	case config.ToolModeAll:
		for _, name := range tools.Names() {
			if t, ok := all[name]; ok {
				reg.Register(t)
			}
		}
	case config.ToolModeList:
		for _, name := range sel.Names {
			if t, ok := all[name]; ok {
				reg.Register(t)
			}
		}
	}
	return reg
}

func readSystemPrompt(configPath, overridePath string) (string, error) {
	p := configPath
	if overridePath != "" {
		p = overridePath
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("app: read system prompt %s: %w", p, err)
	}
	return string(b), nil
}
