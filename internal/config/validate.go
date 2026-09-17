package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ValidateOptions carries the sets known to the layers above config, so config
// can validate references without importing them. Nil sets fall back to the
// built-in defaults for providers and channel types; a nil KnownTools skips the
// tool-name existence check (only syntax is validated).
type ValidateOptions struct {
	KnownProviders    map[string]bool
	KnownChannelTypes map[string]bool
	KnownTools        map[string]bool
}

func (o ValidateOptions) providers() map[string]bool {
	if o.KnownProviders != nil {
		return o.KnownProviders
	}
	return map[string]bool{
		ProviderOpenAIResponses:       true,
		ProviderOpenAIChatCompletions: true,
	}
}

func (o ValidateOptions) channelTypes() map[string]bool {
	if o.KnownChannelTypes != nil {
		return o.KnownChannelTypes
	}
	return map[string]bool{ChannelTypeSSH: true, ChannelTypeLocal: true}
}

// Validate performs semantic validation. All errors are returned together where
// feasible so the user sees the full picture. Secret values are never embedded.
func Validate(cfg *Config, opts ValidateOptions) error {
	if cfg == nil {
		return fmt.Errorf("config: validation failed:\n  - config is nil")
	}
	var errs []string
	if cfg.AiharnHome == "" || !filepath.IsAbs(cfg.AiharnHome) {
		errs = append(errs, "aiharn_home must resolve to an absolute local directory")
	}
	validateShortcuts(&errs, cfg.Shortcuts)
	providers := opts.providers()
	channelTypes := opts.channelTypes()

	// Models.
	for name, m := range cfg.Models {
		if name == "" {
			errs = append(errs, "models: name must not be empty")
		}
		if m.Model == "" {
			errs = append(errs, fmt.Sprintf("models[%q].model is required", name))
		}
		if !providers[m.Provider] {
			errs = append(errs, fmt.Sprintf("models[%q].provider %q is unsupported", name, m.Provider))
		}
		if m.APIKey == "" {
			errs = append(errs, fmt.Sprintf("models[%q].api_key is required", name))
		}
		validateModelBaseURL(&errs, name, m)
		if !validReasoningEffort(m.ReasoningEffort) {
			errs = append(errs, fmt.Sprintf("models[%q].reasoning_effort %q is invalid", name, m.ReasoningEffort))
		}
		if !validReasoningSummary(m.ReasoningSummary) {
			errs = append(errs, fmt.Sprintf("models[%q].reasoning_summary %q is invalid", name, m.ReasoningSummary))
		}
		if m.ReasoningSummary != "" && m.Provider != ProviderOpenAIResponses {
			errs = append(errs, fmt.Sprintf("models[%q].reasoning_summary requires provider %q", name, ProviderOpenAIResponses))
		}
		if m.ReasoningEffort == "none" && m.ReasoningSummary != "" {
			errs = append(errs, fmt.Sprintf("models[%q]: reasoning_summary cannot be requested when reasoning_effort is %q", name, "none"))
		}
	}

	// Channels: names unique, required fields, type known, auth set.
	seenChannels := map[string]bool{}
	for i := range cfg.Channels {
		c := &cfg.Channels[i]
		if c.Name == "" {
			errs = append(errs, fmt.Sprintf("channels[%d].name is required", i))
			continue
		}
		if seenChannels[c.Name] {
			errs = append(errs, fmt.Sprintf("channels: duplicate name %q", c.Name))
		}
		seenChannels[c.Name] = true

		if !channelTypes[c.Type] {
			errs = append(errs, fmt.Sprintf("channels[%q].type %q is unsupported", c.Name, c.Type))
		}
		switch c.Type {
		case ChannelTypeLocal:
			// A local channel runs commands on this machine; it needs no host,
			// port, user, auth, or host-key verification.
			continue
		case ChannelTypeSSH:
			if c.Host == "" {
				errs = append(errs, fmt.Sprintf("channels[%q].host is required", c.Name))
			}
			if c.Port < 1 || c.Port > 65535 {
				errs = append(errs, fmt.Sprintf("channels[%q].port must be between 1 and 65535", c.Name))
			}
			// An SSH alias (name + host only) resolves user/auth/known_hosts from
			// ~/.ssh/config at runtime, so those fields are not required here.
			if c.IsSSHConfigAlias() {
				continue
			}
			if c.User == "" {
				errs = append(errs, fmt.Sprintf("channels[%q].user is required", c.Name))
			}
			if c.Auth.KeyFile == "" && c.Auth.Password == "" {
				errs = append(errs, fmt.Sprintf("channels[%q].auth: one of key_file or password is required", c.Name))
			}
			if c.Auth.KeyFile != "" && c.Auth.Password != "" {
				errs = append(errs, fmt.Sprintf("channels[%q].auth: key_file and password are mutually exclusive", c.Name))
			}
			if c.Auth.KeyFile != "" {
				validateRegularFile(&errs, fmt.Sprintf("channels[%q].auth.key_file", c.Name), c.Auth.KeyFile)
			}
			if !c.Insecure && c.KnownHosts == "" {
				errs = append(errs, fmt.Sprintf("channels[%q].known_hosts is required unless insecure=true", c.Name))
			}
			if c.KnownHosts != "" {
				validateRegularFile(&errs, fmt.Sprintf("channels[%q].known_hosts", c.Name), c.KnownHosts)
			}
		}
	}

	if len(cfg.Channels) == 0 {
		errs = append(errs, "at least one channel is required")
	}

	// Agents.
	for name, a := range cfg.Agents {
		if name == "" {
			errs = append(errs, "agents: name must not be empty")
		}
		if a.Model == "" {
			errs = append(errs, fmt.Sprintf("agents[%q].model is required", name))
		} else if _, ok := cfg.Models[a.Model]; !ok {
			errs = append(errs, fmt.Sprintf("agents[%q].model %q is not defined", name, a.Model))
		}
		if a.Channel != "" && !seenChannels[a.Channel] {
			errs = append(errs, fmt.Sprintf("agents[%q].channel %q is not defined", name, a.Channel))
		}
		if a.SystemPrompt == "" {
			errs = append(errs, fmt.Sprintf("agents[%q].system_prompt is required", name))
		} else {
			validateRegularFile(&errs, fmt.Sprintf("agents[%q].system_prompt", name), a.SystemPrompt)
		}
		switch a.Tools.Mode {
		case ToolModeNone, ToolModeAll:
		case ToolModeList:
			if len(a.Tools.Names) == 0 {
				errs = append(errs, fmt.Sprintf("agents[%q].tools: list must not be empty", name))
			}
			seenTools := make(map[string]bool, len(a.Tools.Names))
			for _, tn := range a.Tools.Names {
				if seenTools[tn] {
					errs = append(errs, fmt.Sprintf("agents[%q].tools: duplicate tool %q", name, tn))
				}
				seenTools[tn] = true
				if opts.KnownTools != nil && !opts.KnownTools[tn] {
					errs = append(errs, fmt.Sprintf("agents[%q].tools: unknown tool %q", name, tn))
				}
			}
		default:
			errs = append(errs, fmt.Sprintf("agents[%q].tools: invalid mode %q", name, a.Tools.Mode))
		}
	}

	// Limits.
	lim := cfg.Limits
	if lim.MaxAgentDepth < 0 {
		errs = append(errs, "limits.max_agent_depth must be positive")
	}
	if lim.MaxOpenAgents < 0 {
		errs = append(errs, "limits.max_open_agents must be positive")
	}
	if lim.CommandTimeout < 0 {
		errs = append(errs, "limits.command_timeout must be positive")
	}
	if lim.CommandOutputBytes < 0 {
		errs = append(errs, "limits.command_output_bytes must be positive")
	}
	if lim.ToolResultBytes < 0 {
		errs = append(errs, "limits.tool_result_bytes must be positive")
	}
	if lim.InboxDepth < 0 {
		errs = append(errs, "limits.inbox_depth must be positive")
	}
	if lim.EventCapacity < 0 {
		errs = append(errs, "limits.event_capacity must be positive")
	}
	if lim.ToolcallsPerTurn < 0 {
		errs = append(errs, "limits.toolcalls_per_turn must be positive")
	}
	if lim.ThinkingTimeout < 0 {
		errs = append(errs, "limits.thinking_timeout must be positive")
	}
	if lim.RequestTimeout < 0 {
		errs = append(errs, "limits.request_timeout must be positive")
	}
	if lim.TranscriptMaxItems < 0 {
		errs = append(errs, "limits.transcript_max_items must be positive")
	}
	if lim.TranscriptMaxBytes < 0 {
		errs = append(errs, "limits.transcript_max_bytes must be positive")
	}

	// Approval.
	switch cfg.Approval.Mode {
	case ApprovalModeAsk, ApprovalModeAllowAll:
	default:
		errs = append(errs, fmt.Sprintf("approval.mode %q is invalid (want %q or %q)", cfg.Approval.Mode, ApprovalModeAsk, ApprovalModeAllowAll))
	}

	validateAPI(&errs, cfg.API)

	if len(errs) > 0 {
		return fmt.Errorf("config: validation failed:\n  - %s", joinErrs(errs))
	}
	return nil
}

