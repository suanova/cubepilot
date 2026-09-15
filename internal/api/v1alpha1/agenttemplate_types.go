package v1alpha1

import (
	"fmt"
	"regexp"
	"strings"

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

// TemplateProviderSpec is one OpenAI-compatible LLM provider of an
// AgentTemplate (design §3.3: models are inlined -- no standalone Model CRD).
// A provider owns the endpoint and the credential once, and lists the backend
// model ids reachable through it, so a gateway that serves many models behind
// one base URL and one key is described once rather than once per model.
type TemplateProviderSpec struct {
	// Name is the provider key: the OpenClaw models.providers key and the
	// prefix of every model ref (<name>/<modelId>). A DNS-1123 label, because
	// it is both a ref segment (refs split on the first "/" and have no
	// escaping) and a resource-name segment.
	//
	// The credential Secret named llm-<name> is a rule of that write API
	// (/api/v1/llms) alone, not of this field: credentialRef is an arbitrary
	// Secret reference, and the platform's own provider is named "platform"
	// while pointing at the Secret "cubepilot-llm".
	//
	// A name that exactly matches an OpenClaw built-in provider key
	// (anthropic, nvidia, xai, google, ...) inherits that provider's model-id
	// normalization, which can rewrite the id sent to the endpoint. Prefer a
	// distinct name such as nvidia-proxy unless that rewrite is intended.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`
	// Endpoint is the OpenAI-compatible base URL.
	// +kubebuilder:validation:MaxLength=2048
	Endpoint string `json:"endpoint"`
	// CredentialRef optionally references a platform-managed Secret (name)
	// holding the apiKey; a provider that needs no credentials omits it (nil).
	// References only -- never the key itself (design §4.4).
	// +optional
	CredentialRef *corev1.LocalObjectReference `json:"credentialRef,omitempty"`
	// Models are the backend model ids served through this endpoint. Each id is
	// sent to the endpoint verbatim and may itself contain "/" (OpenRouter's
	// "anthropic/claude-sonnet-4.5").
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=256
	// +listType=set
	Models []string `json:"models"`
}

// providerNameRE is the DNS-1123 label grammar, mirroring the Pattern marker on
// TemplateProviderSpec.Name.
var providerNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Validate enforces the provider invariants. The same rules are enforced on the
// API server by the markers on the type and the CEL XValidations on Providers.
// The structural rules -- the bounds and the +listType=set uniqueness of Models
// -- are mirrored too, so a request this validator accepts cannot be refused by
// the API server afterwards and surface as a 500.
func (p TemplateProviderSpec) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("provider name is required")
	}
	if len(p.Name) > 63 || !providerNameRE.MatchString(p.Name) {
		return fmt.Errorf("provider %q must be a lowercase DNS-1123 label of at most 63 characters", p.Name)
	}
	if p.Endpoint == "" {
		return fmt.Errorf("provider %q requires an endpoint", p.Name)
	}
	if len(p.Endpoint) > 2048 {
		return fmt.Errorf("provider %q endpoint must be at most 2048 characters", p.Name)
	}
	if p.CredentialRef != nil && p.CredentialRef.Name == "" {
		return fmt.Errorf("provider %q credentialRef must reference a Secret name", p.Name)
	}
	if len(p.Models) == 0 {
		return fmt.Errorf("provider %q requires at least one model", p.Name)
	}
	if len(p.Models) > 64 {
		return fmt.Errorf("provider %q must list at most 64 models", p.Name)
	}
	// Models is +listType=set, so the API server rejects a repeated id outright.
	// This is the Go mirror of that structural rule; the HTTP handlers
	// de-duplicate before they get here (normalizeModels), so it is a hand-written
	// caller that this catches.
	seen := make(map[string]struct{}, len(p.Models))
	for _, id := range p.Models {
		if _, dup := seen[id]; dup {
			return fmt.Errorf("provider %q lists the model id %q more than once", p.Name, id)
		}
		seen[id] = struct{}{}
		if len(id) > 256 {
			return fmt.Errorf("provider %q model id must be at most 256 characters", p.Name)
		}
		if err := validateModelID(id); err != nil {
			return fmt.Errorf("provider %q: %w", p.Name, err)
		}
	}
	return nil
}

// validateModelID rejects the ids that would not survive being used as a model
// ref. Everything else is data sent to the endpoint, so the grammar is
// deliberately loose: ids routinely contain "/".
func validateModelID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("model id must not be empty")
	case id == "*":
		return fmt.Errorf("model id %q is reserved for allowlist wildcards", id)
	case strings.TrimSpace(id) != id || strings.ContainsAny(id, " \t\n\r"):
		return fmt.Errorf("model id %q must not contain whitespace", id)
	case strings.HasPrefix(id, "/"), strings.HasSuffix(id, "/"), strings.Contains(id, "//"):
		return fmt.Errorf("model id %q must not contain an empty path segment", id)
	}
	return nil
}

