package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	arkonisv1alpha1 "github.com/arkonis-dev/ark-operator/api/v1alpha1"
	"github.com/arkonis-dev/ark-operator/internal/agent/queue"
)

// TriggerWebhookServer handles inbound HTTP requests that fire webhook-type ArkEvents.
//
// Async (default):
//
//	POST /triggers/{namespace}/{name}/fire
//	→ 202 Accepted  { "fired": true, "firedAt": "...", "trigger": "...", "targets": N }
//
// Sync mode — holds the connection open until the flow completes:
//
//	POST /triggers/{namespace}/{name}/fire?mode=sync
//	POST /triggers/{namespace}/{name}/fire?mode=sync&timeout=30s
//	→ 200 OK        { "status": "succeeded", "output": "...", "durationMs": N, "tokenUsage": { "inputTokens": N, "outputTokens": N, "totalTokens": N } }
//	→ 500           { "status": "failed",    "error":  "..." }
//	→ 504           { "error": "timed out" }
//
// SSE mode — streams progress events and individual tokens as the flow runs:
//
//	POST /triggers/{namespace}/{name}/fire?mode=sse
//	POST /triggers/{namespace}/{name}/fire?mode=sse&timeout=5m
//	→ text/event-stream
//	  event: step.started
//	  data: {"step":"<name>","phase":"Running"}
//	  event: token
//	  data: {"token":"<text chunk>"}     (one event per generated token)
//	  event: step.completed
//	  data: {"step":"<name>","output":"...","tokenUsage":{...},"durationMs":N}
//	  event: flow.completed
//	  data: {"status":"succeeded","output":"...","tokenUsage":{...},"durationMs":N}
//	  event: flow.failed
//	  data: {"status":"failed","error":"...","durationMs":N}
//	  event: error
//	  data: {"error":"timed out"}
//
// Sync and SSE modes require exactly one target flow. The default timeout is 60s; maximum is 5m.
//
// Authentication: pass the trigger's token as a Bearer token in the Authorization
// header or as the `token` query parameter. The token is stored in a Secret named
// <trigger-name>-webhook-token in the same namespace.
//
// The request body is optional JSON. Fields are available in target input templates
// as {{ .trigger.body.<field> }}.
type TriggerWebhookServer struct {
	reconciler   *ArkEventReconciler
	taskQueueURL string

	rdbOnce sync.Once
	rdb     *redis.Client
}

// NewTriggerWebhookServer returns a new TriggerWebhookServer.
// taskQueueURL is the Redis connection string used for token streaming (may be empty to disable streaming).
func NewTriggerWebhookServer(r *ArkEventReconciler, taskQueueURL string) *TriggerWebhookServer {
	return &TriggerWebhookServer{reconciler: r, taskQueueURL: taskQueueURL}
}

// getRedis returns a lazily-initialized Redis client, or nil if no URL is configured.
func (s *TriggerWebhookServer) getRedis() *redis.Client {
	s.rdbOnce.Do(func() {
		if s.taskQueueURL != "" {
			opts, err := redis.ParseURL(s.taskQueueURL)
			if err != nil {
				opts = &redis.Options{Addr: s.taskQueueURL}
			}
			s.rdb = redis.NewClient(opts)
		}
	})
	return s.rdb
}

const (
	defaultSyncTimeout = 60 * time.Second
	maxSyncTimeout     = 5 * time.Minute
	pollInterval       = 500 * time.Millisecond
)

