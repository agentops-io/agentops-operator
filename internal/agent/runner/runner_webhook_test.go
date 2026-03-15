package runner_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arkonis-dev/ark-operator/internal/agent/config"
	"github.com/arkonis-dev/ark-operator/internal/agent/mcp"
	"github.com/arkonis-dev/ark-operator/internal/agent/queue"
	"github.com/arkonis-dev/ark-operator/internal/agent/runner"
)

const submitSubtaskTool = "submit_subtask"

// TestRunner_WebhookTool_Dispatch verifies that the runner calls an inline webhook
// tool's URL when invoked and returns the response body.
func TestRunner_WebhookTool_Dispatch(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"headlines":["AI advances","New research"]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Model:        "claude-sonnet-4-6",
		SystemPrompt: "test",
		WebhookTools: []config.WebhookToolConfig{
			{Name: "fetch_news", Description: "Get news", URL: srv.URL, Method: "POST"},
		},
	}
	mgr, _ := mcp.NewManager(nil)
	r := runner.New(cfg, mgr, &mockProvider{result: "ok"}, nil)

	found := false
	for _, tool := range r.AllTools() {
		if tool.Name == "fetch_news" {
			found = true
		}
	}
	if !found {
		t.Fatal("fetch_news not in AllTools")
	}

	input := json.RawMessage(`{"topic":"AI","limit":5}`)
	result, err := r.CallTool(context.Background(), "fetch_news", input)
	if err != nil {
		t.Fatalf("CallTool error: %v", err)
	}
	if !strings.Contains(result, "AI advances") {
		t.Errorf("unexpected result: %q", result)
	}
	if !strings.Contains(gotBody, "topic") {
		t.Errorf("server did not receive input body, got: %q", gotBody)
	}
}

// TestRunner_WebhookTool_ServerError verifies that a non-2xx response is surfaced as an error.
func TestRunner_WebhookTool_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Model:        "claude-sonnet-4-6",
		SystemPrompt: "test",
		WebhookTools: []config.WebhookToolConfig{
			{Name: "broken_tool", URL: srv.URL, Method: "POST"},
		},
	}
	mgr, _ := mcp.NewManager(nil)
	r := runner.New(cfg, mgr, &mockProvider{}, nil)

	_, err := r.CallTool(context.Background(), "broken_tool", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for non-2xx response, got nil")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("expected 503 in error, got: %v", err)
	}
}

// TestRunner_SubmitSubtask_WithQueue verifies submit_subtask appears in AllTools when a queue is set.
func TestRunner_SubmitSubtask_WithQueue(t *testing.T) {
	q := &queue.Queue{}
	cfg := &config.Config{Model: "claude-sonnet-4-6", SystemPrompt: "test"}
	mgr, _ := mcp.NewManager(nil)
	r := runner.New(cfg, mgr, &mockProvider{}, q)

	found := false
	for _, tool := range r.AllTools() {
		if tool.Name == submitSubtaskTool {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected %q in AllTools when queue is set, got: %v", submitSubtaskTool, r.AllTools())
	}
}

// TestRunner_SubmitSubtask_NotPresent_WithoutQueue verifies submit_subtask is absent when queue is nil.
func TestRunner_SubmitSubtask_NotPresent_WithoutQueue(t *testing.T) {
	cfg := &config.Config{Model: "claude-sonnet-4-6", SystemPrompt: "test"}
	mgr, _ := mcp.NewManager(nil)
	r := runner.New(cfg, mgr, &mockProvider{}, nil)

	for _, tool := range r.AllTools() {
		if tool.Name == submitSubtaskTool {
			t.Fatalf("expected %q to be absent when queue is nil", submitSubtaskTool)
		}
	}
}

// TestRunner_CallTool_UnknownTool verifies that calling an unknown tool returns an error.
func TestRunner_CallTool_UnknownTool(t *testing.T) {
	cfg := &config.Config{Model: "claude-sonnet-4-6", SystemPrompt: "test"}
	mgr, _ := mcp.NewManager(nil)
	r := runner.New(cfg, mgr, &mockProvider{}, nil)

	_, err := r.CallTool(context.Background(), "nonexistent_tool", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for unknown tool, got nil")
	}
}
