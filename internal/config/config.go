// Package config loads Kestrel's YAML configuration and environment overrides.
//
// Precedence is: built-in defaults < YAML file < environment variables. String
// values in the YAML file may reference environment variables using ${NAME};
// an unset variable expands to an empty string so validation can point at the
// missing value instead of silently using a literal.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from strings such as "30s".
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Tag == "!!int" {
			var secs int64
			if err := n.Decode(&secs); err == nil {
				*d = Duration(time.Duration(secs) * time.Second)
				return nil
			}
		}
		v, err := time.ParseDuration(n.Value)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", n.Value, err)
		}
		*d = Duration(v)
		return nil
	}
	return fmt.Errorf("invalid duration node")
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Config is the root configuration document.
type Config struct {
	DataDir        string     `yaml:"data_dir"`
	Workspace      string     `yaml:"workspace"`
	Persona        string     `yaml:"persona"`
	SystemPrompt   string     `yaml:"system_prompt"`
	LogLevel       string     `yaml:"log_level"`
	LogFormat      string     `yaml:"log_format"`
	HealthAddr     string     `yaml:"health_addr"`
	Pprof          bool       `yaml:"pprof"`
	PprofAddr      string     `yaml:"pprof_addr"`
	Shutdown       Duration   `yaml:"shutdown_timeout"`
	Proxy          string     `yaml:"proxy"`
	Active         string     `yaml:"active"`
	Fallback       []string   `yaml:"fallback"`
	HTTP           HTTP       `yaml:"http"`
	Telegram       Telegram   `yaml:"telegram"`
	Agent          Agent      `yaml:"agent"`
	Tools          Tools      `yaml:"tools"`
	Store          Store      `yaml:"store"`
	Cron           Cron       `yaml:"cron"`
	Memory         Memory     `yaml:"memory"`
	Skills         Skills     `yaml:"skills"`
	Voice          Voice      `yaml:"voice"`
	Providers      []Provider `yaml:"providers"`
	MCP            []MCP      `yaml:"mcp"`
	ConfigPathUsed string     `yaml:"-"`

	// ConfigPath is explicit input; not serialized.
	ConfigPath string `yaml:"-"`
}

// HTTP holds shared HTTP client tuning.
type HTTP struct {
	UserAgent          string   `yaml:"user_agent"`
	MaxIdleConns       int      `yaml:"max_idle_conns"`
	IdleConnTimeout    Duration `yaml:"idle_conn_timeout"`
	TLSHandshakeTimout Duration `yaml:"tls_handshake_timeout"`
	ResponseTimeout    Duration `yaml:"response_timeout"`
}

// Telegram configures the primary gateway.
type Telegram struct {
	Token         string  `yaml:"token"`
	AllowedUsers  []int64 `yaml:"allowed_users"`
	Proxy         string  `yaml:"proxy"`
	Mode          string  `yaml:"mode"` // polling | webhook
	WebhookURL    string  `yaml:"webhook_url"`
	WebhookAddr   string  `yaml:"webhook_addr"`
	WebhookSecret string  `yaml:"webhook_secret"`
	Interrupt     string  `yaml:"interrupt"` // queue | interrupt
	Progress      *bool   `yaml:"progress"`
	LogActions    *bool   `yaml:"log_actions"`
	ShowReasoning *bool   `yaml:"show_reasoning"`
	Stream        *bool   `yaml:"stream"`
	// DraftStreaming uses the Bot API sendMessageDraft method for animated
	// streaming in private chats, falling back to message edits elsewhere.
	DraftStreaming *bool `yaml:"draft_streaming"`
	// PinnedStatus maintains one pinned message with the live run status.
	PinnedStatus   *bool    `yaml:"pinned_status"`
	EditInterval   Duration `yaml:"edit_interval"`
	PollTimeout    Duration `yaml:"poll_timeout"`
	PairingCode    string   `yaml:"pairing_code"`
	APIBaseURL     string   `yaml:"api_base_url"`
	MaxMessageSize int      `yaml:"max_message_size"`
}

