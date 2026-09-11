package config

import "fmt"

// ToolSelection describes which tools an agent may call. It accepts either a
// TOML string ("none" or "all") or an array of tool names.
type ToolSelection struct {
	Mode  string   // "none" | "all" | "list"
	Names []string // populated when Mode == "list"
}

// UnmarshalTOML implements toml.Unmarshaler.
func (t *ToolSelection) UnmarshalTOML(v interface{}) error {
	switch val := v.(type) {
	case string:
		if val != ToolModeNone && val != ToolModeAll {
			return fmt.Errorf("tools must be %q, %q, or a list of names, got %q", ToolModeNone, ToolModeAll, val)
		}
		t.Mode = val
		return nil
	case []string:
		t.Mode = ToolModeList
		t.Names = append([]string(nil), val...)
		return nil
	case []interface{}:
		names := make([]string, 0, len(val))
		for _, item := range val {
			s, ok := item.(string)
			if !ok {
				return fmt.Errorf("tools list entries must be strings, got %T", item)
			}
			names = append(names, s)
		}
		t.Mode = ToolModeList
		t.Names = names
		return nil
	default:
		return fmt.Errorf("tools must be %q, %q, or a list of names, got %T", ToolModeNone, ToolModeAll, v)
	}
}
