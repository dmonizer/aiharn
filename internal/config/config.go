// Package config loads and validates Aiharn's TOML configuration.
//
// Layering: config depends on nothing else in the project. Load performs the
// mechanical steps (parse, environment interpolation, local path resolution,
// and defaults); Validate performs the semantic checks (cross-references,
// limits, paths, auth, and tool names).
package config

import (
	"fmt"
	"math"
	"time"
)

// Duration is a time.Duration that unmarshals from a TOML string ("30s") or an
// integer (seconds). A zero Duration means "unset" and is replaced by the field
// default during Load.
type Duration time.Duration

// Std returns the value as a standard time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// UnmarshalTOML implements toml.Unmarshaler.
func (d *Duration) UnmarshalTOML(v interface{}) error {
	switch t := v.(type) {
	case string:
		p, err := time.ParseDuration(t)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", t, err)
		}
		*d = Duration(p)
		return nil
	case int64:
		if t > math.MaxInt64/int64(time.Second) || t < math.MinInt64/int64(time.Second) {
			return fmt.Errorf("duration integer seconds %d overflows time.Duration", t)
		}
		*d = Duration(time.Duration(t) * time.Second)
		return nil
	default:
		return fmt.Errorf("duration must be a string like \"30s\" or integer seconds, got %T", v)
	}
}

// Config is the fully materialized configuration.
type Config struct {
	AiharnHome string `toml:"aiharn_home"` // local data directory; default is the user's home directory
	Models     map[string]ModelConfig
	Channels   []ChannelConfig
	Agents     map[string]AgentConfig
	Limits     LimitsConfig
	Approval   ApprovalConfig
	API        APIConfig
}

// ModelConfig is a named model/API configuration referenced by agents.
type ModelConfig struct {
	Provider         string `toml:"provider"`
	BaseURL          string `toml:"base_url"`
	APIKey           string `toml:"api_key"`
	Model            string `toml:"model"`
	ReasoningEffort  string `toml:"reasoning_effort"`
	ReasoningSummary string `toml:"reasoning_summary"`
}

// AuthConfig selects how an SSH channel authenticates: exactly one of KeyFile
// or Password must be set (validated in Validate).
type AuthConfig struct {
	KeyFile  string `toml:"key_file"`
	Password string `toml:"password"`
}

// ChannelConfig is an execution channel (transport). The first channel in the
// list is the default unless an agent overrides it.
type ChannelConfig struct {
	Name          string             `toml:"name"`
	Type          string             `toml:"type"`
	Host          string             `toml:"host"`
	Port          int                `toml:"port"`
	User          string             `toml:"user"`
	Auth          AuthConfig         `toml:"auth"`
	KnownHosts    string             `toml:"known_hosts"`
	Insecure      bool               `toml:"insecure"`
	KeepAlive     *bool              `toml:"keep_alive"` // non-nil after Load; true = shared client
	DefaultShell  string             `toml:"default_shell"`
	RemoteCommand string             `toml:"remote_command"` // optional command run in place of the default shell
	WorkingDir    WorkingDirTemplate `toml:"working_dir"`
}

// KeepAliveEnabled reports whether the channel reuses a shared client
// connection. It is non-nil after Load.
func (c ChannelConfig) KeepAliveEnabled() bool {
	return c.KeepAlive == nil || *c.KeepAlive
}

// IsSSHConfigAlias reports whether the channel carries only a name and host —
// no user, auth, or known_hosts — so its host should be resolved as an OpenSSH
// ~/.ssh/config alias rather than an explicit endpoint.
func (c ChannelConfig) IsSSHConfigAlias() bool {
	return c.User == "" && c.Auth.KeyFile == "" && c.Auth.Password == "" &&
		c.KnownHosts == "" && !c.Insecure
}

// AgentConfig is a configured agent type.
type AgentConfig struct {
	Model          string              `toml:"model"`
	Description    string              `toml:"description"`
	SystemPrompt   string              `toml:"system_prompt"`
	Channel        string              `toml:"channel"` // "" = first channel
	WorkingDir     *WorkingDirTemplate `toml:"working_dir"`
	Tools          ToolSelection       `toml:"tools"`
	AllowSubagents bool                `toml:"allow_subagents"`
}

// LimitsConfig holds the runtime bounds. Zero means "use the default"; negative
// values are rejected by Validate.
type LimitsConfig struct {
	MaxAgentDepth      int      `toml:"max_agent_depth"`
	MaxOpenAgents      int      `toml:"max_open_agents"`
	CommandTimeout     Duration `toml:"command_timeout"`
	CommandOutputBytes int64    `toml:"command_output_bytes"`
	ToolResultBytes    int64    `toml:"tool_result_bytes"`
	InboxDepth         int      `toml:"inbox_depth"`
	EventCapacity      int      `toml:"event_capacity"`
	RequestTimeout     Duration `toml:"request_timeout"`
	TranscriptMaxItems int      `toml:"transcript_max_items"`
	TranscriptMaxBytes int64    `toml:"transcript_max_bytes"`
}

// ApprovalConfig holds the command-approval settings.
type ApprovalConfig struct {
	Mode string `toml:"mode"`
}

// APIConfig controls the optional current-session HTTP API. Zero values leave
// the API disabled and keep the terminal UI enabled.
type APIConfig struct {
	Listen       string   `toml:"listen"`
	Token        string   `toml:"token"`
	AllowOrigins []string `toml:"allow_origins"`
	Only         bool     `toml:"only"`
}

// Known tool-set names and provider/channel types used as defaults in Validate.
const (
	ProviderOpenAIResponses       = "openai_responses"
	ProviderOpenAIChatCompletions = "openai_chat_completions"
	ChannelTypeSSH                = "ssh"
	ChannelTypeLocal              = "local"

	ToolModeNone = "none"
	ToolModeAll  = "all"
	ToolModeList = "list"

	ApprovalModeAsk      = "ask"
	ApprovalModeAllowAll = "allow-all"
)

// Defaults used when a field is absent or zero.
const (
	defaultSSHPort      = 22
	defaultShell        = "/bin/bash"
	defaultChannelType  = ChannelTypeSSH
	defaultToolsMode    = ToolModeNone
	defaultApprovalMode = ApprovalModeAsk

	defaultMaxAgentDepth      = 2
	defaultMaxOpenAgents      = 8
	defaultCommandTimeout     = 30 * time.Second
	defaultCommandOutputBytes = 262144 // 256 KiB
	defaultToolResultBytes    = 65536  // 64 KiB
	defaultInboxDepth         = 32
	defaultEventCapacity      = 256
	defaultRequestTimeout     = 60 * time.Second
	defaultTranscriptMaxItems = 200
	defaultTranscriptMaxBytes = 1048576 // 1 MiB
)

// DefaultLimits returns the limit defaults.
func DefaultLimits() LimitsConfig {
	return LimitsConfig{
		MaxAgentDepth:      defaultMaxAgentDepth,
		MaxOpenAgents:      defaultMaxOpenAgents,
		CommandTimeout:     Duration(defaultCommandTimeout),
		CommandOutputBytes: defaultCommandOutputBytes,
		ToolResultBytes:    defaultToolResultBytes,
		InboxDepth:         defaultInboxDepth,
		EventCapacity:      defaultEventCapacity,
		RequestTimeout:     Duration(defaultRequestTimeout),
		TranscriptMaxItems: defaultTranscriptMaxItems,
		TranscriptMaxBytes: defaultTranscriptMaxBytes,
	}
}
