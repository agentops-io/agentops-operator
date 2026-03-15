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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	arkonisv1alpha1 "github.com/arkonis-dev/ark-operator/api/v1alpha1"
)

// ArkServiceReconciler reconciles a ArkService object
type ArkServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=arkonis.dev,resources=arkservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkservices/finalizers,verbs=update
// +kubebuilder:rbac:groups=arkonis.dev,resources=arkagents,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete

func (r *ArkServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	arkonisSvc := &arkonisv1alpha1.ArkService{}
	if err := r.Get(ctx, req.NamespacedName, arkonisSvc); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !arkonisSvc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Look up the backing ArkAgent.
	if arkonisSvc.Spec.Selector.ArkAgent == "" {
		r.setCondition(arkonisSvc, metav1.ConditionFalse, "NoSelector",
			"spec.selector.arkAgent is required")
		_ = r.Status().Update(ctx, arkonisSvc)
		return ctrl.Result{}, nil
	}
	arkAgent := &arkonisv1alpha1.ArkAgent{}
	if err := r.Get(ctx, client.ObjectKey{
		Name:      arkonisSvc.Spec.Selector.ArkAgent,
		Namespace: arkonisSvc.Namespace,
	}, arkAgent); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("ArkAgent not found, requeuing", "name", arkonisSvc.Spec.Selector.ArkAgent)
			r.setCondition(arkonisSvc, metav1.ConditionFalse, "DeploymentNotFound",
				"referenced ArkAgent does not exist")
			_ = r.Status().Update(ctx, arkonisSvc)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if err := r.reconcileService(ctx, arkonisSvc, arkAgent); err != nil {
		logger.Error(err, "failed to reconcile Service")
		r.setCondition(arkonisSvc, metav1.ConditionFalse, "ReconcileError", err.Error())
		_ = r.Status().Update(ctx, arkonisSvc)
		return ctrl.Result{}, err
	}

	arkonisSvc.Status.ReadyReplicas = arkAgent.Status.ReadyReplicas
	arkonisSvc.Status.ObservedGeneration = arkonisSvc.Generation
	r.setCondition(arkonisSvc, metav1.ConditionTrue, "Reconciled", "ArkService reconciled")
	return ctrl.Result{}, r.Status().Update(ctx, arkonisSvc)
}

func (r *ArkServiceReconciler) reconcileService(
	ctx context.Context,
	arkonisSvc *arkonisv1alpha1.ArkService,
	arkAgent *arkonisv1alpha1.ArkAgent,
) error {
	desired := r.buildService(arkonisSvc, arkAgent)

	if err := ctrl.SetControllerReference(arkonisSvc, desired, r.Scheme); err != nil {
		return err
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec.Ports = desired.Spec.Ports
	existing.Spec.Selector = desired.Spec.Selector
	return r.Patch(ctx, existing, patch)
}

func (r *ArkServiceReconciler) buildService(
	arkonisSvc *arkonisv1alpha1.ArkService,
	arkAgent *arkonisv1alpha1.ArkAgent,
) *corev1.Service {
	// Select the pods owned by the referenced ArkAgent.
	selector := map[string]string{
		"arkonis.dev/deployment": arkAgent.Name,
	}

	ports := make([]corev1.ServicePort, 0, len(arkonisSvc.Spec.Ports))
	for _, p := range arkonisSvc.Spec.Ports {
		ports = append(ports, corev1.ServicePort{
			Name:       strings.ToLower(string(p.Protocol)),
			Port:       p.Port,
			TargetPort: intstr.FromInt32(p.Port),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	// Default port when none specified.
	if len(ports) == 0 {
		ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       8081,
			TargetPort: intstr.FromInt32(8080),
			Protocol:   corev1.ProtocolTCP,
		}}
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      arkonisSvc.Name,
			Namespace: arkonisSvc.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "ark-operator",
				"arkonis.dev/service":          arkonisSvc.Name,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Ports:    ports,
			Type:     corev1.ServiceTypeClusterIP,
		},
	}
}

func (r *ArkServiceReconciler) setCondition(
	arkonisSvc *arkonisv1alpha1.ArkService,
	status metav1.ConditionStatus,
	reason, message string,
) {
	apimeta.SetStatusCondition(&arkonisSvc.Status.Conditions, metav1.Condition{
		Type:               arkonisv1alpha1.ConditionReady,
		Status:             status,
		ObservedGeneration: arkonisSvc.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// findServicesForAgent maps an ArkAgent change to the ArkService(s) that
// reference it, so their status.readyReplicas stays in sync.
func (r *ArkServiceReconciler) findServicesForAgent(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	arkAgent, ok := obj.(*arkonisv1alpha1.ArkAgent)
	if !ok {
		return nil
	}

	svcList := &arkonisv1alpha1.ArkServiceList{}
	if err := r.List(ctx, svcList, client.InNamespace(arkAgent.Namespace)); err != nil {
		return nil
	}

	var reqs []reconcile.Request
	for _, svc := range svcList.Items {
		if svc.Spec.Selector.ArkAgent == arkAgent.Name {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: client.ObjectKey{Name: svc.Name, Namespace: svc.Namespace},
			})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *ArkServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&arkonisv1alpha1.ArkService{}).
		Owns(&corev1.Service{}).
		// Re-reconcile any ArkService when its backing ArkAgent changes
		// so that status.readyReplicas is never stale.
		Watches(
			&arkonisv1alpha1.ArkAgent{},
			handler.EnqueueRequestsFromMapFunc(r.findServicesForAgent),
		).
		Named("arkservice").
		Complete(r)
}
