/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/redis/go-redis/v9"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	arkonisv1alpha1 "github.com/arkonis-dev/ark-operator/api/v1alpha1"
)

const (
	taskStream        = "agent-tasks"
	resultsStream     = "agent-tasks-results"
	pipelineRequeueIn = 5 * time.Second
)

// +kubebuilder:rbac:groups=arkonis.dev,resources=arkflows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkflows/finalizers,verbs=update
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkagents,verbs=get;list;watch

// ArkFlowReconciler reconciles a ArkFlow object.
type ArkFlowReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	TaskQueueURL string

	redisOnce sync.Once
	rdb       *redis.Client
}

func (r *ArkFlowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	flow := &arkonisv1alpha1.ArkFlow{}
	if err := r.Get(ctx, req.NamespacedName, flow); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !flow.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Skip flows that serve as trigger templates — they are never executed directly.
	// A flow is a template when it is listed as a target in any ArkEvent.
	if isTemplate, err := r.isTriggerTemplate(ctx, flow); err != nil {
		return ctrl.Result{}, err
	} else if isTemplate {
		return ctrl.Result{}, nil
	}

	// Validate DAG and referenced ArkAgents.
	if err := r.validateDAG(flow); err != nil {
		logger.Error(err, "invalid flow DAG")
		r.setCondition(flow, metav1.ConditionFalse, "InvalidDAG", err.Error())
		_ = r.Status().Update(ctx, flow)
		return ctrl.Result{}, nil
	}
	if err := r.validateAgents(ctx, flow); err != nil {
		logger.Info("waiting for ArkAgents", "reason", err.Error())
		r.setCondition(flow, metav1.ConditionFalse, "DeploymentNotFound", err.Error())
		_ = r.Status().Update(ctx, flow)
		return ctrl.Result{}, nil
	}

	// Terminal phases — nothing to do.
	if isTerminalPhase(flow.Status.Phase) {
		flow.Status.ObservedGeneration = flow.Generation
		return ctrl.Result{}, r.Status().Update(ctx, flow)
	}

	// Require Redis for execution.
	rdb := r.getRedis()
	if rdb == nil {
		msg := "TASK_QUEUE_URL not set; flow execution requires Redis"
		logger.Info(msg)
		r.setCondition(flow, metav1.ConditionFalse, "NoTaskQueue", msg)
		_ = r.Status().Update(ctx, flow)
		return ctrl.Result{}, nil
	}

	r.initializeSteps(flow)

	statusByName := buildStatusByName(flow)

	// Check results for in-flight steps.
	if err := r.collectResults(ctx, rdb, flow, statusByName); err != nil {
		logger.Error(err, "collecting step results from Redis")
		return ctrl.Result{}, fmt.Errorf("collecting step results: %w", err)
	}

	r.parseOutputJSON(flow, statusByName)

	templateData := r.buildTemplateData(flow, statusByName)

	// Reset loop steps that need another iteration before submitting new work.
	r.evaluateLoops(flow, statusByName, templateData)

	if err := r.submitPendingSteps(ctx, rdb, flow, statusByName, templateData, logger); err != nil {
		return ctrl.Result{}, err
	}

	r.updateFlowPhase(flow, templateData)

	flow.Status.ObservedGeneration = flow.Generation
	if err := r.Status().Update(ctx, flow); err != nil {
		return ctrl.Result{}, err
	}

	if flow.Status.Phase == arkonisv1alpha1.ArkFlowPhaseRunning {
		return ctrl.Result{RequeueAfter: pipelineRequeueIn}, nil
	}
	return ctrl.Result{}, nil
}

// isTerminalPhase reports whether the flow has reached a final state.
func isTerminalPhase(phase arkonisv1alpha1.ArkFlowPhase) bool {
	return phase == arkonisv1alpha1.ArkFlowPhaseSucceeded || phase == arkonisv1alpha1.ArkFlowPhaseFailed
}

