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
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentopsv1alpha1 "github.com/agentops-io/agentops-operator/api/v1alpha1"
)

const (
	// agentAPIKeysSecret is the k8s Secret expected to contain ANTHROPIC_API_KEY
	// and TASK_QUEUE_URL, injected via EnvFrom into every agent pod.
	agentAPIKeysSecret = "agentops-operator-api-keys"
)

// AgentDeploymentReconciler reconciles a AgentDeployment object
type AgentDeploymentReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	AgentImage string
}

// +kubebuilder:rbac:groups=agentops.agentops.io,resources=agentdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentops.agentops.io,resources=agentdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentops.agentops.io,resources=agentdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=agentops.agentops.io,resources=agentmemories,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *AgentDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the AgentDeployment CR.
	agentDep := &agentopsv1alpha1.AgentDeployment{}
	if err := r.Get(ctx, req.NamespacedName, agentDep); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. OwnerRef handles child cleanup on deletion — nothing extra needed.
	if !agentDep.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// 3. Optionally load the referenced AgentConfig.
	var agentCfg *agentopsv1alpha1.AgentConfig
	if agentDep.Spec.ConfigRef != nil {
		cfg := &agentopsv1alpha1.AgentConfig{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      agentDep.Spec.ConfigRef.Name,
			Namespace: agentDep.Namespace,
		}, cfg); err != nil {
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("fetching AgentConfig %q: %w", agentDep.Spec.ConfigRef.Name, err)
			}
			logger.Info("AgentConfig not found, proceeding without it", "configRef", agentDep.Spec.ConfigRef.Name)
		} else {
			agentCfg = cfg
		}
	}

	// 3b. Optionally load the referenced AgentMemory.
	var agentMem *agentopsv1alpha1.AgentMemory
	if agentDep.Spec.MemoryRef != nil {
		mem := &agentopsv1alpha1.AgentMemory{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      agentDep.Spec.MemoryRef.Name,
			Namespace: agentDep.Namespace,
		}, mem); err != nil {
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("fetching AgentMemory %q: %w", agentDep.Spec.MemoryRef.Name, err)
			}
			logger.Info("AgentMemory not found, proceeding without it", "memoryRef", agentDep.Spec.MemoryRef.Name)
		} else {
			agentMem = mem
		}
	}

	// 4. Reconcile the owned k8s Deployment.
	if err := r.reconcileDeployment(ctx, agentDep, agentCfg, agentMem); err != nil {
		logger.Error(err, "failed to reconcile Deployment")
		r.setCondition(agentDep, "Ready", metav1.ConditionFalse, "ReconcileError", err.Error())
		_ = r.Status().Update(ctx, agentDep)
		return ctrl.Result{}, err
	}

	// 5. Sync status.readyReplicas from the owned Deployment.
	if err := r.syncStatus(ctx, agentDep); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *AgentDeploymentReconciler) reconcileDeployment(
	ctx context.Context,
	agentDep *agentopsv1alpha1.AgentDeployment,
	agentCfg *agentopsv1alpha1.AgentConfig,
	agentMem *agentopsv1alpha1.AgentMemory,
) error {
	desired := r.buildDeployment(agentDep, agentCfg, agentMem)

	if err := ctrl.SetControllerReference(agentDep, desired, r.Scheme); err != nil {
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
	existing.Spec.Template.Spec.Containers = desired.Spec.Template.Spec.Containers
	return r.Patch(ctx, existing, patch)
}

func (r *AgentDeploymentReconciler) syncStatus(
	ctx context.Context,
	agentDep *agentopsv1alpha1.AgentDeployment,
) error {
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{
		Name:      agentDep.Name + "-agent",
		Namespace: agentDep.Namespace,
	}, dep); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	agentDep.Status.Replicas = dep.Status.Replicas
	agentDep.Status.ReadyReplicas = dep.Status.ReadyReplicas
	agentDep.Status.ObservedGeneration = agentDep.Generation

	condStatus := metav1.ConditionFalse
	condReason := "Progressing"
	condMsg := fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, dep.Status.Replicas)
	if dep.Status.ReadyReplicas == dep.Status.Replicas && dep.Status.Replicas > 0 {
		condStatus = metav1.ConditionTrue
		condReason = "AllReplicasReady"
	}
	r.setCondition(agentDep, "Ready", condStatus, condReason, condMsg)

	return r.Status().Update(ctx, agentDep)
}

