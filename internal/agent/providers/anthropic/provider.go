// Package anthropic implements LLMProvider for the Anthropic Claude API.
// Importing this package (even with a blank import) registers the "anthropic"
// provider in the global registry.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/arkonis-dev/ark-operator/internal/agent/config"
	"github.com/arkonis-dev/ark-operator/internal/agent/mcp"
	"github.com/arkonis-dev/ark-operator/internal/agent/providers"
	"github.com/arkonis-dev/ark-operator/internal/agent/queue"
)

func init() {
	providers.Register("anthropic", func() providers.LLMProvider { return &Provider{} })
}

// Provider implements providers.LLMProvider using the Anthropic Claude API.
type Provider struct{}

// RunTask executes a task through the Anthropic agentic tool-use loop.
// It keeps calling the API until the model stops requesting tool use.
// Token usage is accumulated across all API calls and returned with the result.
// If chunkFn is non-nil, the final text turn is streamed token-by-token via the streaming API.
func (p *Provider) RunTask(
	ctx context.Context,
	cfg *config.Config,
	task queue.Task,
	tools []mcp.Tool,
	callTool func(context.Context, string, json.RawMessage) (string, error),
	chunkFn func(string),
) (string, queue.TokenUsage, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return "", queue.TokenUsage{}, fmt.Errorf("ANTHROPIC_API_KEY is not set; required for the anthropic provider")
	}
	client := anthropicsdk.NewClient(option.WithAPIKey(apiKey))

	anthropicTools := toAnthropicTools(tools)

	messages := []anthropicsdk.MessageParam{
		anthropicsdk.NewUserMessage(anthropicsdk.NewTextBlock(task.Prompt)),
	}

	var usage queue.TokenUsage

	for {
		params := anthropicsdk.MessageNewParams{
			Model:     anthropicsdk.Model(cfg.Model),
			MaxTokens: int64(cfg.MaxTokensPerCall),
			System: []anthropicsdk.TextBlockParam{
				{Text: cfg.SystemPrompt},
			},
			Messages: messages,
		}
		if len(anthropicTools) > 0 {
			params.Tools = anthropicTools
		}

		// Use streaming for the final text turn when chunkFn is set.
		// Tool-use turns always use the non-streaming API so we can inspect tool calls.
		if chunkFn != nil && len(anthropicTools) == 0 {
			text, turnUsage, err := p.runStreamingTurn(ctx, client, params, chunkFn)
			if err != nil {
				return "", usage, err
			}
			usage.InputTokens += turnUsage.InputTokens
			usage.OutputTokens += turnUsage.OutputTokens
			return text, usage, nil
		}

		resp, err := client.Messages.New(ctx, params)
		if err != nil {
			return "", usage, fmt.Errorf("anthropic API error: %w", err)
		}

		// Accumulate token usage across all turns in the tool-use loop.
		usage.InputTokens += resp.Usage.InputTokens
		usage.OutputTokens += resp.Usage.OutputTokens

		messages = append(messages, assistantMessage(resp.Content))

		if resp.StopReason == anthropicsdk.StopReasonEndTurn {
			text := extractText(resp.Content)
			// If we have a chunkFn and this is the final turn, emit the full text as one chunk.
			// This handles tool-use flows where streaming was deferred.
			if chunkFn != nil {
				chunkFn(text)
			}
			return text, usage, nil
		}

		toolResults := executeTools(ctx, resp.Content, callTool)
		if len(toolResults) == 0 {
			text := extractText(resp.Content)
			if chunkFn != nil {
				chunkFn(text)
			}
			return text, usage, nil
		}
		messages = append(messages, anthropicsdk.NewUserMessage(toolResults...))
	}
}

