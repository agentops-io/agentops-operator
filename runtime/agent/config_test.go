package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func setEnv(t *testing.T, key, value string) {
	t.Helper()
	t.Setenv(key, value)
}

func requiredEnvs(t *testing.T) {
	t.Helper()
	setEnv(t, "AGENT_MODEL", "claude-sonnet-4-6")
	setEnv(t, "AGENT_SYSTEM_PROMPT", "You are a test agent.")
	setEnv(t, "TASK_QUEUE_URL", "localhost:6379")
}

func TestLoadConfig_Defaults(t *testing.T) {
	requiredEnvs(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want %q", cfg.Model, "claude-sonnet-4-6")
	}
	if cfg.MaxTokensPerCall != 8000 {
		t.Errorf("MaxTokensPerCall = %d, want 8000", cfg.MaxTokensPerCall)
	}
	if cfg.TimeoutSeconds != 120 {
		t.Errorf("TimeoutSeconds = %d, want 120", cfg.TimeoutSeconds)
	}
	if cfg.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want 3", cfg.MaxRetries)
	}
}

func TestLoadConfig_OptionalOverrides(t *testing.T) {
	requiredEnvs(t)
	setEnv(t, "AGENT_MAX_TOKENS", "4000")
	setEnv(t, "AGENT_TIMEOUT_SECONDS", "60")
	setEnv(t, "AGENT_MAX_RETRIES", "5")
	setEnv(t, "AGENT_PROVIDER", "anthropic")
	setEnv(t, "AGENT_VALIDATOR_PROMPT", "Reply HEALTHY")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cfg.MaxTokensPerCall != 4000 {
		t.Errorf("MaxTokensPerCall = %d, want 4000", cfg.MaxTokensPerCall)
	}
	if cfg.TimeoutSeconds != 60 {
		t.Errorf("TimeoutSeconds = %d, want 60", cfg.TimeoutSeconds)
	}
	if cfg.MaxRetries != 5 {
		t.Errorf("MaxRetries = %d, want 5", cfg.MaxRetries)
	}
	if cfg.Provider != "anthropic" {
		t.Errorf("Provider = %q, want %q", cfg.Provider, "anthropic")
	}
	if cfg.ValidatorPrompt != "Reply HEALTHY" {
		t.Errorf("ValidatorPrompt = %q, want %q", cfg.ValidatorPrompt, "Reply HEALTHY")
	}
}

func TestLoadConfig_MCPServers(t *testing.T) {
	requiredEnvs(t)
	servers := []MCPServerConfig{
		{Name: "search", URL: "https://search.example.com/sse"},
		{Name: "browser", URL: "https://browser.example.com/sse"},
	}
	raw, _ := json.Marshal(servers)
	setEnv(t, "AGENT_MCP_SERVERS", string(raw))

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.MCPServers) != 2 {
		t.Fatalf("MCPServers len = %d, want 2", len(cfg.MCPServers))
	}
	if cfg.MCPServers[0].Name != "search" {
		t.Errorf("MCPServers[0].Name = %q, want %q", cfg.MCPServers[0].Name, "search")
	}
}

func TestLoadConfig_MissingRequired(t *testing.T) {
	_ = os.Unsetenv("AGENT_MODEL")
	_ = os.Unsetenv("AGENT_SYSTEM_PROMPT")
	_ = os.Unsetenv("TASK_QUEUE_URL")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for missing AGENT_MODEL, got nil")
	}
}

func TestLoadConfig_InvalidMaxTokens(t *testing.T) {
	requiredEnvs(t)
	setEnv(t, "AGENT_MAX_TOKENS", "not-a-number")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for invalid AGENT_MAX_TOKENS, got nil")
	}
}

func TestLoadConfig_InvalidTimeout(t *testing.T) {
	requiredEnvs(t)
	setEnv(t, "AGENT_TIMEOUT_SECONDS", "bad")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for invalid AGENT_TIMEOUT_SECONDS, got nil")
	}
}

func TestLoadConfig_InvalidMCPServersJSON(t *testing.T) {
	requiredEnvs(t)
	setEnv(t, "AGENT_MCP_SERVERS", "not-json")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for invalid AGENT_MCP_SERVERS JSON, got nil")
	}
}

func TestTaskTimeout(t *testing.T) {
	cfg := &Config{TimeoutSeconds: 30}
	got := TaskTimeout(cfg)
	want := 30 * time.Second
	if got != want {
		t.Errorf("TaskTimeout = %v, want %v", got, want)
	}
}
