// Package config loads and persists BE-Code configuration
// (~/.be-code/config.json), following the BE-CLI dotdir convention.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// MCPServer configures one stdio MCP tool server.
type MCPServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// ReviewerConfig names the provider/model used for post-task review.
type ReviewerConfig struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// ProviderConfig describes one inference endpoint.
type ProviderConfig struct {
	// Type: "openai" (any OpenAI-compatible server) or "ollama"
	// (OpenAI-compatible chat plus native management).
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	// APIKeyEnv names an environment variable holding the key, so keys
	// never land in the config file. Optional for local endpoints.
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// DefaultModel used when the top-level model is unset for this provider.
	DefaultModel string `json:"default_model,omitempty"`
}

// Config is the persisted tool configuration.
type Config struct {
	DefaultProvider string                    `json:"default_provider"`
	Model           string                    `json:"model"`
	Providers       map[string]ProviderConfig `json:"providers"`

	// Agent tuning
	Temperature   float64 `json:"temperature"`
	MaxTokens     int     `json:"max_tokens"`     // per completion; 0 = backend default
	ContextTokens int     `json:"context_tokens"` // conversation budget for truncation
	MaxTurns      int     `json:"max_turns"`      // tool-loop iterations per request
	MaxRepairs    int     `json:"max_repairs"`    // verification repair attempts

	// CompatToolCalls forces prompt-embedded JSON tool calls for models
	// whose native tool-call support is unreliable. "auto" tries native
	// first and falls back per session; "always"; "never".
	CompatToolCalls string `json:"compat_tool_calls"`

	// AutoApproveShell skips the y/N prompt for shell commands. Off by default.
	AutoApproveShell bool `json:"auto_approve_shell"`

	// ApproveFileWrites shows a diff preview and asks before the agent
	// writes or edits any file. On by default.
	ApproveFileWrites bool `json:"approve_file_writes"`

	// UI selects the interface: "tui" (full-screen, default on terminals)
	// or "plain" (inline REPL, best over dumb terminals and logging).
	UI string `json:"ui"`

	// Theme: "dark" (default), "light", or "mono".
	Theme string `json:"theme"`
	// ThemeTerminalColors lets a theme recolour the terminal window itself
	// (OSC 11/10), restored on exit. Off if your terminal misbehaves.
	ThemeTerminalColors bool `json:"theme_terminal_colors"`

	// LiveIdleLimit is the number of minutes a served session may sit with
	// no clients and no run before it exits (0 = never).
	LiveIdleLimit int `json:"live_idle_limit"`

	// ShellAllow / ShellDeny are glob patterns ('*' matches anything)
	// checked against shell commands. Deny wins and never runs; an allow
	// match runs without an approval prompt.
	ShellAllow []string `json:"shell_allow"`
	ShellDeny  []string `json:"shell_deny"`

	// Hooks: commands run around tool actions. Keys: "post_write"
	// ($FILE = workspace-relative path), "pre_shell" ($COMMAND).
	Hooks map[string][]string `json:"hooks,omitempty"`

	// MCPServers are external MCP tool servers spawned per session
	// (stdio transport). Their tools appear to the agent as
	// mcp_<server>_<tool>.
	MCPServers map[string]MCPServer `json:"mcp_servers,omitempty"`

	// Reviewer is an optional second model that reviews changes after
	// verification passes (small model drafts, bigger model reviews).
	Reviewer     ReviewerConfig `json:"reviewer"`
	ReviewOnDone bool           `json:"review_on_done"`

	// WebSearch enables the web_search (and web_fetch) tools via Google
	// Programmable Search Engine. Off unless CX is set; the API key comes
	// only from the env var named in APIKeyEnv.
	WebSearch WebSearchConfig `json:"web_search"`

	// KeepAlive is how long the backend should keep the model resident after
	// each request (Ollama keep_alive; Go duration, "0" disables). Refreshed
	// after every request so idle expiry does not evict the model between
	// prompts.
	KeepAlive string `json:"keep_alive"`
	// CompactWithModel summarizes old conversation with the model when
	// over budget, instead of just dropping turns.
	CompactWithModel bool `json:"compact_with_model"`

	// RepoMap injects a symbol-level outline of the workspace into the
	// system prompt. RepoMapBudget caps its size in bytes.
	RepoMap       bool `json:"repo_map"`
	RepoMapBudget int  `json:"repo_map_budget"`

	// VerifyOnDone runs the verification pipeline automatically when the
	// agent believes it has finished a coding task.
	VerifyOnDone bool `json:"verify_on_done"`

	// IDE controls the editor bridge (VS Code extension): discovery of the
	// lock file when running inside the editor's terminal, and whether a
	// note about the active file/selection is added to each prompt.
	IDE IDEConfig `json:"ide"`
}

