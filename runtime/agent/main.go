package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/arkonis-dev/ark-operator/internal/agent/config"
	"github.com/arkonis-dev/ark-operator/internal/agent/health"
	"github.com/arkonis-dev/ark-operator/internal/agent/mcp"
	"github.com/arkonis-dev/ark-operator/internal/agent/providers"
	"github.com/arkonis-dev/ark-operator/internal/agent/queue"
	"github.com/arkonis-dev/ark-operator/internal/agent/runner"

	// Register LLM provider implementations via their init() functions.
	_ "github.com/arkonis-dev/ark-operator/internal/agent/providers/anthropic"
	_ "github.com/arkonis-dev/ark-operator/internal/agent/providers/openai"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Connect to MCP servers and discover tools.
	// Failures are non-fatal: the agent runs with whatever tools were successfully discovered.
	mcpManager, err := mcp.NewManager(cfg.MCPServers)
	if err != nil {
		logger.Warn("one or more MCP servers unavailable, continuing with reduced toolset", "error", err)
		mcpManager, _ = mcp.NewManager(nil)
	}
	defer mcpManager.Close()

	provider, err := providers.New(cfg.Provider)
	if err != nil {
		logger.Error("unsupported LLM provider", "provider", cfg.Provider, "error", err)
		os.Exit(1)
	}

	// Queue must be created before runner so submit_subtask is available as a built-in tool.
	q := queue.New(cfg.TaskQueueURL, cfg.MaxRetries)
	defer q.Close()

	r := runner.New(cfg, mcpManager, provider, q)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	logger.Info("agent runtime started",
		"model", cfg.Model,
		"provider", cfg.Provider,
		"mcp_servers", len(cfg.MCPServers),
	)

	// Semantic health probe runs in the background on :8080.
	go health.ServeProbe(":8080", r, cfg.ValidatorPrompt)

	// Poll the task queue until shutdown.
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return
		default:
		}

		task, err := q.Poll(ctx)
		if err != nil {
			logger.Error("queue poll error", "error", err)
			continue
		}
		if task == nil {
			continue
		}

		go func(t queue.Task) {
			taskCtx, taskCancel := context.WithTimeout(ctx, config.TaskTimeout(cfg))
			defer taskCancel()

			result, usage, err := r.RunTask(taskCtx, t)
			if err != nil {
				logger.Error("task failed", "task_id", t.ID, "error", err)
				q.Nack(t, err.Error())
				return
			}
			logger.Info("task completed", "task_id", t.ID,
				"input_tokens", usage.InputTokens,
				"output_tokens", usage.OutputTokens,
			)
			q.Ack(t.ID, result, usage)
		}(*task)
	}
}
