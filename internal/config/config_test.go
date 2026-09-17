package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// setup writes files under a temp dir and returns the dir. Prompt files and a
// known_hosts file are created so validation can pass.
func setup(t *testing.T, configBody string) (configPath string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "prompts", "planner.md"), "# planner\n")
	writeFile(t, filepath.Join(dir, "known_hosts"), "fake-known-hosts\n")
	writeFile(t, filepath.Join(dir, "id_ed25519"), "fake-private-key\n")
	configPath = filepath.Join(dir, "config.toml")
	writeFile(t, configPath, configBody)
	return configPath
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const validConfig = `
[models.opus_via_openrouter]
provider = "openai_responses"
base_url = "https://openrouter.ai/api/v1"
api_key = "${TEST_API_KEY}"
model = "anthropic/claude-opus-4"

[[channels]]
name = "devbox"
type = "ssh"
host = "10.0.0.5"
user = "ubuntu"
auth = { key_file = "id_ed25519" }
known_hosts = "known_hosts"
keep_alive = true
working_dir = "/home/ubuntu/work/${agent.type}-${agent.id}"

[agents.planner]
model = "opus_via_openrouter"
system_prompt = "prompts/planner.md"
channel = "devbox"
tools = "all"
allow_subagents = true

[limits]
max_agent_depth = 3
command_timeout = "45s"

[approval]
mode = "ask"
`

