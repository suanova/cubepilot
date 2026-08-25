package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// InstancePhase is the lifecycle phase of an agent instance (design §3.2:
// Ready when running, Idle when suspended, etc).
type InstancePhase string

const (
	// InstanceCreating means resources are being provisioned.
	InstanceCreating InstancePhase = "Creating"
	// InstanceReady means the instance is running and ready (resident).
	InstanceReady InstancePhase = "Ready"
	// InstanceIdle means the instance is idle (on-demand mode, phase 2).
	InstanceIdle InstancePhase = "Idle"
	// InstanceReclaiming means the instance is being torn down.
	InstanceReclaiming InstancePhase = "Reclaiming"
	// InstanceFailed means the instance is in a failed state.
	InstanceFailed InstancePhase = "Failed"
)

// CredentialSpec is one typed downstream credential of an instance
// (design §4.4: identity = who I am; credentials[] = how I authenticate to
// downstreams).
// The actual secret is platform-managed; refs only, never plaintext.
type CredentialSpec struct {
	// Target is the downstream system: k8s | prometheus | harbor | llm | itsm | ...
	Target string `json:"target"`
	// Type is the credential type: kubeconfig | api-key | oauth2 | bearer-token | ...
	Type string `json:"type"`
	// Ref is the platform-managed Secret reference (namespace/name or name).
	Ref string `json:"ref"`
	// ModelRef optionally binds an llm credential to a model entry in the
	// AgentTemplate models list (by name). Required when multiple external
	// models exist; implicit when exactly one (design §4.4, fail-closed).
	// +optional
	ModelRef string `json:"modelRef,omitempty"`
	// Endpoint optionally binds an llm credential by endpoint (matches
	// the model's endpoint). Mutually exclusive with ModelRef.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
}

// PrincipalRef binds the instance to a concrete principal (design §3.2:
// userRef for mode=user; serviceRef for mode=service -- mutually exclusive).
type PrincipalRef struct {
	// UserRef binds a user identity (mode=user).
	// +optional
	UserRef string `json:"userRef,omitempty"`
	// ServiceRef binds a service identity (mode=service, phase 2+).
	// +optional
	ServiceRef string `json:"serviceRef,omitempty"`
}

// IdentitySpec is the platform-side identity of an instance (design §3.2).
type IdentitySpec struct {
	// Mode must match the Agent definition (inherited, immutable).
	Mode IdentityMode `json:"mode"`
	// PrincipalRef binds the concrete principal.
	PrincipalRef PrincipalRef `json:"principalRef"`
}

// DataVolumeSpec is the per-instance data directory (design §3.2: per-instance
// PVC, default 1 GiB; source of truth = data directory).
type DataVolumeSpec struct {
	// PVC is the per-instance PVC name (platform-generated when empty).
	// +optional
	PVC string `json:"pvc,omitempty"`
	// Size is the requested capacity (default 1Gi, applied in code).
	// +optional
	Size string `json:"size,omitempty"`
}

// LifecycleSpec is the instance lifecycle policy (design §3.2: resident for the
// phase-one builtin agent; on-demand for phase-two user-created agents).
type LifecycleSpec struct {
	// Strategy is resident | on-demand (default resident).
	// +kubebuilder:default=resident
	// +optional
	Strategy string `json:"strategy,omitempty"`
	// IdleTTL is the idle retention for on-demand instances (0 = never
	// reclaimed; default 0).
	// +optional
	IdleTTLSeconds int64 `json:"idleTTLSeconds,omitempty"`
}