// buildStatusByName returns a name→status pointer map for convenient step lookups.
func buildStatusByName(flow *arkonisv1alpha1.ArkFlow) map[string]*arkonisv1alpha1.ArkFlowStepStatus {
	m := make(map[string]*arkonisv1alpha1.ArkFlowStepStatus, len(flow.Status.Steps))
	for i := range flow.Status.Steps {
		m[flow.Status.Steps[i].Name] = &flow.Status.Steps[i]
	}
	return m
}

// initializeSteps sets up step statuses and marks the flow Running on the first reconcile.
func (r *ArkFlowReconciler) initializeSteps(flow *arkonisv1alpha1.ArkFlow) {
	if flow.Status.Phase != "" {
		return
	}
	now := metav1.Now()
	flow.Status.Phase = arkonisv1alpha1.ArkFlowPhaseRunning
	flow.Status.StartTime = &now
	flow.Status.Steps = make([]arkonisv1alpha1.ArkFlowStepStatus, len(flow.Spec.Steps))
	for i, step := range flow.Spec.Steps {
		flow.Status.Steps[i] = arkonisv1alpha1.ArkFlowStepStatus{
			Name:  step.Name,
			Phase: arkonisv1alpha1.ArkFlowStepPhasePending,
		}
	}
	r.setCondition(flow, metav1.ConditionTrue, "Validated", "Flow DAG is valid; execution started")
}

// parseOutputJSON tries to parse completed step outputs as JSON when the step declared an OutputSchema.
// Parsed results are stored in OutputJSON so downstream templates can reference individual fields.
func (r *ArkFlowReconciler) parseOutputJSON(
	flow *arkonisv1alpha1.ArkFlow,
	statusByName map[string]*arkonisv1alpha1.ArkFlowStepStatus,
) {
	schemaByName := make(map[string]string, len(flow.Spec.Steps))
	for _, step := range flow.Spec.Steps {
		if step.OutputSchema != "" {
			schemaByName[step.Name] = step.OutputSchema
		}
	}
	for name, st := range statusByName {
		if _, hasSchema := schemaByName[name]; !hasSchema {
			continue
		}
		if st.Phase != arkonisv1alpha1.ArkFlowStepPhaseSucceeded || st.Output == "" || st.OutputJSON != "" {
			continue
		}
		if raw := extractJSON(st.Output); raw != "" {
			var check any
			if json.Unmarshal([]byte(raw), &check) == nil {
				st.OutputJSON = raw
			}
		}
	}
}

// extractJSON returns the first JSON object or array found in s.
// It handles two cases:
//  1. The whole string is valid JSON — returned as-is.
//  2. JSON is wrapped in a markdown code fence (```json ... ``` or ``` ... ```) —
//     the fenced block is extracted and returned.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Fast path: already valid JSON.
	var check any
	if json.Unmarshal([]byte(s), &check) == nil {
		return s
	}
	// Look for a fenced code block and extract its content.
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "```") {
			// Strip the opening fence marker (e.g. ```json → empty or language tag).
			continue
		}
		if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") {
			// Collect from this line until the closing fence or end of string.
			start := strings.Index(s, line)
			if start < 0 {
				break
			}
			end := strings.Index(s[start:], "\n```")
			if end < 0 {
				candidate := strings.TrimSpace(s[start:])
				if json.Unmarshal([]byte(candidate), &check) == nil {
					return candidate
				}
				break
			}
			candidate := strings.TrimSpace(s[start : start+end])
			if json.Unmarshal([]byte(candidate), &check) == nil {
				return candidate
			}
			break
		}
	}
	return ""
}

