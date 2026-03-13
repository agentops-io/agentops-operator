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

	arkonisv1alpha1 "github.com/arkonis-dev/arkonis-operator/api/v1alpha1"
)

const (
	// agentAPIKeysSecret is the k8s Secret expected to contain ANTHROPIC_API_KEY
	// and TASK_QUEUE_URL, injected via EnvFrom into every agent pod.
	agentAPIKeysSecret = "arkonis-api-keys"
)

// ArkonisDeploymentReconciler reconciles a ArkonisDeployment object
type ArkonisDeploymentReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	AgentImage string
}

// +kubebuilder:rbac:groups=arkonis.dev,resources=arkonisdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkonisdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkonisdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkonismemories,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *ArkonisDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the ArkonisDeployment CR.
	arkonisDep := &arkonisv1alpha1.ArkonisDeployment{}
	if err := r.Get(ctx, req.NamespacedName, arkonisDep); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. OwnerRef handles child cleanup on deletion — nothing extra needed.
	if !arkonisDep.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// 3. Optionally load the referenced ArkonisConfig.
	var arkonisCfg *arkonisv1alpha1.ArkonisConfig
	if arkonisDep.Spec.ConfigRef != nil {
		cfg := &arkonisv1alpha1.ArkonisConfig{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      arkonisDep.Spec.ConfigRef.Name,
			Namespace: arkonisDep.Namespace,
		}, cfg); err != nil {
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("fetching ArkonisConfig %q: %w", arkonisDep.Spec.ConfigRef.Name, err)
			}
			logger.Info("ArkonisConfig not found, proceeding without it", "configRef", arkonisDep.Spec.ConfigRef.Name)
		} else {
			arkonisCfg = cfg
		}
	}

	// 3b. Optionally load the referenced ArkonisMemory.
	var arkonisMem *arkonisv1alpha1.ArkonisMemory
	if arkonisDep.Spec.MemoryRef != nil {
		mem := &arkonisv1alpha1.ArkonisMemory{}
		if err := r.Get(ctx, client.ObjectKey{
			Name:      arkonisDep.Spec.MemoryRef.Name,
			Namespace: arkonisDep.Namespace,
		}, mem); err != nil {
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("fetching ArkonisMemory %q: %w", arkonisDep.Spec.MemoryRef.Name, err)
			}
			logger.Info("ArkonisMemory not found, proceeding without it", "memoryRef", arkonisDep.Spec.MemoryRef.Name)
		} else {
			arkonisMem = mem
		}
	}

	// 4. Reconcile the owned k8s Deployment.
	if err := r.reconcileDeployment(ctx, arkonisDep, arkonisCfg, arkonisMem); err != nil {
		logger.Error(err, "failed to reconcile Deployment")
		r.setCondition(arkonisDep, "Ready", metav1.ConditionFalse, "ReconcileError", err.Error())
		_ = r.Status().Update(ctx, arkonisDep)
		return ctrl.Result{}, err
	}

	// 5. Sync status.readyReplicas from the owned Deployment.
	if err := r.syncStatus(ctx, arkonisDep); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *ArkonisDeploymentReconciler) reconcileDeployment(
	ctx context.Context,
	arkonisDep *arkonisv1alpha1.ArkonisDeployment,
	arkonisCfg *arkonisv1alpha1.ArkonisConfig,
	arkonisMem *arkonisv1alpha1.ArkonisMemory,
) error {
	desired := r.buildDeployment(arkonisDep, arkonisCfg, arkonisMem)

	if err := ctrl.SetControllerReference(arkonisDep, desired, r.Scheme); err != nil {
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

func (r *ArkonisDeploymentReconciler) syncStatus(
	ctx context.Context,
	arkonisDep *arkonisv1alpha1.ArkonisDeployment,
) error {
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{
		Name:      arkonisDep.Name + "-agent",
		Namespace: arkonisDep.Namespace,
	}, dep); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	arkonisDep.Status.Replicas = dep.Status.Replicas
	arkonisDep.Status.ReadyReplicas = dep.Status.ReadyReplicas
	arkonisDep.Status.ObservedGeneration = arkonisDep.Generation

	condStatus := metav1.ConditionFalse
	condReason := "Progressing"
	condMsg := fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, dep.Status.Replicas)
	if dep.Status.ReadyReplicas == dep.Status.Replicas && dep.Status.Replicas > 0 {
		condStatus = metav1.ConditionTrue
		condReason = "AllReplicasReady"
	}
	r.setCondition(arkonisDep, "Ready", condStatus, condReason, condMsg)

	return r.Status().Update(ctx, arkonisDep)
}

