package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/arkonis-dev/ark-operator/internal/agent/config"
	"github.com/arkonis-dev/ark-operator/internal/agent/mcp"
	"github.com/arkonis-dev/ark-operator/internal/agent/providers"
	"github.com/arkonis-dev/ark-operator/internal/agent/queue"
)

const submitSubtaskTool = "submit_subtask"

// Runner executes tasks by delegating to a configured LLMProvider.
// It merges tools from three sources:
//   - MCP servers (discovered at startup)
//   - Inline webhook tools (injected via AGENT_WEBHOOK_TOOLS env var)
//   - Built-in tools (submit_subtask when a task queue is available)
type Runner struct {
	cfg        *config.Config
	mcpManager *mcp.Manager
	provider   providers.LLMProvider
	queue      *queue.Queue
	allTools   []mcp.Tool
}

// New creates a Runner, builds the merged tool list, and wires up the task queue
// for the supervisor/worker submit_subtask built-in.
func New(cfg *config.Config, mcpManager *mcp.Manager, provider providers.LLMProvider, q *queue.Queue) *Runner {
	r := &Runner{cfg: cfg, mcpManager: mcpManager, provider: provider, queue: q}
	r.buildTools()
	return r
}

// buildTools assembles allTools from MCP + webhook + built-in sources.
func (r *Runner) buildTools() {
	r.allTools = append(r.allTools, r.mcpManager.Tools()...)

	// Inline webhook tools defined in spec.tools on the ArkAgent.
	for _, wt := range r.cfg.WebhookTools {
		schema := json.RawMessage(`{}`)
		if wt.InputSchema != "" {
			schema = json.RawMessage(wt.InputSchema)
		}
		r.allTools = append(r.allTools, mcp.Tool{
			Name:        wt.Name,
			Description: wt.Description,
			InputSchema: schema,
			// ServerURL left blank — CallTool handles these by name via cfg.WebhookTools.
		})
	}

	// Built-in: submit_subtask — only available when the task queue is wired in.
	if r.queue != nil {
		const submitSchema = `{"type":"object",` +
			`"properties":{"prompt":{"type":"string","description":"The task prompt to execute"}},` +
			`"required":["prompt"]}`
		r.allTools = append(r.allTools, mcp.Tool{
			Name:        submitSubtaskTool,
			Description: "Enqueue a new agent task for asynchronous processing. Returns the assigned task ID.",
			InputSchema: json.RawMessage(submitSchema),
		})
	}
}

// AllTools returns the merged tool list for use by the health probe and tests.
func (r *Runner) AllTools() []mcp.Tool {
	return r.allTools
}

// CallTool dispatches a tool invocation to the correct handler.
// Priority: built-in → webhook → MCP.
func (r *Runner) CallTool(ctx context.Context, toolName string, input json.RawMessage) (string, error) {
	// Built-in: supervisor/worker sub-task submission.
	if toolName == submitSubtaskTool && r.queue != nil {
		var args struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal(input, &args); err != nil {
			return "", fmt.Errorf("submit_subtask: invalid input: %w", err)
		}
		if strings.TrimSpace(args.Prompt) == "" {
			return "", fmt.Errorf("submit_subtask: prompt must not be empty")
		}
		taskID, err := r.queue.Submit(ctx, args.Prompt)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("subtask submitted with id: %s", taskID), nil
	}

	// Inline webhook tools.
	for _, wt := range r.cfg.WebhookTools {
		if wt.Name == toolName {
			return callWebhook(ctx, wt, input)
		}
	}

	// Fall through to MCP tools.
	return r.mcpManager.CallTool(ctx, toolName, input)
}

// RunTask executes a single task through the provider's agentic loop.
// Returns the text result, accumulated token usage, and any error.
// If task.Meta["stream_key"] is set and a task queue is available, each generated
// token chunk is published to that Redis List in real time.
func (r *Runner) RunTask(ctx context.Context, task queue.Task) (string, queue.TokenUsage, error) {
	var chunkFn func(string)
	if key := task.Meta["stream_key"]; key != "" && r.queue != nil {
		chunkFn = func(chunk string) {
			r.queue.PublishChunk(key, chunk)
		}
	}
	return r.provider.RunTask(ctx, r.cfg, task, r.allTools, r.CallTool, chunkFn)
}

// callWebhook invokes an inline HTTP webhook tool and returns the response body as text.
func callWebhook(ctx context.Context, wt config.WebhookToolConfig, input json.RawMessage) (string, error) {
	method := strings.ToUpper(wt.Method)
	if method == "" {
		method = http.MethodPost
	}

	var bodyReader io.Reader
	if method != http.MethodGet && len(input) > 0 {
		bodyReader = bytes.NewReader(input)
	}

	req, err := http.NewRequestWithContext(ctx, method, wt.URL, bodyReader)
	if err != nil {
		return "", fmt.Errorf("webhook tool %q: building request: %w", wt.Name, err)
	}
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("webhook tool %q: request failed: %w", wt.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("webhook tool %q: reading response: %w", wt.Name, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("webhook tool %q: server returned %d: %s", wt.Name, resp.StatusCode, string(body))
	}
	return string(body), nil
}