func validReasoningEffort(value string) bool {
	switch value {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

func validReasoningSummary(value string) bool {
	switch value {
	case "", "auto", "concise", "detailed":
		return true
	default:
		return false
	}
}

func joinErrs(errs []string) string {
	out := errs[0]
	for _, e := range errs[1:] {
		out += "\n  - " + e
	}
	return out
}

func validateAPI(errs *[]string, api APIConfig) {
	if api.Only && api.Listen == "" {
		*errs = append(*errs, "api.only requires api.listen")
	}
	if api.MaxSessions < 0 {
		*errs = append(*errs, "api.max_sessions must not be negative")
	}
	if api.Listen != "" {
		if api.AuthFile == "" {
			*errs = append(*errs, "api.auth_file is required when api.listen is set")
		}
		_, port, err := net.SplitHostPort(api.Listen)
		if err != nil {
			*errs = append(*errs, fmt.Sprintf("api.listen %q is not a valid host:port address", api.Listen))
		} else {
			number, err := strconv.Atoi(port)
			if err != nil || number < 1 || number > 65535 {
				*errs = append(*errs, "api.listen port must be between 1 and 65535")
			}
		}
	}
	for i, origin := range api.AllowOrigins {
		if origin == "*" {
			continue
		}
		parsed, err := url.Parse(origin)
		invalid := err != nil ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") ||
			parsed.Host == "" || parsed.User != nil || parsed.Path != "" ||
			parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != ""
		if invalid {
			*errs = append(*errs, fmt.Sprintf(
				"api.allow_origins[%d] must be an HTTP(S) origin without credentials, path, query, or fragment", i))
		}
	}
}

func validateRegularFile(errs *[]string, field, path string) {
	info, err := os.Stat(path)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: %v", field, err))
		return
	}
	if !info.Mode().IsRegular() {
		*errs = append(*errs, fmt.Sprintf("%s: %s is not a regular file", field, path))
	}
}

func validateModelBaseURL(errs *[]string, name string, model ModelConfig) {
	field := fmt.Sprintf("models[%q].base_url", name)
	if model.BaseURL == "" {
		*errs = append(*errs, field+" is required")
		return
	}
	parsed, err := url.Parse(model.BaseURL)
	invalid := err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.ForceQuery || parsed.Fragment != ""
	if invalid {
		*errs = append(*errs, field+" must be an HTTP(S) API root without credentials, query, or fragment")
		return
	}
	if parsed.Scheme == "http" && !isLoopbackHost(parsed.Hostname()) {
		*errs = append(*errs, field+" must use HTTPS unless it targets a loopback host")
		return
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch model.Provider {
	case ProviderOpenAIResponses:
		if strings.HasSuffix(path, "/responses") {
			*errs = append(*errs, field+" must be the API root; the adapter appends /responses")
		}
	case ProviderOpenAIChatCompletions:
		if strings.HasSuffix(path, "/chat/completions") {
			*errs = append(*errs, field+" must be the API root; the adapter appends /chat/completions")
		}
	}
}

func isLoopbackHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