// submitPendingSteps enqueues tasks for every step whose dependencies have all succeeded.
func (r *ArkFlowReconciler) submitPendingSteps(
	ctx context.Context,
	rdb *redis.Client,
	flow *arkonisv1alpha1.ArkFlow,
	statusByName map[string]*arkonisv1alpha1.ArkFlowStepStatus,
	templateData map[string]any,
	logger interface {
		Info(string, ...any)
		Error(error, string, ...any)
	},
) error {
	for _, step := range flow.Spec.Steps {
		st := statusByName[step.Name]
		if st == nil || st.Phase != arkonisv1alpha1.ArkFlowStepPhasePending {
			continue
		}
		if !r.depsSucceeded(step.DependsOn, statusByName) {
			continue
		}
		// Evaluate the conditional guard — skip the step if it evaluates to false.
		if step.If != "" {
			condResult, err := r.resolveTemplate(step.If, templateData)
			if err != nil || !isTruthy(condResult) {
				now := metav1.Now()
				st.Phase = arkonisv1alpha1.ArkFlowStepPhaseSkipped
				st.CompletionTime = &now
				st.Message = "skipped: if condition evaluated to false"
				logger.Info("skipping step", "step", step.Name, "condition", step.If)
				continue
			}
		}
		prompt, err := r.resolvePrompt(step, templateData)
		if err != nil {
			logger.Error(err, "resolving step inputs", "step", step.Name)
			now := metav1.Now()
			st.Phase = arkonisv1alpha1.ArkFlowStepPhaseFailed
			st.CompletionTime = &now
			st.Message = fmt.Sprintf("input template error: %v", err)
			continue
		}
		taskID, err := r.submitTask(ctx, rdb, prompt)
		if err != nil {
			logger.Error(err, "submitting task to Redis", "step", step.Name)
			_ = r.Status().Update(ctx, flow)
			return fmt.Errorf("submitting task for step %q: %w", step.Name, err)
		}
		now := metav1.Now()
		st.Phase = arkonisv1alpha1.ArkFlowStepPhaseRunning
		st.TaskID = taskID
		st.StartTime = &now
		logger.Info("submitted task", "step", step.Name, "taskID", taskID)
	}
	return nil
}

// enforceTimeout sets the flow to Failed if the timeout has elapsed. Returns true if timed out.
func (r *ArkFlowReconciler) enforceTimeout(flow *arkonisv1alpha1.ArkFlow, now metav1.Time) bool {
	if flow.Spec.TimeoutSeconds <= 0 || flow.Status.StartTime == nil {
		return false
	}
	deadline := flow.Status.StartTime.Add(time.Duration(flow.Spec.TimeoutSeconds) * time.Second)
	if !now.After(deadline) {
		return false
	}
	flow.Status.Phase = arkonisv1alpha1.ArkFlowPhaseFailed
	flow.Status.CompletionTime = &now
	r.setCondition(flow, metav1.ConditionFalse, "TimedOut",
		fmt.Sprintf("flow exceeded timeout of %ds", flow.Spec.TimeoutSeconds))
	return true
}

// sumStepTokens accumulates input/output token counts across all completed steps
// and updates flow.Status.TotalTokenUsage. Returns the grand total.
func sumStepTokens(flow *arkonisv1alpha1.ArkFlow) int64 {
	var totalIn, totalOut int64
	for _, st := range flow.Status.Steps {
		if st.TokenUsage != nil {
			totalIn += st.TokenUsage.InputTokens
			totalOut += st.TokenUsage.OutputTokens
		}
	}
	if totalIn > 0 || totalOut > 0 {
		flow.Status.TotalTokenUsage = &arkonisv1alpha1.TokenUsage{
			InputTokens:  totalIn,
			OutputTokens: totalOut,
			TotalTokens:  totalIn + totalOut,
		}
	}
	return totalIn + totalOut
}

