package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentRuntime enumerates the supported agent runtime implementations
// (design doc §3.1 `spec.runtime`, E1 Adapter).
type AgentRuntime string

const (
	// DefaultAgentName is the builtin platform agent (design §5.1:
	// agent-for-cloud is the first platform-preset Agent, auto-instantiated per
	// user, and non-deletable).
	DefaultAgentName = "agent-for-cloud"

	// RuntimeOpenClaw is the default runtime (OpenClaw gateway).
	RuntimeOpenClaw AgentRuntime = "openclaw"
	// RuntimeHermes is a future runtime (phase 2+).
	RuntimeHermes AgentRuntime = "hermes"
)

// IdentityMode is how an agent instance derives its platform-side identity
// (design doc §4.4: user = run as the user identity; service = independent
// service identity, phase 2+).
type IdentityMode string

const (
	// IdentityModeUser runs with the creator/user identity (phase one default).
	IdentityModeUser IdentityMode = "user"
	// IdentityModeService runs as an independent service identity (phase 2+).
	IdentityModeService IdentityMode = "service"
)

// ModelSpec is one entry of the ordered model array of an Agent definition.
// model[0] is the primary model; model[1:] are the fallback chain (design doc
// §3.1 FR-M2-003: model-agnostic, supports custom LLMs; fallback depends on
// runtime capability).
type ModelSpec struct {
	// Provider is "platform" (builtin inference pool) or "external"
	// (OpenAI-compatible endpoint; endpoint + apiKeyRef required).
	Provider string `json:"provider"`
	// Name is the model name (and the key for selectedModel on instances).
	Name string `json:"name"`
	// Endpoint is the OpenAI-compatible base URL; required for external.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// APIKeyRef is a platform-managed Secret reference (shared default
	// credential). References only — never the key itself (design §4.4).
	// +optional
	APIKeyRef string `json:"apiKeyRef,omitempty"`
}

// AgentIdentitySpec declares the identity mode and scope an agent runs with
// (design doc §3.1: the definition declares the identity mode and the
// permission scope it needs).
type AgentIdentitySpec struct {
	// Mode is user | service (default user).
	// +kubebuilder:default=user
	// +optional
	Mode IdentityMode `json:"mode,omitempty"`
	// Scope is a coarse permission scope hint (e.g. project-write).
	// +optional
	Scope string `json:"scope,omitempty"`
}

// MemorySpec declares the agent's memory capability (design §3.1).
type MemorySpec struct {
	// Enabled toggles persistent memory for instances of this agent.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

// AgentRegistrySpec carries publish / visibility metadata (design §4.6).
type AgentRegistrySpec struct {
	// Builtin marks platform-preset agents (every user gets an instance
	// automatically; cannot be deleted).
	// +optional
	Builtin bool `json:"builtin,omitempty"`
	// Visibility is system | platform-reviewed | public (default system).
	// +kubebuilder:default=system
	// +optional
	Visibility string `json:"visibility,omitempty"`
}

// QuotaSpec caps resource usage of an agent (design §3.1 / NFR-015).
type QuotaSpec struct {
	// MaxInstancesPerUser caps instances per user for this agent (default 1).
	// +kubebuilder:default=1
	// +optional
	MaxInstancesPerUser int32 `json:"maxInstancesPerUser,omitempty"`
}

// AgentSpec defines what an Agent is: model, instructions, tools (capability
// refs), memory, identity, policy and registry metadata (design §3.1).
// It is the "class": shared by all instances, versioned, user-independent.
type AgentSpec struct {
	// DisplayName is the human-facing agent name.
	DisplayName string `json:"displayName,omitempty"`
	// Description explains what the agent does.
	Description string `json:"description,omitempty"`
	// Runtime selects the runtime implementation (openclaw default).
	// +kubebuilder:default=openclaw
	// +optional
	Runtime AgentRuntime `json:"runtime,omitempty"`
	// Model is the ordered model array (allowlist). model[0] = primary;
	// model[1:] = fallback chain. Instances select within this list.
	Model []ModelSpec `json:"model,omitempty"`
	// Instructions is the default system prompt (definition-level default;
	// instances may override within capability bounds).
	// +optional
	Instructions string `json:"instructions,omitempty"`
	// Tools references Capabilities (atomic thin-overrides + domain skills).
	// Generic tools (resource-manager / list-kinds / describe-kind) are
	// platform-provided and always available — they are NOT listed here.
	// +optional
	Tools []string `json:"tools,omitempty"`
	// Memory declares the memory capability.
	// +optional
	Memory *MemorySpec `json:"memory,omitempty"`
	// Identity declares the identity mode and scope.
	// +optional
	Identity *AgentIdentitySpec `json:"identity,omitempty"`
	// PolicyRefs references confirmation-rule policies (E3, phase 2).
	// +optional
	PolicyRefs []string `json:"policyRefs,omitempty"`
	// Registry carries publish / visibility metadata.
	// +optional
	Registry *AgentRegistrySpec `json:"registry,omitempty"`
	// Quotas caps instances per user (NFR-015).
	// +optional
	Quotas *QuotaSpec `json:"quotas,omitempty"`
}

// AgentStatus is the observed state of an Agent definition (phase one: minimal).
type AgentStatus struct {
	// ObservedGeneration is the most recent generation observed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="DisplayName",type="string",JSONPath=".spec.displayName"
// +kubebuilder:printcolumn:name="Runtime",type="string",JSONPath=".spec.runtime"
// +kubebuilder:printcolumn:name="Builtin",type="boolean",JSONPath=".spec.registry.builtin"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Agent is the declarative definition of an agent (design doc §3.1) — the
// platform's first-class object. The builtin agent-for-cloud is the preset
// first Agent; user-created agents are phase 2+.
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentSpec   `json:"spec,omitempty"`
	Status AgentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentList contains a list of Agent.
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Agent{}, &AgentList{})
}