// Agent configures the tool-calling loop and context management.
type Agent struct {
	// MaxIterations caps the tool-calling loop. Zero (the default) disables
	// the cap: a run ends with a final answer, a provider failure, or /stop.
	MaxIterations int `yaml:"max_iterations"`
	// RunTimeout bounds one run's wall-clock time. Zero (the default) disables
	// the limit. Per-request and per-tool timeouts still apply.
	RunTimeout         Duration `yaml:"run_timeout"`
	ContextWindow      int      `yaml:"context_window"`
	CompactThreshold   float64  `yaml:"compact_threshold"`
	CompactKeep        int      `yaml:"compact_keep"`
	MaxHistoryMessages int      `yaml:"max_history_messages"`
	ToolOutputLimit    int      `yaml:"tool_output_limit"`
	MaxParallelTools   int      `yaml:"max_parallel_tools"`
	MaxQueue           int      `yaml:"max_queue"`
	Interrupt          string   `yaml:"interrupt"` // queue | interrupt
	ShowProgress       *bool    `yaml:"show_progress"`
	ToolTimeout        Duration `yaml:"tool_timeout"`
	MaxTokens          int      `yaml:"max_tokens"`
	Temperature        *float64 `yaml:"temperature"`
	// SystemPromptFile is an alternative to the inline system_prompt.
	SystemPromptFile string   `yaml:"system_prompt_file"`
	ContextFiles     []string `yaml:"context_files"`
}

// Tools configures built-in tools and the approval policy.
type Tools struct {
	WebProxy          string   `yaml:"web_proxy"`
	Approval          string   `yaml:"approval"` // ask | allowlist | off
	ApprovalTimeout   Duration `yaml:"approval_timeout"`
	Roots             []string `yaml:"roots"`
	CommandTimeout    Duration `yaml:"command_timeout"`
	MaxOutputBytes    int      `yaml:"max_output_bytes"`
	AllowCommands     []string `yaml:"allow_commands"`
	Background        *bool    `yaml:"background"`
	WebFetchMaxBytes  int      `yaml:"web_fetch_max_bytes"`
	WebFetchTimeout   Duration `yaml:"web_fetch_timeout"`
	SearchBackend     string   `yaml:"search_backend"` // searxng | brave | none
	SearchURL         string   `yaml:"search_url"`
	SearchAPIKey      string   `yaml:"search_api_key"`
	SearchMaxResults  int      `yaml:"search_max_results"`
	AllowSendFile     *bool    `yaml:"allow_send_file"`
	DisableShell      bool     `yaml:"disable_shell"`
	DisableFileWrites bool     `yaml:"disable_file_writes"`
}

// Store contains SQLite tuning.
type Store struct {
	Path            string `yaml:"path"`
	ReadConns       int    `yaml:"read_conns"`
	BusyTimeoutMS   int    `yaml:"busy_timeout_ms"`
	MaxBatch        int    `yaml:"max_batch"`
	FlushIntervalMS int    `yaml:"flush_interval_ms"`
}

// Cron configures the scheduler.
type Cron struct {
	Enabled bool     `yaml:"enabled"`
	Tick    Duration `yaml:"tick"`
}

// Memory configures curated persistent notes.
type Memory struct {
	Enabled bool   `yaml:"enabled"`
	Dir     string `yaml:"dir"`
}

// Skills configures markdown procedures.
type Skills struct {
	Enabled bool   `yaml:"enabled"`
	Dir     string `yaml:"dir"`
	// GitHubToken raises GitHub API rate limits for skill marketplace search.
	// It is never required for public repositories.
	GitHubToken string `yaml:"github_token"`
}

