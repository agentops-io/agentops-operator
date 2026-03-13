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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	arkonisv1alpha1 "github.com/arkonis-dev/arkonis-operator/api/v1alpha1"
)

var _ = Describe("ArkonisConfig Controller", func() {
	const (
		resourceName = "test-agentconfig"
		namespace    = "default"
	)

	ctx := context.Background()
	namespacedName := types.NamespacedName{Name: resourceName, Namespace: namespace}

	AfterEach(func() {
		cfg := &arkonisv1alpha1.ArkonisConfig{}
		if err := k8sClient.Get(ctx, namespacedName, cfg); err == nil {
			Expect(k8sClient.Delete(ctx, cfg)).To(Succeed())
		}
	})

	Context("When reconciling a minimal ArkonisConfig", func() {
		BeforeEach(func() {
			By("creating an ArkonisConfig with no spec fields set")
			resource := &arkonisv1alpha1.ArkonisConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		It("should set Ready=True with reason Accepted", func() {
			By("running the reconciler")
			r := &ArkonisConfigReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("fetching the updated status")
			cfg := &arkonisv1alpha1.ArkonisConfig{}
			Expect(k8sClient.Get(ctx, namespacedName, cfg)).To(Succeed())

			cond := apimeta.FindStatusCondition(cfg.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("Accepted"))
		})

		It("should set ObservedGeneration to match the resource generation", func() {
			By("running the reconciler")
			r := &ArkonisConfigReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			cfg := &arkonisv1alpha1.ArkonisConfig{}
			Expect(k8sClient.Get(ctx, namespacedName, cfg)).To(Succeed())
			Expect(cfg.Status.ObservedGeneration).To(Equal(cfg.Generation))
		})
	})

	Context("When reconciling an ArkonisConfig with spec values", func() {
		BeforeEach(func() {
			By("creating an ArkonisConfig with temperature, outputFormat, and prompt fragments")
			resource := &arkonisv1alpha1.ArkonisConfig{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: arkonisv1alpha1.ArkonisConfigSpec{
					Temperature:   "0.7",
					OutputFormat:  "structured-json",
					MemoryBackend: arkonisv1alpha1.MemoryBackendInContext,
					PromptFragments: arkonisv1alpha1.PromptFragments{
						Persona:     "You are an expert analyst.",
						OutputRules: "Always cite your sources.",
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		It("should set Ready=True regardless of which spec fields are set", func() {
			r := &ArkonisConfigReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			cfg := &arkonisv1alpha1.ArkonisConfig{}
			Expect(k8sClient.Get(ctx, namespacedName, cfg)).To(Succeed())

			cond := apimeta.FindStatusCondition(cfg.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))

			By("verifying spec values are preserved unchanged")
			Expect(cfg.Spec.Temperature).To(Equal("0.7"))
			Expect(cfg.Spec.OutputFormat).To(Equal("structured-json"))
			Expect(cfg.Spec.PromptFragments.Persona).To(Equal("You are an expert analyst."))
		})
	})

	Context("When reconciling a nonexistent ArkonisConfig", func() {
		It("should return without error", func() {
			r := &ArkonisConfigReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "does-not-exist", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
