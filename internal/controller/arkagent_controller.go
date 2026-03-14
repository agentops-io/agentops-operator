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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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

const (
	// agentAPIKeysSecret is the k8s Secret expected to contain ANTHROPIC_API_KEY
	// and TASK_QUEUE_URL, injected via EnvFrom into every agent pod.
	agentAPIKeysSecret = "arkonis-api-keys"
)

// ArkAgentReconciler reconciles a ArkAgent object
type ArkAgentReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	AgentImage string
}

// +kubebuilder:rbac:groups=arkonis.dev,resources=arkagents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkagents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkagents/finalizers,verbs=update
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkmemories,verbs=get;list;watch
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkflows,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *ArkAgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the ArkAgent CR.
	arkAgent := &arkonisv1alpha1.ArkAgent{}
	if err := r.Get(ctx, req.NamespacedName, arkAgent); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. OwnerRef handles child cleanup on deletion — nothing extra needed.
	if !arkAgent.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// 3. Optionally load the referenced ArkSettings.
	var arkSettings *arkonisv1alpha1.ArkSettings
	if arkAgent.Spec.ConfigRef != nil {
		cfg := &arkonisv1alpha1.ArkSettings{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      arkAgent.Spec.ConfigRef.Name,
			Namespace: arkAgent.Namespace,
		}, cfg); err != nil {
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("fetching ArkSettings %q: %w", arkAgent.Spec.ConfigRef.Name, err)
			}
			logger.Info("ArkSettings not found, proceeding without it", "configRef", arkAgent.Spec.ConfigRef.Name)
		} else {
			arkSettings = cfg
		}
	}

	// 3b. Optionally load the referenced ArkMemory.
	var arkMemory *arkonisv1alpha1.ArkMemory
	if arkAgent.Spec.MemoryRef != nil {
		mem := &arkonisv1alpha1.ArkMemory{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      arkAgent.Spec.MemoryRef.Name,
			Namespace: arkAgent.Namespace,
		}, mem); err != nil {
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("fetching ArkMemory %q: %w", arkAgent.Spec.MemoryRef.Name, err)
			}
			logger.Info("ArkMemory not found, proceeding without it", "memoryRef", arkAgent.Spec.MemoryRef.Name)
		} else {
			arkMemory = mem
		}
	}

	// 3c. Resolve the effective system prompt (inline or from ConfigMap/Secret).
	resolvedPrompt, err := r.resolveSystemPrompt(ctx, arkAgent)
	if err != nil {
		logger.Error(err, "failed to resolve systemPrompt")
		r.setCondition(arkAgent, "Ready", metav1.ConditionFalse, "PromptResolutionError", err.Error())
		_ = r.Status().Update(ctx, arkAgent)
		return ctrl.Result{}, err
	}

	// 4. Calculate rolling 24h token usage and enforce daily budget.
	requeueAfter, err := r.reconcileDailyBudget(ctx, arkAgent)
	if err != nil {
		return ctrl.Result{}, err
	}

	// 5. Reconcile the owned k8s Deployment (budget check may override replicas to 0).
	if err := r.reconcileDeployment(ctx, arkAgent, arkSettings, arkMemory, resolvedPrompt); err != nil {
		logger.Error(err, "failed to reconcile Deployment")
		r.setCondition(arkAgent, "Ready", metav1.ConditionFalse, "ReconcileError", err.Error())
		_ = r.Status().Update(ctx, arkAgent)
		return ctrl.Result{}, err
	}

	// 6. Sync status.readyReplicas from the owned Deployment.
	if err := r.syncStatus(ctx, arkAgent); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *ArkAgentReconciler) reconcileDeployment(
	ctx context.Context,
	arkAgent *arkonisv1alpha1.ArkAgent,
	arkSettings *arkonisv1alpha1.ArkSettings,
	arkMemory *arkonisv1alpha1.ArkMemory,
	resolvedPrompt string,
) error {
	desired := r.buildDeployment(arkAgent, arkSettings, arkMemory, resolvedPrompt)

	if err := ctrl.SetControllerReference(arkAgent, desired, r.Scheme); err != nil {
		return err
	}

	existing := &appsv1.Deployment{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Spec.Template.Annotations = desired.Spec.Template.Annotations
	existing.Spec.Template.Spec.Containers = desired.Spec.Template.Spec.Containers
	return r.Patch(ctx, existing, patch)
}

func (r *ArkAgentReconciler) syncStatus(
	ctx context.Context,
	arkAgent *arkonisv1alpha1.ArkAgent,
) error {
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{
		Name:      arkAgent.Name + "-agent",
		Namespace: arkAgent.Namespace,
	}, dep); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	arkAgent.Status.Replicas = dep.Status.Replicas
	arkAgent.Status.ReadyReplicas = dep.Status.ReadyReplicas
	arkAgent.Status.ObservedGeneration = arkAgent.Generation

	condStatus := metav1.ConditionFalse
	condReason := "Progressing"
	condMsg := fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, dep.Status.Replicas)
	if dep.Status.ReadyReplicas == dep.Status.Replicas && dep.Status.Replicas > 0 {
		condStatus = metav1.ConditionTrue
		condReason = "AllReplicasReady"
	}
	r.setCondition(arkAgent, "Ready", condStatus, condReason, condMsg)

	return r.Status().Update(ctx, arkAgent)
}

