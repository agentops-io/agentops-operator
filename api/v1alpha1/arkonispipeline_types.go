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

// PipelineStepPhase describes the execution state of a single pipeline step.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type PipelineStepPhase string

const (
	PipelineStepPhasePending   PipelineStepPhase = "Pending"
	PipelineStepPhaseRunning   PipelineStepPhase = "Running"
	PipelineStepPhaseSucceeded PipelineStepPhase = "Succeeded"
	PipelineStepPhaseFailed    PipelineStepPhase = "Failed"
)

// PipelinePhase describes the overall execution state of the pipeline.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type PipelinePhase string

const (
	PipelinePhasePending   PipelinePhase = "Pending"
	PipelinePhaseRunning   PipelinePhase = "Running"
	PipelinePhaseSucceeded PipelinePhase = "Succeeded"
	PipelinePhaseFailed    PipelinePhase = "Failed"
)

// PipelineStep defines one node in the agent DAG.
type PipelineStep struct {
	// Name is the unique identifier for this step within the pipeline.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// ArkonisDeployment is the name of the ArkonisDeployment that executes this step.
	// +kubebuilder:validation:Required
	ArkonisDeployment string `json:"arkonisDeployment"`

	// Inputs is a map of input key → Go template expression referencing pipeline
	// inputs or earlier step outputs. Example: "{{ .steps.research.output }}"
	Inputs map[string]string `json:"inputs,omitempty"`

	// DependsOn lists step names that must complete before this step runs.
	DependsOn []string `json:"dependsOn,omitempty"`

	// OutputSchema is an optional JSON Schema string that constrains this step's output.
	// When set, the schema is appended to the agent prompt and the controller validates
	// that the response parses as JSON before marking the step Succeeded.
	// Downstream steps can reference structured fields via "{{ .steps.<name>.data.<field> }}".
	OutputSchema string `json:"outputSchema,omitempty"`
}

// ArkonisPipelineSpec defines the desired state of ArkonisPipeline.
type ArkonisPipelineSpec struct {
	// Steps is the ordered (DAG) list of pipeline nodes.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	Steps []PipelineStep `json:"steps"`

	// Input is the initial data passed into the pipeline. Step inputs can
	// reference these values via "{{ .pipeline.input.<key> }}".
	Input map[string]string `json:"input,omitempty"`

	// Output is a Go template expression that selects the final pipeline result.
	// Example: "{{ .steps.summarize.output }}"
	Output string `json:"output,omitempty"`
}

// PipelineStepStatus captures the observed state of a single step.
type PipelineStepStatus struct {
	// Name matches the step name in spec.
	Name string `json:"name"`
	// Phase is the current execution phase.
	Phase PipelineStepPhase `json:"phase"`
	// TaskID is the Redis stream message ID of the submitted task, used to
	// correlate results from the agent-tasks-results stream.
	TaskID string `json:"taskID,omitempty"`
	// Output is the agent's raw text response once the step has succeeded.
	Output string `json:"output,omitempty"`

	// OutputJSON is the agent's response as a JSON string, populated when the step
	// has an OutputSchema and the response is valid JSON. Downstream steps can
	// reference fields via "{{ .steps.<name>.data.<field> }}".
	OutputJSON string `json:"outputJSON,omitempty"`
	// StartTime is when the step started executing.
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// CompletionTime is when the step finished (success or failure).
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// Message holds a human-readable status detail.
	Message string `json:"message,omitempty"`
}

// ArkonisPipelineStatus defines the observed state of ArkonisPipeline.
type ArkonisPipelineStatus struct {
	// Phase is the overall pipeline execution state.
	Phase PipelinePhase `json:"phase,omitempty"`
	// Steps holds per-step status.
	Steps []PipelineStepStatus `json:"steps,omitempty"`
	// StartTime is when the pipeline started.
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// CompletionTime is when the pipeline finished.
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// Output is the resolved final pipeline output once phase is Succeeded.
	Output string `json:"output,omitempty"`
	// ObservedGeneration is the .metadata.generation this status reflects.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions reflect the current state of the ArkonisPipeline.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Steps",type=integer,JSONPath=`.spec.steps`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:resource:shortName=aopipe,scope=Namespaced

// ArkonisPipeline defines a DAG of agent steps where outputs feed into inputs.
type ArkonisPipeline struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec ArkonisPipelineSpec `json:"spec"`

	// +optional
	Status ArkonisPipelineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ArkonisPipelineList contains a list of ArkonisPipeline.
type ArkonisPipelineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ArkonisPipeline `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ArkonisPipeline{}, &ArkonisPipelineList{})
}
