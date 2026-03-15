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

	arkonisv1alpha1 "github.com/arkonis-dev/ark-operator/api/v1alpha1"
)

// ArkMemoryReconciler reconciles a ArkMemory object.
type ArkMemoryReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=arkonis.dev,resources=arkmemories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkmemories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkmemories/finalizers,verbs=update

// ArkMemory is a configuration resource (analogous to PersistentVolumeClaim).
// The reconciler validates the spec and sets a Ready condition.
// ArkAgents reference it by name; the operator reads it during pod construction.
func (r *ArkMemoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	arkMemory := &arkonisv1alpha1.ArkMemory{}
	if err := r.Get(ctx, req.NamespacedName, arkMemory); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !arkMemory.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if err := r.validate(arkMemory); err != nil {
		arkMemory.Status.ObservedGeneration = arkMemory.Generation
		apimeta.SetStatusCondition(&arkMemory.Status.Conditions, metav1.Condition{
			Type:               arkonisv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: arkMemory.Generation,
			Reason:             "InvalidSpec",
			Message:            err.Error(),
		})
		return ctrl.Result{}, r.Status().Update(ctx, arkMemory)
	}

	arkMemory.Status.ObservedGeneration = arkMemory.Generation
	apimeta.SetStatusCondition(&arkMemory.Status.Conditions, metav1.Condition{
		Type:               arkonisv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: arkMemory.Generation,
		Reason:             "Accepted",
		Message:            "ArkMemory is valid and available",
	})

	return ctrl.Result{}, r.Status().Update(ctx, arkMemory)
}

// validate checks that the spec is consistent (backend-specific config is present).
func (r *ArkMemoryReconciler) validate(arkMemory *arkonisv1alpha1.ArkMemory) error {
	switch arkMemory.Spec.Backend {
	case arkonisv1alpha1.MemoryBackendRedis:
		if arkMemory.Spec.Redis == nil {
			return fmt.Errorf("spec.redis is required when backend is %q", arkonisv1alpha1.MemoryBackendRedis)
		}
		if arkMemory.Spec.Redis.SecretRef.Name == "" {
			return fmt.Errorf("spec.redis.secretRef.name is required")
		}
	case arkonisv1alpha1.MemoryBackendVectorStore:
		if arkMemory.Spec.VectorStore == nil {
			return fmt.Errorf("spec.vectorStore is required when backend is %q", arkonisv1alpha1.MemoryBackendVectorStore)
		}
		if arkMemory.Spec.VectorStore.Endpoint == "" {
			return fmt.Errorf("spec.vectorStore.endpoint is required")
		}
	case arkonisv1alpha1.MemoryBackendInContext:
		// No additional config required.
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ArkMemoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&arkonisv1alpha1.ArkMemory{}).
		Named("arkmemory").
		Complete(r)
}