// runStreamingTurn calls the Anthropic streaming API and forwards each text delta to chunkFn.
func (p *Provider) runStreamingTurn(
	ctx context.Context,
	client anthropicsdk.Client,
	params anthropicsdk.MessageNewParams,
	chunkFn func(string),
) (string, queue.TokenUsage, error) {
	stream := client.Messages.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()

	var sb strings.Builder
	var usage queue.TokenUsage

	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case "message_start":
			v := event.AsMessageStart()
			usage.InputTokens += v.Message.Usage.InputTokens
			usage.OutputTokens += v.Message.Usage.OutputTokens
		case "message_delta":
			v := event.AsMessageDelta()
			usage.OutputTokens += v.Usage.OutputTokens
		case "content_block_delta":
			v := event.AsContentBlockDelta()
			if textDelta, ok := v.Delta.AsAny().(anthropicsdk.TextDelta); ok {
				chunkFn(textDelta.Text)
				sb.WriteString(textDelta.Text)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return "", usage, fmt.Errorf("anthropic streaming error: %w", err)
	}
	return sb.String(), usage, nil
}

// toAnthropicTools converts generic mcp.Tools into the Anthropic SDK format.
// ToolInputSchemaParam expects Properties to hold just the properties map and
// Required to hold the required field names — not the full schema blob.
func toAnthropicTools(tools []mcp.Tool) []anthropicsdk.ToolUnionParam {
	params := make([]anthropicsdk.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		schema := parseToolSchema(t.InputSchema)
		tool := anthropicsdk.ToolParam{
			Name:        t.Name,
			Description: anthropicsdk.String(t.Description),
			InputSchema: schema,
		}
		params = append(params, anthropicsdk.ToolUnionParam{OfTool: &tool})
	}
	return params
}

// parseToolSchema extracts Properties and Required from a full JSON Schema object
// into the form that anthropicsdk.ToolInputSchemaParam expects.
func parseToolSchema(raw json.RawMessage) anthropicsdk.ToolInputSchemaParam {
	if len(raw) == 0 {
		return anthropicsdk.ToolInputSchemaParam{}
	}
	var full struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	if err := json.Unmarshal(raw, &full); err != nil || len(full.Properties) == 0 {
		return anthropicsdk.ToolInputSchemaParam{Properties: raw}
	}
	var props any
	if err := json.Unmarshal(full.Properties, &props); err != nil {
		return anthropicsdk.ToolInputSchemaParam{Properties: raw}
	}
	return anthropicsdk.ToolInputSchemaParam{
		Properties: props,
		Required:   full.Required,
	}
}

// executeTools runs each tool_use block via callTool and returns result blocks.
func executeTools(
	ctx context.Context,
	content []anthropicsdk.ContentBlockUnion,
	callTool func(context.Context, string, json.RawMessage) (string, error),
) []anthropicsdk.ContentBlockParamUnion {
	var results []anthropicsdk.ContentBlockParamUnion
	for _, block := range content {
		if block.Type != "tool_use" {
			continue
		}
		output, err := callTool(ctx, block.Name, block.Input)
		if err != nil {
			results = append(results, anthropicsdk.NewToolResultBlock(block.ID, err.Error(), true))
			continue
		}
		results = append(results, anthropicsdk.NewToolResultBlock(block.ID, output, false))
	}
	return results
}

// assistantMessage converts a response content slice into a MessageParam.
func assistantMessage(content []anthropicsdk.ContentBlockUnion) anthropicsdk.MessageParam {
	params := make([]anthropicsdk.ContentBlockParamUnion, 0, len(content))
	for _, block := range content {
		switch block.Type {
		case "text":
			params = append(params, anthropicsdk.NewTextBlock(block.Text))
		case "tool_use":
			params = append(params, anthropicsdk.NewToolUseBlock(block.ID, block.Input, block.Name))
		}
	}
	return anthropicsdk.NewAssistantMessage(params...)
}

// extractText returns the text of the first text block in content.
func extractText(content []anthropicsdk.ContentBlockUnion) string {
	for _, block := range content {
		if block.Type == "text" {
			return block.Text
		}
	}
	return ""
}
