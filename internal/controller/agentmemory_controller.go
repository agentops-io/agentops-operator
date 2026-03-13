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

	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentopsv1alpha1 "github.com/agentops-io/agentops-operator/api/v1alpha1"
)

// AgentMemoryReconciler reconciles a AgentMemory object.
type AgentMemoryReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agentops.agentops.io,resources=agentmemories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agentops.agentops.io,resources=agentmemories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentops.agentops.io,resources=agentmemories/finalizers,verbs=update

// AgentMemory is a configuration resource (analogous to PersistentVolumeClaim).
// The reconciler validates the spec and sets a Ready condition.
// AgentDeployments reference it by name; the operator reads it during pod construction.
func (r *AgentMemoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	agentMem := &agentopsv1alpha1.AgentMemory{}
	if err := r.Get(ctx, req.NamespacedName, agentMem); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !agentMem.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if err := r.validate(agentMem); err != nil {
		agentMem.Status.ObservedGeneration = agentMem.Generation
		apimeta.SetStatusCondition(&agentMem.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			ObservedGeneration: agentMem.Generation,
			Reason:             "InvalidSpec",
			Message:            err.Error(),
		})
		return ctrl.Result{}, r.Status().Update(ctx, agentMem)
	}

	agentMem.Status.ObservedGeneration = agentMem.Generation
	apimeta.SetStatusCondition(&agentMem.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: agentMem.Generation,
		Reason:             "Accepted",
		Message:            "AgentMemory is valid and available",
	})

	return ctrl.Result{}, r.Status().Update(ctx, agentMem)
}

// validate checks that the spec is consistent (backend-specific config is present).
func (r *AgentMemoryReconciler) validate(agentMem *agentopsv1alpha1.AgentMemory) error {
	switch agentMem.Spec.Backend {
	case agentopsv1alpha1.MemoryBackendRedis:
		if agentMem.Spec.Redis == nil {
			return fmt.Errorf("spec.redis is required when backend is %q", agentopsv1alpha1.MemoryBackendRedis)
		}
		if agentMem.Spec.Redis.SecretRef.Name == "" {
			return fmt.Errorf("spec.redis.secretRef.name is required")
		}
	case agentopsv1alpha1.MemoryBackendVectorStore:
		if agentMem.Spec.VectorStore == nil {
			return fmt.Errorf("spec.vectorStore is required when backend is %q", agentopsv1alpha1.MemoryBackendVectorStore)
		}
		if agentMem.Spec.VectorStore.Endpoint == "" {
			return fmt.Errorf("spec.vectorStore.endpoint is required")
		}
	case agentopsv1alpha1.MemoryBackendInContext:
		// No additional config required.
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentMemoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentopsv1alpha1.AgentMemory{}).
		Named("agentmemory").
		Complete(r)
}
