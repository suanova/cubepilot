package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TaskPhase is the lifecycle phase of a Task (design §3.3.3: Ready / Paused).
type TaskPhase string

const (
	// TaskPhaseReady means the task is scheduled and will fire.
	TaskPhaseReady TaskPhase = "Ready"
	// TaskPhasePaused means the task is paused (no firing).
	TaskPhasePaused TaskPhase = "Paused"
)

// TaskSpec is a task instance (design §3.3.3) — whose task, when it runs. It
// links the
// execution subject (agentRef → Agent) with the task content (templateRef →
// TaskTemplate); creator decides the execution identity.
type TaskSpec struct {
	// TemplateRef points to the TaskTemplate (optional: inline instruction
	// tasks are also allowed, phase-one compatibility).
	// +optional
	TemplateRef string `json:"templateRef,omitempty"`
	// Instruction is the inline prompt (used when TemplateRef is empty).
	// +optional
	Instruction string `json:"instruction,omitempty"`
	// Params overrides template parameter defaults.
	// +optional
	Params map[string]string `json:"params,omitempty"`
	// AgentRef points to the Agent definition that executes the task
	// (default agent-for-cloud). Design §3.3.3: which Agent executes it.
	// +kubebuilder:default=agent-for-cloud
	// +optional
	AgentRef string `json:"agentRef,omitempty"`
	// Creator is the task creator; execution identity = creator (RBAC matches
	// the creator).
	Creator string `json:"creator"`
	// Trigger is manual | cron.
	Trigger TaskTriggerKind `json:"trigger"`
	// Cron is the 5-field cron expression (trigger=cron).
	// +optional
	Cron string `json:"cron,omitempty"`
	// Enabled gates firing (false = paused).
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// TaskStatus is the observed state of a Task.
type TaskStatus struct {
	// Phase is Ready / Paused.
	// +optional
	Phase TaskPhase `json:"phase,omitempty"`
	// LastRunTime is the last successful scheduling time.
	// +optional
	LastRunTime *metav1.Time `json:"lastRunTime,omitempty"`
	// LastStatus is the last run outcome (success | failed).
	// +optional
	LastStatus string `json:"lastStatus,omitempty"`
	// NextRunTime is the computed next fire time.
	// +optional
	NextRunTime *metav1.Time `json:"nextRunTime,omitempty"`
	// LastTaskRunName is the most recent TaskRun created for this Task.
	// +optional
	LastTaskRunName string `json:"lastTaskRunName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Template",type="string",JSONPath=".spec.templateRef"
// +kubebuilder:printcolumn:name="Agent",type="string",JSONPath=".spec.agentRef"
// +kubebuilder:printcolumn:name="Creator",type="string",JSONPath=".spec.creator"
// +kubebuilder:printcolumn:name="Trigger",type="string",JSONPath=".spec.trigger"
// +kubebuilder:printcolumn:name="Enabled",type="boolean",JSONPath=".spec.enabled"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Task is a task instance (design §3.3.3) — the "object" of the task domain:
// who owns it, when it runs, which Agent executes it. The scheduler reads
// Task CRDs and writes TaskRun (written with the platform identity, credential
// minimization).
type Task struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TaskSpec   `json:"spec,omitempty"`
	Status TaskStatus `json:"status,omitempty"`
}

// Enabled returns whether the task fires (default true).
func (t *Task) Enabled() bool {
	return t.Spec.Enabled == nil || *t.Spec.Enabled
}

// +kubebuilder:object:root=true

// TaskList contains a list of Task.
type TaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Task `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Task{}, &TaskList{})
}