func (r *AgentDeploymentReconciler) buildDeployment(agentDep *agentopsv1alpha1.AgentDeployment, agentCfg *agentopsv1alpha1.AgentConfig, agentMem *agentopsv1alpha1.AgentMemory) *appsv1.Deployment {
	labels := map[string]string{
		"app.kubernetes.io/name":       "agent",
		"app.kubernetes.io/instance":   agentDep.Name,
		"app.kubernetes.io/managed-by": "agentops-operator",
		"agentops.io/deployment":       agentDep.Name,
	}

	replicas := int32(1)
	if agentDep.Spec.Replicas != nil {
		replicas = *agentDep.Spec.Replicas
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentDep.Name + "-agent",
			Namespace: agentDep.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            "agent",
						Image:           r.AgentImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Ports: []corev1.ContainerPort{
							{Name: "health", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
						},
						Env: r.buildEnvVars(agentDep, agentCfg, agentMem),
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

func (r *AgentDeploymentReconciler) buildEnvVars(
	agentDep *agentopsv1alpha1.AgentDeployment,
	agentCfg *agentopsv1alpha1.AgentConfig,
	agentMem *agentopsv1alpha1.AgentMemory,
) []corev1.EnvVar {
	mcpJSON, _ := json.Marshal(agentDep.Spec.MCPServers)

	maxTokens := 8000
	timeoutSecs := 120
	if agentDep.Spec.Limits != nil {
		if agentDep.Spec.Limits.MaxTokensPerCall > 0 {
			maxTokens = agentDep.Spec.Limits.MaxTokensPerCall
		}
		if agentDep.Spec.Limits.TimeoutSeconds > 0 {
			timeoutSecs = agentDep.Spec.Limits.TimeoutSeconds
		}
	}

	// Merge AgentConfig prompt fragments into the effective system prompt.
	systemPrompt := agentDep.Spec.SystemPrompt
	if agentCfg != nil {
		if agentCfg.Spec.PromptFragments.Persona != "" {
			systemPrompt = agentCfg.Spec.PromptFragments.Persona + "\n\n" + systemPrompt
		}
		if agentCfg.Spec.PromptFragments.OutputRules != "" {
			systemPrompt = systemPrompt + "\n\n" + agentCfg.Spec.PromptFragments.OutputRules
		}
	}

	envVars := []corev1.EnvVar{
		{Name: "AGENT_MODEL", Value: agentDep.Spec.Model},
		{Name: "AGENT_SYSTEM_PROMPT", Value: systemPrompt},
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
	if agentDep.Spec.LivenessProbe != nil &&
		agentDep.Spec.LivenessProbe.Type == agentopsv1alpha1.ProbeTypeSemantic &&
		agentDep.Spec.LivenessProbe.ValidatorPrompt != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "AGENT_VALIDATOR_PROMPT",
			Value: agentDep.Spec.LivenessProbe.ValidatorPrompt,
		})
	}

	// Propagate optional AgentConfig settings as env vars for the runtime.
	if agentCfg != nil {
		if agentCfg.Spec.Temperature != "" {
			envVars = append(envVars, corev1.EnvVar{Name: "AGENT_TEMPERATURE", Value: agentCfg.Spec.Temperature})
		}
		if agentCfg.Spec.OutputFormat != "" {
			envVars = append(envVars, corev1.EnvVar{Name: "AGENT_OUTPUT_FORMAT", Value: agentCfg.Spec.OutputFormat})
		}
		if agentCfg.Spec.MemoryBackend != "" {
			envVars = append(envVars, corev1.EnvVar{Name: "AGENT_MEMORY_BACKEND", Value: string(agentCfg.Spec.MemoryBackend)})
		}
	}

	// Propagate AgentMemory config as env vars. AgentMemory takes precedence over
	// any AGENT_MEMORY_BACKEND set via AgentConfig.
	if agentMem != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "AGENT_MEMORY_BACKEND",
			Value: string(agentMem.Spec.Backend),
		})
		switch agentMem.Spec.Backend {
		case agentopsv1alpha1.MemoryBackendRedis:
			if agentMem.Spec.Redis != nil {
				// Inject Redis URL from the referenced Secret.
				envVars = append(envVars, corev1.EnvVar{
					Name: "AGENT_MEMORY_REDIS_URL",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: agentMem.Spec.Redis.SecretRef.Name},
							Key:                  "REDIS_URL",
						},
					},
				})
				if agentMem.Spec.Redis.TTLSeconds > 0 {
					envVars = append(envVars, corev1.EnvVar{
						Name:  "AGENT_MEMORY_REDIS_TTL",
						Value: fmt.Sprintf("%d", agentMem.Spec.Redis.TTLSeconds),
					})
				}
				if agentMem.Spec.Redis.MaxEntries > 0 {
					envVars = append(envVars, corev1.EnvVar{
						Name:  "AGENT_MEMORY_REDIS_MAX_ENTRIES",
						Value: fmt.Sprintf("%d", agentMem.Spec.Redis.MaxEntries),
					})
				}
			}
		case agentopsv1alpha1.MemoryBackendVectorStore:
			if agentMem.Spec.VectorStore != nil {
				envVars = append(envVars,
					corev1.EnvVar{Name: "AGENT_MEMORY_VECTOR_STORE_PROVIDER", Value: string(agentMem.Spec.VectorStore.Provider)},
					corev1.EnvVar{Name: "AGENT_MEMORY_VECTOR_STORE_ENDPOINT", Value: agentMem.Spec.VectorStore.Endpoint},
					corev1.EnvVar{Name: "AGENT_MEMORY_VECTOR_STORE_COLLECTION", Value: agentMem.Spec.VectorStore.Collection},
				)
				if agentMem.Spec.VectorStore.SecretRef != nil {
					envVars = append(envVars, corev1.EnvVar{
						Name: "AGENT_MEMORY_VECTOR_STORE_API_KEY",
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: agentMem.Spec.VectorStore.SecretRef.Name},
								Key:                  "VECTOR_STORE_API_KEY",
							},
						},
					})
				}
				if agentMem.Spec.VectorStore.TTLSeconds > 0 {
					envVars = append(envVars, corev1.EnvVar{
						Name:  "AGENT_MEMORY_VECTOR_STORE_TTL",
						Value: fmt.Sprintf("%d", agentMem.Spec.VectorStore.TTLSeconds),
					})
				}
			}
		}
	}

	return envVars
}

func (r *AgentDeploymentReconciler) setCondition(
	agentDep *agentopsv1alpha1.AgentDeployment,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	apimeta.SetStatusCondition(&agentDep.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: agentDep.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentopsv1alpha1.AgentDeployment{}).
		Owns(&appsv1.Deployment{}).
		Named("agentdeployment").
		Complete(r)
}