// ApprovalPolicy is the platform confirmation intent (design §3.1 / issue
// #116). Uniform across runtimes: each value describes which operations
// require a human on an interactive turn; each runtime adapter enforces the
// intent with its own mechanism. It lives on the AgentTemplate (with an
// optional AgentInstance override) -- not on the skill -- so different
// templates reusing the same skill can have different confirmation rules.
// +kubebuilder:validation:Enum=None;Allowlist;AlwaysAsk
type ApprovalPolicy string

const (
	// ApprovalPolicyNone means no confirmation is required (reads and writes
	// both pass through, audited).
	ApprovalPolicyNone ApprovalPolicy = "None"
	// ApprovalPolicyAllowlist requires confirmation for operations outside the
	// effective allowlist: entries on the safe allowlist auto-pass, everything
	// else on an interactive turn asks a human. This is the default (formerly
	// ConfirmWrites -- its real behavior always was "allowlist-miss asks", not
	// "writes only").
	ApprovalPolicyAllowlist ApprovalPolicy = "Allowlist"
	// ApprovalPolicyAlwaysAsk requires confirmation for every operation on an
	// interactive turn (strictest posture; the allowlist does not apply).
	ApprovalPolicyAlwaysAsk ApprovalPolicy = "AlwaysAsk"
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
// tools (skill refs) and policy (design §3.1). It is the "class": shared by
// all instances, versioned, user-independent.
//
// Like gateway.ModelKey, the rule below leaves an id that already starts with
// "<provider>/" unprefixed, and it tests that prefix case-insensitively
// (lowerAscii) because ModelKey lowercases both sides. Without that the two
// diverge: provider "vllm" serving the id "VLLM/x" makes ModelKey return
// "VLLM/x" -- the ref the renderer writes as the allowlist key and as the
// primary -- while CEL computed "vllm/VLLM/x" and rejected the only ref the
// platform has for that id. Provider names are lowercase by the Pattern on
// Name, so lowercasing the id alone is enough. ModelKey also trims, which is a
// no-op on the ids the rule ranges over: the XValidation on Providers rejects
// whitespace, exactly as validateModelID does.
//
// Both fields this rule reads are omitempty, so either key can be absent from
// the serialized object: DefaultModel whenever it is cleared (which is exactly
// what the API's clear-the-default writes do) and Providers for a provider-less
// template. CEL errors on a missing key rather than treating it as empty, so
// the rule guards both with has(). The guards are load-bearing, not defensive:
// without them a fresh install with no LLM configured cannot create the
// builtin template at all. When defaultModel is set but providers is absent the
// guard makes the rule false -- the ref names nothing, so the write is rejected
// with the message rather than erroring.
//
// The two XValidations on Providers need no such guard: the API server does not
// evaluate field-level rules on an absent field (verified against a live
// cluster -- an object with no providers key passes them untouched).
// +kubebuilder:validation:XValidation:rule="!has(self.defaultModel) || self.defaultModel == \"\" || (has(self.providers) && self.providers.exists(p, p.models.exists(m, (m.lowerAscii().startsWith(p.name + '/') ? m : p.name + '/' + m) == self.defaultModel)))",message="defaultModel must name a provider/model listed in providers"
type AgentTemplateSpec struct {
	// DisplayName is the human-facing template name.
	DisplayName string `json:"displayName,omitempty"`
	// Description explains what the template does.
	Description string `json:"description,omitempty"`
	// Runtime selects the runtime implementation (OpenClaw default).
	// +kubebuilder:default=OpenClaw
	// +optional
	Runtime AgentRuntime `json:"runtime,omitempty"`
	// DefaultModel is the model ref (<provider>/<modelId>) used when an
	// instance does not select one explicitly. Empty = no default / runtime
	// default.
	// +kubebuilder:validation:MaxLength=320
	// +optional
	DefaultModel string `json:"defaultModel,omitempty"`
	// Providers is the inline provider list (design §3.3: models are inlined in
	// the template -- no standalone Model CRD). Each provider declares an
	// endpoint, an optional credential and the model ids it serves; an instance
	// selects a <provider>/<modelId> ref within this list.
	// +kubebuilder:validation:XValidation:rule="self.all(p, !has(p.credentialRef) || p.credentialRef.name != \"\")",message="credentialRef must reference a Secret name"
	// +kubebuilder:validation:XValidation:rule="self.all(p, p.models.all(m, m != \"\" && m != '*' && !m.contains('//') && !m.startsWith('/') && !m.endsWith('/') && !m.matches('.*\\\\s.*')))",message="every model id must be non-empty, without an empty path segment, without whitespace, and not the wildcard"
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	// +optional
	Providers []TemplateProviderSpec `json:"providers,omitempty"`
	// ApprovalPolicy is the template's default confirmation intent (default
	// Allowlist). Instances inherit it until they override
	// (AgentInstance.spec.approvalPolicy).
	// +kubebuilder:default=Allowlist
	// +optional
	ApprovalPolicy ApprovalPolicy `json:"approvalPolicy,omitempty"`
	// Allowlist optionally extends the template's default safe-command
	// allowlist (issue #116): the effective allowlist is the union of the
	// platform builtin, these entries and the instance's own (issue #185). Only
	// meaningful under Allowlist policy.
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
