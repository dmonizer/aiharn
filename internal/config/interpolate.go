package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// envPattern matches ${VAR_NAME} with a shell-like variable name.
var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// interpolateEnv replaces ${VAR} references using lookup. Missing variables are
// collected and reported together as an error; the reference is left in place
// otherwise so the caller can report it unambiguously.
func interpolateEnv(s string, lookup func(string) (string, bool)) (string, error) {
	if s == "" {
		return s, nil
	}
	var missing []string
	out := envPattern.ReplaceAllStringFunc(s, func(m string) string {
		name := m[2 : len(m)-1]
		v, ok := lookup(name)
		if !ok {
			missing = append(missing, name)
			return m
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("missing environment variable(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// interpolate walks the connection/secret fields of cfg and replaces ${VAR}
// references from the environment. Missing variables are fatal. The working-dir
// templates are NOT interpolated here (their ${...} are runtime placeholders).
func interpolate(cfg *Config) error {
	lookup := func(name string) (string, bool) { return os.LookupEnv(name) }

	v, err := interpolateEnv(cfg.AiharnHome, lookup)
	if err != nil {
		return fmt.Errorf("aiharn_home: %w", err)
	}
	cfg.AiharnHome = v

	if err := interpolateShortcuts(cfg, lookup); err != nil {
		return err
	}
	if err := interpolateModels(cfg, lookup); err != nil {
		return err
	}
	if err := interpolateChannels(cfg, lookup); err != nil {
		return err
	}
	if err := interpolateAPI(cfg, lookup); err != nil {
		return err
	}
	return nil
}

func interpolateShortcuts(cfg *Config, lookup func(string) (string, bool)) error {
	fields := []struct {
		name  string
		value *string
	}{
		{"toggle_actions", &cfg.Shortcuts.ToggleActions},
		{"toggle_thinking", &cfg.Shortcuts.ToggleThinking},
		{"toggle_loop", &cfg.Shortcuts.ToggleLoop},
		{"cycle_shell", &cfg.Shortcuts.CycleShell},
		{"grow_input", &cfg.Shortcuts.GrowInput},
		{"shrink_input", &cfg.Shortcuts.ShrinkInput},
		{"insert_newline", &cfg.Shortcuts.InsertNewline},
	}
	for _, field := range fields {
		v, err := interpolateEnv(*field.value, lookup)
		if err != nil {
			return fmt.Errorf("shortcuts.%s: %w", field.name, err)
		}
		*field.value = v
	}
	return nil
}

func interpolateModels(cfg *Config, lookup func(string) (string, bool)) error {
	for name, m := range cfg.Models {
		v, err := interpolateEnv(m.BaseURL, lookup)
		if err != nil {
			return fmt.Errorf("models[%q].base_url: %w", name, err)
		}
		m.BaseURL = v
		v, err = interpolateEnv(m.APIKey, lookup)
		if err != nil {
			return fmt.Errorf("models[%q].api_key: %w", name, err)
		}
		m.APIKey = v
		cfg.Models[name] = m
	}
	return nil
}

func interpolateChannels(cfg *Config, lookup func(string) (string, bool)) error {
	for i := range cfg.Channels {
		c := &cfg.Channels[i]
		fields := []struct {
			name  string
			value *string
		}{
			{"host", &c.Host},
			{"user", &c.User},
			{"auth.key_file", &c.Auth.KeyFile},
			{"auth.password", &c.Auth.Password},
			{"known_hosts", &c.KnownHosts},
		}
		for _, f := range fields {
			v, err := interpolateEnv(*f.value, lookup)
			if err != nil {
				return fmt.Errorf("channels[%q].%s: %w", c.Name, f.name, err)
			}
			*f.value = v
		}
	}
	return nil
}

func interpolateAPI(cfg *Config, lookup func(string) (string, bool)) error {
	v, err := interpolateEnv(cfg.API.Listen, lookup)
	if err != nil {
		return fmt.Errorf("api.listen: %w", err)
	}
	cfg.API.Listen = v

	v, err = interpolateEnv(cfg.API.AuthFile, lookup)
	if err != nil {
		return fmt.Errorf("api.auth_file: %w", err)
	}
	cfg.API.AuthFile = v

	for i, origin := range cfg.API.AllowOrigins {
		v, err := interpolateEnv(origin, lookup)
		if err != nil {
			return fmt.Errorf("api.allow_origins[%d]: %w", i, err)
		}
		cfg.API.AllowOrigins[i] = v
	}
	return nil
}
