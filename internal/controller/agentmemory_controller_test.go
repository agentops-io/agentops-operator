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

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	agentopsv1alpha1 "github.com/agentops-io/agentops-operator/api/v1alpha1"
)

var _ = Describe("AgentMemory Controller", func() {
	const (
		namespace = "default"
	)
	ctx := context.Background()

	newReconciler := func() *AgentMemoryReconciler {
		return &AgentMemoryReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}

	reconcileAndFetch := func(name string) *agentopsv1alpha1.AgentMemory {
		nn := types.NamespacedName{Name: name, Namespace: namespace}
		_, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		mem := &agentopsv1alpha1.AgentMemory{}
		Expect(k8sClient.Get(ctx, nn, mem)).To(Succeed())
		return mem
	}

	cleanup := func(name string) {
		mem := &agentopsv1alpha1.AgentMemory{}
		nn := types.NamespacedName{Name: name, Namespace: namespace}
		if err := k8sClient.Get(ctx, nn, mem); err == nil {
			Expect(k8sClient.Delete(ctx, mem)).To(Succeed())
		}
	}

	Context("in-context backend", func() {
		const name = "mem-incontext"
		AfterEach(func() { cleanup(name) })

		It("should set Ready=True with no extra config required", func() {
			Expect(k8sClient.Create(ctx, &agentopsv1alpha1.AgentMemory{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: agentopsv1alpha1.AgentMemorySpec{
					Backend: agentopsv1alpha1.MemoryBackendInContext,
				},
			})).To(Succeed())

			mem := reconcileAndFetch(name)
			cond := apimeta.FindStatusCondition(mem.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("Accepted"))
		})
	})

	Context("redis backend", func() {
		const name = "mem-redis"
		AfterEach(func() { cleanup(name) })

		It("should set Ready=True when secretRef is provided", func() {
			Expect(k8sClient.Create(ctx, &agentopsv1alpha1.AgentMemory{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: agentopsv1alpha1.AgentMemorySpec{
					Backend: agentopsv1alpha1.MemoryBackendRedis,
					Redis: &agentopsv1alpha1.RedisMemoryConfig{
						SecretRef:  agentopsv1alpha1.LocalObjectReference{Name: "redis-secret"},
						TTLSeconds: 3600,
					},
				},
			})).To(Succeed())

			mem := reconcileAndFetch(name)
			cond := apimeta.FindStatusCondition(mem.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("should set Ready=False when spec.redis is missing", func() {
			Expect(k8sClient.Create(ctx, &agentopsv1alpha1.AgentMemory{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: agentopsv1alpha1.AgentMemorySpec{
					Backend: agentopsv1alpha1.MemoryBackendRedis,
					// Redis field intentionally omitted
				},
			})).To(Succeed())

			mem := reconcileAndFetch(name)
			cond := apimeta.FindStatusCondition(mem.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("InvalidSpec"))
			Expect(cond.Message).To(ContainSubstring("spec.redis is required"))
		})

		It("should set Ready=False when secretRef.name is empty", func() {
			Expect(k8sClient.Create(ctx, &agentopsv1alpha1.AgentMemory{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: agentopsv1alpha1.AgentMemorySpec{
					Backend: agentopsv1alpha1.MemoryBackendRedis,
					Redis: &agentopsv1alpha1.RedisMemoryConfig{
						SecretRef: agentopsv1alpha1.LocalObjectReference{Name: ""},
					},
				},
			})).To(Succeed())

			mem := reconcileAndFetch(name)
			cond := apimeta.FindStatusCondition(mem.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Message).To(ContainSubstring("secretRef.name is required"))
		})
	})

	Context("vector-store backend", func() {
		const name = "mem-vectorstore"
		AfterEach(func() { cleanup(name) })

		It("should set Ready=True when endpoint is provided", func() {
			Expect(k8sClient.Create(ctx, &agentopsv1alpha1.AgentMemory{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: agentopsv1alpha1.AgentMemorySpec{
					Backend: agentopsv1alpha1.MemoryBackendVectorStore,
					VectorStore: &agentopsv1alpha1.VectorStoreMemoryConfig{
						Provider:   agentopsv1alpha1.VectorStoreProviderQdrant,
						Endpoint:   "http://qdrant.agent-infra.svc.cluster.local:6333",
						Collection: "agent-memories",
					},
				},
			})).To(Succeed())

			mem := reconcileAndFetch(name)
			cond := apimeta.FindStatusCondition(mem.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})

		It("should set Ready=False when spec.vectorStore is missing", func() {
			Expect(k8sClient.Create(ctx, &agentopsv1alpha1.AgentMemory{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: agentopsv1alpha1.AgentMemorySpec{
					Backend: agentopsv1alpha1.MemoryBackendVectorStore,
					// VectorStore field intentionally omitted
				},
			})).To(Succeed())

			mem := reconcileAndFetch(name)
			cond := apimeta.FindStatusCondition(mem.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Message).To(ContainSubstring("spec.vectorStore is required"))
		})

		It("should set Ready=False when endpoint is empty", func() {
			Expect(k8sClient.Create(ctx, &agentopsv1alpha1.AgentMemory{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: agentopsv1alpha1.AgentMemorySpec{
					Backend: agentopsv1alpha1.MemoryBackendVectorStore,
					VectorStore: &agentopsv1alpha1.VectorStoreMemoryConfig{
						Provider: agentopsv1alpha1.VectorStoreProviderQdrant,
						Endpoint: "", // missing
					},
				},
			})).To(Succeed())

			mem := reconcileAndFetch(name)
			cond := apimeta.FindStatusCondition(mem.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Message).To(ContainSubstring("endpoint is required"))
		})
	})

	Context("when the AgentMemory does not exist", func() {
		It("should return without error", func() {
			_, err := newReconciler().Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "does-not-exist", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
