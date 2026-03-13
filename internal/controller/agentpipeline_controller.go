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

	arkonisv1alpha1 "github.com/arkonis-dev/arkonis-operator/api/v1alpha1"
)

const (
	taskStream        = "agent-tasks"
	resultsStream     = "agent-tasks-results"
	pipelineRequeueIn = 5 * time.Second
)

// ArkonisPipelineReconciler reconciles a ArkonisPipeline object.
type ArkonisPipelineReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	TaskQueueURL string

	redisOnce sync.Once
	rdb       *redis.Client
}

// +kubebuilder:rbac:groups=arkonis.dev,resources=agentpipelines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=arkonis.dev,resources=agentpipelines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=arkonis.dev,resources=agentpipelines/finalizers,verbs=update
// +kubebuilder:rbac:groups=arkonis.dev,resources=agentdeployments,verbs=get;list;watch

func (r *ArkonisPipelineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	pipeline := &arkonisv1alpha1.ArkonisPipeline{}
	if err := r.Get(ctx, req.NamespacedName, pipeline); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !pipeline.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Validate DAG and referenced ArkonisDeployments.
	if err := r.validateDAG(pipeline); err != nil {
		logger.Error(err, "invalid pipeline DAG")
		r.setCondition(pipeline, metav1.ConditionFalse, "InvalidDAG", err.Error())
		_ = r.Status().Update(ctx, pipeline)
		return ctrl.Result{}, nil
	}
	if err := r.validateDeployments(ctx, pipeline); err != nil {
		logger.Info("waiting for ArkonisDeployments", "reason", err.Error())
		r.setCondition(pipeline, metav1.ConditionFalse, "DeploymentNotFound", err.Error())
		_ = r.Status().Update(ctx, pipeline)
		return ctrl.Result{}, nil
	}

	// Terminal phases — nothing to do.
	if pipeline.Status.Phase == arkonisv1alpha1.PipelinePhaseSucceeded ||
		pipeline.Status.Phase == arkonisv1alpha1.PipelinePhaseFailed {
		pipeline.Status.ObservedGeneration = pipeline.Generation
		return ctrl.Result{}, r.Status().Update(ctx, pipeline)
	}

	// Require Redis for execution.
	rdb := r.getRedis()
	if rdb == nil {
		msg := "TASK_QUEUE_URL not set; pipeline execution requires Redis"
		logger.Info(msg)
		r.setCondition(pipeline, metav1.ConditionFalse, "NoTaskQueue", msg)
		_ = r.Status().Update(ctx, pipeline)
		return ctrl.Result{}, nil
	}

	r.initializeSteps(pipeline)

	// Build lookup maps for convenient access.
	statusByName := make(map[string]*arkonisv1alpha1.PipelineStepStatus, len(pipeline.Status.Steps))
	for i := range pipeline.Status.Steps {
		statusByName[pipeline.Status.Steps[i].Name] = &pipeline.Status.Steps[i]
	}

	// Check results for in-flight steps.
	if err := r.collectResults(ctx, rdb, pipeline, statusByName); err != nil {
		logger.Error(err, "collecting step results from Redis")
		return ctrl.Result{}, fmt.Errorf("collecting step results: %w", err)
	}

	r.parseOutputJSON(pipeline, statusByName)

	templateData := r.buildTemplateData(pipeline, statusByName)
	if err := r.submitPendingSteps(ctx, rdb, pipeline, statusByName, templateData, logger); err != nil {
		return ctrl.Result{}, err
	}

	r.updatePipelinePhase(pipeline, templateData)

	pipeline.Status.ObservedGeneration = pipeline.Generation
	if err := r.Status().Update(ctx, pipeline); err != nil {
		return ctrl.Result{}, err
	}

	if pipeline.Status.Phase == arkonisv1alpha1.PipelinePhaseRunning {
		return ctrl.Result{RequeueAfter: pipelineRequeueIn}, nil
	}
	return ctrl.Result{}, nil
}

// initializeSteps sets up step statuses and marks the pipeline Running on the first reconcile.
func (r *ArkonisPipelineReconciler) initializeSteps(pipeline *arkonisv1alpha1.ArkonisPipeline) {
	if pipeline.Status.Phase != "" {
		return
	}
	now := metav1.Now()
	pipeline.Status.Phase = arkonisv1alpha1.PipelinePhaseRunning
	pipeline.Status.StartTime = &now
	pipeline.Status.Steps = make([]arkonisv1alpha1.PipelineStepStatus, len(pipeline.Spec.Steps))
	for i, step := range pipeline.Spec.Steps {
		pipeline.Status.Steps[i] = arkonisv1alpha1.PipelineStepStatus{
			Name:  step.Name,
			Phase: arkonisv1alpha1.PipelineStepPhasePending,
		}
	}
	r.setCondition(pipeline, metav1.ConditionTrue, "Validated", "Pipeline DAG is valid; execution started")
}

