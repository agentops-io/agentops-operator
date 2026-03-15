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

	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	arkonisv1alpha1 "github.com/arkonis-dev/ark-operator/api/v1alpha1"
)

// ArkSettingsReconciler reconciles a ArkSettings object
type ArkSettingsReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=arkonis.dev,resources=arksettings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=arkonis.dev,resources=arksettings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=arkonis.dev,resources=arksettings/finalizers,verbs=update

// ArkSettings is a storage-only resource (analogous to ConfigMap).
// The reconciler just acknowledges the resource and sets a Ready condition.
// ArkAgents reference it by name; the operator reads it during pod construction.
func (r *ArkSettingsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	arkSettings := &arkonisv1alpha1.ArkSettings{}
	if err := r.Get(ctx, req.NamespacedName, arkSettings); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !arkSettings.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	arkSettings.Status.ObservedGeneration = arkSettings.Generation
	apimeta.SetStatusCondition(&arkSettings.Status.Conditions, metav1.Condition{
		Type:               arkonisv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: arkSettings.Generation,
		Reason:             "Accepted",
		Message:            "ArkSettings is valid and available",
	})

	return ctrl.Result{}, r.Status().Update(ctx, arkSettings)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ArkSettingsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&arkonisv1alpha1.ArkSettings{}).
		Named("arksettings").
		Complete(r)
}
