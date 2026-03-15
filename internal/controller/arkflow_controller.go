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
	"context"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	arkonisv1alpha1 "github.com/arkonis-dev/ark-operator/api/v1alpha1"
	"github.com/arkonis-dev/ark-operator/internal/flow"
)

const (
	taskStream        = "agent-tasks"
	resultsStream     = "agent-tasks-results"
	pipelineRequeueIn = 5 * 1e9 // 5 * time.Second — avoid importing "time" for a single constant
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

	f := &arkonisv1alpha1.ArkFlow{}
	if err := r.Get(ctx, req.NamespacedName, f); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !f.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if isTemplate, err := r.isTriggerTemplate(ctx, f); err != nil {
		return ctrl.Result{}, err
	} else if isTemplate {
		return ctrl.Result{}, nil
	}

	if err := flow.ValidateDAG(f); err != nil {
		logger.Error(err, "invalid flow DAG")
		flow.SetCondition(f, metav1.ConditionFalse, "InvalidDAG", err.Error())
		_ = r.Status().Update(ctx, f)
		return ctrl.Result{}, nil
	}
	if err := r.validateAgents(ctx, f); err != nil {
		logger.Info("waiting for ArkAgents", "reason", err.Error())
		flow.SetCondition(f, metav1.ConditionFalse, "DeploymentNotFound", err.Error())
		_ = r.Status().Update(ctx, f)
		return ctrl.Result{}, nil
	}

	if flow.IsTerminalPhase(f.Status.Phase) {
		f.Status.ObservedGeneration = f.Generation
		return ctrl.Result{}, r.Status().Update(ctx, f)
	}

	rdb := r.getRedis()
	if rdb == nil {
		msg := "TASK_QUEUE_URL not set; flow execution requires Redis"
		logger.Info(msg)
		flow.SetCondition(f, metav1.ConditionFalse, "NoTaskQueue", msg)
		_ = r.Status().Update(ctx, f)
		return ctrl.Result{}, nil
	}

	flow.InitializeSteps(f)

	statusByName := flow.BuildStatusByName(f)

	if err := r.collectResults(ctx, rdb, statusByName); err != nil {
		logger.Error(err, "collecting step results from Redis")
		return ctrl.Result{}, fmt.Errorf("collecting step results: %w", err)
	}

	flow.ParseOutputJSON(f, statusByName)

	templateData := flow.BuildTemplateData(f, statusByName)

	flow.EvaluateLoops(f, statusByName, templateData)

	if err := r.submitPendingSteps(ctx, rdb, f, statusByName, templateData, logger); err != nil {
		return ctrl.Result{}, err
	}

	flow.UpdateFlowPhase(f, templateData)

	f.Status.ObservedGeneration = f.Generation
	if err := r.Status().Update(ctx, f); err != nil {
		return ctrl.Result{}, err
	}

	if f.Status.Phase == arkonisv1alpha1.ArkFlowPhaseRunning {
		return ctrl.Result{RequeueAfter: pipelineRequeueIn}, nil
	}
	return ctrl.Result{}, nil
}

// submitPendingSteps enqueues tasks for every step whose dependencies have all succeeded.
func (r *ArkFlowReconciler) submitPendingSteps(
	ctx context.Context,
	rdb *redis.Client,
	f *arkonisv1alpha1.ArkFlow,
	statusByName map[string]*arkonisv1alpha1.ArkFlowStepStatus,
	templateData map[string]any,
	logger interface {
		Info(string, ...any)
		Error(error, string, ...any)
	},
) error {
	for _, step := range f.Spec.Steps {
		st := statusByName[step.Name]
		if st == nil || st.Phase != arkonisv1alpha1.ArkFlowStepPhasePending {
			continue
		}
		if !flow.DepsSucceeded(step.DependsOn, statusByName) {
			continue
		}
		if step.If != "" {
			condResult, err := flow.ResolveTemplate(step.If, templateData)
			if err != nil || !flow.IsTruthy(condResult) {
				now := metav1.Now()
				st.Phase = arkonisv1alpha1.ArkFlowStepPhaseSkipped
				st.CompletionTime = &now
				st.Message = "skipped: if condition evaluated to false"
				logger.Info("skipping step", "step", step.Name, "condition", step.If)
				continue
			}
		}
		prompt, err := flow.ResolvePrompt(step, templateData)
		if err != nil {
			logger.Error(err, "resolving step inputs", "step", step.Name)
			now := metav1.Now()
			st.Phase = arkonisv1alpha1.ArkFlowStepPhaseFailed
			st.CompletionTime = &now
			st.Message = fmt.Sprintf("input template error: %v", err)
			continue
		}
		taskID, err := r.submitTask(rdb, prompt, f.Annotations["arkonis.dev/stream-key"])
		if err != nil {
			logger.Error(err, "submitting task to Redis", "step", step.Name)
			_ = r.Status().Update(ctx, f)
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

// collectResults scans agent-tasks-results for results matching in-flight step task IDs.
func (r *ArkFlowReconciler) collectResults(
	ctx context.Context,
	rdb *redis.Client,
	statusByName map[string]*arkonisv1alpha1.ArkFlowStepStatus,
) error {
	waiting := make(map[string]*arkonisv1alpha1.ArkFlowStepStatus)
	for _, st := range statusByName {
		if st.Phase == arkonisv1alpha1.ArkFlowStepPhaseRunning && st.TaskID != "" {
			waiting[st.TaskID] = st
		}
	}
	if len(waiting) == 0 {
		return nil
	}
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

		inTok := flow.ToInt64(msg.Values["input_tokens"])
		outTok := flow.ToInt64(msg.Values["output_tokens"])
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

// submitTask enqueues a task on the shared agent-tasks stream and returns the Redis message ID.
// streamKey, if non-empty, is included in the task payload so the agent can publish token chunks.
func (r *ArkFlowReconciler) submitTask(rdb *redis.Client, prompt, streamKey string) (string, error) {
	values := map[string]any{"prompt": prompt}
	if streamKey != "" {
		values["stream_key"] = streamKey
	}
	id, err := rdb.XAdd(context.Background(), &redis.XAddArgs{
		Stream: taskStream,
		Values: values,
	}).Result()
	if err != nil {
		return "", fmt.Errorf("XADD %s: %w", taskStream, err)
	}
	return id, nil
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

// validateAgents checks that each step's ArkAgent exists in the cluster.
func (r *ArkFlowReconciler) validateAgents(ctx context.Context, f *arkonisv1alpha1.ArkFlow) error {
	for _, step := range f.Spec.Steps {
		agent := &arkonisv1alpha1.ArkAgent{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      step.ArkAgent,
			Namespace: f.Namespace,
		}, agent); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("step %q references missing ArkAgent %q", step.Name, step.ArkAgent)
			}
			return err
		}
	}
	return nil
}

// isTriggerTemplate returns true when this flow is referenced as a target
// by any ArkEvent in the same namespace. Templates are never executed directly.
func (r *ArkFlowReconciler) isTriggerTemplate(ctx context.Context, f *arkonisv1alpha1.ArkFlow) (bool, error) {
	var events arkonisv1alpha1.ArkEventList
	if err := r.List(ctx, &events, client.InNamespace(f.Namespace)); err != nil {
		return false, err
	}
	for _, t := range events.Items {
		for _, target := range t.Spec.Targets {
			if target.Pipeline == f.Name {
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