// parseOutputJSON tries to parse completed step outputs as JSON when the step declared an OutputSchema.
// Parsed results are stored in OutputJSON so downstream templates can reference individual fields.
func (r *ArkonisPipelineReconciler) parseOutputJSON(
	pipeline *arkonisv1alpha1.ArkonisPipeline,
	statusByName map[string]*arkonisv1alpha1.PipelineStepStatus,
) {
	schemaByName := make(map[string]string, len(pipeline.Spec.Steps))
	for _, step := range pipeline.Spec.Steps {
		if step.OutputSchema != "" {
			schemaByName[step.Name] = step.OutputSchema
		}
	}
	for name, st := range statusByName {
		if _, hasSchema := schemaByName[name]; !hasSchema {
			continue
		}
		if st.Phase != arkonisv1alpha1.PipelineStepPhaseSucceeded || st.Output == "" || st.OutputJSON != "" {
			continue
		}
		var check any
		if json.Unmarshal([]byte(st.Output), &check) == nil {
			st.OutputJSON = st.Output
		}
	}
}

// submitPendingSteps enqueues tasks for every step whose dependencies have all succeeded.
func (r *ArkonisPipelineReconciler) submitPendingSteps(
	ctx context.Context,
	rdb *redis.Client,
	pipeline *arkonisv1alpha1.ArkonisPipeline,
	statusByName map[string]*arkonisv1alpha1.PipelineStepStatus,
	templateData map[string]any,
	logger interface {
		Info(string, ...any)
		Error(error, string, ...any)
	},
) error {
	for _, step := range pipeline.Spec.Steps {
		st := statusByName[step.Name]
		if st == nil || st.Phase != arkonisv1alpha1.PipelineStepPhasePending {
			continue
		}
		if !r.depsSucceeded(step.DependsOn, statusByName) {
			continue
		}
		prompt, err := r.resolvePrompt(step, templateData)
		if err != nil {
			logger.Error(err, "resolving step inputs", "step", step.Name)
			now := metav1.Now()
			st.Phase = arkonisv1alpha1.PipelineStepPhaseFailed
			st.CompletionTime = &now
			st.Message = fmt.Sprintf("input template error: %v", err)
			continue
		}
		taskID, err := r.submitTask(ctx, rdb, prompt)
		if err != nil {
			logger.Error(err, "submitting task to Redis", "step", step.Name)
			_ = r.Status().Update(ctx, pipeline)
			return fmt.Errorf("submitting task for step %q: %w", step.Name, err)
		}
		now := metav1.Now()
		st.Phase = arkonisv1alpha1.PipelineStepPhaseRunning
		st.TaskID = taskID
		st.StartTime = &now
		logger.Info("submitted task", "step", step.Name, "taskID", taskID)
	}
	return nil
}

// updatePipelinePhase inspects step statuses and transitions the pipeline to Succeeded or Failed.
func (r *ArkonisPipelineReconciler) updatePipelinePhase(
	pipeline *arkonisv1alpha1.ArkonisPipeline,
	templateData map[string]any,
) {
	failed, allDone := false, true
	for _, st := range pipeline.Status.Steps {
		switch st.Phase {
		case arkonisv1alpha1.PipelineStepPhaseFailed:
			failed = true
		case arkonisv1alpha1.PipelineStepPhaseSucceeded:
			// ok
		default:
			allDone = false
		}
	}

	now := metav1.Now()
	switch {
	case failed:
		pipeline.Status.Phase = arkonisv1alpha1.PipelinePhaseFailed
		pipeline.Status.CompletionTime = &now
		r.setCondition(pipeline, metav1.ConditionFalse, "StepFailed", "one or more steps failed")
	case allDone:
		pipeline.Status.Phase = arkonisv1alpha1.PipelinePhaseSucceeded
		pipeline.Status.CompletionTime = &now
		if pipeline.Spec.Output != "" {
			out, _ := r.resolveTemplate(pipeline.Spec.Output, templateData)
			pipeline.Status.Output = out
		}
		r.setCondition(pipeline, metav1.ConditionTrue, "Succeeded", "all steps completed successfully")
	}
}

