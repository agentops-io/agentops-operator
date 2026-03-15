package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

// MCPServerConfig holds the connection details for one MCP tool server.
type MCPServerConfig struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// WebhookToolConfig defines an inline HTTP tool injected by the operator via AGENT_WEBHOOK_TOOLS.
type WebhookToolConfig struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"`
	// Method is the HTTP method (GET, POST, PUT, PATCH). Defaults to POST.
	Method string `json:"method"`
	// InputSchema is a JSON Schema string describing the tool's input parameters.
	InputSchema string `json:"inputSchema"`
}

// Config holds all runtime configuration for an agent pod.
// All fields are populated from environment variables injected by the operator.
// Provider-specific credentials (e.g. ANTHROPIC_API_KEY) are read directly
// by each LLMProvider implementation, not stored here.
type Config struct {
	// Provider selects the LLM backend (e.g. "anthropic"). Set via AGENT_PROVIDER.
	// Defaults to "anthropic".
	Provider         string
	Model            string
	SystemPrompt     string
	MCPServers       []MCPServerConfig
	MaxTokensPerCall int
	TimeoutSeconds   int
	TaskQueueURL     string
	ValidatorPrompt  string
	// MaxRetries is the number of times a failed task is requeued before dead-lettering.
	// Set via AGENT_MAX_RETRIES. Defaults to 3.
	MaxRetries int
	// WebhookTools is the list of inline HTTP tools injected by the operator.
	// Set via AGENT_WEBHOOK_TOOLS (JSON array).
	WebhookTools []WebhookToolConfig
}

// Load reads agent configuration from environment variables.
func Load() (*Config, error) {
	model, err := requireEnv("AGENT_MODEL")
	if err != nil {
		return nil, err
	}
	systemPrompt, err := requireEnv("AGENT_SYSTEM_PROMPT")
	if err != nil {
		return nil, err
	}
	queueURL, err := requireEnv("TASK_QUEUE_URL")
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Provider:         os.Getenv("AGENT_PROVIDER"), // defaults to "anthropic" if empty
		Model:            model,
		SystemPrompt:     systemPrompt,
		TaskQueueURL:     queueURL,
		MaxTokensPerCall: 8000,
		TimeoutSeconds:   120,
		MaxRetries:       3,
		ValidatorPrompt:  os.Getenv("AGENT_VALIDATOR_PROMPT"),
	}

	if v := os.Getenv("AGENT_MAX_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid AGENT_MAX_TOKENS %q: %w", v, err)
		}
		cfg.MaxTokensPerCall = n
	}

	if v := os.Getenv("AGENT_TIMEOUT_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid AGENT_TIMEOUT_SECONDS %q: %w", v, err)
		}
		cfg.TimeoutSeconds = n
	}

	if raw := os.Getenv("AGENT_MCP_SERVERS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.MCPServers); err != nil {
			return nil, fmt.Errorf("invalid AGENT_MCP_SERVERS JSON: %w", err)
		}
	}

	if v := os.Getenv("AGENT_MAX_RETRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid AGENT_MAX_RETRIES %q: %w", v, err)
		}
		cfg.MaxRetries = n
	}

	if raw := os.Getenv("AGENT_WEBHOOK_TOOLS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.WebhookTools); err != nil {
			return nil, fmt.Errorf("invalid AGENT_WEBHOOK_TOOLS JSON: %w", err)
		}
	}

	return cfg, nil
}

// TaskTimeout returns the per-task deadline derived from cfg.
func TaskTimeout(cfg *Config) time.Duration {
	return time.Duration(cfg.TimeoutSeconds) * time.Second
}

func requireEnv(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("required env var %s is not set", key)
	}
	return v, nil
}