// updateFlowPhase inspects step statuses and transitions the flow to Succeeded or Failed.
func (r *ArkFlowReconciler) updateFlowPhase(
	flow *arkonisv1alpha1.ArkFlow,
	templateData map[string]any,
) {
	now := metav1.Now()

	if r.enforceTimeout(flow, now) {
		return
	}

	totalTokens := sumStepTokens(flow)

	// Enforce token budget.
	if flow.Spec.MaxTokens > 0 && totalTokens > flow.Spec.MaxTokens {
		flow.Status.Phase = arkonisv1alpha1.ArkFlowPhaseFailed
		flow.Status.CompletionTime = &now
		r.setCondition(flow, metav1.ConditionFalse, "BudgetExceeded",
			fmt.Sprintf("token budget of %d exceeded: used %d", flow.Spec.MaxTokens, totalTokens))
		return
	}

	failed, allDone := false, true
	for _, st := range flow.Status.Steps {
		switch st.Phase {
		case arkonisv1alpha1.ArkFlowStepPhaseFailed:
			failed = true
		case arkonisv1alpha1.ArkFlowStepPhaseSucceeded, arkonisv1alpha1.ArkFlowStepPhaseSkipped:
			// ok — both count as done
		default:
			allDone = false
		}
	}

	switch {
	case failed:
		flow.Status.Phase = arkonisv1alpha1.ArkFlowPhaseFailed
		flow.Status.CompletionTime = &now
		r.setCondition(flow, metav1.ConditionFalse, "StepFailed", "one or more steps failed")
	case allDone:
		flow.Status.Phase = arkonisv1alpha1.ArkFlowPhaseSucceeded
		flow.Status.CompletionTime = &now
		if flow.Spec.Output != "" {
			out, _ := r.resolveTemplate(flow.Spec.Output, templateData)
			flow.Status.Output = out
		}
		r.setCondition(flow, metav1.ConditionTrue, "Succeeded", "all steps completed successfully")
	}
}

// collectResults scans agent-tasks-results for results matching in-flight step task IDs.
func (r *ArkFlowReconciler) collectResults(
	ctx context.Context,
	rdb *redis.Client,
	_ *arkonisv1alpha1.ArkFlow,
	statusByName map[string]*arkonisv1alpha1.ArkFlowStepStatus,
) error {
	// Build a set of task IDs we're waiting on.
	waiting := make(map[string]*arkonisv1alpha1.ArkFlowStepStatus)
	for _, st := range statusByName {
		if st.Phase == arkonisv1alpha1.ArkFlowStepPhaseRunning && st.TaskID != "" {
			waiting[st.TaskID] = st
		}
	}
	if len(waiting) == 0 {
		return nil
	}

	// XRANGE results stream to find matching entries.
	msgs, err := rdb.XRange(ctx, resultsStream, "-", "+").Result()
	if err != nil && err != redis.Nil {
		return fmt.Errorf("XRANGE %s: %w", resultsStream, err)
	}
	for _, msg := range msgs {
		taskID, _ := msg.Values["task_id"].(string)
		st, ok := waiting[taskID]
		if !ok {
			continue
		}
		result, _ := msg.Values["result"].(string)
		now := metav1.Now()
		st.Phase = arkonisv1alpha1.ArkFlowStepPhaseSucceeded
		st.Output = result
		st.CompletionTime = &now

		// Capture token usage written by the agent runtime.
		inTok := toInt64(msg.Values["input_tokens"])
		outTok := toInt64(msg.Values["output_tokens"])
		if inTok > 0 || outTok > 0 {
			st.TokenUsage = &arkonisv1alpha1.TokenUsage{
				InputTokens:  inTok,
				OutputTokens: outTok,
				TotalTokens:  inTok + outTok,
			}
		}
	}
	return nil
}

// toInt64 coerces a Redis value (string or int64) to int64.
func toInt64(v any) int64 {
	switch val := v.(type) {
	case int64:
		return val
	case string:
		n, _ := strconv.ParseInt(val, 10, 64)
		return n
	}
	return 0
}

// submitTask enqueues a task on the shared agent-tasks stream and returns the Redis message ID.
func (r *ArkFlowReconciler) submitTask(_ context.Context, rdb *redis.Client, prompt string) (string, error) {
	// Use Background so the enqueue survives reconcile-context cancellation
	// (e.g. a concurrent status update re-queues the object mid-flight).
	id, err := rdb.XAdd(context.Background(), &redis.XAddArgs{
		Stream: taskStream,
		Values: map[string]any{"prompt": prompt},
	}).Result()
	if err != nil {
		return "", fmt.Errorf("XADD %s: %w", taskStream, err)
	}
	return id, nil
}

