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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MemoryBackend defines the memory storage strategy for an agent.
// +kubebuilder:validation:Enum=in-context;vector-store;redis
type MemoryBackend string

const (
	MemoryBackendInContext   MemoryBackend = "in-context"
	MemoryBackendVectorStore MemoryBackend = "vector-store"
	MemoryBackendRedis       MemoryBackend = "redis"
)

// PromptFragments holds reusable prompt components.
type PromptFragments struct {
	// Persona is a persona/role description prepended to the system prompt.
	Persona string `json:"persona,omitempty"`
	// OutputRules defines output format constraints appended to the system prompt.
	OutputRules string `json:"outputRules,omitempty"`
}

// ArkonisConfigSpec defines the shared configuration values.
type ArkonisConfigSpec struct {
	// Temperature controls response randomness (0.0–1.0).
	// +kubebuilder:validation:Pattern=`^(0(\.[0-9]+)?|1(\.0+)?)$`
	Temperature string `json:"temperature,omitempty"`

	// OutputFormat specifies the expected output format (e.g. "structured-json").
	OutputFormat string `json:"outputFormat,omitempty"`

	// MemoryBackend defines where agent memory is stored.
	// +kubebuilder:default=in-context
	MemoryBackend MemoryBackend `json:"memoryBackend,omitempty"`

	// PromptFragments holds reusable prompt snippets.
	PromptFragments PromptFragments `json:"promptFragments,omitempty"`
}

// ArkonisConfigStatus defines the observed state of ArkonisConfig.
type ArkonisConfigStatus struct {
	// ObservedGeneration is the .metadata.generation this status reflects.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions reflect the current state of the ArkonisConfig.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Memory",type=string,JSONPath=`.spec.memoryBackend`
// +kubebuilder:printcolumn:name="OutputFormat",type=string,JSONPath=`.spec.outputFormat`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:resource:shortName=aocfg,scope=Namespaced

// ArkonisConfig holds shared configuration consumed by ArkonisDeployments.
type ArkonisConfig struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec ArkonisConfigSpec `json:"spec"`

	// +optional
	Status ArkonisConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ArkonisConfigList contains a list of ArkonisConfig.
type ArkonisConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ArkonisConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ArkonisConfig{}, &ArkonisConfigList{})
}
