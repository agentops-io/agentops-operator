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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"text/template"
	"time"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	arkonisv1alpha1 "github.com/arkonis-dev/ark-operator/api/v1alpha1"
)

// ArkEventReconciler reconciles ArkEvent objects.
//
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkevents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkevents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkevents/finalizers,verbs=update
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkflows,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create
type ArkEventReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// TriggerWebhookURL is the base URL exposed by the operator's webhook HTTP server.
	// Example: "http://ark-operator.ark-system.svc.cluster.local:8092"
	// If empty, webhook-type triggers will record an empty webhookURL in status.
	TriggerWebhookURL string
}

// FireContext carries information about a trigger firing event, used to resolve
// input templates on dispatched flows.
type FireContext struct {
	Name    string
	FiredAt string
	Output  string         // upstream flow output (pipeline-output type)
	Body    map[string]any // webhook request body fields
}

func (r *ArkEventReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	trigger := &arkonisv1alpha1.ArkEvent{}
	if err := r.Get(ctx, req.NamespacedName, trigger); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if trigger.Spec.Suspended {
		r.setCondition(trigger, metav1.ConditionFalse, "Suspended", "trigger is suspended")
		trigger.Status.ObservedGeneration = trigger.Generation
		return ctrl.Result{}, r.Status().Update(ctx, trigger)
	}

	var result ctrl.Result
	var err error

	switch trigger.Spec.Source.Type {
	case arkonisv1alpha1.TriggerSourceCron:
		result, err = r.reconcileCron(ctx, trigger)
	case arkonisv1alpha1.TriggerSourceWebhook:
		err = r.reconcileWebhook(ctx, trigger)
	case arkonisv1alpha1.TriggerSourcePipelineOutput:
		err = r.reconcilePipelineOutput(ctx, trigger)
	default:
		r.setCondition(trigger, metav1.ConditionFalse, "InvalidSource",
			fmt.Sprintf("unknown source type %q", trigger.Spec.Source.Type))
		trigger.Status.ObservedGeneration = trigger.Generation
		return ctrl.Result{}, r.Status().Update(ctx, trigger)
	}

	if err != nil {
		logger.Error(err, "reconciling trigger")
		return ctrl.Result{}, err
	}

	// Re-fetch before status update to avoid conflicts with the webhook server
	// which may have written lastFired concurrently.
	latest := &arkonisv1alpha1.ArkEvent{}
	if err := r.Get(ctx, req.NamespacedName, latest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	latest.Status = trigger.Status
	latest.Status.ObservedGeneration = latest.Generation
	if statusErr := r.Status().Update(ctx, latest); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return result, nil
}

// reconcileCron checks whether the cron schedule is due and fires if so.
func (r *ArkEventReconciler) reconcileCron(ctx context.Context, trigger *arkonisv1alpha1.ArkEvent) (ctrl.Result, error) {
	if trigger.Spec.Source.Cron == "" {
		r.setCondition(trigger, metav1.ConditionFalse, "InvalidCron", "spec.source.cron is required for cron type")
		return ctrl.Result{}, nil
	}

	schedule, err := cron.ParseStandard(trigger.Spec.Source.Cron)
	if err != nil {
		r.setCondition(trigger, metav1.ConditionFalse, "InvalidCron",
			fmt.Sprintf("invalid cron expression %q: %v", trigger.Spec.Source.Cron, err))
		return ctrl.Result{}, nil
	}

	now := time.Now().UTC()

	// Determine the most recent scheduled time before now.
	// We look back far enough to catch the last missed fire.
	lastScheduled := mostRecentSchedule(schedule, now, trigger.Status.LastFiredAt)

	// Fire if the last scheduled time is after the last fire (or we've never fired).
	shouldFire := lastScheduled != nil &&
		(trigger.Status.LastFiredAt == nil || lastScheduled.After(trigger.Status.LastFiredAt.Time))

	if shouldFire {
		fireCtx := FireContext{
			Name:    trigger.Name,
			FiredAt: now.Format(time.RFC3339),
		}
		if _, err := r.fire(ctx, trigger, fireCtx); err != nil {
			return ctrl.Result{}, err
		}
		nowMeta := metav1.NewTime(now)
		trigger.Status.LastFiredAt = &nowMeta
		trigger.Status.FiredCount++
	}

	// Compute and store next fire time.
	next := schedule.Next(now)
	nextMeta := metav1.NewTime(next)
	trigger.Status.NextFireAt = &nextMeta
	r.setCondition(trigger, metav1.ConditionTrue, "Active", fmt.Sprintf("next fire at %s", next.Format(time.RFC3339)))

	return ctrl.Result{RequeueAfter: time.Until(next) + time.Second}, nil
}

// reconcileWebhook ensures a token Secret exists and the webhook URL is in status.
// Actual firing is handled by the TriggerWebhookServer HTTP handler.
func (r *ArkEventReconciler) reconcileWebhook(ctx context.Context, trigger *arkonisv1alpha1.ArkEvent) error {
	if err := r.ensureWebhookToken(ctx, trigger); err != nil {
		return err
	}
	if r.TriggerWebhookURL != "" {
		trigger.Status.WebhookURL = fmt.Sprintf("%s/triggers/%s/%s/fire",
			r.TriggerWebhookURL, trigger.Namespace, trigger.Name)
	}
	r.setCondition(trigger, metav1.ConditionTrue, arkonisv1alpha1.ConditionReady, "waiting for webhook POST")
	return nil
}

// ensureWebhookToken creates a Secret containing a random token for webhook auth
// if one does not already exist.
func (r *ArkEventReconciler) ensureWebhookToken(ctx context.Context, trigger *arkonisv1alpha1.ArkEvent) error {
	secretName := trigger.Name + "-webhook-token"
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: trigger.Namespace}, existing)
	if err == nil {
		return nil // already exists
	}
	if !errors.IsNotFound(err) {
		return err
	}

	// Generate a cryptographically random 32-byte hex token.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("generating webhook token: %w", err)
	}
	token := hex.EncodeToString(raw)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: trigger.Namespace,
		},
		Data: map[string][]byte{"token": []byte(token)},
	}
	if err := ctrl.SetControllerReference(trigger, secret, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, secret)
}

