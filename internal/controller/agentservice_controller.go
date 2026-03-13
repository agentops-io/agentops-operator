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

	arkonisv1alpha1 "github.com/arkonis-dev/arkonis-operator/api/v1alpha1"
)

// ArkonisServiceReconciler reconciles a ArkonisService object
type ArkonisServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=arkonis.dev,resources=agentservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=arkonis.dev,resources=agentservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=arkonis.dev,resources=agentservices/finalizers,verbs=update
// +kubebuilder:rbac:groups=arkonis.dev,resources=agentdeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete

func (r *ArkonisServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	agentSvc := &arkonisv1alpha1.ArkonisService{}
	if err := r.Get(ctx, req.NamespacedName, agentSvc); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !agentSvc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Look up the backing ArkonisDeployment.
	if agentSvc.Spec.Selector.ArkonisDeployment == "" {
		r.setCondition(agentSvc, metav1.ConditionFalse, "NoSelector",
			"spec.selector.agentDeployment is required")
		_ = r.Status().Update(ctx, agentSvc)
		return ctrl.Result{}, nil
	}
	agentDep := &arkonisv1alpha1.ArkonisDeployment{}
	if err := r.Get(ctx, client.ObjectKey{
		Name:      agentSvc.Spec.Selector.ArkonisDeployment,
		Namespace: agentSvc.Namespace,
	}, agentDep); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("ArkonisDeployment not found, requeuing", "name", agentSvc.Spec.Selector.ArkonisDeployment)
			r.setCondition(agentSvc, metav1.ConditionFalse, "DeploymentNotFound",
				"referenced ArkonisDeployment does not exist")
			_ = r.Status().Update(ctx, agentSvc)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if err := r.reconcileService(ctx, agentSvc, agentDep); err != nil {
		logger.Error(err, "failed to reconcile Service")
		r.setCondition(agentSvc, metav1.ConditionFalse, "ReconcileError", err.Error())
		_ = r.Status().Update(ctx, agentSvc)
		return ctrl.Result{}, err
	}

	agentSvc.Status.ReadyReplicas = agentDep.Status.ReadyReplicas
	agentSvc.Status.ObservedGeneration = agentSvc.Generation
	r.setCondition(agentSvc, metav1.ConditionTrue, "Reconciled", "ArkonisService reconciled")
	return ctrl.Result{}, r.Status().Update(ctx, agentSvc)
}

func (r *ArkonisServiceReconciler) reconcileService(
	ctx context.Context,
	agentSvc *arkonisv1alpha1.ArkonisService,
	agentDep *arkonisv1alpha1.ArkonisDeployment,
) error {
	desired := r.buildService(agentSvc, agentDep)

	if err := ctrl.SetControllerReference(agentSvc, desired, r.Scheme); err != nil {
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

func (r *ArkonisServiceReconciler) buildService(
	agentSvc *arkonisv1alpha1.ArkonisService,
	agentDep *arkonisv1alpha1.ArkonisDeployment,
) *corev1.Service {
	// Select the pods owned by the referenced ArkonisDeployment.
	selector := map[string]string{
		"arkonis.dev/deployment": agentDep.Name,
	}

	ports := make([]corev1.ServicePort, 0, len(agentSvc.Spec.Ports))
	for _, p := range agentSvc.Spec.Ports {
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
			Name:      agentSvc.Name,
			Namespace: agentSvc.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "arkonis-operator",
				"arkonis.dev/service":          agentSvc.Name,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Ports:    ports,
			Type:     corev1.ServiceTypeClusterIP,
		},
	}
}

func (r *ArkonisServiceReconciler) setCondition(
	agentSvc *arkonisv1alpha1.ArkonisService,
	status metav1.ConditionStatus,
	reason, message string,
) {
	apimeta.SetStatusCondition(&agentSvc.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		ObservedGeneration: agentSvc.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// findServicesForDeployment maps an ArkonisDeployment change to the ArkonisService(s) that
// reference it, so their status.readyReplicas stays in sync.
func (r *ArkonisServiceReconciler) findServicesForDeployment(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	agentDep, ok := obj.(*arkonisv1alpha1.ArkonisDeployment)
	if !ok {
		return nil
	}

	svcList := &arkonisv1alpha1.ArkonisServiceList{}
	if err := r.List(ctx, svcList, client.InNamespace(agentDep.Namespace)); err != nil {
		return nil
	}

	var reqs []reconcile.Request
	for _, svc := range svcList.Items {
		if svc.Spec.Selector.ArkonisDeployment == agentDep.Name {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: client.ObjectKey{Name: svc.Name, Namespace: svc.Namespace},
			})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *ArkonisServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&arkonisv1alpha1.ArkonisService{}).
		Owns(&corev1.Service{}).
		// Re-reconcile any ArkonisService when its backing ArkonisDeployment changes
		// so that status.readyReplicas is never stale.
		Watches(
			&arkonisv1alpha1.ArkonisDeployment{},
			handler.EnqueueRequestsFromMapFunc(r.findServicesForDeployment),
		).
		Named("agentservice").
		Complete(r)
}