func (r *ArkAgentReconciler) buildDeployment(arkAgent *arkonisv1alpha1.ArkAgent, arkSettings *arkonisv1alpha1.ArkSettings, arkMemory *arkonisv1alpha1.ArkMemory, resolvedPrompt string) *appsv1.Deployment {
	promptHashBytes := sha256.Sum256([]byte(resolvedPrompt))
	promptHash := fmt.Sprintf("%x", promptHashBytes)
	labels := map[string]string{
		"app.kubernetes.io/name":       "agent",
		"app.kubernetes.io/instance":   arkAgent.Name,
		"app.kubernetes.io/managed-by": "ark-operator",
		"arkonis.dev/deployment":       arkAgent.Name,
	}

	replicas := int32(1)
	if arkAgent.Spec.Replicas != nil {
		replicas = *arkAgent.Spec.Replicas
	}
	// Budget enforcement: scale to 0 while the daily token limit is exceeded.
	if apimeta.IsStatusConditionTrue(arkAgent.Status.Conditions, "BudgetExceeded") {
		replicas = 0
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      arkAgent.Name + "-agent",
			Namespace: arkAgent.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						// Hash changes trigger automatic rolling restart when the prompt is updated.
						"arkonis.dev/system-prompt-hash": promptHash,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            "agent",
						Image:           r.AgentImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Ports: []corev1.ContainerPort{
							{Name: "health", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
						},
						Env: r.buildEnvVars(arkAgent, arkSettings, arkMemory, resolvedPrompt),
						// Shared secret supplies ANTHROPIC_API_KEY and TASK_QUEUE_URL.
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: agentAPIKeysSecret,
								},
								Optional: boolPtr(true),
							},
						}},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstrFromInt32(8080),
								},
							},
							InitialDelaySeconds: 10,
							PeriodSeconds:       30,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/readyz",
									Port: intstrFromInt32(8080),
								},
							},
							InitialDelaySeconds: 10,
							PeriodSeconds:       30,
							TimeoutSeconds:      20,
						},
					}},
				},
			},
		},
	}
}

// mergeSystemPrompt prepends/appends ArkSettings prompt fragments around the resolved prompt.
func mergeSystemPrompt(base string, settings *arkonisv1alpha1.ArkSettings) string {
	if settings == nil {
		return base
	}
	if settings.Spec.PromptFragments.Persona != "" {
		base = settings.Spec.PromptFragments.Persona + "\n\n" + base
	}
	if settings.Spec.PromptFragments.OutputRules != "" {
		base = base + "\n\n" + settings.Spec.PromptFragments.OutputRules
	}
	return base
}

// buildSettingsEnvVars returns the env vars derived from an ArkSettings resource.
func buildSettingsEnvVars(settings *arkonisv1alpha1.ArkSettings) []corev1.EnvVar {
	if settings == nil {
		return nil
	}
	var envs []corev1.EnvVar
	if settings.Spec.Temperature != "" {
		envs = append(envs, corev1.EnvVar{Name: "AGENT_TEMPERATURE", Value: settings.Spec.Temperature})
	}
	if settings.Spec.OutputFormat != "" {
		envs = append(envs, corev1.EnvVar{Name: "AGENT_OUTPUT_FORMAT", Value: settings.Spec.OutputFormat})
	}
	if settings.Spec.MemoryBackend != "" {
		envs = append(envs, corev1.EnvVar{Name: "AGENT_MEMORY_BACKEND", Value: string(settings.Spec.MemoryBackend)})
	}
	return envs
}