// reconcilePipelineOutput checks if the watched flow has completed and fires if so.
func (r *ArkEventReconciler) reconcilePipelineOutput(ctx context.Context, trigger *arkonisv1alpha1.ArkEvent) error {
	src := trigger.Spec.Source.PipelineOutput
	if src == nil || src.Name == "" {
		r.setCondition(trigger, metav1.ConditionFalse, "InvalidSource",
			"spec.source.pipelineOutput.name is required for pipeline-output type")
		return nil
	}

	flow := &arkonisv1alpha1.ArkFlow{}
	if err := r.Get(ctx, types.NamespacedName{Name: src.Name, Namespace: trigger.Namespace}, flow); err != nil {
		r.setCondition(trigger, metav1.ConditionFalse, "PipelineNotFound",
			fmt.Sprintf("flow %q not found: %v", src.Name, err))
		return client.IgnoreNotFound(err)
	}

	desiredPhase := src.OnPhase
	if desiredPhase == "" {
		desiredPhase = arkonisv1alpha1.ArkFlowPhaseSucceeded
	}

	if flow.Status.Phase != desiredPhase {
		r.setCondition(trigger, metav1.ConditionTrue, "Watching",
			fmt.Sprintf("waiting for flow %q to reach phase %s (current: %s)",
				src.Name, desiredPhase, flow.Status.Phase))
		return nil
	}

	// Don't re-fire for the same completion.
	if flow.Status.CompletionTime != nil && trigger.Status.LastFiredAt != nil &&
		!flow.Status.CompletionTime.After(trigger.Status.LastFiredAt.Time) {
		r.setCondition(trigger, metav1.ConditionTrue, "Active", "already fired for this flow completion")
		return nil
	}

	now := time.Now().UTC()
	fireCtx := FireContext{
		Name:    trigger.Name,
		FiredAt: now.Format(time.RFC3339),
		Output:  flow.Status.Output,
	}
	if _, err := r.fire(ctx, trigger, fireCtx); err != nil {
		return err
	}
	nowMeta := metav1.NewTime(now)
	trigger.Status.LastFiredAt = &nowMeta
	trigger.Status.FiredCount++
	r.setCondition(trigger, metav1.ConditionTrue, "Active", "fired on flow completion")
	return nil
}

