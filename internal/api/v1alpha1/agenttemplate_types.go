package v1alpha1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentRuntime enumerates the supported agent runtime implementations
// (design doc §3.1 `spec.runtime`, E1 Adapter).
// +kubebuilder:validation:Enum=OpenClaw;Hermes
type AgentRuntime string

const (
	// DefaultAgentName is the builtin platform agent template (design §3.1:
	// agent-for-cloud is the first platform-preset AgentTemplate,
	// auto-instantiated per user, and non-deletable).
	DefaultAgentName = "agent-for-cloud"

	// RuntimeOpenClaw is the default runtime (OpenClaw gateway).
	RuntimeOpenClaw AgentRuntime = "OpenClaw"
	// RuntimeHermes is a future runtime (phase 2+).
	RuntimeHermes AgentRuntime = "Hermes"
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

// ModelProvider is how a model endpoint is provided
// (design §3.3: platform = platform-managed inference pool; external =
// OpenAI-compatible endpoint with platform-managed credential). Models are
// inlined in the template -- there is no standalone Model CRD.
// +kubebuilder:validation:Enum=Platform;External
type ModelProvider string

const (
	// ModelProviderPlatform is the platform-managed inference pool: a builtin
	// runtime model (no endpoint -- the runtime resolves it internally) or a
	// manually deployed inference service the admin registered inline here
	// (endpoint set).
	ModelProviderPlatform ModelProvider = "Platform"
	// ModelProviderExternal is an OpenAI-compatible endpoint: endpoint +
	// credentialRef required (platform-managed Secret, never plaintext).
	ModelProviderExternal ModelProvider = "External"
)

// TemplateModelSpec is one entry of the inline model list of an
// AgentTemplate (design §3.3: models are inlined -- no standalone Model CRD;
// each entry carries name + provider + endpoint + credentialRef).
type TemplateModelSpec struct {
	// Name is the model name (and the key for selectedModel on instances).
	Name string `json:"name"`
	// Provider is "Platform" (platform-managed inference) or "External"
	// (OpenAI-compatible endpoint; endpoint + credentialRef required).
	Provider ModelProvider `json:"provider"`
	// Endpoint is the OpenAI-compatible base URL; required for external.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// CredentialRef is a platform-managed Secret reference (namespace/name or
	// name) holding the apiKey; required for external. References only --
	// never the key itself (design §4.4).
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

// Validate enforces the inline-model invariants (design §3.3): an External
// model requires endpoint + credentialRef; the provider must be known. The
// same rule is enforced on the API server by the CEL XValidation on Models.
func (m TemplateModelSpec) Validate() error {
	switch m.Provider {
	case ModelProviderPlatform:
		return nil
	case ModelProviderExternal:
		if m.Endpoint == "" || m.CredentialRef == "" {
			return fmt.Errorf("external model %q requires endpoint and credentialRef", m.Name)
		}
		return nil
	default:
		return fmt.Errorf("model %q: unknown provider %q", m.Name, m.Provider)
	}
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
	// Enabled toggles persistent memory for instances of this template.
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

// AgentRegistrySpec carries publish / visibility metadata (design §4.6).
type AgentRegistrySpec struct {
	// Builtin marks platform-preset templates (every user gets an instance
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
	// MaxInstancesPerUser caps instances per user for this template
	// (default 1).
	// +kubebuilder:default=1
	// +optional
	MaxInstancesPerUser int32 `json:"maxInstancesPerUser,omitempty"`
}

// ConfirmPolicy is the template-level confirmation policy for write/high-risk
// operations (design §3.1: the policy lives on the AgentTemplate, not on the
// skill, so different templates reusing the same skill can have different
// confirmation rules).
// +kubebuilder:validation:Enum=None;ConfirmWrites
type ConfirmPolicy string

const (
	// ConfirmPolicyNone means no confirmation is required (reads and writes
	// both pass through).
	ConfirmPolicyNone ConfirmPolicy = "None"
	// ConfirmPolicyConfirmWrites requires user confirmation for write
	// operations (reads pass through). This is the default.
	ConfirmPolicyConfirmWrites ConfirmPolicy = "ConfirmWrites"
)

// AgentTemplateSpec defines what an AgentTemplate is: model, instructions,
// tools (skill refs), memory, identity, policy and registry metadata
// (design §3.1). It is the "class": shared by all instances, versioned,
// user-independent.
type AgentTemplateSpec struct {
	// DisplayName is the human-facing template name.
	DisplayName string `json:"displayName,omitempty"`
	// Description explains what the template does.
	Description string `json:"description,omitempty"`
	// Runtime selects the runtime implementation (OpenClaw default).
	// +kubebuilder:default=OpenClaw
	// +optional
	Runtime AgentRuntime `json:"runtime,omitempty"`
	// DefaultModel is the model name from Models used when an instance does
	// not select a model explicitly. Empty = no default / runtime default.
	// +optional
	DefaultModel string `json:"defaultModel,omitempty"`
	// Models is the inline model list (design §3.3: models are inlined in the
	// template -- no standalone Model CRD). The first entry is the primary;
	// instances select within this list. External models require endpoint +
	// credentialRef (enforced by CEL on the API server, see Validate).
	// +kubebuilder:validation:XValidation:rule="self.all(m, m.provider != 'External' || (has(m.endpoint) && has(m.credentialRef)))",message="external models require endpoint and credentialRef"
	// +optional
	Models []TemplateModelSpec `json:"models,omitempty"`
	// ConfirmPolicy is the template-level confirmation policy for write
	// operations (default ConfirmWrites).
	// +kubebuilder:default=ConfirmWrites
	// +optional
	ConfirmPolicy ConfirmPolicy `json:"confirmPolicy,omitempty"`
	// Instructions is the default system prompt (definition-level default;
	// instances may append within capability bounds).
	// +optional
	Instructions string `json:"instructions,omitempty"`
	// Skills references Skills (domain knowledge + controlled scripts),
	// design §3.1 `spec.skills` / §3.4. Generic tools (kubectl exec + schema
	// discovery) are platform-provided and always available -- NOT listed here.
	// +optional
	Skills []string `json:"skills,omitempty"`
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

// AgentTemplateStatus is the observed state of an AgentTemplate definition
// (phase one: minimal).
type AgentTemplateStatus struct {
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

// AgentTemplate is the declarative definition of an agent (design doc §3.1)
// -- the platform's first-class object. The builtin agent-for-cloud is the
// preset first template; user-created templates are phase 2+.
type AgentTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentTemplateSpec   `json:"spec,omitempty"`
	Status AgentTemplateStatus `json:"status,omitempty"`
}

// Revision returns an immutable content fingerprint of the template
// (design §3.1: template changes generate an immutable revision for audit and
// rollback). Content hash -- deterministic across object re-creation,
// spec-only (status updates never change the revision).
func (in *AgentTemplate) Revision() string {
	return specRevision(in.Spec)
}

// +kubebuilder:object:root=true

// AgentTemplateList contains a list of AgentTemplate.
type AgentTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentTemplate{}, &AgentTemplateList{})
}