// buildRedisMemoryEnvVars returns env vars for a Redis memory backend.
func buildRedisMemoryEnvVars(mem *arkonisv1alpha1.RedisMemoryConfig) []corev1.EnvVar {
	if mem == nil {
		return nil
	}
	envs := []corev1.EnvVar{{
		Name: "AGENT_MEMORY_REDIS_URL",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: mem.SecretRef.Name},
				Key:                  "REDIS_URL",
			},
		},
	}}
	if mem.TTLSeconds > 0 {
		envs = append(envs, corev1.EnvVar{Name: "AGENT_MEMORY_REDIS_TTL", Value: fmt.Sprintf("%d", mem.TTLSeconds)})
	}
	if mem.MaxEntries > 0 {
		envs = append(envs, corev1.EnvVar{Name: "AGENT_MEMORY_REDIS_MAX_ENTRIES", Value: fmt.Sprintf("%d", mem.MaxEntries)})
	}
	return envs
}

// buildVectorStoreMemoryEnvVars returns env vars for a vector-store memory backend.
func buildVectorStoreMemoryEnvVars(vs *arkonisv1alpha1.VectorStoreMemoryConfig) []corev1.EnvVar {
	if vs == nil {
		return nil
	}
	envs := []corev1.EnvVar{
		{Name: "AGENT_MEMORY_VECTOR_STORE_PROVIDER", Value: string(vs.Provider)},
		{Name: "AGENT_MEMORY_VECTOR_STORE_ENDPOINT", Value: vs.Endpoint},
		{Name: "AGENT_MEMORY_VECTOR_STORE_COLLECTION", Value: vs.Collection},
	}
	if vs.SecretRef != nil {
		envs = append(envs, corev1.EnvVar{
			Name: "AGENT_MEMORY_VECTOR_STORE_API_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: vs.SecretRef.Name},
					Key:                  "VECTOR_STORE_API_KEY",
				},
			},
		})
	}
	if vs.TTLSeconds > 0 {
		envs = append(envs, corev1.EnvVar{Name: "AGENT_MEMORY_VECTOR_STORE_TTL", Value: fmt.Sprintf("%d", vs.TTLSeconds)})
	}
	return envs
}

// buildMemoryEnvVars returns env vars for the ArkMemory backend (Redis or vector-store).
func buildMemoryEnvVars(arkMemory *arkonisv1alpha1.ArkMemory) []corev1.EnvVar {
	if arkMemory == nil {
		return nil
	}
	envs := []corev1.EnvVar{{Name: "AGENT_MEMORY_BACKEND", Value: string(arkMemory.Spec.Backend)}}
	switch arkMemory.Spec.Backend {
	case arkonisv1alpha1.MemoryBackendRedis:
		envs = append(envs, buildRedisMemoryEnvVars(arkMemory.Spec.Redis)...)
	case arkonisv1alpha1.MemoryBackendVectorStore:
		envs = append(envs, buildVectorStoreMemoryEnvVars(arkMemory.Spec.VectorStore)...)
	}
	return envs
}

func (r *ArkAgentReconciler) buildEnvVars(
	arkAgent *arkonisv1alpha1.ArkAgent,
	arkSettings *arkonisv1alpha1.ArkSettings,
	arkMemory *arkonisv1alpha1.ArkMemory,
	resolvedPrompt string,
) []corev1.EnvVar {
	mcpJSON, _ := json.Marshal(arkAgent.Spec.MCPServers)

	maxTokens := 8000
	timeoutSecs := 120
	if arkAgent.Spec.Limits != nil {
		if arkAgent.Spec.Limits.MaxTokensPerCall > 0 {
			maxTokens = arkAgent.Spec.Limits.MaxTokensPerCall
		}
		if arkAgent.Spec.Limits.TimeoutSeconds > 0 {
			timeoutSecs = arkAgent.Spec.Limits.TimeoutSeconds
		}
	}

	envVars := []corev1.EnvVar{
		{Name: "AGENT_MODEL", Value: arkAgent.Spec.Model},
		{Name: "AGENT_SYSTEM_PROMPT", Value: mergeSystemPrompt(resolvedPrompt, arkSettings)},
		{Name: "AGENT_MCP_SERVERS", Value: string(mcpJSON)},
		{Name: "AGENT_MAX_TOKENS", Value: fmt.Sprintf("%d", maxTokens)},
		{Name: "AGENT_TIMEOUT_SECONDS", Value: fmt.Sprintf("%d", timeoutSecs)},
		// POD_NAME is used as the Redis consumer group identity.
		{
			Name: "POD_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
			},
		},
	}

	// Propagate the custom validator prompt when a semantic liveness probe is configured.
	if arkAgent.Spec.LivenessProbe != nil &&
		arkAgent.Spec.LivenessProbe.Type == arkonisv1alpha1.ProbeTypeSemantic &&
		arkAgent.Spec.LivenessProbe.ValidatorPrompt != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "AGENT_VALIDATOR_PROMPT",
			Value: arkAgent.Spec.LivenessProbe.ValidatorPrompt,
		})
	}

	envVars = append(envVars, buildSettingsEnvVars(arkSettings)...)
	// ArkMemory takes precedence over any AGENT_MEMORY_BACKEND set via ArkSettings.
	envVars = append(envVars, buildMemoryEnvVars(arkMemory)...)

	// Inject inline webhook tool definitions so the agent runtime can call them directly.
	if len(arkAgent.Spec.Tools) > 0 {
		toolsJSON, _ := json.Marshal(arkAgent.Spec.Tools)
		envVars = append(envVars, corev1.EnvVar{
			Name:  "AGENT_WEBHOOK_TOOLS",
			Value: string(toolsJSON),
		})
	}

	return envVars
}