// fire dispatches all target flows for a trigger firing event.
// It respects the concurrency policy and sets owner references on created flows.
// Returns the list of created ArkFlow objects so callers can wait for results.
func (r *ArkEventReconciler) fire(ctx context.Context, trigger *arkonisv1alpha1.ArkEvent, fireCtx FireContext) ([]*arkonisv1alpha1.ArkFlow, error) {
	logger := log.FromContext(ctx)

	if trigger.Spec.ConcurrencyPolicy == arkonisv1alpha1.ConcurrencyForbid {
		running, err := r.hasRunningFlow(ctx, trigger)
		if err != nil {
			return nil, err
		}
		if running {
			logger.Info("skipping fire: ConcurrencyPolicy=Forbid and a flow is still running",
				"trigger", trigger.Name)
			return nil, nil
		}
	}

	var flows []*arkonisv1alpha1.ArkFlow
	for _, target := range trigger.Spec.Targets {
		flow, err := r.buildFlow(ctx, trigger, target, fireCtx)
		if err != nil {
			return nil, fmt.Errorf("building flow for target %q: %w", target.Pipeline, err)
		}
		if err := r.Create(ctx, flow); err != nil {
			return nil, fmt.Errorf("creating flow for target %q: %w", target.Pipeline, err)
		}
		logger.Info("dispatched flow", "trigger", trigger.Name, "flow", flow.Name)
		flows = append(flows, flow)
	}
	return flows, nil
}

// buildFlow constructs an ArkFlow from the template flow and fire context.
func (r *ArkEventReconciler) buildFlow(
	ctx context.Context,
	trigger *arkonisv1alpha1.ArkEvent,
	target arkonisv1alpha1.ArkEventTarget,
	fireCtx FireContext,
) (*arkonisv1alpha1.ArkFlow, error) {
	// Load the template flow.
	tmpl := &arkonisv1alpha1.ArkFlow{}
	if err := r.Get(ctx, types.NamespacedName{Name: target.Pipeline, Namespace: trigger.Namespace}, tmpl); err != nil {
		return nil, fmt.Errorf("flow template %q not found: %w", target.Pipeline, err)
	}

	// Resolve input overrides using the fire context.
	resolvedInput := make(map[string]string, len(tmpl.Spec.Input))
	maps.Copy(resolvedInput, tmpl.Spec.Input)
	for k, expr := range target.Input {
		resolved, err := resolveTriggerTemplate(expr, fireCtx)
		if err != nil {
			return nil, fmt.Errorf("resolving input %q: %w", k, err)
		}
		resolvedInput[k] = resolved
	}

	// Generated name: <trigger>-<template>-<timestamp-suffix>
	suffix := time.Now().UTC().Format("20060102-150405")
	flow := &arkonisv1alpha1.ArkFlow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s-%s", trigger.Name, target.Pipeline, suffix),
			Namespace: trigger.Namespace,
			Labels: map[string]string{
				"arkonis.dev/trigger":          trigger.Name,
				"arkonis.dev/trigger-template": target.Pipeline,
			},
		},
		Spec: arkonisv1alpha1.ArkFlowSpec{
			Steps:          tmpl.Spec.Steps,
			Input:          resolvedInput,
			Output:         tmpl.Spec.Output,
			TimeoutSeconds: tmpl.Spec.TimeoutSeconds,
			MaxTokens:      tmpl.Spec.MaxTokens,
		},
	}
	if err := ctrl.SetControllerReference(trigger, flow, r.Scheme); err != nil {
		return nil, err
	}
	return flow, nil
}

