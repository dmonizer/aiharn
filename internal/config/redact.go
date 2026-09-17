package config

// redactedValue replaces a non-empty secret with a fixed marker.
func redactedValue(s string) string {
	if s == "" {
		return ""
	}
	return "***"
}

// Redacted returns a deep copy of cfg with secret-bearing fields (API keys,
// passwords) masked. Use it before logging or formatting a Config.
func (c *Config) Redacted() *Config {
	out := *c

	out.Models = make(map[string]ModelConfig, len(c.Models))
	for name, m := range c.Models {
		m.APIKey = redactedValue(m.APIKey)
		out.Models[name] = m
	}

	out.Channels = make([]ChannelConfig, len(c.Channels))
	for i, ch := range c.Channels {
		ch.Auth.Password = redactedValue(ch.Auth.Password)
		if ch.KeepAlive != nil {
			keepAlive := *ch.KeepAlive
			ch.KeepAlive = &keepAlive
		}
		out.Channels[i] = ch
	}

	out.Agents = make(map[string]AgentConfig, len(c.Agents))
	for name, a := range c.Agents {
		if a.WorkingDir != nil {
			workingDir := *a.WorkingDir
			a.WorkingDir = &workingDir
		}
		out.Agents[name] = a
	}

	out.API.AllowOrigins = append([]string(nil), c.API.AllowOrigins...)

	return &out
}
