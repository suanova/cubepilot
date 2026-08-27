package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SkillType is the skill layer (design §3.4 skill market (two layers: generic + skill)).
// +kubebuilder:validation:Enum=Atomic;Domain
type SkillType string

const (
	// SkillAtomic is a thin override bound to a CRD: semantics + security
	// only, never touches fields (parameters come from the CRD schema).
	SkillAtomic SkillType = "Atomic"
	// SkillDomain is domain knowledge: uses[] orchestration +
	// instructions/scripts.
	SkillDomain SkillType = "Domain"
)

// SkillTarget binds an atomic skill to a CRD (design §3.4: the
// platform validates the target exists + schema at registration, fail-fast).
type SkillTarget struct {
	Group   string `json:"group"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

// SkillSemantics is the LLM-facing semantic overlay of an atomic
// skill (design §3.4: changes only the semantics the LLM sees).
type SkillSemantics struct {
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Examples    []string `json:"examples,omitempty"`
}

// SkillFile is a small inline file of a domain skill.
type SkillFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// SkillSpec is a platform skill (design §3.4). Generic tools
// (list-kinds / describe-kind / resource-manager / kubectl-raw) are
// platform-provided and NOT registered as Skills -- zero registration.
type SkillSpec struct {
	// Type is atomic (CRD thin override) or domain (domain knowledge).
	Type SkillType `json:"type"`
	// Title is the display title.
	// +optional
	Title string `json:"title,omitempty"`
	// Description explains when to use this skill.
	// +optional
	Description string `json:"description,omitempty"`
	// Override marks an atomic skill as an overlay (not a fresh
	// definition). Required for atomic.
	// +optional
	Override bool `json:"override,omitempty"`
	// Target binds an atomic skill to a CRD.
	// +optional
	Target *SkillTarget `json:"target,omitempty"`
	// Semantics is the atomic semantic overlay.
	// +optional
	Semantics *SkillSemantics `json:"semantics,omitempty"`
	// Uses orchestrates generic/atomic/MCP skills (domain only).
	// +optional
	Uses []string `json:"uses,omitempty"`
	// Instructions is the domain knowledge instruction (inline default).
	// +optional
	Instructions string `json:"instructions,omitempty"`
	// Files are small inline scripts (domain).
	// +optional
	Files []SkillFile `json:"files,omitempty"`
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

// SkillStatus is the observed state of a Skill.
type SkillStatus struct {
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

// Skill is the skill catalog entry (design §3.4). It answers
// "what an Agent can use" -- atomic thin overrides bound to CRDs and domain
// knowledge.
// Generic tools are platform-provided and need no registration.
type Skill struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SkillSpec   `json:"spec,omitempty"`
	Status SkillStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SkillList contains a list of Skill.
type SkillList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Skill `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Skill{}, &SkillList{})
}

// Revision returns an immutable content fingerprint of the skill spec
// (design §3.4/§3.5: TaskRuns record the skill revision actually used at
// run time for audit/rollback). Content hash -- deterministic across object
// re-creation, spec-only (status updates never change the revision).
func (c *Skill) Revision() string {
	return specRevision(c.Spec)
}