// reconcileDailyBudget sums token usage from all pipeline steps that reference this
// agent and completed within the last 24 hours. If the sum exceeds spec.limits.maxDailyTokens
// it sets a BudgetExceeded condition (buildDeployment will scale replicas to 0).
// Returns a requeue duration so the controller wakes up when the oldest entry leaves the window.
func (r *ArkAgentReconciler) reconcileDailyBudget(
	ctx context.Context,
	dep *arkonisv1alpha1.ArkAgent,
) (time.Duration, error) {
	limit := int64(0)
	if dep.Spec.Limits != nil {
		limit = dep.Spec.Limits.MaxDailyTokens
	}

	if limit <= 0 {
		// No limit configured — clear any stale condition and usage.
		apimeta.RemoveStatusCondition(&dep.Status.Conditions, "BudgetExceeded")
		dep.Status.DailyTokenUsage = nil
		return 0, nil
	}

	now := time.Now().UTC()
	windowStart := now.Add(-24 * time.Hour)

	flows := &arkonisv1alpha1.ArkFlowList{}
	if err := r.List(ctx, flows, client.InNamespace(dep.Namespace)); err != nil {
		return 0, fmt.Errorf("listing flows for budget: %w", err)
	}

	var usage arkonisv1alpha1.TokenUsage
	// earliestInWindow is the oldest CompletionTime still inside the window.
	// We use it to compute when the window shrinks enough to fall below the limit.
	var earliestInWindow *time.Time

	for i := range flows.Items {
		flow := &flows.Items[i]

		// Build a step-name → agent-name index from the spec.
		stepAgent := make(map[string]string, len(flow.Spec.Steps))
		for _, s := range flow.Spec.Steps {
			stepAgent[s.Name] = s.ArkAgent
		}

		for _, st := range flow.Status.Steps {
			if stepAgent[st.Name] != dep.Name {
				continue
			}
			if st.TokenUsage == nil || st.CompletionTime == nil {
				continue
			}
			t := st.CompletionTime.Time
			if t.Before(windowStart) {
				continue // outside 24h window
			}
			usage.InputTokens += st.TokenUsage.InputTokens
			usage.OutputTokens += st.TokenUsage.OutputTokens
			usage.TotalTokens += st.TokenUsage.TotalTokens
			if earliestInWindow == nil || t.Before(*earliestInWindow) {
				earliestInWindow = &t
			}
		}
	}

	dep.Status.DailyTokenUsage = &usage

	if usage.TotalTokens >= limit {
		r.setCondition(dep, "BudgetExceeded", metav1.ConditionTrue, "DailyLimitReached",
			fmt.Sprintf("daily token usage %d exceeds limit %d; replicas scaled to 0", usage.TotalTokens, limit))
		// Requeue when the oldest entry leaves the window so we can restore replicas.
		if earliestInWindow != nil {
			ttl := earliestInWindow.Add(24 * time.Hour).Sub(now)
			return ttl + time.Minute, nil // +1m buffer to avoid racing the boundary
		}
		return time.Hour, nil
	}

	// Under limit — clear condition.
	apimeta.RemoveStatusCondition(&dep.Status.Conditions, "BudgetExceeded")
	return 0, nil
}