// ServeHTTP implements http.Handler.
func (s *TriggerWebhookServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := log.FromContext(ctx)

	// Expect: /triggers/{namespace}/{name}/fire
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "triggers" || parts[3] != "fire" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	namespace, name := parts[1], parts[2]

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Load the trigger.
	trigger := &arkonisv1alpha1.ArkEvent{}
	if err := s.reconciler.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, trigger); err != nil {
		http.Error(w, "trigger not found", http.StatusNotFound)
		return
	}

	if trigger.Spec.Source.Type != arkonisv1alpha1.TriggerSourceWebhook {
		http.Error(w, "trigger is not of type webhook", http.StatusBadRequest)
		return
	}

	if trigger.Spec.Suspended {
		http.Error(w, "trigger is suspended", http.StatusServiceUnavailable)
		return
	}

	// Validate token.
	token := bearerToken(r)
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	if !s.validToken(ctx, trigger, token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Parse optional JSON body.
	var body map[string]any
	if r.ContentLength != 0 {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err == nil && len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
	}

	now := time.Now().UTC()

	// Determine mode: async (default), sync, or sse.
	mode := r.URL.Query().Get("mode")

	// In SSE mode, generate a unique Redis List key so the agent can publish token chunks.
	var streamKey string
	if mode == "sse" && s.getRedis() != nil {
		streamKey = fmt.Sprintf("chunks:%s:%s:%d", namespace, name, now.UnixNano())
	}

	fireCtx := FireContext{
		Name:      trigger.Name,
		FiredAt:   now.Format(time.RFC3339),
		Body:      body,
		StreamKey: streamKey,
	}

	flows, err := s.reconciler.fire(ctx, trigger, fireCtx)
	if err != nil {
		logger.Error(err, "firing webhook trigger", "trigger", name)
		http.Error(w, fmt.Sprintf("fire failed: %v", err), http.StatusInternalServerError)
		return
	}

	nowMeta := metav1.NewTime(now)
	trigger.Status.LastFiredAt = &nowMeta
	trigger.Status.FiredCount++
	trigger.Status.ObservedGeneration = trigger.Generation
	if err := s.reconciler.Status().Update(ctx, trigger); err != nil {
		logger.Error(err, "updating trigger status after webhook fire")
	}

	switch mode {
	case "sync":
		s.handleSync(w, r, flows)
		return
	case "sse":
		s.handleSSE(w, r, flows, streamKey)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"fired":   true,
		"firedAt": now.Format(time.RFC3339),
		"trigger": name,
		"targets": len(trigger.Spec.Targets),
	})
}

// handleSync waits for the single dispatched flow to finish and writes the result.
func (s *TriggerWebhookServer) handleSync(w http.ResponseWriter, r *http.Request, flows []*arkonisv1alpha1.ArkFlow) {
	if len(flows) != 1 {
		http.Error(w, "sync mode requires exactly one target flow", http.StatusBadRequest)
		return
	}

	timeout := parseSyncTimeout(r)

	syncCtx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	start := time.Now()
	result, err := s.waitForFlow(syncCtx, flows[0])
	if err != nil {
		if syncCtx.Err() != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGatewayTimeout)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":      fmt.Sprintf("timed out after %s waiting for flow to complete", timeout),
				"durationMs": time.Since(start).Milliseconds(),
			})
			return
		}
		http.Error(w, fmt.Sprintf("waiting for flow: %v", err), http.StatusInternalServerError)
		return
	}

	result.DurationMs = time.Since(start).Milliseconds()

	status := http.StatusOK
	if result.Status == "failed" {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(result)
}

// sseMsg is a single SSE event payload used internally.
type sseMsg struct {
	event string
	data  any
}

