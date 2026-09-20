package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// InstancePhase is the lifecycle phase of an agent instance: Creating while
// provisioning, Ready when running, Failed on error.
type InstancePhase string

const (
	// InstanceCreating means resources are being provisioned.
	InstanceCreating InstancePhase = "Creating"
	// InstanceReady means the instance is running and ready (resident).
	InstanceReady InstancePhase = "Ready"
	// InstanceFailed means the instance is in a failed state.
	InstanceFailed InstancePhase = "Failed"
)

// DataVolumeSpec is the per-instance data directory: per-instance PVC,
// default 1 GiB; source of truth = data directory. The PVC name is
// platform-generated -- a writer cannot choose it, because the finalizer
// deletes the name this resolves to.
type DataVolumeSpec struct {
	// Size is the requested capacity (default 1Gi, applied in code).
	// +optional
	Size string `json:"size,omitempty"`
}

// AgentInstanceSpec is the runtime instance of an AgentTemplate definition for
// one tenant. The instance key is `user + template` -- one
// instance per user per template, single-writer.
type AgentInstanceSpec struct {
	// TemplateRef points to the AgentTemplate definition (e.g.
	// cubepilot). Not pinned to a revision -- template updates take
	// effect on the next reconcile/restart.
	TemplateRef string `json:"templateRef"`
	// Owner is the user the instance belongs to.
	Owner string `json:"owner"`
	// SelectedModel optionally selects a model by its ref
	// "<provider>/<modelId>", one of the model ids of the providers inlined in
	// the template (overrides defaultModel).
	// +optional
	SelectedModel string `json:"selectedModel,omitempty"`
	// DataVolume is the per-instance data directory.
	// +optional
	DataVolume *DataVolumeSpec `json:"dataVolume,omitempty"`
	// UserInstructions optionally appends user preferences to the definition
	// default system prompt (appended after the template instructions; cannot
	// remove or weaken security/identity bounds).
	// +optional
	UserInstructions string `json:"userInstructions,omitempty"`
	// ApprovalPolicy optionally overrides the template's approvalPolicy
	// (inherit-or-own): empty = follow the template default (live); set = the
	// instance's own posture.
	// +optional
	ApprovalPolicy ApprovalPolicy `json:"approvalPolicy,omitempty"`
	// Allowlist is the instance's own safe-command allowlist: the rules the
	// user added by hand. Learned allow-always grants live in the per-user
	// grants ConfigMap, not here -- the chat approval path records
	// them there, and a client must not copy them back into this field, where
	// they would survive a revocation. The effective list is the union of the
	// platform builtin, the template allowlist and these entries: the instance
	// adds to it and cannot remove from it. A client PUTting this field
	// replaces it wholesale, so it must send back every entry it means to keep.
	// +optional
	Allowlist []AllowlistRule `json:"allowlist,omitempty"`
	// EnabledSkills optionally restricts the skills the AgentTemplate declares
	// (the instance may enable a subset; empty = all declared).
	// +optional
	EnabledSkills []string `json:"enabledSkills,omitempty"`
}

// AgentInstanceStatus is the observed state of an instance, written by the
// Instance Manager controller -- users do not edit it.
type AgentInstanceStatus struct {
	// Phase is Creating / Ready / Failed.
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
// +kubebuilder:printcolumn:name="Template",type="string",JSONPath=".spec.templateRef"
// +kubebuilder:printcolumn:name="Owner",type="string",JSONPath=".spec.owner"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Pod",type="string",JSONPath=".status.podName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AgentInstance is the runtime instance of an AgentTemplate for one user. It
// is reconciled by the Instance Manager controller:
// provision / self-heal / data-directory GC. The instance key is user +
// template (one instance per user per template, single-writer).
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

// EffectiveDataVolumeSize returns the requested size of the instance's data
// PVC (default 1Gi).
//
// The PVC's *name* deliberately lives elsewhere: it is generated from the
// instance name by k8s.GeneratedName (and k8s.GeneratedServiceName for the
// Service, whose name is bounded more tightly), the single place that derives
// the instance's PVC/Pod/Service names, so the create path and the finalizer
// that reclaims them cannot drift apart. This accessor used to return the name too
// ("data-" + name), which made it a second, unbounded source of truth for a
// name the finalizer deletes by.
func (in *AgentInstance) EffectiveDataVolumeSize() string {
	if in.Spec.DataVolume != nil && in.Spec.DataVolume.Size != "" {
		return in.Spec.DataVolume.Size
	}
	return "1Gi"
}
