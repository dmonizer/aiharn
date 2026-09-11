package config

import (
	"fmt"
	"os"
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
	return map[string]bool{defaultProvider: true}
}

func (o ValidateOptions) channelTypes() map[string]bool {
	if o.KnownChannelTypes != nil {
		return o.KnownChannelTypes
	}
	return map[string]bool{defaultChannelType: true}
}

// Validate performs semantic validation. All errors are returned together where
// feasible so the user sees the full picture. Secret values are never embedded.
func Validate(cfg *Config, opts ValidateOptions) error {
	var errs []string

	// Models.
	for name, m := range cfg.Models {
		if m.Model == "" {
			errs = append(errs, fmt.Sprintf("models[%q].model is required", name))
		}
		if !opts.providers()[m.Provider] {
			errs = append(errs, fmt.Sprintf("models[%q].provider %q is unsupported", name, m.Provider))
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

		if !opts.channelTypes()[c.Type] {
			errs = append(errs, fmt.Sprintf("channels[%q].type %q is unsupported", c.Name, c.Type))
		}
		if c.Host == "" {
			errs = append(errs, fmt.Sprintf("channels[%q].host is required", c.Name))
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
		if !c.Insecure && c.KnownHosts == "" {
			errs = append(errs, fmt.Sprintf("channels[%q].known_hosts is required unless insecure=true", c.Name))
		}
		if c.KnownHosts != "" {
			if _, err := os.Stat(c.KnownHosts); err != nil {
				errs = append(errs, fmt.Sprintf("channels[%q].known_hosts: %v", c.Name, err))
			}
		}
	}

	if len(cfg.Channels) == 0 {
		errs = append(errs, "at least one channel is required")
	}

	// Agents.
	for name, a := range cfg.Agents {
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
		} else if _, err := os.Stat(a.SystemPrompt); err != nil {
			errs = append(errs, fmt.Sprintf("agents[%q].system_prompt: %v", name, err))
		}
		switch a.Tools.Mode {
		case ToolModeNone, ToolModeAll:
		case ToolModeList:
			if len(a.Tools.Names) == 0 {
				errs = append(errs, fmt.Sprintf("agents[%q].tools: list must not be empty", name))
			}
			for _, tn := range a.Tools.Names {
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

	if len(errs) > 0 {
		return fmt.Errorf("config: validation failed:\n  - %s", joinErrs(errs))
	}
	return nil
}

func joinErrs(errs []string) string {
	out := errs[0]
	for _, e := range errs[1:] {
		out += "\n  - " + e
	}
	return out
}
