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

	arkonisv1alpha1 "github.com/arkonis-dev/arkonis-operator/api/v1alpha1"
)

// ArkonisConfigReconciler reconciles a ArkonisConfig object
type ArkonisConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=arkonis.dev,resources=arkonisconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkonisconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkonisconfigs/finalizers,verbs=update

// ArkonisConfig is a storage-only resource (analogous to ConfigMap).
// The reconciler just acknowledges the resource and sets a Ready condition.
// ArkonisDeployments reference it by name; the operator reads it during pod construction.
func (r *ArkonisConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	arkonisCfg := &arkonisv1alpha1.ArkonisConfig{}
	if err := r.Get(ctx, req.NamespacedName, arkonisCfg); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !arkonisCfg.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	arkonisCfg.Status.ObservedGeneration = arkonisCfg.Generation
	apimeta.SetStatusCondition(&arkonisCfg.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: arkonisCfg.Generation,
		Reason:             "Accepted",
		Message:            "ArkonisConfig is valid and available",
	})

	return ctrl.Result{}, r.Status().Update(ctx, arkonisCfg)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ArkonisConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&arkonisv1alpha1.ArkonisConfig{}).
		Named("arkonisconfig").
		Complete(r)
}
