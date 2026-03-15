// Package mock provides a deterministic LLMProvider for local testing.
// It returns canned responses without making any API calls, making it
// ideal for testing ArkFlow logic (DAG order, conditionals, loops) without
// burning API credits or requiring an Anthropic API key.
//
// Usage — register and select:
//
//	import _ "github.com/arkonis-dev/ark-operator/internal/agent/providers/mock"
//	provider, _ := providers.New("mock")
//
// Or construct directly for fine-grained control:
//
//	p := &mock.Provider{
//	    Responses: map[string]string{
//	        "research": "Here are the findings...",
//	        "summarize": "• Point one\n• Point two",
//	    },
//	    Default: "mock response",
//	    Delay:   200 * time.Millisecond,
//	}
package mock

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/arkonis-dev/ark-operator/internal/agent/config"
	"github.com/arkonis-dev/ark-operator/internal/agent/mcp"
	"github.com/arkonis-dev/ark-operator/internal/agent/providers"
	"github.com/arkonis-dev/ark-operator/internal/agent/queue"
)

func init() {
	providers.Register("mock", func() providers.LLMProvider {
		return &Provider{Default: "mock response"}
	})
}

// Provider is a configurable mock LLM backend.
//
// Response matching: for each entry in Responses, if the task prompt contains
// the key (case-insensitive substring match), that value is returned.
// Falls back to Default when nothing matches.
type Provider struct {
	// Responses maps prompt substrings to canned reply text.
	// Matched case-insensitively; first match wins.
	Responses map[string]string

	// Default is the response returned when no Responses entry matches.
	// Defaults to "mock response" when constructed via the registry.
	Default string

	// Delay simulates LLM latency. Zero means no delay.
	Delay time.Duration
}

// RunTask implements providers.LLMProvider.
// It matches the task prompt against Responses and returns the canned reply.
// No real LLM calls are made; tools are never invoked.
func (p *Provider) RunTask(
	ctx context.Context,
	_ *config.Config,
	task queue.Task,
	_ []mcp.Tool,
	_ func(context.Context, string, json.RawMessage) (string, error),
) (string, queue.TokenUsage, error) {
	if p.Delay > 0 {
		select {
		case <-time.After(p.Delay):
		case <-ctx.Done():
			return "", queue.TokenUsage{}, ctx.Err()
		}
	}

	reply := p.match(task.Prompt)

	// Simulate token counts proportional to prompt/reply length
	// so callers can exercise budget and cost-tracking logic.
	usage := queue.TokenUsage{
		InputTokens:  int64(len(task.Prompt) / 4),
		OutputTokens: int64(len(reply) / 4),
	}

	return reply, usage, nil
}

// match returns the first Responses value whose key is a case-insensitive
// substring of prompt, or Default if nothing matches.
func (p *Provider) match(prompt string) string {
	lower := strings.ToLower(prompt)
	for key, reply := range p.Responses {
		if strings.Contains(lower, strings.ToLower(key)) {
			return reply
		}
	}
	if p.Default != "" {
		return p.Default
	}
	return "mock response"
}