// collectResults scans agent-tasks-results for results matching in-flight step task IDs.
func (r *ArkonisPipelineReconciler) collectResults(
	ctx context.Context,
	rdb *redis.Client,
	_ *arkonisv1alpha1.ArkonisPipeline,
	statusByName map[string]*arkonisv1alpha1.PipelineStepStatus,
) error {
	// Build a set of task IDs we're waiting on.
	waiting := make(map[string]*arkonisv1alpha1.PipelineStepStatus)
	for _, st := range statusByName {
		if st.Phase == arkonisv1alpha1.PipelineStepPhaseRunning && st.TaskID != "" {
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
		if st, ok := waiting[taskID]; ok {
			result, _ := msg.Values["result"].(string)
			now := metav1.Now()
			st.Phase = arkonisv1alpha1.PipelineStepPhaseSucceeded
			st.Output = result
			st.CompletionTime = &now
		}
	}
	return nil
}

// submitTask enqueues a task on the shared agent-tasks stream and returns the Redis message ID.
func (r *ArkonisPipelineReconciler) submitTask(ctx context.Context, rdb *redis.Client, prompt string) (string, error) {
	id, err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: taskStream,
		Values: map[string]any{"prompt": prompt},
	}).Result()
	if err != nil {
		return "", fmt.Errorf("XADD %s: %w", taskStream, err)
	}
	return id, nil
}

// buildTemplateData assembles the Go template context from pipeline inputs and completed step outputs.
// Each step entry exposes:
//   - .steps.<name>.output  — raw text response
//   - .steps.<name>.data    — parsed JSON map (only when OutputJSON is populated)
func (r *ArkonisPipelineReconciler) buildTemplateData(
	pipeline *arkonisv1alpha1.ArkonisPipeline,
	statusByName map[string]*arkonisv1alpha1.PipelineStepStatus,
) map[string]any {
	stepsData := make(map[string]any, len(pipeline.Status.Steps))
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
		"pipeline": map[string]any{"input": pipeline.Spec.Input},
		"steps":    stepsData,
	}
}

// resolvePrompt resolves all step input templates and concatenates them into a prompt string.
// When the step has an OutputSchema, the schema is appended as an instruction so the
// agent knows to respond with JSON matching that shape.
func (r *ArkonisPipelineReconciler) resolvePrompt(step arkonisv1alpha1.PipelineStep, data map[string]any) (string, error) {
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
func (r *ArkonisPipelineReconciler) resolveTemplate(tmplStr string, data map[string]any) (string, error) {
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

// depsSucceeded returns true when every name in deps is in Succeeded phase.
func (r *ArkonisPipelineReconciler) depsSucceeded(
	deps []string,
	statusByName map[string]*arkonisv1alpha1.PipelineStepStatus,
) bool {
	for _, dep := range deps {
		st, ok := statusByName[dep]
		if !ok || st.Phase != arkonisv1alpha1.PipelineStepPhaseSucceeded {
			return false
		}
	}
	return true
}

// getRedis returns a lazily-initialized Redis client, or nil if no URL is configured.
func (r *ArkonisPipelineReconciler) getRedis() *redis.Client {
	r.redisOnce.Do(func() {
		if r.TaskQueueURL != "" {
			r.rdb = redis.NewClient(&redis.Options{Addr: r.TaskQueueURL})
		}
	})
	return r.rdb
}

// validateDAG checks that every dependsOn entry names a step defined in the spec.
func (r *ArkonisPipelineReconciler) validateDAG(pipeline *arkonisv1alpha1.ArkonisPipeline) error {
	stepNames := make(map[string]struct{}, len(pipeline.Spec.Steps))
	for _, step := range pipeline.Spec.Steps {
		stepNames[step.Name] = struct{}{}
	}
	for _, step := range pipeline.Spec.Steps {
		for _, dep := range step.DependsOn {
			if _, ok := stepNames[dep]; !ok {
				return fmt.Errorf("step %q depends on unknown step %q", step.Name, dep)
			}
		}
	}
	return nil
}

// validateDeployments checks that each step's ArkonisDeployment exists.
func (r *ArkonisPipelineReconciler) validateDeployments(
	ctx context.Context,
	pipeline *arkonisv1alpha1.ArkonisPipeline,
) error {
	for _, step := range pipeline.Spec.Steps {
		dep := &arkonisv1alpha1.ArkonisDeployment{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      step.ArkonisDeployment,
			Namespace: pipeline.Namespace,
		}, dep); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("step %q references missing ArkonisDeployment %q", step.Name, step.ArkonisDeployment)
			}
			return err
		}
	}
	return nil
}

func (r *ArkonisPipelineReconciler) setCondition(
	pipeline *arkonisv1alpha1.ArkonisPipeline,
	status metav1.ConditionStatus,
	reason, message string,
) {
	apimeta.SetStatusCondition(&pipeline.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		ObservedGeneration: pipeline.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *ArkonisPipelineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&arkonisv1alpha1.ArkonisPipeline{}).
		Named("agentpipeline").
		Complete(r)
}
