package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadEnvAndProxy(t *testing.T) {
	t.Setenv("AGENT_DATA_DIR", t.TempDir())
	t.Setenv("TEST_KEY", "secret-value")
	t.Setenv("AGENT_TELEGRAM_TOKEN", "from-env")
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := `proxy: socks5://global:1080
telegram:
  token: from-file
  proxy: none
  allowed_users: [42]
providers:
  - name: cloud
    base_url: https://example.org/v1
    api_key: ${TEST_KEY}
    default_model: test
    proxy: https://provider:8443
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Telegram.Token != "from-env" || c.Providers[0].APIKey != "secret-value" {
		t.Fatalf("env precedence or interpolation failed: %+v", c)
	}
	if got := c.ResolveProxy(c.Telegram.Proxy, ""); got != "none" {
		t.Fatalf("Telegram proxy = %q", got)
	}
	if got := c.ResolveProxy(c.Telegram.Proxy, c.Providers[0].Proxy); got != "https://provider:8443" {
		t.Fatalf("provider proxy = %q", got)
	}
	if err := c.ValidateGateway(); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidProvider(t *testing.T) {
	c := Default()
	c.Providers = []Provider{{Name: "bad", BaseURL: "ftp://example.com", DefaultModel: "m"}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "http(s)") {
		t.Fatalf("validation: %v", err)
	}
}

func TestEnvironmentOnlyGateway(t *testing.T) {
	t.Setenv("AGENT_DATA_DIR", t.TempDir())
	t.Setenv("AGENT_TELEGRAM_TOKEN", "123:test")
	t.Setenv("AGENT_TELEGRAM_ALLOWED_USERS", "42, 43")
	t.Setenv("AGENT_PROVIDER_BASE_URL", "http://localhost:11434/v1")
	t.Setenv("AGENT_PROVIDER_MODEL", "local")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateGateway(); err != nil {
		t.Fatal(err)
	}
	if c.Providers[0].Name != "default" || c.Providers[0].DefaultModel != "local" || len(c.Telegram.AllowedUsers) != 2 {
		t.Fatalf("environment configuration not applied: %+v", c)
	}
}

func TestInvalidAllowlistEnvironment(t *testing.T) {
	t.Setenv("AGENT_DATA_DIR", t.TempDir())
	t.Setenv("AGENT_TELEGRAM_ALLOWED_USERS", "not-an-id")
	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "AGENT_TELEGRAM_ALLOWED_USERS") {
		t.Fatalf("expected actionable allowlist error, got %v", err)
	}
}

func TestUnlimitedAgentDefaults(t *testing.T) {
	c := Default()
	if c.Agent.MaxIterations != 0 {
		t.Fatalf("max_iterations default = %d, want 0 (unlimited)", c.Agent.MaxIterations)
	}
	if c.Agent.RunTimeout.D() != 0 {
		t.Fatalf("run_timeout default = %s, want 0", c.Agent.RunTimeout.D())
	}
	if c.Telegram.LogActions == nil || !*c.Telegram.LogActions {
		t.Fatal("telegram.log_actions should default to true")
	}
	if c.Telegram.ShowReasoning == nil || !*c.Telegram.ShowReasoning {
		t.Fatal("telegram.show_reasoning should default to true")
	}
	if c.Telegram.DraftStreaming == nil || !*c.Telegram.DraftStreaming {
		t.Fatal("telegram.draft_streaming should default to true")
	}
	if c.Telegram.PinnedStatus == nil || !*c.Telegram.PinnedStatus {
		t.Fatal("telegram.pinned_status should default to true")
	}
}

func TestLogActionsEnvironmentOverride(t *testing.T) {
	t.Setenv("AGENT_DATA_DIR", t.TempDir())
	t.Setenv("AGENT_TELEGRAM_LOG_ACTIONS", "false")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Telegram.LogActions == nil || *c.Telegram.LogActions {
		t.Fatal("AGENT_TELEGRAM_LOG_ACTIONS=false was not applied")
	}
}

func TestShowReasoningEnvironmentOverride(t *testing.T) {
	t.Setenv("AGENT_DATA_DIR", t.TempDir())
	t.Setenv("AGENT_TELEGRAM_SHOW_REASONING", "false")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Telegram.ShowReasoning == nil || *c.Telegram.ShowReasoning {
		t.Fatal("AGENT_TELEGRAM_SHOW_REASONING=false was not applied")
	}
}

func TestStreamingAndSkillsEnvironmentOverride(t *testing.T) {
	t.Setenv("AGENT_DATA_DIR", t.TempDir())
	t.Setenv("AGENT_TELEGRAM_DRAFT_STREAMING", "false")
	t.Setenv("AGENT_TELEGRAM_PINNED_STATUS", "false")
	t.Setenv("AGENT_SKILLS_GITHUB_TOKEN", "gh-test-token")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Telegram.DraftStreaming == nil || *c.Telegram.DraftStreaming {
		t.Fatal("AGENT_TELEGRAM_DRAFT_STREAMING=false was not applied")
	}
	if c.Telegram.PinnedStatus == nil || *c.Telegram.PinnedStatus {
		t.Fatal("AGENT_TELEGRAM_PINNED_STATUS=false was not applied")
	}
	if c.Skills.GitHubToken != "gh-test-token" {
		t.Fatal("AGENT_SKILLS_GITHUB_TOKEN was not applied")
	}
}
