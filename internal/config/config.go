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

// CoworkerConfig names one model the primary can consult mid-task.
type CoworkerConfig struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Skills   string `json:"skills,omitempty"`
	Online   bool   `json:"online,omitempty"`
}

// CoworkConfig tunes co-worker consultations.
type CoworkConfig struct {
	Auto              bool `json:"auto"`
	MaxConsultsPerRun int  `json:"max_consults_per_run"`
	ConsultTurns      int  `json:"consult_turns"`
	// ConsultTimeout bounds one consultation, in seconds. A co-worker whose
	// backend has stopped answering must not park the primary's run forever.
	ConsultTimeout int `json:"consult_timeout"`
}

// EngineConfig tunes the working-memory engine (internal/engine).
type EngineConfig struct {
	Enabled bool `json:"enabled"`
	// Budget caps the Working memory block in the system prompt, in bytes.
	Budget int `json:"budget"`
	// NotesCap caps the durable notes.md, in bytes.
	NotesCap int `json:"notes_cap"`
	// ItemCap caps one recorded tool result in the verbatim buffer, and
	// NodeCap caps that buffer for a whole node. Both in bytes; the
	// oldest items are dropped (and counted) when a node goes over.
	ItemCap int `json:"item_cap"`
	NodeCap int `json:"node_cap"`
	// Tools is "full" (task, lookup, history, show, changes) or "minimal"
	// (task and lookup only) for tight compat-mode prompts.
	Tools string `json:"tools"`
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
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"` // per completion; 0 = backend default
	// ReasoningEffort is the thinking budget asked of a reasoning model:
	// "low", "medium" (default) or "high"; "" leaves the backend's own
	// default, which for Qwen3 templates is the highest. The tool loop
	// adapts it per call: one level down when the prompt already fills
	// more than half the window, and "low" for the rest of a request once
	// reasoning has exhausted the window.
	ReasoningEffort string `json:"reasoning_effort"`
	ContextTokens   int    `json:"context_tokens"` // conversation budget for truncation
	MaxTurns        int    `json:"max_turns"`      // tool-loop iterations per request
	MaxRepairs      int    `json:"max_repairs"`    // verification repair attempts

	// CompatToolCalls forces prompt-embedded JSON tool calls for models
	// whose native tool-call support is unreliable. "auto" tries native
	// first and falls back per session; "always"; "never".
	CompatToolCalls string `json:"compat_tool_calls"`

	// AutoApproveShell skips the y/N prompt for shell commands. Off by default.
	AutoApproveShell bool `json:"auto_approve_shell"`

	// AutoApproveConsult skips the consent prompt before code is sent to an
	// online co-worker. Set only by -y; never read from the config file,
	// because "always run shell commands" is not a standing yes to shipping
	// the workspace off this machine.
	AutoApproveConsult bool `json:"-"`

	// ApproveFileWrites shows a diff preview and asks before the agent
	// writes or edits any file. On by default.
	ApproveFileWrites bool `json:"approve_file_writes"`

	// UI selects the interface: "tui" (full-screen, default on terminals)
	// or "plain" (inline REPL, best over dumb terminals and logging).
	UI string `json:"ui"`

	// Layout controls the TUI's reduced small-terminal layout: "auto"
	// (the default) engages it automatically below the compact size
	// thresholds, "compact" forces it on, "full" forces it off.
	Layout string `json:"layout"` // auto | compact | full

	// Theme: "dark" (default), "light", or "mono".
	Theme string `json:"theme"`
	// ClientThemes remembers a theme per device in a shared session, keyed
	// by the attached terminal's label with its trailing pid stripped (see
	// live.LabelKey). Set by /theme <name>; /theme default <name> changes
	// Theme instead. Never nil, so a client's write never panics.
	ClientThemes map[string]string `json:"client_themes"`
	// StallNoticeSeconds is how long the backend may stay silent before the
	// "waiting for backend" notice appears (a second notice follows at four
	// times this). 0 means the default of 45.
	StallNoticeSeconds int `json:"stall_notice_seconds,omitempty"`
	// ThemeTerminalColors lets a theme recolour the terminal window itself
	// (OSC 11/10), restored on exit. Off if your terminal misbehaves.
	ThemeTerminalColors bool `json:"theme_terminal_colors"`

	// LiveIdleLimit is the number of minutes a served session may sit with
	// no clients and no run before it exits (0 = never).
	LiveIdleLimit int `json:"live_idle_limit"`

	// HostSessions runs each interactive TUI session in a detached host
	// process the terminal attaches to, so the session survives the
	// terminal and other terminals can attach to it. On by default;
	// false (or --no-host) keeps the session in the launching process.
	HostSessions bool `json:"host_sessions"`

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

	// Coworkers are models the primary can consult mid-task (see README
	// "Co-working models"); order matters: the first is the default.
	Coworkers []CoworkerConfig `json:"coworkers"`
	// Cowork tunes consultations: Auto enables the harness's own triggers,
	// MaxConsultsPerRun caps consultations per request, ConsultTurns caps
	// a co-worker's tool loop.
	Cowork CoworkConfig `json:"cowork"`

	// Engine tunes the working-memory engine (internal/engine).
	Engine EngineConfig `json:"engine"`
	// ResumeReplay replays the saved transcript into the terminal when a
	// session is resumed, so the person sees where they left off. On by
	// default; ResumeReplayTurns caps the replay to the last N user turns
	// (0 = everything the saved history holds).
	ResumeReplay      bool `json:"resume_replay"`
	ResumeReplayTurns int  `json:"resume_replay_turns"`

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
	// Review is where a proposed file change is reviewed (see
	// internal/review): auto | editor | tui | both. auto resolves per
	// write — the editor alone while VS Code's terminal is the only one
	// attached, both places once another terminal joins.
	Review string `json:"review"`
}