// buildTemplateData assembles the Go template context from flow inputs and completed step outputs.
// Each step entry exposes:
//   - .steps.<name>.output  — raw text response
//   - .steps.<name>.data    — parsed JSON map (only when OutputJSON is populated)
func (r *ArkFlowReconciler) buildTemplateData(
	flow *arkonisv1alpha1.ArkFlow,
	statusByName map[string]*arkonisv1alpha1.ArkFlowStepStatus,
) map[string]any {
	stepsData := make(map[string]any, len(flow.Status.Steps))
	for name, st := range statusByName {
		entry := map[string]any{"output": st.Output}
		if st.OutputJSON != "" {
			var parsed any
			if json.Unmarshal([]byte(st.OutputJSON), &parsed) == nil {
				entry["data"] = parsed
			}
		}
		stepsData[name] = entry
	}
	return map[string]any{
		"pipeline": map[string]any{"input": flow.Spec.Input},
		"steps":    stepsData,
	}
}

// resolvePrompt resolves all step input templates and concatenates them into a prompt string.
// When the step has an OutputSchema, the schema is appended as an instruction so the
// agent knows to respond with JSON matching that shape.
func (r *ArkFlowReconciler) resolvePrompt(step arkonisv1alpha1.ArkFlowStep, data map[string]any) (string, error) {
	var buf bytes.Buffer
	for key, tmplStr := range step.Inputs {
		resolved, err := r.resolveTemplate(tmplStr, data)
		if err != nil {
			return "", fmt.Errorf("input %q: %w", key, err)
		}
		fmt.Fprintf(&buf, "%s: %s\n", key, resolved)
	}
	if step.OutputSchema != "" {
		fmt.Fprintf(&buf, "\nRespond with valid JSON matching this schema:\n%s\n", step.OutputSchema)
	}
	return buf.String(), nil
}

