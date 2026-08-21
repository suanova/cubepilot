package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CapabilityType is the capability layer (design §3.3.1 three-layer model).
type CapabilityType string

const (
	// CapabilityAtomic is a thin override bound to a CRD: semantics + security
	// only, never touches fields (parameters come from the CRD schema).
	CapabilityAtomic CapabilityType = "Atomic"
	// CapabilityDomain is domain knowledge: uses[] orchestration +
	// instructions/scripts.
	CapabilityDomain CapabilityType = "Domain"
)

// CapabilityTarget binds an atomic capability to a CRD (design §3.3.1: the
// platform validates the target exists + schema at registration, fail-fast).
type CapabilityTarget struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

// CapabilitySemantics is the LLM-facing semantic overlay of an atomic
// capability (design §3.3.1: changes only the semantics the LLM sees).
type CapabilitySemantics struct {
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Examples    []string `json:"examples,omitempty"`
}

// CapabilityFile is a small inline file of a domain capability.
type CapabilityFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// CapabilitySpec is a platform capability (design §3.3.1). Generic tools
// (list-kinds / describe-kind / resource-manager / kubectl-raw) are
// platform-provided and NOT registered as Capabilities — zero registration.
type CapabilitySpec struct {
	// Type is atomic (CRD thin override) or domain (domain knowledge).
	Type CapabilityType `json:"type"`
	// Title is the display title.
	// +optional
	Title string `json:"title,omitempty"`
	// Description explains when to use this capability.
	// +optional
	Description string `json:"description,omitempty"`
	// Override marks an atomic capability as an overlay (not a fresh
	// definition). Required for atomic.
	// +optional
	Override bool `json:"override,omitempty"`
	// Target binds an atomic capability to a CRD.
	// +optional
	Target *CapabilityTarget `json:"target,omitempty"`
	// Semantics is the atomic semantic overlay.
	// +optional
	Semantics *CapabilitySemantics `json:"semantics,omitempty"`
	// Uses orchestrates generic/atomic/MCP capabilities (domain only).
	// +optional
	Uses []string `json:"uses,omitempty"`
	// Instructions is the domain knowledge instruction (inline default).
	// +optional
	Instructions string `json:"instructions,omitempty"`
	// Files are small inline scripts (domain).
	// +optional
	Files []CapabilityFile `json:"files,omitempty"`
	// ContentRef points to external content (ConfigMap) for large content.
	// +optional
	ContentRef string `json:"contentRef,omitempty"`
	// Agents restricts visibility: empty = visible to all agents.
	// +optional
	Agents []string `json:"agents,omitempty"`
	// OwnerModule is the owning platform module (e.g. training).
	// +optional
	OwnerModule string `json:"ownerModule,omitempty"`
}

// CapabilityStatus is the observed state of a Capability.
type CapabilityStatus struct {
	// ObservedGeneration is the most recent generation observed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Valid reports whether the registration passed validation (target CRD
	// exists with schema; fail-fast).
	// +optional
	Valid bool `json:"valid,omitempty"`
	// Message carries the validation detail.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Target",type="string",JSONPath=".spec.target.kind"
// +kubebuilder:printcolumn:name="Valid",type="boolean",JSONPath=".status.valid"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Capability is the capability catalog entry (design §3.3.1). It answers
// "what an Agent can use" — atomic thin overrides bound to CRDs and domain
// knowledge.
// Generic tools are platform-provided and need no registration.
type Capability struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CapabilitySpec   `json:"spec,omitempty"`
	Status CapabilityStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// CapabilityList contains a list of Capability.
type CapabilityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Capability `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Capability{}, &CapabilityList{})
}

// Revision returns an immutable content fingerprint of the capability spec
// (design §3.4/§3.5: TaskRuns record the capability revision actually used at
// run time for audit/rollback). Content hash — deterministic across object
// re-creation, spec-only (status updates never change the revision).
func (c *Capability) Revision() string {
	return specRevision(c.Spec)
}