// handleSSE streams Server-Sent Events as the flow progresses.
// streamKey, if non-empty, enables token-level streaming: each generated token is emitted
// as a "token" SSE event before the terminal step/flow events arrive.
func (s *TriggerWebhookServer) handleSSE(w http.ResponseWriter, r *http.Request, flows []*arkonisv1alpha1.ArkFlow, streamKey string) {
	if len(flows) != 1 {
		http.Error(w, "sse mode requires exactly one target flow", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	timeout := parseSyncTimeout(r)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sseCtx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	// If streaming is enabled, drain token chunks from the Redis List in a goroutine.
	tokenCh := s.startTokenStream(sseCtx, streamKey)

	flowKey := types.NamespacedName{Name: flows[0].Name, Namespace: flows[0].Namespace}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	start := time.Now()
	stepPhases := map[string]arkonisv1alpha1.ArkFlowStepPhase{}

	for {
		// Drain any pending token chunks before blocking on K8s poll.
		drainTokens(w, flusher, &tokenCh)

		select {
		case <-sseCtx.Done():
			sseEvent(w, flusher, "error", map[string]any{"error": fmt.Sprintf("timed out after %s", timeout)})
			return
		case msg, ok := <-tokenCh:
			if ok {
				sseEvent(w, flusher, msg.event, msg.data)
			}
		case <-ticker.C:
			f := &arkonisv1alpha1.ArkFlow{}
			if err := s.reconciler.Get(sseCtx, flowKey, f); err != nil {
				sseEvent(w, flusher, "error", map[string]any{"error": err.Error()})
				return
			}
			s.emitStepEvents(w, flusher, f, stepPhases)
			if s.emitFlowTerminal(w, flusher, f, tokenCh, start) {
				return
			}
		}
	}
}

// parseSyncTimeout reads the ?timeout query parameter and clamps it to maxSyncTimeout.
func parseSyncTimeout(r *http.Request) time.Duration {
	timeout := defaultSyncTimeout
	if raw := r.URL.Query().Get("timeout"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			timeout = d
		}
	}
	if timeout > maxSyncTimeout {
		return maxSyncTimeout
	}
	return timeout
}

// startTokenStream starts a goroutine that reads token chunks from the Redis List identified
// by streamKey and forwards them as sseMsg values. Returns a closed nil-channel when
// streamKey is empty or Redis is not configured (no-op).
func (s *TriggerWebhookServer) startTokenStream(ctx context.Context, streamKey string) chan sseMsg {
	ch := make(chan sseMsg, 256)
	rdb := s.getRedis()
	if streamKey == "" || rdb == nil {
		close(ch)
		return ch
	}
	go func() {
		defer close(ch)
		for {
			vals, err := rdb.BLPop(ctx, 1*time.Second, streamKey).Result()
			if err == redis.Nil {
				continue
			}
			if err != nil {
				return
			}
			if len(vals) < 2 {
				continue
			}
			if vals[1] == queue.StreamDone {
				return
			}
			select {
			case ch <- sseMsg{"token", map[string]any{"token": vals[1]}}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

// drainTokens non-blockingly reads all pending messages from tokenCh and writes them as SSE events.
// Sets *ch to nil if the channel is closed.
func drainTokens(w http.ResponseWriter, flusher http.Flusher, ch *chan sseMsg) {
	if *ch == nil {
		return
	}
	for {
		select {
		case msg, ok := <-*ch:
			if !ok {
				*ch = nil
				return
			}
			sseEvent(w, flusher, msg.event, msg.data)
		default:
			return
		}
	}
}

// emitStepEvents emits SSE events for any step phase transitions detected since the last call.
func (s *TriggerWebhookServer) emitStepEvents(
	w http.ResponseWriter,
	flusher http.Flusher,
	f *arkonisv1alpha1.ArkFlow,
	stepPhases map[string]arkonisv1alpha1.ArkFlowStepPhase,
) {
	for _, step := range f.Status.Steps {
		if stepPhases[step.Name] == step.Phase {
			continue
		}
		stepPhases[step.Name] = step.Phase
		switch step.Phase {
		case arkonisv1alpha1.ArkFlowStepPhaseRunning:
			sseEvent(w, flusher, "step.started", map[string]any{"step": step.Name, "phase": "Running"})
		case arkonisv1alpha1.ArkFlowStepPhaseSucceeded:
			payload := map[string]any{
				"step":       step.Name,
				"output":     step.Output,
				"durationMs": step.CompletionTime.Sub(step.StartTime.Time).Milliseconds(),
			}
			if step.TokenUsage != nil {
				payload["tokenUsage"] = step.TokenUsage
			}
			sseEvent(w, flusher, "step.completed", payload)
		case arkonisv1alpha1.ArkFlowStepPhaseFailed:
			sseEvent(w, flusher, "step.failed", map[string]any{"step": step.Name})
		}
	}
}

// emitFlowTerminal emits the terminal flow event if the flow has reached a final phase.
// Returns true if a terminal event was emitted (caller should return).
func (s *TriggerWebhookServer) emitFlowTerminal(
	w http.ResponseWriter,
	flusher http.Flusher,
	f *arkonisv1alpha1.ArkFlow,
	tokenCh chan sseMsg,
	start time.Time,
) bool {
	switch f.Status.Phase {
	case arkonisv1alpha1.ArkFlowPhaseSucceeded:
		// Drain remaining token chunks before the final event.
		if tokenCh != nil {
			for msg := range tokenCh {
				sseEvent(w, flusher, msg.event, msg.data)
			}
		}
		payload := map[string]any{
			"status":     "succeeded",
			"output":     f.Status.Output,
			"durationMs": time.Since(start).Milliseconds(),
		}
		if f.Status.TotalTokenUsage != nil {
			payload["tokenUsage"] = f.Status.TotalTokenUsage
		}
		sseEvent(w, flusher, "flow.completed", payload)
		return true
	case arkonisv1alpha1.ArkFlowPhaseFailed:
		msg := "flow failed"
		for _, c := range f.Status.Conditions {
			if c.Type == arkonisv1alpha1.ConditionReady && c.Status == metav1.ConditionFalse {
				msg = c.Message
				break
			}
		}
		sseEvent(w, flusher, "flow.failed", map[string]any{
			"status":     "failed",
			"error":      msg,
			"durationMs": time.Since(start).Milliseconds(),
		})
		return true
	}
	return false
}

// sseEvent writes a single SSE event and flushes the connection.
func sseEvent(w http.ResponseWriter, flusher http.Flusher, event string, data any) {
	b, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	flusher.Flush()
}

// syncResult is the response body for sync mode.
type syncResult struct {
	Status     string                      `json:"status"`
	Output     string                      `json:"output,omitempty"`
	Error      string                      `json:"error,omitempty"`
	DurationMs int64                       `json:"durationMs"`
	TokenUsage *arkonisv1alpha1.TokenUsage `json:"tokenUsage,omitempty"`
}

// waitForFlow polls the flow until it reaches a terminal phase or ctx is cancelled.
func (s *TriggerWebhookServer) waitForFlow(ctx context.Context, flow *arkonisv1alpha1.ArkFlow) (*syncResult, error) {
	key := types.NamespacedName{Name: flow.Name, Namespace: flow.Namespace}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			f := &arkonisv1alpha1.ArkFlow{}
			if err := s.reconciler.Get(ctx, key, f); err != nil {
				return nil, err
			}
			switch f.Status.Phase {
			case arkonisv1alpha1.ArkFlowPhaseSucceeded:
				return &syncResult{Status: "succeeded", Output: f.Status.Output, TokenUsage: f.Status.TotalTokenUsage}, nil
			case arkonisv1alpha1.ArkFlowPhaseFailed:
				msg := "flow failed"
				for _, c := range f.Status.Conditions {
					if c.Type == arkonisv1alpha1.ConditionReady && c.Status == metav1.ConditionFalse {
						msg = c.Message
						break
					}
				}
				return &syncResult{Status: "failed", Error: msg}, nil
			}
		}
	}
}

// validToken checks the provided token against the one stored in the trigger's webhook token Secret.
func (s *TriggerWebhookServer) validToken(ctx context.Context, trigger *arkonisv1alpha1.ArkEvent, token string) bool {
	if token == "" {
		return false
	}
	stored, err := webhookTokenFromSecret(ctx, s.reconciler.Client, trigger)
	if err != nil {
		return false
	}
	return token == stored
}

// webhookTokenFromSecret reads the token from the trigger's token Secret.
// Secret name: <trigger-name>-webhook-token, key: token.
func webhookTokenFromSecret(ctx context.Context, c client.Client, trigger *arkonisv1alpha1.ArkEvent) (string, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{
		Name:      trigger.Name + "-webhook-token",
		Namespace: trigger.Namespace,
	}, secret); err != nil {
		return "", err
	}
	return string(secret.Data["token"]), nil
}

// bearerToken extracts the Bearer token from the Authorization header.
func bearerToken(r *http.Request) string {
	if token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return token
	}
	return ""
}
