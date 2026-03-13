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

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	arkonisv1alpha1 "github.com/arkonis-dev/arkonis-operator/api/v1alpha1"
)

const testAgentImage = "ghcr.io/arkonis-dev/arkonis-runtime:latest"

var _ = Describe("ArkonisDeployment Controller", func() {
	const (
		resourceName = "test-agent"
		namespace    = "default"
	)

	ctx := context.Background()

	namespacedName := types.NamespacedName{Name: resourceName, Namespace: namespace}
	backingDeploymentName := types.NamespacedName{Name: resourceName + "-agent", Namespace: namespace}

	AfterEach(func() {
		ad := &arkonisv1alpha1.ArkonisDeployment{}
		if err := k8sClient.Get(ctx, namespacedName, ad); err == nil {
			Expect(k8sClient.Delete(ctx, ad)).To(Succeed())
		}
		dep := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, backingDeploymentName, dep); err == nil {
			Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
		}
	})

	Context("When reconciling a valid ArkonisDeployment", func() {
		BeforeEach(func() {
			By("creating an ArkonisDeployment with required fields")
			replicas := int32(2)
			resource := &arkonisv1alpha1.ArkonisDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: arkonisv1alpha1.ArkonisDeploymentSpec{
					Replicas:     &replicas,
					Model:        "claude-haiku-4-5",
					SystemPrompt: "You are a helpful assistant.",
				},
			}
			err := k8sClient.Get(ctx, namespacedName, &arkonisv1alpha1.ArkonisDeployment{})
			if errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		It("should create a backing Deployment", func() {
			By("running the reconciler")
			reconciler := &ArkonisDeploymentReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				AgentImage: testAgentImage,
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the backing Deployment was created")
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, backingDeploymentName, dep)).To(Succeed())

			By("verifying the Deployment has the correct replica count")
			Expect(dep.Spec.Replicas).NotTo(BeNil())
			Expect(*dep.Spec.Replicas).To(Equal(int32(2)))

			By("verifying the Deployment has the agent selector label")
			Expect(dep.Spec.Selector.MatchLabels).To(HaveKey("arkonis.dev/deployment"))
			Expect(dep.Spec.Selector.MatchLabels["arkonis.dev/deployment"]).To(Equal(resourceName))

			By("verifying the container uses the agent runtime image")
			Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(dep.Spec.Template.Spec.Containers[0].Name).To(Equal("agent"))
			Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(testAgentImage))

			By("verifying AGENT_MODEL env var is set correctly")
			envVars := dep.Spec.Template.Spec.Containers[0].Env
			var modelEnv string
			for _, e := range envVars {
				if e.Name == "AGENT_MODEL" {
					modelEnv = e.Value
				}
			}
			Expect(modelEnv).To(Equal("claude-haiku-4-5"))
		})

		It("should set status conditions after reconciliation", func() {
			By("running the reconciler twice (create + sync status)")
			reconciler := &ArkonisDeploymentReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				AgentImage: testAgentImage,
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile picks up the Deployment status.
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("fetching the updated ArkonisDeployment status")
			ad := &arkonisv1alpha1.ArkonisDeployment{}
			Expect(k8sClient.Get(ctx, namespacedName, ad)).To(Succeed())

			By("verifying the Ready condition is present")
			cond := apimeta.FindStatusCondition(ad.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
		})

		It("should set an owner reference on the backing Deployment", func() {
			By("running the reconciler")
			reconciler := &ArkonisDeploymentReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				AgentImage: testAgentImage,
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("verifying the Deployment has an owner reference pointing to the ArkonisDeployment")
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, backingDeploymentName, dep)).To(Succeed())
			Expect(dep.OwnerReferences).To(HaveLen(1))
			Expect(dep.OwnerReferences[0].Kind).To(Equal("ArkonisDeployment"))
			Expect(dep.OwnerReferences[0].Name).To(Equal(resourceName))
		})
	})

	Context("When the ArkonisDeployment is deleted", func() {
		It("should reconcile without error for a missing resource", func() {
			reconciler := &ArkonisDeploymentReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				AgentImage: testAgentImage,
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Context("When an ArkonisDeployment references an ArkonisMemory", func() {
		const (
			arkonisName = "mem-agent"
			memName     = "test-mem"
		)
		arkonisKey := types.NamespacedName{Name: arkonisName, Namespace: namespace}
		memKey := types.NamespacedName{Name: memName, Namespace: namespace}
		backingKey := types.NamespacedName{Name: arkonisName + "-agent", Namespace: namespace}

		BeforeEach(func() {
			By("creating an ArkonisMemory with redis backend")
			Expect(k8sClient.Create(ctx, &arkonisv1alpha1.ArkonisMemory{
				ObjectMeta: metav1.ObjectMeta{Name: memName, Namespace: namespace},
				Spec: arkonisv1alpha1.ArkonisMemorySpec{
					Backend: arkonisv1alpha1.MemoryBackendRedis,
					Redis: &arkonisv1alpha1.RedisMemoryConfig{
						SecretRef:  arkonisv1alpha1.LocalObjectReference{Name: "redis-secret"},
						TTLSeconds: 1800,
					},
				},
			})).To(Succeed())

			By("creating an ArkonisDeployment that references the ArkonisMemory")
			replicas := int32(1)
			Expect(k8sClient.Create(ctx, &arkonisv1alpha1.ArkonisDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: arkonisName, Namespace: namespace},
				Spec: arkonisv1alpha1.ArkonisDeploymentSpec{
					Replicas:     &replicas,
					Model:        "claude-haiku-4-5",
					SystemPrompt: "You are a helpful assistant.",
					MemoryRef:    &arkonisv1alpha1.LocalObjectReference{Name: memName},
				},
			})).To(Succeed())
		})

		AfterEach(func() {
			ad := &arkonisv1alpha1.ArkonisDeployment{}
			if err := k8sClient.Get(ctx, arkonisKey, ad); err == nil {
				Expect(k8sClient.Delete(ctx, ad)).To(Succeed())
			}
			mem := &arkonisv1alpha1.ArkonisMemory{}
			if err := k8sClient.Get(ctx, memKey, mem); err == nil {
				Expect(k8sClient.Delete(ctx, mem)).To(Succeed())
			}
			dep := &appsv1.Deployment{}
			if err := k8sClient.Get(ctx, backingKey, dep); err == nil {
				Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
			}
		})

		It("should inject AGENT_MEMORY_BACKEND env var into the backing Deployment", func() {
			reconciler := &ArkonisDeploymentReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				AgentImage: testAgentImage,
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: arkonisKey})
			Expect(err).NotTo(HaveOccurred())

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, backingKey, dep)).To(Succeed())

			envVars := dep.Spec.Template.Spec.Containers[0].Env
			envMap := make(map[string]string)
			for _, e := range envVars {
				envMap[e.Name] = e.Value
			}

			Expect(envMap).To(HaveKeyWithValue("AGENT_MEMORY_BACKEND", string(arkonisv1alpha1.MemoryBackendRedis)))
			Expect(envMap).To(HaveKeyWithValue("AGENT_MEMORY_REDIS_TTL", "1800"))
		})

		It("should reconcile without error when the referenced ArkonisMemory does not exist", func() {
			By("deleting the ArkonisMemory before reconciling")
			mem := &arkonisv1alpha1.ArkonisMemory{}
			Expect(k8sClient.Get(ctx, memKey, mem)).To(Succeed())
			Expect(k8sClient.Delete(ctx, mem)).To(Succeed())

			reconciler := &ArkonisDeploymentReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				AgentImage: testAgentImage,
			}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: arkonisKey})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