// AgentInstanceSpec is the runtime instance of an AgentTemplate definition for
// one tenant (design §3.2). The instance key is `user + template` -- one
// instance per user per template, single-writer.
type AgentInstanceSpec struct {
	// TemplateRef points to the AgentTemplate definition (e.g.
	// agent-for-cloud). Not pinned to a revision -- template updates take
	// effect on the next reconcile/restart (design §3.1/§3.2).
	TemplateRef string `json:"templateRef"`
	// Owner is the user the instance belongs to.
	Owner string `json:"owner"`
	// Identity is the platform-side identity (mode + principal).
	Identity IdentitySpec `json:"identity"`
	// Credentials are the typed downstream credentials.
	// +optional
	Credentials []CredentialSpec `json:"credentials,omitempty"`
	// SelectedModel optionally selects a model within the template's inline
	// models list (overrides defaultModel). FR-M2-005 / design §3.2.
	// +optional
	SelectedModel string `json:"selectedModel,omitempty"`
	// DataVolume is the per-instance data directory.
	// +optional
	DataVolume *DataVolumeSpec `json:"dataVolume,omitempty"`
	// Lifecycle overrides the definition defaults (within quota bounds).
	// +optional
	Lifecycle *LifecycleSpec `json:"lifecycle,omitempty"`
	// UserInstructions optionally appends user preferences to the definition
	// default system prompt (design §3.2: appended after the template
	// instructions; cannot remove or weaken security/identity bounds).
	// +optional
	UserInstructions string `json:"userInstructions,omitempty"`
	// EnabledSkills optionally restricts the skills the AgentTemplate declares
	// (design §3.2: the instance may enable a subset; empty = all declared).
	// +optional
	EnabledSkills []string `json:"enabledSkills,omitempty"`
}

// AgentInstanceStatus is the observed state of an instance (design §3.2,
// written by the Instance Manager controller -- users do not edit it).
type AgentInstanceStatus struct {
	// Phase is Creating / Ready / Idle / Reclaiming / Failed.
	// +optional
	Phase InstancePhase `json:"phase,omitempty"`
	// PodName is the running agent Pod (empty when not running).
	// +optional
	PodName string `json:"podName,omitempty"`
	// ServiceName is the ClusterIP service exposing the gateway.
	// +optional
	ServiceName string `json:"serviceName,omitempty"`
	// PVCName is the per-instance data PVC.
	// +optional
	PVCName string `json:"pvcName,omitempty"`
	// LastActivity is the last observed activity time (agentKey -> activity).
	// +optional
	LastActivity *metav1.Time `json:"lastActivity,omitempty"`
	// Conditions carries detail (Ready / Reclaiming / Failed reason).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// Message is a human-readable status detail.
	// +optional
	Message string `json:"message,omitempty"`
	// ObservedGeneration is the most recent generation observed.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Template",type="string",JSONPath=".spec.templateRef"
// +kubebuilder:printcolumn:name="Owner",type="string",JSONPath=".spec.owner"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Pod",type="string",JSONPath=".status.podName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AgentInstance is the runtime instance of an AgentTemplate for one user
// (design §3.2). It is reconciled by the Instance Manager controller:
// provision / self-heal / idle reclaim / data-directory GC. The instance key
// is user + template (one instance per user per template, single-writer).
type AgentInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentInstanceSpec   `json:"spec,omitempty"`
	Status AgentInstanceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentInstanceList contains a list of AgentInstance.
type AgentInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentInstance `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentInstance{}, &AgentInstanceList{})
}

// EffectiveDataVolume returns the PVC name and size for the instance.
func (in *AgentInstance) EffectiveDataVolume() (pvc, size string) {
	size = "1Gi"
	if in.Spec.DataVolume != nil {
		if in.Spec.DataVolume.Size != "" {
			size = in.Spec.DataVolume.Size
		}
		pvc = in.Spec.DataVolume.PVC
	}
	if pvc == "" {
		pvc = "data-" + in.Name
	}
	return pvc, size
}

// ReadyCondition returns the Ready condition if present.
func (in *AgentInstance) ReadyCondition() (metav1.Condition, bool) {
	for _, c := range in.Status.Conditions {
		if c.Type == "Ready" {
			return c, true
		}
	}
	return metav1.Condition{}, false
}

// CredentialFor returns the first credential matching target (and optional
// modelRef/endpoint), or nil.
func (in *AgentInstance) CredentialFor(target string) *CredentialSpec {
	for i := range in.Spec.Credentials {
		if in.Spec.Credentials[i].Target == target {
			return &in.Spec.Credentials[i]
		}
	}
	return nil
}

// PodResources returns the container resources for the agent Pod (defaults).
func (in *AgentInstance) PodResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{}
}
