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

	arkonisv1alpha1 "github.com/arkonis-dev/arkonis-operator/api/v1alpha1"
)

var _ = Describe("ArkonisPipeline Controller", func() {
	const (
		resourceName = "test-pipeline"
		namespace    = "default"
	)

	ctx := context.Background()
	namespacedName := types.NamespacedName{Name: resourceName, Namespace: namespace}

	AfterEach(func() {
		pipeline := &arkonisv1alpha1.ArkonisPipeline{}
		if err := k8sClient.Get(ctx, namespacedName, pipeline); err == nil {
			Expect(k8sClient.Delete(ctx, pipeline)).To(Succeed())
		}
	})

	Context("When a step references an unknown dependency (invalid DAG)", func() {
		BeforeEach(func() {
			By("creating a pipeline where a step depends on a step that does not exist")
			resource := &arkonisv1alpha1.ArkonisPipeline{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: arkonisv1alpha1.ArkonisPipelineSpec{
					Steps: []arkonisv1alpha1.PipelineStep{
						{
							Name:              "summarize",
							ArkonisDeployment: "summarizer-agent",
							DependsOn:         []string{"nonexistent-step"},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		It("should set Ready=False with reason InvalidDAG", func() {
			By("running the reconciler")
			r := &ArkonisPipelineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("fetching the updated pipeline status")
			pipeline := &arkonisv1alpha1.ArkonisPipeline{}
			Expect(k8sClient.Get(ctx, namespacedName, pipeline)).To(Succeed())

			cond := apimeta.FindStatusCondition(pipeline.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("InvalidDAG"))
			Expect(cond.Message).To(ContainSubstring("nonexistent-step"))
		})
	})

	Context("When the referenced ArkonisDeployment is missing", func() {
		BeforeEach(func() {
			By("creating an ArkonisPipeline referencing a nonexistent ArkonisDeployment")
			resource := &arkonisv1alpha1.ArkonisPipeline{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: arkonisv1alpha1.ArkonisPipelineSpec{
					Steps: []arkonisv1alpha1.PipelineStep{
						{
							Name:              "research",
							ArkonisDeployment: "research-agent",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		It("should set Ready=False with reason DeploymentNotFound", func() {
			By("running the reconciler")
			r := &ArkonisPipelineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			pipeline := &arkonisv1alpha1.ArkonisPipeline{}
			Expect(k8sClient.Get(ctx, namespacedName, pipeline)).To(Succeed())

			cond := apimeta.FindStatusCondition(pipeline.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("DeploymentNotFound"))
		})
	})

	Context("When the pipeline is valid but no Redis is configured", func() {
		const agentDepName = "pipeline-agent"
		agentDepKey := types.NamespacedName{Name: agentDepName, Namespace: namespace}

		BeforeEach(func() {
			By("creating the ArkonisDeployment the pipeline references")
			replicas := int32(1)
			dep := &arkonisv1alpha1.ArkonisDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: agentDepName, Namespace: namespace},
				Spec: arkonisv1alpha1.ArkonisDeploymentSpec{
					Replicas:     &replicas,
					Model:        "claude-haiku-4-5",
					SystemPrompt: "You are helpful.",
				},
			}
			Expect(k8sClient.Create(ctx, dep)).To(Succeed())

			By("creating the ArkonisPipeline with a valid step")
			resource := &arkonisv1alpha1.ArkonisPipeline{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: arkonisv1alpha1.ArkonisPipelineSpec{
					Steps: []arkonisv1alpha1.PipelineStep{
						{
							Name:              "research",
							ArkonisDeployment: agentDepName,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			dep := &arkonisv1alpha1.ArkonisDeployment{}
			if err := k8sClient.Get(ctx, agentDepKey, dep); err == nil {
				Expect(k8sClient.Delete(ctx, dep)).To(Succeed())
			}
		})

		It("should set Ready=False with reason NoTaskQueue", func() {
			By("running the reconciler with no TaskQueueURL set")
			r := &ArkonisPipelineReconciler{
				Client:       k8sClient,
				Scheme:       k8sClient.Scheme(),
				TaskQueueURL: "", // no Redis
			}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: namespacedName})
			Expect(err).NotTo(HaveOccurred())

			pipeline := &arkonisv1alpha1.ArkonisPipeline{}
			Expect(k8sClient.Get(ctx, namespacedName, pipeline)).To(Succeed())

			cond := apimeta.FindStatusCondition(pipeline.Status.Conditions, "Ready")
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("NoTaskQueue"))
		})
	})

	Context("When reconciling a nonexistent ArkonisPipeline", func() {
		It("should return without error", func() {
			r := &ArkonisPipelineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "does-not-exist", Namespace: namespace},
			})
			Expect(err).NotTo(HaveOccurred())
		})
	})
})