// flowToAgents maps a changed ArkFlow to all ArkAgents referenced
// by its steps, so budget recalculations fire automatically when flow steps complete.
func (r *ArkAgentReconciler) flowToAgents(ctx context.Context, obj client.Object) []reconcile.Request {
	flow, ok := obj.(*arkonisv1alpha1.ArkFlow)
	if !ok {
		return nil
	}
	seen := make(map[string]struct{})
	var reqs []reconcile.Request
	for _, step := range flow.Spec.Steps {
		if step.ArkAgent == "" {
			continue
		}
		if _, dup := seen[step.ArkAgent]; dup {
			continue
		}
		seen[step.ArkAgent] = struct{}{}
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      step.ArkAgent,
				Namespace: flow.Namespace,
			},
		})
	}
	return reqs
}

// resolveSystemPrompt returns the effective system prompt text for arkAgent.
// If spec.systemPromptRef is set it reads from the referenced ConfigMap or Secret.
// Falls back to spec.systemPrompt when no ref is configured.
func (r *ArkAgentReconciler) resolveSystemPrompt(
	ctx context.Context,
	dep *arkonisv1alpha1.ArkAgent,
) (string, error) {
	if dep.Spec.SystemPromptRef == nil {
		return dep.Spec.SystemPrompt, nil
	}
	ref := dep.Spec.SystemPromptRef
	switch {
	case ref.ConfigMapKeyRef != nil:
		cm := &corev1.ConfigMap{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      ref.ConfigMapKeyRef.Name,
			Namespace: dep.Namespace,
		}, cm); err != nil {
			return "", fmt.Errorf("reading ConfigMap %q for systemPromptRef: %w", ref.ConfigMapKeyRef.Name, err)
		}
		val, ok := cm.Data[ref.ConfigMapKeyRef.Key]
		if !ok {
			return "", fmt.Errorf("key %q not found in ConfigMap %q", ref.ConfigMapKeyRef.Key, ref.ConfigMapKeyRef.Name)
		}
		return val, nil
	case ref.SecretKeyRef != nil:
		sec := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      ref.SecretKeyRef.Name,
			Namespace: dep.Namespace,
		}, sec); err != nil {
			return "", fmt.Errorf("reading Secret %q for systemPromptRef: %w", ref.SecretKeyRef.Name, err)
		}
		val, ok := sec.Data[ref.SecretKeyRef.Key]
		if !ok {
			return "", fmt.Errorf("key %q not found in Secret %q", ref.SecretKeyRef.Key, ref.SecretKeyRef.Name)
		}
		return string(val), nil
	default:
		return dep.Spec.SystemPrompt, nil
	}
}

// configMapToAgents re-enqueues ArkAgents that reference a changed ConfigMap
// via spec.systemPromptRef, so prompt content changes trigger automatic rolling restarts.
func (r *ArkAgentReconciler) configMapToAgents(ctx context.Context, obj client.Object) []reconcile.Request {
	cm, ok := obj.(*corev1.ConfigMap)
	if !ok {
		return nil
	}
	agents := &arkonisv1alpha1.ArkAgentList{}
	if err := r.List(ctx, agents, client.InNamespace(cm.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, dep := range agents.Items {
		if dep.Spec.SystemPromptRef == nil || dep.Spec.SystemPromptRef.ConfigMapKeyRef == nil {
			continue
		}
		if dep.Spec.SystemPromptRef.ConfigMapKeyRef.Name == cm.Name {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace},
			})
		}
	}
	return reqs
}

// secretToAgents re-enqueues ArkAgents that reference a changed Secret
// via spec.systemPromptRef.
func (r *ArkAgentReconciler) secretToAgents(ctx context.Context, obj client.Object) []reconcile.Request {
	sec, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}
	agents := &arkonisv1alpha1.ArkAgentList{}
	if err := r.List(ctx, agents, client.InNamespace(sec.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, dep := range agents.Items {
		if dep.Spec.SystemPromptRef == nil || dep.Spec.SystemPromptRef.SecretKeyRef == nil {
			continue
		}
		if dep.Spec.SystemPromptRef.SecretKeyRef.Name == sec.Name {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace},
			})
		}
	}
	return reqs
}

func (r *ArkAgentReconciler) setCondition(
	arkAgent *arkonisv1alpha1.ArkAgent,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	apimeta.SetStatusCondition(&arkAgent.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: arkAgent.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *ArkAgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&arkonisv1alpha1.ArkAgent{}).
		Owns(&appsv1.Deployment{}).
		// Re-evaluate daily budget whenever a flow step completes.
		Watches(
			&arkonisv1alpha1.ArkFlow{},
			handler.EnqueueRequestsFromMapFunc(r.flowToAgents),
		).
		// Trigger rolling restart when the referenced system prompt content changes.
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.configMapToAgents),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToAgents),
		).
		Named("arkagent").
		Complete(r)
}
