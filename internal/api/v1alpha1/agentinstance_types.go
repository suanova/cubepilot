package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// InstancePhase is the lifecycle phase of an agent instance (design §3.2:
// Creating while provisioning, Ready when running, Failed on error).
type InstancePhase string

const (
	// InstanceCreating means resources are being provisioned.
	InstanceCreating InstancePhase = "Creating"
	// InstanceReady means the instance is running and ready (resident).
	InstanceReady InstancePhase = "Ready"
	// InstanceFailed means the instance is in a failed state.
	InstanceFailed InstancePhase = "Failed"
)

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

// AgentInstanceSpec is the runtime instance of an AgentTemplate definition for
// one tenant (design §3.2). The instance key is `user + template` -- one
// instance per user per template, single-writer.
type AgentInstanceSpec struct {
	// TemplateRef points to the AgentTemplate definition (e.g.
	// cubepilot). Not pinned to a revision -- template updates take
	// effect on the next reconcile/restart (design §3.1/§3.2).
	TemplateRef string `json:"templateRef"`
	// Owner is the user the instance belongs to.
	Owner string `json:"owner"`
	// SelectedModel optionally selects a model by its ref
	// "<provider>/<modelId>", one of the model ids of the providers inlined in
	// the template (overrides defaultModel). Design §3.2.
	// +optional
	SelectedModel string `json:"selectedModel,omitempty"`
	// DataVolume is the per-instance data directory.
	// +optional
	DataVolume *DataVolumeSpec `json:"dataVolume,omitempty"`
	// UserInstructions optionally appends user preferences to the definition
	// default system prompt (design §3.2: appended after the template
	// instructions; cannot remove or weaken security/identity bounds).
	// +optional
	UserInstructions string `json:"userInstructions,omitempty"`
	// ApprovalPolicy optionally overrides the template's approvalPolicy (issue
	// #116, design §3.2 inherit-or-own): empty = follow the template default
	// (live); set = the instance's own posture.
	// +optional
	ApprovalPolicy ApprovalPolicy `json:"approvalPolicy,omitempty"`
	// Allowlist is the instance's own safe-command allowlist: the rules the
	// user added by hand. Learned allow-always grants live in the per-user
	// grants ConfigMap, not here (issue #185) -- the chat approval path records
	// them there, and a client must not copy them back into this field, where
	// they would survive a revocation. The effective list is the union of the
	// platform builtin, the template allowlist and these entries: the instance
	// adds to it and cannot remove from it. A client PUTting this field
	// replaces it wholesale, so it must send back every entry it means to keep.
	// +optional
	Allowlist []AllowlistRule `json:"allowlist,omitempty"`
	// EnabledSkills optionally restricts the skills the AgentTemplate declares
	// (design §3.2: the instance may enable a subset; empty = all declared).
	// +optional
	EnabledSkills []string `json:"enabledSkills,omitempty"`
}

// AgentInstanceStatus is the observed state of an instance (design §3.2,
// written by the Instance Manager controller -- users do not edit it).
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
// +kubebuilder:printcolumn:name="Template",type="string",JSONPath=".spec.templateRef"
// +kubebuilder:printcolumn:name="Owner",type="string",JSONPath=".spec.owner"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Pod",type="string",JSONPath=".status.podName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AgentInstance is the runtime instance of an AgentTemplate for one user
// (design §3.2). It is reconciled by the Instance Manager controller:
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

// PodResources returns the container resources for the agent Pod (defaults).
func (in *AgentInstance) PodResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{}
}
