package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelProvider is how a model endpoint is provided
// (design §3.3: platform = platform-managed inference, either a builtin
// runtime model or a manually deployed endpoint; external =
// OpenAI-compatible endpoint with platform-managed credential).
type ModelProvider string

const (
	// ModelProviderPlatform is the platform-managed inference pool: a builtin
	// runtime model (no endpoint — the runtime resolves it internally) or a
	// manually deployed inference service the admin registered here (endpoint
	// set; probed like external).
	ModelProviderPlatform ModelProvider = "Platform"
	// ModelProviderExternal is an OpenAI-compatible endpoint: endpoint +
	// credentialRef required (platform-managed Secret, never plaintext).
	ModelProviderExternal ModelProvider = "External"
)

// ModelPhase is the observed availability of a model catalog entry
// (design §3.3: Available / Unreachable, probed by the controller at
// registration).
type ModelPhase string

const (
	// ModelAvailable means the endpoint is reachable (or platform-provided).
	ModelAvailable ModelPhase = "Available"
	// ModelUnreachable means the endpoint probe failed (external only).
	ModelUnreachable ModelPhase = "Unreachable"
)

// ModelSpec is a platform-level LLM model catalog entry (design §3.3),
// maintained by administrators. Templates and instances reference models by
// name — decoupled from endpoints and credentials, which live inside the
// Model object (platform-managed, never plaintext).
type ModelSpec struct {
	// DisplayName is the human-facing model name.
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// Provider is "Platform" (platform-managed inference: builtin runtime
	// model or manually deployed endpoint) or "External" (OpenAI-compatible
	// endpoint; endpoint + credentialRef required).
	Provider ModelProvider `json:"provider"`
	// Endpoint is the OpenAI-compatible base URL. Required for external;
	// optional for platform (empty = builtin runtime model, no probe).
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// CredentialRef is a platform-managed Secret reference (namespace/name or
	// name) holding the apiKey; required for external. References only — never
	// the key itself.
	// +optional
	CredentialRef string `json:"credentialRef,omitempty"`
	// ModelID is the backend model identifier passed to the LLM endpoint:
	// for external it is the model name sent to the OpenAI-compatible
	// endpoint; for platform it is the runtime-known model id (e.g.
	// "deepseek/deepseek-v4-flash"). Empty for platform means "use the
	// runtime's default model" (no override).
	// +optional
	ModelID string `json:"modelId,omitempty"`
}

// ModelStatus is the observed state of a Model (design §3.3: probed by the
// controller at registration and on a periodic requeue).
type ModelStatus struct {
	// Phase is Available / Unreachable.
	// +optional
	Phase ModelPhase `json:"phase,omitempty"`
	// Message carries the probe detail (e.g. the failure reason).
	// +optional
	Message string `json:"message,omitempty"`
	// LastProbeTime is when the connectivity probe last ran.
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
	// ObservedGeneration is the most recent generation observed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Provider",type="string",JSONPath=".spec.provider"
// +kubebuilder:printcolumn:name="Endpoint",type="string",JSONPath=".spec.endpoint"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Model is a platform-level LLM model catalog entry (design §3.3),
// maintained by administrators. Agent templates and instances reference
// models by name (defaultModel / selectedModel); endpoints and credentials
// stay inside the Model object, decoupled from the referencing objects.
type Model struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelSpec   `json:"spec,omitempty"`
	Status ModelStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelList contains a list of Model.
type ModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Model `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Model{}, &ModelList{})
}