// IDEConfig controls the editor bridge (see internal/ide).
type IDEConfig struct {
	Enabled     bool `json:"enabled"`
	AutoContext bool `json:"auto_context"`
}

// Default returns the out-of-the-box configuration: Ollama on localhost,
// with llama.cpp and the BE AI Engine fabric pre-wired as named endpoints.
func Default() *Config {
	return &Config{
		DefaultProvider: "ollama",
		Model:           "qwen3:8b",
		Providers: map[string]ProviderConfig{
			"ollama": {
				Type:    "ollama",
				BaseURL: "http://localhost:11434/v1",
			},
			"llamacpp": {
				Type:    "openai",
				BaseURL: "http://localhost:8080/v1",
			},
			"lmstudio": {
				Type:    "openai",
				BaseURL: "http://localhost:1234/v1",
			},
			"be-ai-engine": {
				Type:      "openai",
				BaseURL:   "http://localhost:9800/v1",
				APIKeyEnv: "BE_AI_ENGINE_KEY",
			},
			"vllm": {
				Type:    "openai",
				BaseURL: "http://localhost:8000/v1",
			},
		},
		Temperature:         0.2,
		MaxTokens:           0,
		ContextTokens:       16384,
		MaxTurns:            24,
		MaxRepairs:          3,
		CompatToolCalls:     "auto",
		VerifyOnDone:        true,
		ApproveFileWrites:   true,
		UI:                  "tui",
		Theme:               "dark",
		ThemeTerminalColors: true,
		ShellAllow: []string{
			"go build*", "go test*", "go vet*", "gofmt*", "go run*",
			"npm test*", "npx tsc*", "python3 -m pytest*", "cargo check*",
			"cargo test*", "make test*", "ls*", "cat *", "git status*",
			"git diff*", "git log*",
		},
		ShellDeny: []string{
			"sudo *", "su *", "rm -rf /*", "rm -rf ~*", "mkfs*", "dd if=*",
			"shutdown*", "reboot*", ":(){*", "chmod -R 777 /*",
		},
		WebSearch: WebSearchConfig{
			Provider: "google", APIKeyEnv: "GOOGLE_PSE_API_KEY", MaxResults: 5, AllowFetch: true,
		},
		KeepAlive:        "30m",
		CompactWithModel: true,
		RepoMap:          true,
		RepoMapBudget:    6144,
		IDE:              IDEConfig{Enabled: true, AutoContext: true},
	}
}

// WebSearchConfig configures Google Programmable Search Engine access.
type WebSearchConfig struct {
	Provider   string `json:"provider"`    // "google" (Programmable Search Engine)
	CX         string `json:"cx"`          // search engine ID from programmablesearchengine.google.com
	APIKeyEnv  string `json:"api_key_env"` // env var holding the Custom Search JSON API key
	MaxResults int    `json:"max_results"` // results per query (1-10)
	AllowFetch bool   `json:"allow_fetch"` // also expose web_fetch to read result pages
}

// Enabled reports whether web search is configured.
func (w WebSearchConfig) Enabled() bool { return w.CX != "" }

// Dir returns the BE-Code dotdir (~/.be-code), creating it if needed.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(home, ".be-code")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// Path returns the config file path.
func Path() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

// Exists reports whether a config file has been written yet.
func Exists() bool {
	p, err := Path()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Load reads the config, creating a default file on first run.
func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		cfg := Default()
		if err := cfg.Save(); err != nil {
			return nil, fmt.Errorf("writing default config: %w", err)
		}
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	cfg := Default() // defaults for fields missing from older files
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p, err)
	}
	return cfg, nil
}

// Save writes the config atomically.
func (c *Config) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// APIKey resolves the key for a provider from its configured env var.
func (pc ProviderConfig) APIKey() string {
	if pc.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(pc.APIKeyEnv)
}
