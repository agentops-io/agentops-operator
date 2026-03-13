package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := LoadConfig()
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Connect to MCP servers and discover tools.
	// Failures are non-fatal: the agent runs with whatever tools were successfully discovered.
	mcpManager, err := NewMCPManager(cfg.MCPServers)
	if err != nil {
		logger.Warn("one or more MCP servers unavailable, continuing with reduced toolset", "error", err)
		mcpManager = &MCPManager{}
	}
	defer mcpManager.Close()

	provider, err := NewProvider(cfg.Provider)
	if err != nil {
		logger.Error("unsupported LLM provider", "provider", cfg.Provider, "error", err)
		os.Exit(1)
	}

	runner := NewRunner(cfg, mcpManager, provider)

	queue := NewTaskQueue(cfg.TaskQueueURL, cfg.MaxRetries)
	defer queue.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	logger.Info("agent runtime started",
		"model", cfg.Model,
		"mcp_servers", len(cfg.MCPServers),
	)

	// Semantic health probe runs in the background on :8080.
	go ServeHealthProbe(":8080", runner, cfg.ValidatorPrompt)

	// Poll the task queue until shutdown.
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutting down")
			return
		default:
		}

		task, err := queue.Poll(ctx)
		if err != nil {
			logger.Error("queue poll error", "error", err)
			continue
		}
		if task == nil {
			continue
		}

		go func(t Task) {
			taskCtx, taskCancel := context.WithTimeout(ctx, TaskTimeout(cfg))
			defer taskCancel()

			result, err := runner.RunTask(taskCtx, t)
			if err != nil {
				logger.Error("task failed", "task_id", t.ID, "error", err)
				queue.Nack(t, err.Error())
				return
			}
			logger.Info("task completed", "task_id", t.ID)
			queue.Ack(t.ID, result)
		}(*task)
	}
}