// resolveTemplate executes a Go template string against the provided data.
func (r *ArkFlowReconciler) resolveTemplate(tmplStr string, data map[string]any) (string, error) {
	t, err := template.New("").Option("missingkey=zero").Parse(tmplStr)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// depsSucceeded returns true when every name in deps has completed (Succeeded or Skipped).
func (r *ArkFlowReconciler) depsSucceeded(
	deps []string,
	statusByName map[string]*arkonisv1alpha1.ArkFlowStepStatus,
) bool {
	for _, dep := range deps {
		st, ok := statusByName[dep]
		if !ok {
			return false
		}
		if st.Phase != arkonisv1alpha1.ArkFlowStepPhaseSucceeded &&
			st.Phase != arkonisv1alpha1.ArkFlowStepPhaseSkipped {
			return false
		}
	}
	return true
}

// evaluateLoops checks every Succeeded step that has a Loop spec. If the loop condition
// is still truthy and max iterations haven't been reached, the step is reset to Pending
// so it will be re-submitted on the next pass.
func (r *ArkFlowReconciler) evaluateLoops(
	flow *arkonisv1alpha1.ArkFlow,
	statusByName map[string]*arkonisv1alpha1.ArkFlowStepStatus,
	templateData map[string]any,
) {
	for _, step := range flow.Spec.Steps {
		if step.Loop == nil {
			continue
		}
		st := statusByName[step.Name]
		if st == nil || st.Phase != arkonisv1alpha1.ArkFlowStepPhaseSucceeded {
			continue
		}
		maxIter := step.Loop.MaxIterations
		if maxIter <= 0 {
			maxIter = 10
		}
		if st.Iterations >= maxIter {
			continue // reached limit — leave as Succeeded
		}
		condResult, err := r.resolveTemplate(step.Loop.Condition, templateData)
		if err != nil || !isTruthy(condResult) {
			continue // condition false — leave as Succeeded
		}
		// Reset for another iteration.
		st.Iterations++
		st.Phase = arkonisv1alpha1.ArkFlowStepPhasePending
		st.TaskID = ""
		st.Output = ""
		st.OutputJSON = ""
		st.StartTime = nil
		st.CompletionTime = nil
		st.Message = fmt.Sprintf("loop iteration %d/%d", st.Iterations, maxIter)
	}
}

// isTruthy returns false for blank, "false", "0", or "no" (case-insensitive).
func isTruthy(s string) bool {
	s = strings.TrimSpace(strings.ToLower(s))
	return s != "" && s != "false" && s != "0" && s != "no"
}

// getRedis returns a lazily-initialized Redis client, or nil if no URL is configured.
func (r *ArkFlowReconciler) getRedis() *redis.Client {
	r.redisOnce.Do(func() {
		if r.TaskQueueURL != "" {
			opts, err := redis.ParseURL(r.TaskQueueURL)
			if err != nil {
				opts = &redis.Options{Addr: r.TaskQueueURL}
			}
			r.rdb = redis.NewClient(opts)
		}
	})
	return r.rdb
}

// validateDAG checks that every dependsOn entry names a known step and that there are no cycles.
func (r *ArkFlowReconciler) validateDAG(flow *arkonisv1alpha1.ArkFlow) error {
	stepNames := make(map[string]struct{}, len(flow.Spec.Steps))
	for _, step := range flow.Spec.Steps {
		stepNames[step.Name] = struct{}{}
	}
	// First pass: verify all referenced steps exist.
	for _, step := range flow.Spec.Steps {
		for _, dep := range step.DependsOn {
			if _, ok := stepNames[dep]; !ok {
				return fmt.Errorf("step %q depends on unknown step %q", step.Name, dep)
			}
		}
	}
	// Build adjacency map (step → its dependencies).
	adj := make(map[string][]string, len(flow.Spec.Steps))
	for _, step := range flow.Spec.Steps {
		adj[step.Name] = step.DependsOn
	}
	// DFS cycle detection: 0=white (unvisited), 1=gray (in stack), 2=black (done).
	const white, gray, black = 0, 1, 2
	color := make(map[string]int, len(flow.Spec.Steps))
	var dfs func(name string) error
	dfs = func(name string) error {
		color[name] = gray
		for _, dep := range adj[name] {
			switch color[dep] {
			case gray:
				return fmt.Errorf("cycle detected: step %q → %q forms a cycle", name, dep)
			case white:
				if err := dfs(dep); err != nil {
					return err
				}
			}
		}
		color[name] = black
		return nil
	}
	for _, step := range flow.Spec.Steps {
		if color[step.Name] == white {
			if err := dfs(step.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateAgents checks that each step's ArkAgent exists.
func (r *ArkFlowReconciler) validateAgents(
	ctx context.Context,
	flow *arkonisv1alpha1.ArkFlow,
) error {
	for _, step := range flow.Spec.Steps {
		agent := &arkonisv1alpha1.ArkAgent{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      step.ArkAgent,
			Namespace: flow.Namespace,
		}, agent); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("step %q references missing ArkAgent %q", step.Name, step.ArkAgent)
			}
			return err
		}
	}
	return nil
}

func (r *ArkFlowReconciler) setCondition(
	flow *arkonisv1alpha1.ArkFlow,
	status metav1.ConditionStatus,
	reason, message string,
) {
	apimeta.SetStatusCondition(&flow.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		ObservedGeneration: flow.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// isTriggerTemplate returns true when this flow is referenced as a target
// by any ArkEvent in the same namespace. Templates are never executed directly.
func (r *ArkFlowReconciler) isTriggerTemplate(ctx context.Context, flow *arkonisv1alpha1.ArkFlow) (bool, error) {
	var events arkonisv1alpha1.ArkEventList
	if err := r.List(ctx, &events, client.InNamespace(flow.Namespace)); err != nil {
		return false, err
	}
	for _, t := range events.Items {
		for _, target := range t.Spec.Targets {
			if target.Pipeline == flow.Name {
				return true, nil
			}
		}
	}
	return false, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ArkFlowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&arkonisv1alpha1.ArkFlow{}).
		Named("arkflow").
		Complete(r)
}
