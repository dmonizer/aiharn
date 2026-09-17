// Package app assembles a running harness from configuration. It is the single
// place where the config, llm, execution, approval, tools, and agent layers
// meet; cmd/aiharn stays a thin flag parser and TUI bootstrap.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"aiharn/internal/agent"
	"aiharn/internal/approval"
	"aiharn/internal/config"
	"aiharn/internal/execution"
	execlocal "aiharn/internal/execution/local"
	execssh "aiharn/internal/execution/ssh"
	"aiharn/internal/llm"
	"aiharn/internal/llm/chatcompletions"
	"aiharn/internal/llm/responses"
	"aiharn/internal/sessions"
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

	// Observer, when set, receives every history item appended by any agent (the
	// top-level agent and all subagents), enabling a full conversation log.
	Observer agent.HistoryObserver

	// NewTranscript, when set, is called once per session before that session's
	// runtime is built. It supersedes Observer, which remains the single shared
	// transcript used when no factory is set.
	NewTranscript func(sessionID, sessionName string) (sessions.Transcript, error)
}

// sessionTranscript resolves the history observer for a new session. With
// Options.NewTranscript set it creates a per-session transcript and returns it
// as both the observer and the session-owned sink, so Session can close it
// independently; otherwise every session shares Options.Observer and owns no
// transcript of its own. The returned transcript is nil in the shared case.
func sessionTranscript(opts Options, sessionID, sessionName string) (agent.HistoryObserver, sessions.Transcript, error) {
	if opts.NewTranscript == nil {
		return opts.Observer, nil, nil
	}
	tr, err := opts.NewTranscript(sessionID, sessionName)
	if err != nil {
		return nil, nil, fmt.Errorf("app: create transcript for session %q: %w", sessionID, err)
	}
	if tr == nil {
		return nil, nil, fmt.Errorf("app: transcript factory returned nil for session %q", sessionID)
	}
	return tr, tr, nil
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
	if cfg == nil {
		return nil, errors.New("app: config is nil")
	}
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
	observer := opts.Observer
	mgr = agent.NewManager(agent.ManagerOptions{
		MaxDepth:      cfg.Limits.MaxAgentDepth,
		MaxAgents:     cfg.Limits.MaxOpenAgents,
		InboxCapacity: cfg.Limits.InboxDepth,
		EventCapacity: cfg.Limits.EventCapacity,
		SubagentTypes: configuredSubagentTypes(cfg),
		Builder: func(ctx context.Context, spec agent.SpawnSpec) (*agent.Agent, error) {
			return buildAgent(ctx, cfg, tc, mgr, gate, spec, agentOverrides{}, observer)
		},
	})

	fmt.Fprintf(os.Stderr, "aiharn: connecting to channel %q (%s)...\n", channelCfg.Name, channelTarget(channelCfg))

	top, err := buildAgent(ctx, cfg, tc, mgr, gate, agent.SpawnSpec{
		ID:            agentType,
		Type:          agentType,
		Depth:         0,
		CallerID:      "",
		InboxCapacity: cfg.Limits.InboxDepth,
		EventCapacity: cfg.Limits.EventCapacity,
	}, agentOverrides{PromptFile: opts.PromptFile, Model: opts.Model, Channel: opts.Channel}, observer)
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
func buildAgent(ctx context.Context, cfg *config.Config, tc *transportCache, mgr *agent.Manager, gate *approval.Gate, spec agent.SpawnSpec, o agentOverrides, observer agent.HistoryObserver) (*agent.Agent, error) {
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
	client, err := buildModelClient(modelCfg)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("app: open channel %q session (%s): %w", channelCfg.Name, channelTarget(channelCfg), err)
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

	reg, err := buildRegistry(session, gate, cfg.Limits.CommandOutputBytes, defaultCwd,
		cfg.Limits.CommandTimeout.Std(), agentCfg.Tools, mgr, spec.ID, spec.Type)
	if err != nil {
		session.Close()
		return nil, err
	}

	a := agent.New(agent.Spec{
		ID:               spec.ID,
		Name:             spec.Name,
		Type:             spec.Type,
		Model:            modelCfg.Model,
		System:           system,
		ReasoningEffort:  modelCfg.ReasoningEffort,
		ReasoningSummary: modelCfg.ReasoningSummary,
		Client:           client,
		Tools:            reg,
		Depth:            spec.Depth,
		CallerID:         spec.CallerID,
		AllowSubagents:   agentCfg.AllowSubagents,
		EventCapacity:    spec.EventCapacity,
		InboxCapacity:    spec.InboxCapacity,
		RequestTimeout:   cfg.Limits.RequestTimeout.Std(),
		ToolResultBytes:  cfg.Limits.ToolResultBytes,
		TranscriptItems:  cfg.Limits.TranscriptMaxItems,
		TranscriptBytes:  cfg.Limits.TranscriptMaxBytes,
		Observer:         observer,
		Cleanup:          func() { session.Close() },
	})
	return a, nil
}

func buildModelClient(model config.ModelConfig) (llm.Client, error) {
	switch model.Provider {
	case config.ProviderOpenAIResponses:
		return responses.NewAdapter(model.BaseURL, model.APIKey), nil
	case config.ProviderOpenAIChatCompletions:
		return chatcompletions.NewAdapter(model.BaseURL, model.APIKey), nil
	default:
		return nil, fmt.Errorf("app: unsupported model provider %q", model.Provider)
	}
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
	if needsRemoteHome(tmpl.Raw) {
		r, err := session.Exec(ctx, `printf '%s' "$HOME"`, execution.ExecOptions{MaxOutputBytes: 64 << 10})
		if err != nil {
			return "", fmt.Errorf("app: resolve remote $HOME on channel %q for working_dir %q: %w", channelCfg.Name, tmpl.Raw, err)
		}
		if r.ExitCode != 0 {
			return "", fmt.Errorf("app: resolve remote $HOME on channel %q: command exited %d: %s", channelCfg.Name, r.ExitCode, strings.TrimSpace(r.Stderr))
		}
		if r.Truncated {
			return "", fmt.Errorf("app: resolve remote $HOME on channel %q: output exceeded limit", channelCfg.Name)
		}
		home = strings.TrimSpace(r.Stdout)
		if home == "" {
			return "", fmt.Errorf("app: resolve remote $HOME on channel %q: remote returned an empty path", channelCfg.Name)
		}
	}

	dir, err := execution.RenderWorkingDir(tmpl.Raw, agentType, agentID, home)
	if err != nil {
		return "", err
	}
	if err := ensureRemoteDir(ctx, session, channelCfg.Name, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// ensureRemoteDir makes dir exist on the remote host (mkdir -p), so a command
// never fails because its configured working directory is missing.
func ensureRemoteDir(ctx context.Context, session execution.Session, channelName, dir string) error {
	r, err := session.Exec(ctx, "mkdir -p -- "+shellQuote(dir), execution.ExecOptions{MaxOutputBytes: 64 << 10})
	if err != nil {
		return fmt.Errorf("app: create working_dir %q on channel %q: %w", dir, channelName, err)
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("app: create working_dir %q on channel %q: mkdir exited %d: %s", dir, channelName, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return nil
}

// shellQuote single-quotes s so it is treated as one literal argument when the
// remote shell evals the command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func needsRemoteHome(template string) bool {
	for _, segment := range strings.Split(template, "/") {
		if segment == "~" || segment == "$HOME" {
			return true
		}
	}
	return false
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

// channelTarget is the human-readable connection target for a channel: the
// remote host for SSH, or "local" for a local channel.
func channelTarget(c config.ChannelConfig) string {
	if c.Type == config.ChannelTypeLocal {
		return "local"
	}
	return c.Host
}

func buildTransport(c config.ChannelConfig) (execution.Transport, error) {
	switch c.Type {
	case config.ChannelTypeSSH:
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
			RemoteCommand:  c.RemoteCommand,
			SSHConfigAlias: c.IsSSHConfigAlias(),
		})
	case config.ChannelTypeLocal:
		return execlocal.NewTransport(execlocal.Options{
			DefaultShell:  c.DefaultShell,
			RemoteCommand: c.RemoteCommand,
		})
	default:
		return nil, fmt.Errorf("app: channel type %q is not implemented", c.Type)
	}
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
func buildRegistry(session execution.Session, gate *approval.Gate, maxOutput int64, defaultCwd string, commandTimeout time.Duration, sel config.ToolSelection, backend tools.SubagentBackend, callerID, callerType string) (*tools.Registry, error) {
	reg := tools.New()
	spawnToolEnabled := sel.Mode == config.ToolModeAll || (sel.Mode == config.ToolModeList && slices.Contains(sel.Names, tools.NameSpawnSubagent))
	all := map[string]tools.Tool{
		tools.NameExecuteCommand:      tools.ExecuteCommand(session, gate, maxOutput, defaultCwd, commandTimeout, callerID, callerType),
		tools.NameListSubagentTypes:   tools.ListSubagentTypes(backend, callerID, spawnToolEnabled),
		tools.NameSpawnSubagent:       tools.SpawnSubagent(backend, gate, callerID, callerType),
		tools.NameSendSubagentMessage: tools.SendSubagentMessage(backend, callerID),
		tools.NameSendAgentMessage:    tools.SendAgentMessage(backend, callerID),
		tools.NameCheckSubagent:       tools.CheckSubagent(backend, callerID),
		tools.NameListSubagents:       tools.ListSubagents(backend, callerID),
		tools.NameCloseSubagent:       tools.CloseSubagent(backend, callerID),
		tools.NameSetApproval:         tools.SetApproval(gate),
	}
	switch sel.Mode {
	case config.ToolModeNone:
		return reg, nil
	case config.ToolModeAll:
		for _, name := range tools.Names() {
			if t, ok := all[name]; ok {
				if err := reg.Register(t); err != nil {
					return nil, fmt.Errorf("app: register tool %q: %w", name, err)
				}
			}
		}
	case config.ToolModeList:
		for _, name := range sel.Names {
			t, ok := all[name]
			if !ok {
				return nil, fmt.Errorf("app: unknown tool %q", name)
			}
			if err := reg.Register(t); err != nil {
				return nil, fmt.Errorf("app: register tool %q: %w", name, err)
			}
		}
	default:
		return nil, fmt.Errorf("app: invalid tool selection mode %q", sel.Mode)
	}
	return reg, nil
}

func configuredSubagentTypes(cfg *config.Config) []tools.SubagentType {
	types := make([]tools.SubagentType, 0, len(cfg.Agents))
	for name, agentCfg := range cfg.Agents {
		channel := agentCfg.Channel
		if channel == "" && len(cfg.Channels) > 0 {
			channel = cfg.Channels[0].Name
		}
		types = append(types, tools.SubagentType{
			Name:           name,
			Description:    agentCfg.Description,
			Model:          agentCfg.Model,
			Channel:        channel,
			AllowSubagents: agentCfg.AllowSubagents,
		})
	}
	sort.Slice(types, func(i, j int) bool { return types[i].Name < types[j].Name })
	return types
}

func readSystemPrompt(configPath, overridePath string) (string, error) {
	p := configPath
	if overridePath != "" {
		p = overridePath
	}
	const maxSystemPromptBytes = 4 << 20
	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("app: read system prompt %s: %w", p, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSystemPromptBytes+1))
	if err != nil {
		return "", fmt.Errorf("app: read system prompt %s: %w", p, err)
	}
	if len(b) > maxSystemPromptBytes {
		return "", fmt.Errorf("app: system prompt %s exceeds %d-byte limit", p, maxSystemPromptBytes)
	}
	return string(b), nil
}
