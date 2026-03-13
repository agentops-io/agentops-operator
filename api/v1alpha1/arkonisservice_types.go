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

// RoutingStrategy defines how tasks are distributed across agent instances.
// +kubebuilder:validation:Enum=round-robin;least-busy;random
type RoutingStrategy string

const (
	RoutingStrategyRoundRobin RoutingStrategy = "round-robin"
	RoutingStrategyLeastBusy  RoutingStrategy = "least-busy"
	RoutingStrategyRandom     RoutingStrategy = "random"
)

// AgentProtocol defines the protocol for an ArkonisService port.
// +kubebuilder:validation:Enum=A2A;HTTP
type AgentProtocol string

const (
	AgentProtocolA2A  AgentProtocol = "A2A"
	AgentProtocolHTTP AgentProtocol = "HTTP"
)

// ArkonisServicePort defines a port exposed by the ArkonisService.
type ArkonisServicePort struct {
	// Protocol is the communication protocol: "A2A" (agent-to-agent) or "HTTP" (external).
	Protocol AgentProtocol `json:"protocol"`
	// Port is the network port number.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// ArkonisServiceRouting defines the task routing configuration.
type ArkonisServiceRouting struct {
	// Strategy controls how tasks are distributed across agent replicas.
	// +kubebuilder:default=round-robin
	Strategy RoutingStrategy `json:"strategy,omitempty"`
}

// ArkonisServiceSpec defines the desired state of ArkonisService.
type ArkonisServiceSpec struct {
	// Selector identifies the ArkonisDeployment this service routes tasks to.
	// +kubebuilder:validation:Required
	Selector ArkonisServiceSelector `json:"selector"`

	// Routing configures how incoming tasks are distributed.
	Routing ArkonisServiceRouting `json:"routing,omitempty"`

	// Ports lists the network ports exposed by this service.
	Ports []ArkonisServicePort `json:"ports,omitempty"`
}

// ArkonisServiceSelector identifies the target ArkonisDeployment.
type ArkonisServiceSelector struct {
	// ArkonisDeployment is the name of the ArkonisDeployment to route tasks to.
	// +kubebuilder:validation:Required
	ArkonisDeployment string `json:"arkonisDeployment"`
}

// ArkonisServiceStatus defines the observed state of ArkonisService.
type ArkonisServiceStatus struct {
	// ReadyReplicas is the number of ready agent replicas currently backing this service.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// ObservedGeneration is the .metadata.generation this status reflects.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions reflect the current state of the ArkonisService.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Deployment",type=string,JSONPath=`.spec.selector.arkonisDeployment`
// +kubebuilder:printcolumn:name="Strategy",type=string,JSONPath=`.spec.routing.strategy`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:resource:shortName=aosvc,scope=Namespaced

// ArkonisService routes incoming tasks to a pool of ArkonisDeployment replicas.
type ArkonisService struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec ArkonisServiceSpec `json:"spec"`

	// +optional
	Status ArkonisServiceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ArkonisServiceList contains a list of ArkonisService.
type ArkonisServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ArkonisService `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ArkonisService{}, &ArkonisServiceList{})
}
