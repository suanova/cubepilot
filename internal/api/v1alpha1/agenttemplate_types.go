package v1alpha1

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentRuntime enumerates the supported agent runtime implementations
// (design doc §3.1 `spec.runtime`, E1 Adapter).
// +kubebuilder:validation:Enum=OpenClaw;Hermes
type AgentRuntime string

const (
	// DefaultAgentName is the builtin platform agent template (design §3.1:
	// cubepilot is the first platform-preset AgentTemplate,
	// auto-instantiated per user, and non-deletable).
	DefaultAgentName = "cubepilot"

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

// TemplateModelSpec is one entry of the inline model list of an AgentTemplate
// (design §3.3: models are inlined -- no standalone Model CRD). Name is the
// catalog name, the selection key (selectedModel), the gateway provider key
// and the backend model id sent to the endpoint. Every model is a concrete
// OpenAI-compatible endpoint; a public model simply omits CredentialRef.
type TemplateModelSpec struct {
	// Name is the model name: the key for selectedModel on instances, the
	// gateway provider key, and the model id passed to the LLM endpoint.
	Name string `json:"name"`
	// Endpoint is the OpenAI-compatible base URL; required for every model.
	Endpoint string `json:"endpoint"`
	// CredentialRef optionally references a platform-managed Secret (name)
	// holding the apiKey; public models omit it (nil). References only -- never
	// the key itself (design §4.4).
	// +optional
	CredentialRef *corev1.LocalObjectReference `json:"credentialRef,omitempty"`
}

// Validate enforces the inline-model invariants (design §3.3): every model
// needs an endpoint; a present credentialRef must carry a name. The same rules
// are enforced on the API server by the CEL XValidations on Models.
func (m TemplateModelSpec) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("model name is required")
	}
	if m.Endpoint == "" {
		return fmt.Errorf("model %q requires an endpoint", m.Name)
	}
	if m.CredentialRef != nil && m.CredentialRef.Name == "" {
		return fmt.Errorf("model %q credentialRef must reference a Secret name", m.Name)
	}
	return nil
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

// ConfirmPolicy is the platform confirmation intent (design §3.1 / issue
// #116). Uniform across runtimes: each value describes which operations
// require a human on an interactive turn; each runtime adapter enforces the
// intent with its own mechanism. It lives on the AgentTemplate (with an
// optional AgentInstance override) -- not on the skill -- so different
// templates reusing the same skill can have different confirmation rules.
// +kubebuilder:validation:Enum=None;Allowlist;AlwaysAsk
type ConfirmPolicy string

const (
	// ConfirmPolicyNone means no confirmation is required (reads and writes
	// both pass through, audited).
	ConfirmPolicyNone ConfirmPolicy = "None"
	// ConfirmPolicyAllowlist requires confirmation for operations outside the
	// effective allowlist: entries on the safe allowlist auto-pass, everything
	// else on an interactive turn asks a human. This is the default (formerly
	// ConfirmWrites -- its real behavior always was "allowlist-miss asks", not
	// "writes only").
	ConfirmPolicyAllowlist ConfirmPolicy = "Allowlist"
	// ConfirmPolicyAlwaysAsk requires confirmation for every operation on an
	// interactive turn (strictest posture; the allowlist does not apply).
	ConfirmPolicyAlwaysAsk ConfirmPolicy = "AlwaysAsk"
)

// AllowlistRule is one entry of a safe-command allowlist (issue #116). The
// grammar is runtime-shaped today ({pattern, argPattern?} -- the OpenClaw argv
// allowlist shape the builtin default uses); a runtime-neutral grammar is
// deferred until a second runtime lands. Pattern is the command/executable;
// ArgPattern optionally constrains the remaining argv (empty = any argv).
type AllowlistRule struct {
	// Pattern is the command executable or glob to allow (e.g. "kubectl").
	Pattern string `json:"pattern"`
	// ArgPattern optionally constrains the remaining argv (empty = any argv).
	// +optional
	ArgPattern string `json:"argPattern,omitempty"`
}

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
	// instances select within this list. Every model requires an endpoint;
	// credentialRef is optional (a public model has none).
	// +kubebuilder:validation:XValidation:rule="self.all(m, has(m.endpoint))",message="every model requires an endpoint"
	// +kubebuilder:validation:XValidation:rule="self.all(m, !has(m.credentialRef) || has(m.credentialRef.name))",message="credentialRef must reference a Secret name"
	// +optional
	Models []TemplateModelSpec `json:"models,omitempty"`
	// ConfirmPolicy is the template's default confirmation intent (default
	// Allowlist). Instances inherit it until they override
	// (AgentInstance.spec.confirmPolicy).
	// +kubebuilder:default=Allowlist
	// +optional
	ConfirmPolicy ConfirmPolicy `json:"confirmPolicy,omitempty"`
	// Allowlist optionally extends the template's default safe-command
	// allowlist (issue #116): the effective default is the platform builtin
	// allowlist ∪ these entries. Instances inherit it until they take ownership
	// (AgentInstance.spec.allowlist). Only meaningful under Allowlist policy.
	// +optional
	Allowlist []AllowlistRule `json:"allowlist,omitempty"`
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
// +kubebuilder:printcolumn:name="DisplayName",type="string",JSONPath=".spec.displayName"
// +kubebuilder:printcolumn:name="Runtime",type="string",JSONPath=".spec.runtime"
// +kubebuilder:printcolumn:name="Builtin",type="boolean",JSONPath=".spec.registry.builtin"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AgentTemplate is the declarative definition of an agent (design doc §3.1)
// -- the platform's first-class object. The builtin cubepilot is the
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