// Voice configures optional audio transcription.
type Voice struct {
	Enabled  bool     `yaml:"enabled"`
	BaseURL  string   `yaml:"base_url"`
	APIKey   string   `yaml:"api_key"`
	Model    string   `yaml:"model"`
	Proxy    string   `yaml:"proxy"`
	Language string   `yaml:"language"`
	Timeout  Duration `yaml:"timeout"`
}

// MCP describes an MCP server (stdio or HTTP).
type MCP struct {
	Name    string            `yaml:"name"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	Proxy   string            `yaml:"proxy"`
}

// Provider describes an OpenAI-compatible API endpoint.
type Provider struct {
	Name              string            `yaml:"name"`
	BaseURL           string            `yaml:"base_url"`
	APIKey            string            `yaml:"api_key"`
	DefaultModel      string            `yaml:"default_model"`
	Models            []string          `yaml:"models"`
	Headers           map[string]string `yaml:"headers"`
	Proxy             string            `yaml:"proxy"`
	Timeout           Duration          `yaml:"timeout"`
	ConnectTimeout    Duration          `yaml:"connect_timeout"`
	SupportsTools     *bool             `yaml:"supports_tools"`
	SupportsStreaming *bool             `yaml:"supports_streaming"`
	SupportsVision    *bool             `yaml:"supports_vision"`
	ContextWindow     int               `yaml:"context_window"`
	MaxTokens         int               `yaml:"max_tokens"`
	Temperature       *float64          `yaml:"temperature"`
	ChatPath          string            `yaml:"chat_path"`
	AudioPath         string            `yaml:"audio_path"`
	ExtraBody         map[string]any    `yaml:"extra_body"`

	ProviderProxyDisabled bool `yaml:"-"`
}

func boolp(v bool) *bool { return &v }

// Default returns the built-in configuration.
func Default() *Config {
	return &Config{
		DataDir:    envOr("AGENT_DATA_DIR", "./data"),
		Workspace:  envOr("AGENT_WORKSPACE", "./workspace"),
		LogLevel:   "info",
		LogFormat:  "text",
		HealthAddr: "127.0.0.1:8081",
		PprofAddr:  "127.0.0.1:6060",
		Shutdown:   Duration(20 * time.Second),
		HTTP: HTTP{
			UserAgent:       "Kestrel/1.0 (+https://github.com/bitvibe/kestrel)",
			MaxIdleConns:    16,
			IdleConnTimeout: Duration(90 * time.Second),
			ResponseTimeout: Duration(120 * time.Second),
		},
		Telegram: Telegram{
			Mode:           "polling",
			Interrupt:      "queue",
			Progress:       boolp(true),
			LogActions:     boolp(true),
			ShowReasoning:  boolp(true),
			Stream:         boolp(true),
			DraftStreaming: boolp(true),
			PinnedStatus:   boolp(true),
			EditInterval:   Duration(1200 * time.Millisecond),
			PollTimeout:    Duration(30 * time.Second),
			APIBaseURL:     "https://api.telegram.org",
			MaxMessageSize: 4096,
		},
		Agent: Agent{
			MaxIterations:      0, // unlimited; the run ends with a final answer or /stop
			RunTimeout:         0, // no wall-clock limit for one run
			ContextWindow:      32768,
			CompactThreshold:   0.80,
			CompactKeep:        8,
			MaxHistoryMessages: 200,
			ToolOutputLimit:    32000,
			MaxParallelTools:   4,
			MaxQueue:           8,
			Interrupt:          "queue",
			ShowProgress:       boolp(true),
			ToolTimeout:        Duration(10 * time.Minute),
		},
		Tools: Tools{
			Approval:         "ask",
			ApprovalTimeout:  Duration(2 * time.Minute),
			CommandTimeout:   Duration(2 * time.Minute),
			MaxOutputBytes:   64 * 1024,
			Background:       boolp(true),
			WebFetchMaxBytes: 2 * 1024 * 1024,
			WebFetchTimeout:  Duration(45 * time.Second),
			SearchMaxResults: 8,
			AllowSendFile:    boolp(true),
		},
		Store: Store{
			ReadConns:       3,
			BusyTimeoutMS:   5000,
			MaxBatch:        128,
			FlushIntervalMS: 5,
		},
		Cron:   Cron{Enabled: true, Tick: Duration(30 * time.Second)},
		Memory: Memory{Enabled: true},
		Skills: Skills{Enabled: true},
		Voice:  Voice{Model: "whisper-1", Timeout: Duration(60 * time.Second)},
	}
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandEnv(in []byte) []byte {
	return envRef.ReplaceAllFunc(in, func(m []byte) []byte {
		name := envRef.FindSubmatch(m)[1]
		return []byte(os.Getenv(string(name)))
	})
}

// Load reads configuration from path. If path is empty, $AGENT_CONFIG or
// $DATA_DIR/config.yaml is used. A missing default file is not an error (the
// agent can be configured entirely from environment variables); a missing
// explicitly-specified file is.
func Load(path string) (*Config, error) {
	explicit := path != ""
	if !explicit {
		if p := os.Getenv("AGENT_CONFIG"); p != "" {
			path, explicit = p, true
		} else {
			path = filepath.Join(envOr("AGENT_DATA_DIR", "./data"), "config.yaml")
		}
	}
	cfg := Default()
	cfg.ConfigPath = path
	if b, err := os.ReadFile(path); err == nil {
		cfg.ConfigPathUsed = path
		if err := yaml.Unmarshal(expandEnv(b), cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if explicit || !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := applyEnv(cfg); err != nil {
		return nil, err
	}
	applyDefaults(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func applyDefaults(c *Config) {
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.Workspace == "" {
		c.Workspace = "./workspace"
	}
	if c.Store.Path == "" {
		c.Store.Path = filepath.Join(c.DataDir, "kestrel.db")
	}
	if c.Memory.Dir == "" {
		c.Memory.Dir = filepath.Join(c.DataDir, "memory")
	}
	if c.Skills.Dir == "" {
		c.Skills.Dir = filepath.Join(c.DataDir, "skills")
	}
	if len(c.Tools.Roots) == 0 {
		c.Tools.Roots = []string{c.Workspace}
	}
	if c.Tools.Approval == "" {
		c.Tools.Approval = "ask"
	}
	if c.Agent.ContextWindow <= 0 {
		c.Agent.ContextWindow = 32768
	}
	if c.Agent.CompactThreshold <= 0 || c.Agent.CompactThreshold > 1 {
		c.Agent.CompactThreshold = 0.80
	}
	if c.Agent.Interrupt == "" {
		c.Agent.Interrupt = "queue"
	}
	if c.Telegram.Interrupt == "" {
		c.Telegram.Interrupt = c.Agent.Interrupt
	}
	if c.Store.ReadConns <= 0 {
		c.Store.ReadConns = 3
	}
	if c.Store.MaxBatch <= 0 {
		c.Store.MaxBatch = 128
	}
	if c.Store.FlushIntervalMS <= 0 {
		c.Store.FlushIntervalMS = 5
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
		if p.ChatPath == "" {
			p.ChatPath = "/chat/completions"
		}
		if p.AudioPath == "" {
			p.AudioPath = "/audio/transcriptions"
		}
	}
}

func applyEnv(c *Config) error {
	set := func(k string, dst *string) {
		if v, ok := os.LookupEnv(k); ok {
			*dst = v
		}
	}
	set("AGENT_DATA_DIR", &c.DataDir)
	set("AGENT_WORKSPACE", &c.Workspace)
	set("AGENT_LOG_LEVEL", &c.LogLevel)
	set("AGENT_LOG_FORMAT", &c.LogFormat)
	set("AGENT_HEALTH_ADDR", &c.HealthAddr)
	set("AGENT_PPROF_ADDR", &c.PprofAddr)
	set("AGENT_PROXY", &c.Proxy)
	set("AGENT_WEB_PROXY", &c.Tools.WebProxy)
	set("AGENT_ACTIVE", &c.Active)
	set("AGENT_PERSONA", &c.Persona)
	set("AGENT_SYSTEM_PROMPT", &c.SystemPrompt)
	set("AGENT_TELEGRAM_TOKEN", &c.Telegram.Token)
	if v, ok := os.LookupEnv("AGENT_TELEGRAM_PROXY"); ok {
		c.Telegram.Proxy = v
	}
	if v, ok := os.LookupEnv("AGENT_TELEGRAM_MODE"); ok {
		c.Telegram.Mode = v
	}
	if v, ok := os.LookupEnv("AGENT_TELEGRAM_PAIRING_CODE"); ok {
		c.Telegram.PairingCode = v
	}
	if v, ok := os.LookupEnv("AGENT_TELEGRAM_LOG_ACTIONS"); ok {
		def := c.Telegram.LogActions == nil || *c.Telegram.LogActions
		c.Telegram.LogActions = boolp(parseBool(v, def))
	}
	if v, ok := os.LookupEnv("AGENT_TELEGRAM_SHOW_REASONING"); ok {
		def := c.Telegram.ShowReasoning == nil || *c.Telegram.ShowReasoning
		c.Telegram.ShowReasoning = boolp(parseBool(v, def))
	}
	if v, ok := os.LookupEnv("AGENT_TELEGRAM_DRAFT_STREAMING"); ok {
		def := c.Telegram.DraftStreaming == nil || *c.Telegram.DraftStreaming
		c.Telegram.DraftStreaming = boolp(parseBool(v, def))
	}
	if v, ok := os.LookupEnv("AGENT_TELEGRAM_PINNED_STATUS"); ok {
		def := c.Telegram.PinnedStatus == nil || *c.Telegram.PinnedStatus
		c.Telegram.PinnedStatus = boolp(parseBool(v, def))
	}
	set("AGENT_SKILLS_GITHUB_TOKEN", &c.Skills.GitHubToken)
	if v, ok := os.LookupEnv("AGENT_TELEGRAM_ALLOWED_USERS"); ok {
		ids, err := ParseIDs(v)
		if err != nil {
			return fmt.Errorf("AGENT_TELEGRAM_ALLOWED_USERS: %w", err)
		}
		c.Telegram.AllowedUsers = ids
	}
	if v, ok := os.LookupEnv("AGENT_TELEGRAM_API_BASE_URL"); ok {
		c.Telegram.APIBaseURL = v
	}
	if v, ok := os.LookupEnv("AGENT_FALLBACK"); ok {
		c.Fallback = splitList(v)
	}
	if v, ok := os.LookupEnv("AGENT_TOOLS_ROOTS"); ok {
		c.Tools.Roots = splitList(v)
	}
	if v, ok := os.LookupEnv("AGENT_APPROVAL"); ok {
		c.Tools.Approval = v
	}
	if v, ok := os.LookupEnv("AGENT_SEARCH_URL"); ok {
		c.Tools.SearchURL = v
	}
	if v, ok := os.LookupEnv("AGENT_SEARCH_API_KEY"); ok {
		c.Tools.SearchAPIKey = v
	}
	for _, kv := range []struct {
		key string
		dst *bool
	}{
		{"AGENT_PPROF", &c.Pprof},
		{"AGENT_CRON_ENABLED", &c.Cron.Enabled},
	} {
		if v, ok := os.LookupEnv(kv.key); ok {
			*kv.dst = parseBool(v, *kv.dst)
		}
	}
	// Generic provider overrides: AGENT_PROVIDER_<NAME>_BASE_URL, _API_KEY,
	// _MODEL, _PROXY, _TIMEOUT, _TOOLS, _STREAMING.
	if len(c.Providers) == 0 {
		if v, ok := os.LookupEnv("AGENT_PROVIDER_BASE_URL"); ok && v != "" {
			c.Providers = append(c.Providers, Provider{Name: envOr("AGENT_PROVIDER_NAME", "default")})
		}
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		san := sanitizeEnvName(p.Name)
		set("AGENT_PROVIDER_"+san+"_BASE_URL", &p.BaseURL)
		set("AGENT_PROVIDER_"+san+"_API_KEY", &p.APIKey)
		set("AGENT_PROVIDER_"+san+"_MODEL", &p.DefaultModel)
		set("AGENT_PROVIDER_"+san+"_PROXY", &p.Proxy)
		if v, ok := os.LookupEnv("AGENT_PROVIDER_" + san + "_TIMEOUT"); ok {
			if d, err := time.ParseDuration(v); err == nil {
				p.Timeout = Duration(d)
			}
		}
		if v, ok := os.LookupEnv("AGENT_PROVIDER_" + san + "_TOOLS"); ok {
			p.SupportsTools = boolp(parseBool(v, true))
		}
		if v, ok := os.LookupEnv("AGENT_PROVIDER_" + san + "_STREAMING"); ok {
			p.SupportsStreaming = boolp(parseBool(v, true))
		}
	}
	if len(c.Providers) > 0 && c.Providers[0].Name == envOr("AGENT_PROVIDER_NAME", "default") {
		p := &c.Providers[0]
		set("AGENT_PROVIDER_BASE_URL", &p.BaseURL)
		set("AGENT_PROVIDER_API_KEY", &p.APIKey)
		set("AGENT_PROVIDER_MODEL", &p.DefaultModel)
	}
	return nil
}

func sanitizeEnvName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func parseBool(v string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ParseIDs parses a comma- or space-separated list of Telegram user IDs.
func ParseIDs(v string) ([]int64, error) {
	fields := strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	})
	out := make([]int64, 0, len(fields))
	for _, f := range fields {
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid Telegram user ID %q", f)
		}
		out = append(out, id)
	}
	return out, nil
}

// Validate performs semantic validation with actionable messages.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	if strings.TrimSpace(c.DataDir) == "" {
		add("data_dir must not be empty")
	}
	if strings.TrimSpace(c.Workspace) == "" {
		add("workspace must not be empty")
	}
	switch c.Tools.Approval {
	case "ask", "allowlist", "off":
	default:
		add("tools.approval must be one of ask, allowlist, off (got %q)", c.Tools.Approval)
	}
	switch c.Agent.Interrupt {
	case "queue", "interrupt":
	default:
		add("agent.interrupt must be queue or interrupt (got %q)", c.Agent.Interrupt)
	}
	switch c.Telegram.Interrupt {
	case "queue", "interrupt":
	default:
		add("telegram.interrupt must be queue or interrupt (got %q)", c.Telegram.Interrupt)
	}
	if c.Telegram.Mode != "" && c.Telegram.Mode != "polling" && c.Telegram.Mode != "webhook" {
		add("telegram.mode must be polling or webhook (got %q)", c.Telegram.Mode)
	}
	seen := map[string]bool{}
	for i, p := range c.Providers {
		if p.Name == "" {
			add("providers[%d].name is required", i)
			continue
		}
		if seen[p.Name] {
			add("duplicate provider name %q", p.Name)
		}
		seen[p.Name] = true
		if p.BaseURL == "" {
			add("provider %q: base_url is required", p.Name)
		} else if u, err := url.Parse(p.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("provider %q: base_url must be an http(s) URL, got %q", p.Name, p.BaseURL)
		}
		if p.DefaultModel == "" {
			add("provider %q: default_model is required", p.Name)
		}
	}
	if c.Active != "" {
		name, _, err := SplitModel(c.Active)
		if err != nil {
			add("active must be provider:model: %v", err)
		} else if !seen[name] {
			add("active provider %q is not defined in providers", name)
		}
	}
	for _, f := range c.Fallback {
		if !seen[f] {
			add("fallback provider %q is not defined in providers", f)
		}
	}
	if c.Pprof && c.PprofAddr == "" {
		add("pprof_addr must be set when pprof is enabled")
	}
	if len(errs) > 0 {
		return errors.New("invalid configuration:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

// ValidateGateway adds requirements specific to running the bot.
func (c *Config) ValidateGateway() error {
	if err := c.Validate(); err != nil {
		return err
	}
	var errs []string
	if c.Telegram.Token == "" {
		errs = append(errs, "telegram.token is required (set AGENT_TELEGRAM_TOKEN)")
	}
	if len(c.Telegram.AllowedUsers) == 0 {
		errs = append(errs, "telegram.allowed_users must contain at least one numeric user ID")
	}
	if len(c.Providers) == 0 {
		errs = append(errs, "at least one provider must be configured")
	}
	if c.Active == "" && len(c.Providers) > 0 {
		c.Active = c.Providers[0].Name + ":" + c.Providers[0].DefaultModel
	}
	if len(errs) > 0 {
		return errors.New("invalid gateway configuration:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

// SplitModel parses "provider:model" (model may contain colons).
func SplitModel(s string) (provider, model string, err error) {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return "", "", fmt.Errorf("expected provider:model, got %q", s)
	}
	return s[:i], s[i+1:], nil
}

// Provider returns a provider by name.
func (c *Config) Provider(name string) (Provider, bool) {
	for _, p := range c.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// ActiveProviderModel resolves the configured active selection, falling back
// to the first provider and its default model.
func (c *Config) ActiveProviderModel() (provider, model string, err error) {
	if c.Active != "" {
		return SplitModel(c.Active)
	}
	if len(c.Providers) == 0 {
		return "", "", errors.New("no providers configured")
	}
	p := c.Providers[0]
	return p.Name, p.DefaultModel, nil
}

// ResolveProxy implements the documented precedence: per-provider, then
// per-component, then global. "none"/"off"/"direct" explicitly disables the
// proxy; empty means "fall back" (and ultimately the standard environment).
func (c *Config) ResolveProxy(component, provider string) string {
	for _, v := range []string{provider, component, c.Proxy} {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ProxyDisabled reports whether a proxy spec explicitly disables proxying.
func ProxyDisabled(spec string) bool {
	switch strings.ToLower(strings.TrimSpace(spec)) {
	case "none", "off", "direct", "disable":
		return true
	}
	return false
}

// WriteFile writes the configuration as YAML with 0600 permissions.
func (c *Config) WriteFile(path string) error {
	if path == "" {
		path = c.ConfigPath
	}
	if path == "" {
		path = filepath.Join(c.DataDir, "config.yaml")
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Prepare creates the runtime directories.
func (c *Config) Prepare() error {
	dirs := []string{c.DataDir, c.Workspace, c.Memory.Dir, c.Skills.Dir, filepath.Dir(c.Store.Path)}
	for _, d := range dirs {
		if d == "" || d == "." {
			continue
		}
		if err := os.MkdirAll(d, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	for _, r := range c.Tools.Roots {
		if err := os.MkdirAll(r, 0o750); err != nil {
			return fmt.Errorf("create tool root %s: %w", r, err)
		}
	}
	return nil
}

// ToolsEnabled reports whether native tool calling should be used.
func (p Provider) ToolsEnabled() bool {
	return p.SupportsTools == nil || *p.SupportsTools
}

// StreamingEnabled reports whether SSE streaming is enabled.
func (p Provider) StreamingEnabled() bool {
	return p.SupportsStreaming == nil || *p.SupportsStreaming
}

// VisionEnabled reports whether image inputs are supported.
func (p Provider) VisionEnabled() bool {
	return p.SupportsVision != nil && *p.SupportsVision
}
