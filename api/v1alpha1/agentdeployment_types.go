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

// MCPServerSpec defines one MCP tool server.
type MCPServerSpec struct {
	// Name is a human-readable identifier for this MCP server.
	Name string `json:"name"`
	// URL is the SSE endpoint of the MCP server.
	URL string `json:"url"`
}

// ArkonisLimits constrains per-agent resource usage.
type ArkonisLimits struct {
	// MaxTokensPerCall is the maximum number of tokens per Anthropic API call.
	// +kubebuilder:default=8000
	MaxTokensPerCall int `json:"maxTokensPerCall,omitempty"`
	// MaxConcurrentTasks is the maximum number of tasks processed in parallel per replica.
	// +kubebuilder:default=5
	MaxConcurrentTasks int `json:"maxConcurrentTasks,omitempty"`
	// TimeoutSeconds is the per-task deadline.
	// +kubebuilder:default=120
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

// ProbeType defines the kind of liveness check.
// +kubebuilder:validation:Enum=semantic;ping
type ProbeType string

const (
	ProbeTypeSemantic ProbeType = "semantic"
	ProbeTypePing     ProbeType = "ping"
)

// ArkonisProbe defines how to evaluate agent health.
type ArkonisProbe struct {
	// Type is the probe strategy: "semantic" (LLM-based) or "ping" (HTTP).
	Type ProbeType `json:"type"`
	// IntervalSeconds is how often the probe runs.
	// +kubebuilder:default=30
	IntervalSeconds int `json:"intervalSeconds,omitempty"`
	// ValidatorPrompt is the prompt sent when Type is "semantic".
	ValidatorPrompt string `json:"validatorPrompt,omitempty"`
}

// ArkonisDeploymentSpec defines the desired state of ArkonisDeployment.
type ArkonisDeploymentSpec struct {
	// Replicas is the number of agent instances to run.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=50
	Replicas *int32 `json:"replicas,omitempty"`

	// Model is the LLM model ID (e.g. "claude-sonnet-4-20250514").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Model string `json:"model"`

	// SystemPrompt is injected as the agent's system message.
	// +kubebuilder:validation:Required
	SystemPrompt string `json:"systemPrompt"`

	// MCPServers lists the MCP tool servers available to this agent.
	MCPServers []MCPServerSpec `json:"mcpServers,omitempty"`

	// Limits constrains agent resource usage.
	Limits *ArkonisLimits `json:"limits,omitempty"`

	// LivenessProbe defines how agent health is evaluated.
	LivenessProbe *ArkonisProbe `json:"livenessProbe,omitempty"`

	// ConfigRef optionally references an ArkonisConfig for shared settings.
	ConfigRef *LocalObjectReference `json:"configRef,omitempty"`

	// MemoryRef optionally references an ArkonisMemory that defines the persistent
	// memory backend for agent instances. When set, the operator injects memory
	// connection details as environment variables into agent pods.
	MemoryRef *LocalObjectReference `json:"memoryRef,omitempty"`
}

// LocalObjectReference identifies an object in the same namespace.
type LocalObjectReference struct {
	// Name of the referenced object.
	Name string `json:"name"`
}

// ArkonisDeploymentStatus defines the observed state of ArkonisDeployment.
type ArkonisDeploymentStatus struct {
	// ReadyReplicas is the number of agent pods ready to accept tasks.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// Replicas is the total number of agent pods (ready or not).
	Replicas int32 `json:"replicas,omitempty"`
	// ObservedGeneration is the .metadata.generation this status reflects.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions reflect the current state of the ArkonisDeployment.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.model`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:resource:shortName=aodep,scope=Namespaced

// ArkonisDeployment manages a pool of LLM agent instances.
type ArkonisDeployment struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec ArkonisDeploymentSpec `json:"spec"`

	// +optional
	Status ArkonisDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ArkonisDeploymentList contains a list of ArkonisDeployment.
type ArkonisDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ArkonisDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ArkonisDeployment{}, &ArkonisDeploymentList{})
}
