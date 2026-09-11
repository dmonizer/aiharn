package config

import (
	"fmt"
	"regexp"
	"strings"
)

// WorkingDirTemplate is a remote working-directory template. Phase 1 only
// parses and validates it; runtime substitution of ${agent.type}/${agent.id}
// and remote-home ($HOME/~) resolution happen in the execution package.
type WorkingDirTemplate struct {
	Raw string
}

// UnmarshalTOML implements toml.Unmarshaler.
func (t *WorkingDirTemplate) UnmarshalTOML(v interface{}) error {
	s, ok := v.(string)
	if !ok {
		return fmt.Errorf("working_dir must be a string, got %T", v)
	}
	parsed, err := ParseWorkingDir(s)
	if err != nil {
		return err
	}
	*t = parsed
	return nil
}

// allowed placeholders for ${...}: runtime agent values. $HOME and ~ are
// separate remote-home tokens handled below, not ${...} placeholders.
var placeholderPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// remoteHomeTokens are the literal spellings that resolve on the remote host.
const (
	remoteHomeTilde = "~"
	remoteHomeVar   = "$HOME"
)

// ParseWorkingDir validates a working-directory template and returns it.
// Allowed placeholders: ${agent.type}, ${agent.id}. Allowed remote-home tokens:
// "~" and "$HOME" (resolved remotely, not here). Any other ${...} is rejected.
func ParseWorkingDir(s string) (WorkingDirTemplate, error) {
	matches := placeholderPattern.FindAllStringSubmatch(s, -1)
	for _, m := range matches {
		name := m[1]
		switch name {
		case "agent.type", "agent.id":
			// ok
		default:
			return WorkingDirTemplate{}, fmt.Errorf(
				"working_dir: unknown placeholder ${%s} (allowed: ${agent.type}, ${agent.id}, ~, $HOME)", name)
		}
	}
	if strings.TrimSpace(s) == "" {
		return WorkingDirTemplate{}, fmt.Errorf("working_dir must not be empty")
	}
	return WorkingDirTemplate{Raw: s}, nil
}

// UsesRuntimePlaceholder reports whether the template references a value only
// known at runtime (agent type or id).
func (t WorkingDirTemplate) UsesRuntimePlaceholder() bool {
	return placeholderPattern.MatchString(t.Raw)
}