func TestLoadValid(t *testing.T) {
	t.Setenv("TEST_API_KEY", "sk-test-123")
	path := setup(t, validConfig)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := home; cfg.AiharnHome != want {
		t.Fatalf("AiharnHome = %q, want default %q", cfg.AiharnHome, want)
	}

	// Env interpolation.
	if got := cfg.Models["opus_via_openrouter"].APIKey; got != "sk-test-123" {
		t.Errorf("APIKey = %q, want interpolated value", got)
	}

	// Channel defaults and parsing.
	ch := cfg.Channels[0]
	if ch.Port != 22 {
		t.Errorf("Port = %d, want 22", ch.Port)
	}
	if !ch.KeepAliveEnabled() {
		t.Errorf("KeepAlive should default/enable true")
	}
	if ch.DefaultShell != "/bin/bash" {
		t.Errorf("DefaultShell = %q", ch.DefaultShell)
	}
	if ch.WorkingDir.Raw != "/home/ubuntu/work/${agent.type}-${agent.id}" {
		t.Errorf("WorkingDir = %q", ch.WorkingDir.Raw)
	}

	// Local path resolution is absolute and relative to config dir.
	dir := filepath.Dir(path)
	if got := cfg.Agents["planner"].SystemPrompt; got != filepath.Join(dir, "prompts", "planner.md") {
		t.Errorf("SystemPrompt = %q, want resolved under config dir", got)
	}
	if got := cfg.Channels[0].Auth.KeyFile; got != filepath.Join(dir, "id_ed25519") {
		t.Errorf("KeyFile = %q, want resolved under config dir", got)
	}

	// Limits: explicit override + defaults for the rest.
	if cfg.Limits.MaxAgentDepth != 3 {
		t.Errorf("MaxAgentDepth = %d, want 3", cfg.Limits.MaxAgentDepth)
	}
	if cfg.Limits.CommandTimeout.Std() != 45*time.Second {
		t.Errorf("CommandTimeout = %v, want 45s", cfg.Limits.CommandTimeout.Std())
	}
	if cfg.Limits.MaxOpenAgents != 8 {
		t.Errorf("MaxOpenAgents = %d, want default 8", cfg.Limits.MaxOpenAgents)
	}
	if cfg.Approval.Mode != ApprovalModeAsk {
		t.Errorf("Approval.Mode = %q", cfg.Approval.Mode)
	}

	if err := Validate(cfg, ValidateOptions{KnownTools: map[string]bool{"execute_command": true}}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestLoadAiharnHomeOverride(t *testing.T) {
	t.Setenv("TEST_API_KEY", "x")
	for _, tc := range []struct {
		name, setting string
		want          func(string) string
	}{
		{"relative", `aiharn_home = "state"`, func(path string) string { return filepath.Join(filepath.Dir(path), "state") }},
		{"absolute from environment", `aiharn_home = "${AIHARN_TEST_HOME}"`, func(string) string { return filepath.Join(t.TempDir(), "absolute") }},
		{"tilde", `aiharn_home = "~/.aiharn-test"`, func(string) string { home, _ := os.UserHomeDir(); return filepath.Join(home, ".aiharn-test") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.setting + "\n" + validConfig
			path := setup(t, body)
			want := tc.want(path)
			if tc.name == "absolute from environment" {
				t.Setenv("AIHARN_TEST_HOME", want)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.AiharnHome != want {
				t.Fatalf("AiharnHome = %q, want %q", cfg.AiharnHome, want)
			}
		})
	}
}

func TestLoadAiharnHomeMissingEnvironmentVariable(t *testing.T) {
	t.Setenv("TEST_API_KEY", "x")
	path := setup(t, `aiharn_home = "${AIHARN_TEST_MISSING}"`+"\n"+validConfig)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "aiharn_home") || !strings.Contains(err.Error(), "AIHARN_TEST_MISSING") {
		t.Fatalf("Load error = %v", err)
	}
}

func TestLoadAgentDescription(t *testing.T) {
	t.Setenv("TEST_API_KEY", "x")
	body := strings.Replace(validConfig, "[agents.planner]\n", "[agents.planner]\ndescription = \"Plans and delegates scoped work\"\n", 1)
	cfg, err := Load(setup(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Agents["planner"].Description; got != "Plans and delegates scoped work" {
		t.Fatalf("description = %q", got)
	}
}

func TestLoadAndValidateReasoningOptions(t *testing.T) {
	t.Setenv("TEST_API_KEY", "x")
	body := strings.Replace(validConfig, `model = "anthropic/claude-opus-4"`, `model = "anthropic/claude-opus-4"
reasoning_effort = "high"
reasoning_summary = "auto"`, 1)
	cfg, err := Load(setup(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	model := cfg.Models["opus_via_openrouter"]
	if model.ReasoningEffort != "high" || model.ReasoningSummary != "auto" {
		t.Fatalf("reasoning options = %q/%q, want high/auto", model.ReasoningEffort, model.ReasoningSummary)
	}
	if err := Validate(cfg, ValidateOptions{KnownTools: map[string]bool{"execute_command": true}}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateReasoningOptions(t *testing.T) {
	t.Setenv("TEST_API_KEY", "x")
	tests := []struct {
		name    string
		mutate  func(*ModelConfig)
		wantErr string
	}{
		{"invalid effort", func(m *ModelConfig) { m.ReasoningEffort = "extreme" }, "reasoning_effort"},
		{"invalid summary", func(m *ModelConfig) { m.ReasoningSummary = "full" }, "reasoning_summary"},
		{"summary needs responses", func(m *ModelConfig) {
			m.Provider = ProviderOpenAIChatCompletions
			m.ReasoningSummary = "auto"
		}, "requires provider"},
		{"summary incompatible with none", func(m *ModelConfig) {
			m.ReasoningEffort = "none"
			m.ReasoningSummary = "auto"
		}, "cannot be requested"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(setup(t, validConfig))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			model := cfg.Models["opus_via_openrouter"]
			tc.mutate(&model)
			cfg.Models["opus_via_openrouter"] = model
			err = Validate(cfg, ValidateOptions{KnownTools: map[string]bool{"execute_command": true}})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadAPIConfig(t *testing.T) {
	t.Setenv("TEST_API_KEY", "model-secret")
	t.Setenv("TEST_API_LISTEN", "127.0.0.1:7331")
	t.Setenv("TEST_API_AUTH_FILE", "users.txt")
	t.Setenv("TEST_API_ORIGIN", "https://console.example")
	path := setup(t, validConfig+`

[api]
listen = "${TEST_API_LISTEN}"
auth_file = "${TEST_API_AUTH_FILE}"
allow_origins = ["${TEST_API_ORIGIN}", "https://backup.example"]
only = true
max_sessions = 4
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.Listen != "127.0.0.1:7331" {
		t.Fatalf("API.Listen = %q", cfg.API.Listen)
	}
	if cfg.API.AuthFile != filepath.Join(filepath.Dir(path), "users.txt") {
		t.Fatalf("API.AuthFile = %q, want path relative to config", cfg.API.AuthFile)
	}
	if len(cfg.API.AllowOrigins) != 2 ||
		cfg.API.AllowOrigins[0] != "https://console.example" {
		t.Fatalf("API.AllowOrigins = %#v", cfg.API.AllowOrigins)
	}
	if !cfg.API.Only {
		t.Fatal("API.Only = false, want true")
	}
	if cfg.API.MaxSessions != 4 {
		t.Fatalf("API.MaxSessions = %d, want 4", cfg.API.MaxSessions)
	}
}

func TestAPIWithoutMaxSessionsKeepsZero(t *testing.T) {
	t.Setenv("TEST_API_KEY", "model-secret")
	cfg, err := Load(setup(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Zero is the "use the built-in default" sentinel; the session layer, not
	// the config layer, resolves it.
	if cfg.API.MaxSessions != 0 {
		t.Fatalf("API.MaxSessions = %d, want 0", cfg.API.MaxSessions)
	}
}

func TestLoadMissingEnvVar(t *testing.T) {
	path := setup(t, validConfig)
	// TEST_API_KEY intentionally unset.
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "missing environment variable") {
		t.Fatalf("Load error = %v, want missing environment variable", err)
	}
	if strings.Contains(err.Error(), "sk-test") {
		t.Fatalf("error leaked a secret: %v", err)
	}
}

func TestLoadUnknownField(t *testing.T) {
	body := validConfig + "\nbogus_field = true\n"
	path := setup(t, body)
	t.Setenv("TEST_API_KEY", "x")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Load error = %v, want unknown field", err)
	}
}

func TestLoadInvalidDuration(t *testing.T) {
	body := strings.Replace(validConfig, `command_timeout = "45s"`, `command_timeout = "not-a-duration"`, 1)
	path := setup(t, body)
	t.Setenv("TEST_API_KEY", "x")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("Load error = %v, want invalid duration", err)
	}
}

func TestThinkingAndResponseTimeouts(t *testing.T) {
	t.Setenv("TEST_API_KEY", "x")
	body := strings.Replace(validConfig, `command_timeout = "45s"`, `command_timeout = "45s"
thinking_timeout = "3m"
request_timeout = "75s"`, 1)
	cfg, err := Load(setup(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.ThinkingTimeout.Std() != 3*time.Minute || cfg.Limits.RequestTimeout.Std() != 75*time.Second {
		t.Fatalf("timeouts = thinking %s, response %s", cfg.Limits.ThinkingTimeout.Std(), cfg.Limits.RequestTimeout.Std())
	}
}

func TestDurationIntegerOverflow(t *testing.T) {
	var d Duration
	if err := d.UnmarshalTOML(int64(^uint64(0) >> 1)); err == nil {
		t.Fatal("expected duration overflow error")
	}
}

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if got := expandTilde("~/key"); got != filepath.Join(home, "key") {
		t.Fatalf("expandTilde = %q", got)
	}
}

func TestLoadInvalidTools(t *testing.T) {
	body := strings.Replace(validConfig, `tools = "all"`, `tools = "execute_command,,spawn_subagent"`, 1)
	path := setup(t, body)
	t.Setenv("TEST_API_KEY", "x")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "empty name") {
		t.Fatalf("Load error = %v, want tools error", err)
	}
}

func TestValidateDuplicateChannel(t *testing.T) {
	body := validConfig + `
[[channels]]
name = "devbox"
type = "ssh"
host = "10.0.0.6"
user = "ubuntu"
auth = { key_file = "~/.ssh/id_ed25519" }
known_hosts = "known_hosts"
`
	path := setup(t, body)
	t.Setenv("TEST_API_KEY", "x")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = Validate(cfg, ValidateOptions{})
	if err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Fatalf("Validate error = %v, want duplicate channel", err)
	}
}

func TestValidateReferences(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{"unknown model", func(s string) string {
			return strings.Replace(s, `model = "opus_via_openrouter"`, `model = "nope"`, 1)
		}, "is not defined"},
		{"unknown channel", func(s string) string {
			return strings.Replace(s, `channel = "devbox"`, `channel = "nope"`, 1)
		}, "is not defined"},
		{"unknown tool", func(s string) string {
			return strings.Replace(s, `tools = "all"`, `tools = "execute_command,mystery_tool"`, 1)
		}, "unknown tool"},
		{"unsupported provider", func(s string) string {
			return strings.Replace(s, `provider = "openai_responses"`, `provider = "anthropic"`, 1)
		}, "unsupported"},
		{"invalid approval", func(s string) string {
			return strings.Replace(s, `mode = "ask"`, `mode = "sometimes"`, 1)
		}, "approval.mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := setup(t, tc.mutate(validConfig))
			t.Setenv("TEST_API_KEY", "x")
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			err = Validate(cfg, ValidateOptions{KnownTools: map[string]bool{"execute_command": true}})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateAPIConfig(t *testing.T) {
	tests := []struct {
		name    string
		api     APIConfig
		wantErr string
	}{
		{
			name: "loopback",
			api: APIConfig{
				Listen:       "127.0.0.1:7331",
				AuthFile:     "users.txt",
				AllowOrigins: []string{"https://console.example"},
			},
		},
		{
			name:    "only without listen",
			api:     APIConfig{Only: true},
			wantErr: "api.only requires api.listen",
		},
		{
			name:    "invalid listen",
			api:     APIConfig{Listen: "not-an-address"},
			wantErr: "api.listen",
		},
		{
			name:    "remote without auth file",
			api:     APIConfig{Listen: "0.0.0.0:7331"},
			wantErr: "api.auth_file is required",
		},
		{
			name:    "negative max sessions",
			api:     APIConfig{Listen: "127.0.0.1:7331", AuthFile: "users.txt", MaxSessions: -1},
			wantErr: "api.max_sessions must not be negative",
		},
		{
			name: "max sessions accepted",
			api:  APIConfig{Listen: "127.0.0.1:7331", AuthFile: "users.txt", MaxSessions: 4},
		},
		{
			name:    "invalid origin",
			api:     APIConfig{Listen: "127.0.0.1:7331", AuthFile: "users.txt", AllowOrigins: []string{"https://console.example/path"}},
			wantErr: "api.allow_origins[0]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_API_KEY", "x")
			cfg, err := Load(setup(t, validConfig))
			if err != nil {
				t.Fatal(err)
			}
			cfg.API = tc.api
			err = Validate(cfg, ValidateOptions{})
			if tc.wantErr == "" && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("Validate error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateModelEndpointAndCredentials(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{"missing base URL", func(s string) string {
			return strings.Replace(s, `base_url = "https://openrouter.ai/api/v1"`, `base_url = ""`, 1)
		}, "base_url is required"},
		{"missing API key", func(s string) string {
			return strings.Replace(s, `api_key = "${TEST_API_KEY}"`, `api_key = ""`, 1)
		}, "api_key is required"},
		{"credentials in URL", func(s string) string {
			return strings.Replace(s, `https://openrouter.ai/api/v1`, `https://secret@openrouter.ai/api/v1`, 1)
		}, "without credentials"},
		{"plaintext remote URL", func(s string) string {
			return strings.Replace(s, `https://openrouter.ai/api/v1`, `http://openrouter.ai/api/v1`, 1)
		}, "must use HTTPS"},
		{"responses suffix", func(s string) string {
			return strings.Replace(s, `https://openrouter.ai/api/v1`, `https://openrouter.ai/api/v1/responses`, 1)
		}, "appends /responses"},
		{"chat suffix", func(s string) string {
			s = strings.Replace(s, `provider = "openai_responses"`, `provider = "openai_chat_completions"`, 1)
			return strings.Replace(s, `https://openrouter.ai/api/v1`, `https://example.com/chat/completions`, 1)
		}, "appends /chat/completions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := setup(t, tc.mutate(validConfig))
			t.Setenv("TEST_API_KEY", "x")
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := Validate(cfg, ValidateOptions{}); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateNegativeLimits(t *testing.T) {
	body := strings.Replace(validConfig, "max_agent_depth = 3", "max_agent_depth = -1", 1)
	path := setup(t, body)
	t.Setenv("TEST_API_KEY", "x")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = Validate(cfg, ValidateOptions{})
	if err == nil || !strings.Contains(err.Error(), "max_agent_depth") {
		t.Fatalf("Validate error = %v, want negative limit", err)
	}
}

func TestValidateAuthMutuallyExclusive(t *testing.T) {
	body := strings.Replace(validConfig, `auth = { key_file = "id_ed25519" }`,
		`auth = { key_file = "id_ed25519", password = "hunter2" }`, 1)
	path := setup(t, body)
	t.Setenv("TEST_API_KEY", "x")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = Validate(cfg, ValidateOptions{})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("Validate error = %v, want mutually exclusive auth", err)
	}
}

func TestValidateKnownHostsRequired(t *testing.T) {
	// Remove known_hosts and don't set insecure.
	body := strings.Replace(validConfig, "\nknown_hosts = \"known_hosts\"\n", "\n", 1)
	path := setup(t, body)
	t.Setenv("TEST_API_KEY", "x")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = Validate(cfg, ValidateOptions{})
	if err == nil || !strings.Contains(err.Error(), "known_hosts is required") {
		t.Fatalf("Validate error = %v, want known_hosts required", err)
	}
}

func TestIsSSHConfigAlias(t *testing.T) {
	if !(ChannelConfig{Host: "foo"}).IsSSHConfigAlias() {
		t.Fatal("host-only channel should be alias")
	}
	for name, c := range map[string]ChannelConfig{
		"user":        {Host: "foo", User: "u"},
		"key_file":    {Host: "foo", Auth: AuthConfig{KeyFile: "k"}},
		"password":    {Host: "foo", Auth: AuthConfig{Password: "p"}},
		"known_hosts": {Host: "foo", KnownHosts: "kh"},
		"insecure":    {Host: "foo", Insecure: true},
	} {
		if c.IsSSHConfigAlias() {
			t.Fatalf("%s: should not be alias", name)
		}
	}
}

func TestValidateSSHConfigAlias(t *testing.T) {
	body := `
[models.m]
provider = "openai_responses"
base_url = "https://example.com"
api_key = "${TEST_API_KEY}"
model = "m"

[[channels]]
name = "devbox"
type = "ssh"
host = "myalias"

[agents.p]
model = "m"
system_prompt = "prompts/planner.md"
channel = "devbox"
tools = "none"

[approval]
mode = "ask"
`
	path := setup(t, body)
	t.Setenv("TEST_API_KEY", "x")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Channels[0].IsSSHConfigAlias() {
		t.Fatal("channel should be in alias mode")
	}
	if err := Validate(cfg, ValidateOptions{}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateSSHConfigAliasPartialStillRequiresAuth(t *testing.T) {
	// A host + user (but no auth) is not a pure alias; auth is still required.
	body := `
[models.m]
provider = "openai_responses"
base_url = "https://example.com"
api_key = "${TEST_API_KEY}"
model = "m"

[[channels]]
name = "devbox"
type = "ssh"
host = "myalias"
user = "ubuntu"

[agents.p]
model = "m"
system_prompt = "prompts/planner.md"
channel = "devbox"
tools = "none"

[approval]
mode = "ask"
`
	path := setup(t, body)
	t.Setenv("TEST_API_KEY", "x")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = Validate(cfg, ValidateOptions{})
	if err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("Validate error = %v, want auth required", err)
	}
}

func TestRedacted(t *testing.T) {
	t.Setenv("TEST_API_KEY", "supersecret")
	path := setup(t, validConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wd, _ := ParseWorkingDir("/original")
	agentCfg := cfg.Agents["planner"]
	agentCfg.WorkingDir = &wd
	cfg.Agents["planner"] = agentCfg
	cfg.API = APIConfig{
		AuthFile: "users.txt", AllowOrigins: []string{"https://console.example"},
	}
	r := cfg.Redacted()
	if got := r.Models["opus_via_openrouter"].APIKey; got == "supersecret" || got == "" {
		t.Errorf("Redacted APIKey = %q, want masked marker", got)
	}
	if strings.Contains(strings.Join([]string{
		r.Models["opus_via_openrouter"].APIKey,
	}, ""), "secret") {
		t.Errorf("Redacted config still contains secret")
	}
	*r.Channels[0].KeepAlive = false
	r.Agents["planner"].WorkingDir.Raw = "/changed"
	r.API.AllowOrigins[0] = "https://changed.example"
	if !*cfg.Channels[0].KeepAlive || cfg.Agents["planner"].WorkingDir.Raw != "/original" ||
		cfg.API.AllowOrigins[0] != "https://console.example" {
		t.Fatal("Redacted returned shared mutable pointers")
	}
}

func TestParseWorkingDir(t *testing.T) {
	ok := []string{
		"/home/ubuntu/work/${agent.type}-${agent.id}",
		"~/work/planner",
		"$HOME/scratch",
		"/fixed/path",
	}
	for _, s := range ok {
		if _, err := ParseWorkingDir(s); err != nil {
			t.Errorf("ParseWorkingDir(%q) unexpected error: %v", s, err)
		}
	}
	bad := []string{
		"${HOME}/x",   // ${HOME} is not a runtime placeholder; use ~ or $HOME
		"${user}/x",   // unknown
		"/x/${agent}", // unknown
		"   ",
	}
	for _, s := range bad {
		if _, err := ParseWorkingDir(s); err == nil {
			t.Errorf("ParseWorkingDir(%q) expected error, got nil", s)
		}
	}
}

func TestToolSelectionUnmarshal(t *testing.T) {
	// Exercises selection.go via Load.
	body := strings.Replace(validConfig, `tools = "all"`, `tools = ["execute_command", "spawn_subagent"]`, 1)
	path := setup(t, body)
	t.Setenv("TEST_API_KEY", "x")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sel := cfg.Agents["planner"].Tools
	if sel.Mode != ToolModeList || len(sel.Names) != 2 {
		t.Errorf("ToolSelection = %+v, want list of 2", sel)
	}
}

func TestToolSelectionStringAndArray(t *testing.T) {
	for _, tc := range []struct {
		value, mode string
		names       []string
	}{
		{`"ALL"`, ToolModeAll, nil},
		{`"NONE"`, ToolModeNone, nil},
		{`["ALL"]`, ToolModeAll, nil},
		{`["NONE"]`, ToolModeNone, nil},
		{`"execute_command, spawn_subagent"`, ToolModeList, []string{"execute_command", "spawn_subagent"}},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("TEST_API_KEY", "x")
			body := strings.Replace(validConfig, `tools = "all"`, `tools = `+tc.value, 1)
			cfg, err := Load(setup(t, body))
			if err != nil {
				t.Fatal(err)
			}
			sel := cfg.Agents["planner"].Tools
			if sel.Mode != tc.mode || !slices.Equal(sel.Names, tc.names) {
				t.Fatalf("tools = %+v", sel)
			}
			if err := Validate(cfg, ValidateOptions{KnownTools: map[string]bool{"execute_command": true, "spawn_subagent": true}}); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}
