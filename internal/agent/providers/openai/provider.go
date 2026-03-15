// Package openai implements LLMProvider for OpenAI-compatible APIs.
// Importing this package (even with a blank import) registers the "openai"
// provider in the global registry.
//
// The provider reads OPENAI_API_KEY from the environment. Any OpenAI-compatible
// endpoint can be targeted by setting OPENAI_BASE_URL (e.g. a local Ollama
// instance or Azure OpenAI).
package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	openaisdk "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"

	"github.com/arkonis-dev/ark-operator/internal/agent/config"
	"github.com/arkonis-dev/ark-operator/internal/agent/mcp"
	"github.com/arkonis-dev/ark-operator/internal/agent/providers"
	"github.com/arkonis-dev/ark-operator/internal/agent/queue"
)

func init() {
	providers.Register("openai", func() providers.LLMProvider { return &Provider{} })
}

// Provider implements providers.LLMProvider using the OpenAI Chat Completions API.
type Provider struct{}

// RunTask executes a task through the OpenAI agentic tool-use loop.
// It keeps calling the API until the model stops requesting tool calls.
// Token usage is accumulated across all API calls and returned with the result.
// If chunkFn is non-nil, the final text turn is streamed token-by-token.
func (p *Provider) RunTask(
	ctx context.Context,
	cfg *config.Config,
	task queue.Task,
	tools []mcp.Tool,
	callTool func(context.Context, string, json.RawMessage) (string, error),
	chunkFn func(string),
) (string, queue.TokenUsage, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return "", queue.TokenUsage{}, fmt.Errorf("OPENAI_API_KEY is not set; required for the openai provider")
	}

	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL := os.Getenv("OPENAI_BASE_URL"); baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	client := openaisdk.NewClient(opts...)

	messages := []openaisdk.ChatCompletionMessageParamUnion{
		openaisdk.SystemMessage(cfg.SystemPrompt),
		openaisdk.UserMessage(task.Prompt),
	}

	openaiTools := toOpenAITools(tools)
	var usage queue.TokenUsage

	for {
		params := openaisdk.ChatCompletionNewParams{
			Model:    cfg.Model,
			Messages: messages,
		}
		if cfg.MaxTokensPerCall > 0 {
			params.MaxCompletionTokens = openaisdk.Int(int64(cfg.MaxTokensPerCall))
		}
		if len(openaiTools) > 0 {
			params.Tools = openaiTools
		}

		// Use streaming for the final text turn when chunkFn is set and no tools are active.
		if chunkFn != nil && len(openaiTools) == 0 {
			text, turnUsage, err := p.runStreamingTurn(ctx, client, params, chunkFn)
			if err != nil {
				return "", usage, err
			}
			usage.InputTokens += turnUsage.InputTokens
			usage.OutputTokens += turnUsage.OutputTokens
			return text, usage, nil
		}

		resp, err := client.Chat.Completions.New(ctx, params)
		if err != nil {
			return "", usage, fmt.Errorf("openai API error: %w", err)
		}

		// Accumulate token usage across all turns in the tool-use loop.
		usage.InputTokens += resp.Usage.PromptTokens
		usage.OutputTokens += resp.Usage.CompletionTokens

		if len(resp.Choices) == 0 {
			return "", usage, fmt.Errorf("openai: empty choices in response")
		}
		choice := resp.Choices[0]

		// Append the assistant turn before deciding what to do next.
		messages = append(messages, assistantMessage(choice.Message))

		if choice.FinishReason != "tool_calls" || len(choice.Message.ToolCalls) == 0 {
			text := choice.Message.Content
			if chunkFn != nil {
				chunkFn(text)
			}
			return text, usage, nil
		}

		// Execute all requested tool calls and append results.
		for _, tc := range choice.Message.ToolCalls {
			output, execErr := callTool(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			if execErr != nil {
				messages = append(messages, openaisdk.ToolMessage(execErr.Error(), tc.ID))
				continue
			}
			messages = append(messages, openaisdk.ToolMessage(output, tc.ID))
		}
	}
}

// toOpenAITools converts generic mcp.Tools into the OpenAI ChatCompletionToolParam format.
func toOpenAITools(tools []mcp.Tool) []openaisdk.ChatCompletionToolParam {
	params := make([]openaisdk.ChatCompletionToolParam, 0, len(tools))
	for _, t := range tools {
		fn := shared.FunctionDefinitionParam{
			Name:        t.Name,
			Description: openaisdk.String(t.Description),
		}
		if len(t.InputSchema) > 0 {
			fn.Parameters = shared.FunctionParameters(rawToMap(t.InputSchema))
		}
		params = append(params, openaisdk.ChatCompletionToolParam{
			Function: fn,
		})
	}
	return params
}

// rawToMap converts a json.RawMessage into map[string]any for FunctionParameters.
func rawToMap(raw json.RawMessage) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{}
	}
	return m
}

// runStreamingTurn calls the OpenAI streaming API and forwards each text delta to chunkFn.
func (p *Provider) runStreamingTurn(
	ctx context.Context,
	client openaisdk.Client,
	params openaisdk.ChatCompletionNewParams,
	chunkFn func(string),
) (string, queue.TokenUsage, error) {
	stream := client.Chat.Completions.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()

	var sb strings.Builder
	var usage queue.TokenUsage

	for stream.Next() {
		chunk := stream.Current()
		if chunk.Usage.CompletionTokens > 0 {
			usage.InputTokens = chunk.Usage.PromptTokens
			usage.OutputTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta.Content
			if delta != "" {
				chunkFn(delta)
				sb.WriteString(delta)
			}
		}
	}
	if err := stream.Err(); err != nil {
		return "", usage, fmt.Errorf("openai streaming error: %w", err)
	}
	return sb.String(), usage, nil
}

// assistantMessage converts a ChatCompletionMessage response into a MessageParamUnion
// that can be appended to the conversation history for the next API call.
func assistantMessage(msg openaisdk.ChatCompletionMessage) openaisdk.ChatCompletionMessageParamUnion {
	toolCalls := make([]openaisdk.ChatCompletionMessageToolCallParam, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		toolCalls = append(toolCalls, openaisdk.ChatCompletionMessageToolCallParam{
			ID: tc.ID,
			Function: openaisdk.ChatCompletionMessageToolCallFunctionParam{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		})
	}
	return openaisdk.ChatCompletionMessageParamUnion{
		OfAssistant: &openaisdk.ChatCompletionAssistantMessageParam{
			Content:   openaisdk.ChatCompletionAssistantMessageParamContentUnion{OfString: openaisdk.String(msg.Content)},
			ToolCalls: toolCalls,
		},
	}
}