// Default returns the out-of-the-box configuration: Ollama on localhost,
// with llama.cpp and the BE AI Engine fabric pre-wired as named endpoints.
func Default() *Config {
	return &Config{
		StallNoticeSeconds: 45,
		DefaultProvider:    "ollama",
		Model:              "qwen3:8b",
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
		Layout:              "auto",
		Theme:               "dark",
		ClientThemes:        map[string]string{},
		ThemeTerminalColors: true,
		HostSessions:        true,
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
		IDE:              IDEConfig{Enabled: true, AutoContext: true, Review: "auto"},
		Cowork:           CoworkConfig{Auto: true, MaxConsultsPerRun: 3, ConsultTurns: 12, ConsultTimeout: 300},
		Coworkers:        nil,
		ReasoningEffort:  "medium",
		ResumeReplay:     true,
		Engine:           EngineConfig{Enabled: true, Budget: 6144, NotesCap: 4096, ItemCap: 4096, NodeCap: 32768, Tools: "full"},
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
	if cfg.ClientThemes == nil {
		cfg.ClientThemes = map[string]string{}
	}
	// An older file, or one written by hand without the cowork block, must
	// not zero the tuning: 0 means "default" for the three counts. Auto
	// needs no such rescue — Default() sets it true and encoding/json leaves
	// a field the document does not mention alone, so it survives unless the
	// file says "auto": false.
	if cfg.Cowork.MaxConsultsPerRun == 0 {
		cfg.Cowork.MaxConsultsPerRun = 3
	}
	if cfg.Cowork.ConsultTurns == 0 {
		cfg.Cowork.ConsultTurns = 12
	}
	if cfg.Cowork.ConsultTimeout == 0 {
		cfg.Cowork.ConsultTimeout = 300
	}
	if cfg.Engine.Budget == 0 {
		cfg.Engine.Budget = 6144
	}
	if cfg.Engine.NotesCap == 0 {
		cfg.Engine.NotesCap = 4096
	}
	if cfg.Engine.ItemCap == 0 {
		cfg.Engine.ItemCap = 4096
	}
	if cfg.Engine.NodeCap == 0 {
		cfg.Engine.NodeCap = 32768
	}
	if cfg.Engine.Tools == "" {
		cfg.Engine.Tools = "full"
	}
	return cfg, nil
}

// ValidCoworkers is the configured co-workers that can actually be used,
// in order, plus one warning per entry dropped: an empty name or model, a
// provider that is not in Providers, or a name already taken.
func (c *Config) ValidCoworkers() ([]CoworkerConfig, []string) {
	var ok []CoworkerConfig
	var warns []string
	seen := map[string]bool{}
	for _, cw := range c.Coworkers {
		switch {
		case cw.Name == "":
			warns = append(warns, fmt.Sprintf("coworker %q: name is empty", cw.Name))
		case cw.Model == "":
			warns = append(warns, fmt.Sprintf("coworker %q: model is empty", cw.Name))
		case seen[cw.Name]:
			warns = append(warns, fmt.Sprintf("coworker %q: duplicate name", cw.Name))
		default:
			if _, found := c.Providers[cw.Provider]; !found {
				warns = append(warns, fmt.Sprintf("coworker %q: provider %q is not configured", cw.Name, cw.Provider))
				continue
			}
			seen[cw.Name] = true
			ok = append(ok, cw)
		}
	}
	return ok, warns
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
