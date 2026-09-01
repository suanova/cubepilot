package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SkillSourceType is the discriminant of the skill content address (design
// §3.4). Phase 1 supports only Path; S3 is phase 2.
// +kubebuilder:validation:Enum=Path;S3
type SkillSourceType string

const (
	SkillSourcePath SkillSourceType = "Path"
	SkillSourceS3   SkillSourceType = "S3"
)

// SkillVisibility controls who may see a skill (design §3.4). Phase 1 ships
// only Platform.
// +kubebuilder:validation:Enum=Platform;Tenant;User
type SkillVisibility string

const (
	SkillVisibilityPlatform SkillVisibility = "Platform"
	SkillVisibilityTenant   SkillVisibility = "Tenant"
	SkillVisibilityUser     SkillVisibility = "User"
)

// SkillPhase is the observed reachability of the skill content.
// +kubebuilder:validation:Enum=Available;Unreachable
type SkillPhase string

const (
	SkillPhaseAvailable   SkillPhase = "Available"
	SkillPhaseUnreachable SkillPhase = "Unreachable"
)

// SkillS3Source is the object-store addressing (phase 2).
type SkillS3Source struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

// SkillSource addresses the skill content in the repository (design §3.4:
// content lives in the repo; the CRD registers where + which version).
type SkillSource struct {
	// Type is the discriminant: Path | S3.
	Type SkillSourceType `json:"type"`
	// Path is the repo-relative tar path (e.g. skills/harbor/v1.tar.gz),
	// versioned and immutable. Required when type=Path.
	Path string `json:"path,omitempty"`
	// S3 is the object-store addressing. Forbidden when type=Path (phase 2).
	S3 *SkillS3Source `json:"s3,omitempty"`
	// Sha256 is the content fingerprint, backfilled by publish/seed. Optional:
	// manual kubectl apply may leave it empty (audit via the versioned path).
	Sha256 string `json:"sha256,omitempty"`
}

// SkillSpec is a marketplace skill (design §3.4). It registers "what skill
// exists, where, which version, who can see it"; the content lives in the
// repository.
type SkillSpec struct {
	// DisplayName is the market-facing title.
	DisplayName string `json:"displayName"`
	// Description explains when to use the skill.
	Description string `json:"description,omitempty"`
	// Visibility is Platform | Tenant | User; phase 1 only Platform.
	Visibility SkillVisibility `json:"visibility"`
	// Source addresses the skill content (discriminant type; CEL enforces the
	// mutually exclusive Path/S3 branches on the API server).
	// +kubebuilder:validation:XValidation:rule="self.type=='Path' ? has(self.path) && !has(self.s3) : true",message="source.type=Path requires source.path and forbids source.s3"
	// +kubebuilder:validation:XValidation:rule="self.type=='S3' ? has(self.s3) && !has(self.path) : true",message="source.type=S3 requires source.s3 and forbids source.path"
	Source SkillSource `json:"source"`
}

// SkillStatus is the observed state of a Skill.
type SkillStatus struct {
	// Phase is Available | Unreachable. Set by the seed/publish flow after the
	// tar is written to the repository.
	Phase SkillPhase `json:"phase,omitempty"`
	// Conditions carry condition details.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// ObservedGeneration is the most recent generation observed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="DisplayName",type="string",JSONPath=".spec.displayName"
// +kubebuilder:printcolumn:name="Visibility",type="string",JSONPath=".spec.visibility"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Skill is the skill catalog entry (design §3.4). Generic tools are
// platform-provided and need no registration.
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
// (design §3.4/§3.5: TaskRuns record the skill revision actually used at run
// time for audit/rollback). Content hash -- deterministic across object
// re-creation, spec-only (status updates never change the revision).
func (c *Skill) Revision() string {
	return specRevision(c.Spec)
}