// hasRunningFlow returns true if any flow owned by this trigger is still Running.
func (r *ArkEventReconciler) hasRunningFlow(ctx context.Context, trigger *arkonisv1alpha1.ArkEvent) (bool, error) {
	list := &arkonisv1alpha1.ArkFlowList{}
	if err := r.List(ctx, list,
		client.InNamespace(trigger.Namespace),
		client.MatchingLabels{"arkonis.dev/trigger": trigger.Name},
	); err != nil {
		return false, err
	}
	for _, p := range list.Items {
		if p.Status.Phase == arkonisv1alpha1.ArkFlowPhaseRunning {
			return true, nil
		}
	}
	return false, nil
}

// resolveTriggerTemplate evaluates a Go template expression against the FireContext.
func resolveTriggerTemplate(expr string, fireCtx FireContext) (string, error) {
	tmpl, err := template.New("").Option("missingkey=zero").Parse(expr)
	if err != nil {
		return expr, nil // not a template — return as-is
	}
	data := map[string]any{
		"trigger": map[string]any{
			"name":    fireCtx.Name,
			"firedAt": fireCtx.FiredAt,
			"output":  fireCtx.Output,
			"body":    fireCtx.Body,
		},
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// mostRecentSchedule returns the most recent time before now that the schedule was due,
// looking back at most 24 hours. Returns nil if no scheduled time is found.
func mostRecentSchedule(schedule cron.Schedule, now time.Time, lastFired *metav1.Time) *time.Time {
	// Look back from whichever is earlier: lastFired or 24h ago.
	lookback := now.Add(-24 * time.Hour)
	if lastFired != nil && lastFired.After(lookback) {
		lookback = lastFired.Time
	}

	// Step forward through schedule times until we pass now.
	t := lookback
	var last *time.Time
	for {
		next := schedule.Next(t)
		if next.After(now) {
			break
		}
		last = &next
		t = next
	}
	return last
}

func (r *ArkEventReconciler) setCondition(
	trigger *arkonisv1alpha1.ArkEvent,
	status metav1.ConditionStatus,
	reason, message string,
) {
	now := metav1.Now()
	condType := arkonisv1alpha1.ConditionReady
	for i, c := range trigger.Status.Conditions {
		if c.Type == condType {
			trigger.Status.Conditions[i].Status = status
			trigger.Status.Conditions[i].Reason = reason
			trigger.Status.Conditions[i].Message = message
			trigger.Status.Conditions[i].LastTransitionTime = now
			return
		}
	}
	trigger.Status.Conditions = append(trigger.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

// SetupWithManager registers the controller and sets up watches.
func (r *ArkEventReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// flowToTriggers maps a flow change to all pipeline-output triggers watching it.
	flowToTriggers := func(ctx context.Context, obj client.Object) []reconcile.Request {
		flow, ok := obj.(*arkonisv1alpha1.ArkFlow)
		if !ok {
			return nil
		}
		// Only react to terminal phases.
		if flow.Status.Phase != arkonisv1alpha1.ArkFlowPhaseSucceeded &&
			flow.Status.Phase != arkonisv1alpha1.ArkFlowPhaseFailed {
			return nil
		}

		var events arkonisv1alpha1.ArkEventList
		if err := r.List(ctx, &events, client.InNamespace(flow.Namespace)); err != nil {
			return nil
		}

		var reqs []reconcile.Request
		for _, t := range events.Items {
			if t.Spec.Source.Type != arkonisv1alpha1.TriggerSourcePipelineOutput {
				continue
			}
			if t.Spec.Source.PipelineOutput == nil {
				continue
			}
			if t.Spec.Source.PipelineOutput.Name == flow.Name {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      t.Name,
						Namespace: t.Namespace,
					},
				})
			}
		}
		return reqs
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&arkonisv1alpha1.ArkEvent{}).
		Owns(&arkonisv1alpha1.ArkFlow{}).
		Watches(
			&arkonisv1alpha1.ArkFlow{},
			handler.EnqueueRequestsFromMapFunc(flowToTriggers),
		).
		Complete(r)
}