func (r *ArkonisDeploymentReconciler) buildDeployment(arkonisDep *arkonisv1alpha1.ArkonisDeployment, arkonisCfg *arkonisv1alpha1.ArkonisConfig, arkonisMem *arkonisv1alpha1.ArkonisMemory) *appsv1.Deployment {
	labels := map[string]string{
		"app.kubernetes.io/name":       "agent",
		"app.kubernetes.io/instance":   arkonisDep.Name,
		"app.kubernetes.io/managed-by": "arkonis-operator",
		"arkonis.dev/deployment":       arkonisDep.Name,
	}

	replicas := int32(1)
	if arkonisDep.Spec.Replicas != nil {
		replicas = *arkonisDep.Spec.Replicas
	}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      arkonisDep.Name + "-agent",
			Namespace: arkonisDep.Namespace,
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
						Env: r.buildEnvVars(arkonisDep, arkonisCfg, arkonisMem),
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

func (r *ArkonisDeploymentReconciler) buildEnvVars(
	arkonisDep *arkonisv1alpha1.ArkonisDeployment,
	arkonisCfg *arkonisv1alpha1.ArkonisConfig,
	arkonisMem *arkonisv1alpha1.ArkonisMemory,
) []corev1.EnvVar {
	mcpJSON, _ := json.Marshal(arkonisDep.Spec.MCPServers)

	maxTokens := 8000
	timeoutSecs := 120
	if arkonisDep.Spec.Limits != nil {
		if arkonisDep.Spec.Limits.MaxTokensPerCall > 0 {
			maxTokens = arkonisDep.Spec.Limits.MaxTokensPerCall
		}
		if arkonisDep.Spec.Limits.TimeoutSeconds > 0 {
			timeoutSecs = arkonisDep.Spec.Limits.TimeoutSeconds
		}
	}

	// Merge ArkonisConfig prompt fragments into the effective system prompt.
	systemPrompt := arkonisDep.Spec.SystemPrompt
	if arkonisCfg != nil {
		if arkonisCfg.Spec.PromptFragments.Persona != "" {
			systemPrompt = arkonisCfg.Spec.PromptFragments.Persona + "\n\n" + systemPrompt
		}
		if arkonisCfg.Spec.PromptFragments.OutputRules != "" {
			systemPrompt = systemPrompt + "\n\n" + arkonisCfg.Spec.PromptFragments.OutputRules
		}
	}

	envVars := []corev1.EnvVar{
		{Name: "AGENT_MODEL", Value: arkonisDep.Spec.Model},
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
	if arkonisDep.Spec.LivenessProbe != nil &&
		arkonisDep.Spec.LivenessProbe.Type == arkonisv1alpha1.ProbeTypeSemantic &&
		arkonisDep.Spec.LivenessProbe.ValidatorPrompt != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "AGENT_VALIDATOR_PROMPT",
			Value: arkonisDep.Spec.LivenessProbe.ValidatorPrompt,
		})
	}

	// Propagate optional ArkonisConfig settings as env vars for the runtime.
	if arkonisCfg != nil {
		if arkonisCfg.Spec.Temperature != "" {
			envVars = append(envVars, corev1.EnvVar{Name: "AGENT_TEMPERATURE", Value: arkonisCfg.Spec.Temperature})
		}
		if arkonisCfg.Spec.OutputFormat != "" {
			envVars = append(envVars, corev1.EnvVar{Name: "AGENT_OUTPUT_FORMAT", Value: arkonisCfg.Spec.OutputFormat})
		}
		if arkonisCfg.Spec.MemoryBackend != "" {
			envVars = append(envVars, corev1.EnvVar{Name: "AGENT_MEMORY_BACKEND", Value: string(arkonisCfg.Spec.MemoryBackend)})
		}
	}

	// Propagate ArkonisMemory config as env vars. ArkonisMemory takes precedence over
	// any AGENT_MEMORY_BACKEND set via ArkonisConfig.
	if arkonisMem != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "AGENT_MEMORY_BACKEND",
			Value: string(arkonisMem.Spec.Backend),
		})
		switch arkonisMem.Spec.Backend {
		case arkonisv1alpha1.MemoryBackendRedis:
			if arkonisMem.Spec.Redis != nil {
				// Inject Redis URL from the referenced Secret.
				envVars = append(envVars, corev1.EnvVar{
					Name: "AGENT_MEMORY_REDIS_URL",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: arkonisMem.Spec.Redis.SecretRef.Name},
							Key:                  "REDIS_URL",
						},
					},
				})
				if arkonisMem.Spec.Redis.TTLSeconds > 0 {
					envVars = append(envVars, corev1.EnvVar{
						Name:  "AGENT_MEMORY_REDIS_TTL",
						Value: fmt.Sprintf("%d", arkonisMem.Spec.Redis.TTLSeconds),
					})
				}
				if arkonisMem.Spec.Redis.MaxEntries > 0 {
					envVars = append(envVars, corev1.EnvVar{
						Name:  "AGENT_MEMORY_REDIS_MAX_ENTRIES",
						Value: fmt.Sprintf("%d", arkonisMem.Spec.Redis.MaxEntries),
					})
				}
			}
		case arkonisv1alpha1.MemoryBackendVectorStore:
			if arkonisMem.Spec.VectorStore != nil {
				envVars = append(envVars,
					corev1.EnvVar{Name: "AGENT_MEMORY_VECTOR_STORE_PROVIDER", Value: string(arkonisMem.Spec.VectorStore.Provider)},
					corev1.EnvVar{Name: "AGENT_MEMORY_VECTOR_STORE_ENDPOINT", Value: arkonisMem.Spec.VectorStore.Endpoint},
					corev1.EnvVar{Name: "AGENT_MEMORY_VECTOR_STORE_COLLECTION", Value: arkonisMem.Spec.VectorStore.Collection},
				)
				if arkonisMem.Spec.VectorStore.SecretRef != nil {
					envVars = append(envVars, corev1.EnvVar{
						Name: "AGENT_MEMORY_VECTOR_STORE_API_KEY",
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: arkonisMem.Spec.VectorStore.SecretRef.Name},
								Key:                  "VECTOR_STORE_API_KEY",
							},
						},
					})
				}
				if arkonisMem.Spec.VectorStore.TTLSeconds > 0 {
					envVars = append(envVars, corev1.EnvVar{
						Name:  "AGENT_MEMORY_VECTOR_STORE_TTL",
						Value: fmt.Sprintf("%d", arkonisMem.Spec.VectorStore.TTLSeconds),
					})
				}
			}
		}
	}

	return envVars
}

func (r *ArkonisDeploymentReconciler) setCondition(
	arkonisDep *arkonisv1alpha1.ArkonisDeployment,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	apimeta.SetStatusCondition(&arkonisDep.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: arkonisDep.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *ArkonisDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&arkonisv1alpha1.ArkonisDeployment{}).
		Owns(&appsv1.Deployment{}).
		Named("arkonisdeployment").
		Complete(r)
}
