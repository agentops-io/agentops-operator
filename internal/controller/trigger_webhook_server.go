package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	arkonisv1alpha1 "github.com/arkonis-dev/ark-operator/api/v1alpha1"
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
// Sync mode requires exactly one target flow. The default timeout is 60s; maximum is 5m.
//
// Authentication: pass the trigger's token as a Bearer token in the Authorization
// header or as the `token` query parameter. The token is stored in a Secret named
// <trigger-name>-webhook-token in the same namespace.
//
// The request body is optional JSON. Fields are available in target input templates
// as {{ .trigger.body.<field> }}.
type TriggerWebhookServer struct {
	reconciler *ArkEventReconciler
}

// NewTriggerWebhookServer returns a new TriggerWebhookServer.
func NewTriggerWebhookServer(r *ArkEventReconciler) *TriggerWebhookServer {
	return &TriggerWebhookServer{reconciler: r}
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
	fireCtx := FireContext{
		Name:    trigger.Name,
		FiredAt: now.Format(time.RFC3339),
		Body:    body,
	}

	// Determine sync vs async mode.
	syncMode := r.URL.Query().Get("mode") == "sync"

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

	if syncMode {
		s.handleSync(w, r, flows)
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

	timeout := defaultSyncTimeout
	if raw := r.URL.Query().Get("timeout"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil {
			timeout = d
		}
	}
	if timeout > maxSyncTimeout {
		timeout = maxSyncTimeout
	}

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
					if c.Type == "Ready" && c.Status == metav1.ConditionFalse {
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
