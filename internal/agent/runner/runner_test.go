package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/arkonis-dev/ark-operator/internal/agent/config"
	"github.com/arkonis-dev/ark-operator/internal/agent/mcp"
	"github.com/arkonis-dev/ark-operator/internal/agent/queue"
	"github.com/arkonis-dev/ark-operator/internal/agent/runner"
)

type mockProvider struct {
	result string
	err    error
}

func (m *mockProvider) RunTask(
	_ context.Context,
	_ *config.Config,
	_ queue.Task,
	_ []mcp.Tool,
	_ func(context.Context, string, json.RawMessage) (string, error),
) (string, queue.TokenUsage, error) {
	return m.result, queue.TokenUsage{}, m.err
}

func newMCPManager(t *testing.T) *mcp.Manager {
	t.Helper()
	mgr, err := mcp.NewManager(nil)
	if err != nil {
		t.Fatalf("mcp.NewManager: %v", err)
	}
	return mgr
}

func TestRunner_RunTask_Success(t *testing.T) {
	cfg := &config.Config{Model: "claude-sonnet-4-6", SystemPrompt: "test"}
	r := runner.New(cfg, newMCPManager(t), &mockProvider{result: "done"}, nil)
	result, _, err := r.RunTask(context.Background(), queue.Task{ID: "1", Prompt: "hello"})
	if err != nil {
		t.Fatalf("RunTask error: %v", err)
	}
	if result != "done" {
		t.Errorf("RunTask = %q, want %q", result, "done")
	}
}

func TestRunner_RunTask_ProviderError(t *testing.T) {
	cfg := &config.Config{Model: "claude-sonnet-4-6", SystemPrompt: "test"}
	r := runner.New(cfg, newMCPManager(t), &mockProvider{err: errors.New("api error")}, nil)
	_, _, err := r.RunTask(context.Background(), queue.Task{ID: "2", Prompt: "hello"})
	if err == nil {
		t.Fatal("expected error from provider, got nil")
	}
}
