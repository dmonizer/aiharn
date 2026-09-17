package config

import (
	"fmt"
	"strings"
)

// ToolSelection describes which tools an agent may call. It accepts either a
// TOML string ("none", "all", or comma-separated tool names) or an array.
type ToolSelection struct {
	Mode  string   // "none" | "all" | "list"
	Names []string // populated when Mode == "list"
}

// UnmarshalTOML implements toml.Unmarshaler.
func (t *ToolSelection) UnmarshalTOML(v interface{}) error {
	switch val := v.(type) {
	case string:
		val = strings.TrimSpace(val)
		switch strings.ToLower(val) {
		case ToolModeNone, ToolModeAll:
			t.Mode = strings.ToLower(val)
			t.Names = nil
			return nil
		}
		if val == "" {
			return fmt.Errorf("tools must be %q, %q, or a list of names", ToolModeNone, ToolModeAll)
		}
		parts := strings.Split(val, ",")
		names := make([]string, 0, len(parts))
		for _, part := range parts {
			name := strings.TrimSpace(part)
			if name == "" {
				return fmt.Errorf("tools contains an empty name in %q", val)
			}
			names = append(names, name)
		}
		return t.setNames(names)
	case []string:
		return t.setNames(val)
	case []interface{}:
		names := make([]string, 0, len(val))
		for _, item := range val {
			s, ok := item.(string)
			if !ok {
				return fmt.Errorf("tools list entries must be strings, got %T", item)
			}
			names = append(names, s)
		}
		return t.setNames(names)
	default:
		return fmt.Errorf("tools must be %q, %q, or a list of names, got %T", ToolModeNone, ToolModeAll, v)
	}
}

func (t *ToolSelection) setNames(names []string) error {
	if len(names) == 1 {
		switch strings.ToLower(strings.TrimSpace(names[0])) {
		case ToolModeNone, ToolModeAll:
			t.Mode = strings.ToLower(strings.TrimSpace(names[0]))
			t.Names = nil
			return nil
		}
	}
	for _, name := range names {
		if strings.EqualFold(strings.TrimSpace(name), ToolModeAll) || strings.EqualFold(strings.TrimSpace(name), ToolModeNone) {
			return fmt.Errorf("tools %q must appear alone", name)
		}
	}
	t.Mode = ToolModeList
	t.Names = append([]string(nil), names...)
	return nil
}
