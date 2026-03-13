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

// VectorStoreProvider names a supported vector database.
// +kubebuilder:validation:Enum=qdrant;pinecone;weaviate
type VectorStoreProvider string

const (
	VectorStoreProviderQdrant   VectorStoreProvider = "qdrant"
	VectorStoreProviderPinecone VectorStoreProvider = "pinecone"
	VectorStoreProviderWeaviate VectorStoreProvider = "weaviate"
)

// RedisMemoryConfig configures the Redis memory backend.
type RedisMemoryConfig struct {
	// SecretRef names a Secret whose REDIS_URL key is injected into agent pods.
	SecretRef LocalObjectReference `json:"secretRef"`

	// TTLSeconds is how long memory entries are retained. 0 means no expiry.
	// +kubebuilder:default=3600
	TTLSeconds int `json:"ttlSeconds,omitempty"`

	// MaxEntries caps the number of stored entries per agent instance. 0 means unlimited.
	MaxEntries int `json:"maxEntries,omitempty"`
}

// VectorStoreMemoryConfig configures the vector-store memory backend.
type VectorStoreMemoryConfig struct {
	// Provider is the vector database to use.
	Provider VectorStoreProvider `json:"provider"`

	// Endpoint is the base URL of the vector database (e.g. "http://qdrant:6333").
	// +kubebuilder:validation:Required
	Endpoint string `json:"endpoint"`

	// Collection is the collection/index name to store memories in.
	// +kubebuilder:default=agent-memories
	Collection string `json:"collection,omitempty"`

	// SecretRef optionally names a Secret whose VECTOR_STORE_API_KEY is injected into agent pods.
	SecretRef *LocalObjectReference `json:"secretRef,omitempty"`

	// TTLSeconds is how long memory entries are retained. 0 means no expiry.
	TTLSeconds int `json:"ttlSeconds,omitempty"`
}

// ArkonisMemorySpec defines the desired memory configuration.
type ArkonisMemorySpec struct {
	// Backend selects the memory storage strategy.
	// +kubebuilder:validation:Required
	Backend MemoryBackend `json:"backend"`

	// Redis configures the Redis backend. Required when backend is "redis".
	Redis *RedisMemoryConfig `json:"redis,omitempty"`

	// VectorStore configures the vector-store backend. Required when backend is "vector-store".
	VectorStore *VectorStoreMemoryConfig `json:"vectorStore,omitempty"`
}

// ArkonisMemoryStatus defines the observed state of ArkonisMemory.
type ArkonisMemoryStatus struct {
	// ObservedGeneration is the .metadata.generation this status reflects.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions reflect the current state of the ArkonisMemory.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.backend`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:resource:shortName=aomem,scope=Namespaced

// ArkonisMemory defines the persistent memory backend for agent instances.
// Reference it from an ArkonisDeployment via spec.memoryRef to give agents
// durable memory across tasks.
type ArkonisMemory struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec ArkonisMemorySpec `json:"spec"`

	// +optional
	Status ArkonisMemoryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ArkonisMemoryList contains a list of ArkonisMemory.
type ArkonisMemoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ArkonisMemory `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ArkonisMemory{}, &ArkonisMemoryList{})
}
