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

// TaskState is the task enablement state (design §3.5: string enum, not bool).
// +kubebuilder:validation:Enum=Enabled;Paused
type TaskState string

const (
	// TaskStateEnabled means the task is scheduled and will fire.
	TaskStateEnabled TaskState = "Enabled"
	// TaskStatePaused means the task is paused (no firing).
	TaskStatePaused TaskState = "Paused"
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
	// Owner is the task owner; execution identity = owner (RBAC matches the
	// owner; the per-user instance is derived from it — design §3.5: phase
	// one has one agent-for-cloud instance per user, no agentInstanceRef).
	Owner string `json:"owner"`
	// Trigger is Manual | Cron.
	Trigger TaskTriggerKind `json:"trigger"`
	// Cron is the 5-field cron expression (trigger=Cron).
	// +optional
	Cron string `json:"cron,omitempty"`
	// State is the task enablement state (design §3.5: string enum, not
	// bool): Enabled fires on schedule, Paused never fires.
	// +kubebuilder:default=Enabled
	// +optional
	State TaskState `json:"state,omitempty"`
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
// +kubebuilder:printcolumn:name="Owner",type="string",JSONPath=".spec.owner"
// +kubebuilder:printcolumn:name="Trigger",type="string",JSONPath=".spec.trigger"
// +kubebuilder:printcolumn:name="State",type="string",JSONPath=".spec.state"
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

// Enabled returns whether the task fires (default Enabled; empty state is
// treated as Enabled for backward compatibility with pre-CRD CRs).
func (t *Task) Enabled() bool {
	return t.Spec.State == "" || t.Spec.State == TaskStateEnabled
}

// +kubebuilder:object:root=true

// TaskList contains a list of Task.
type TaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Task `json:"items"`
}

// Annotation keys on Task (not CRD schema — Portal/operator coordination).
const (
	// TaskDisplayNameAnnotation carries the human-facing task name (the CR
	// name is DNS-1123 and may be sanitized/lossy for CJK input).
	TaskDisplayNameAnnotation = "cubepilot/display-name"
	// TaskManualRunAnnotation is set by the API on POST /api/tasks/{id}/run;
	// the operator's scheduler fires the task once (trigger=manual) and
	// removes the annotation. Value = RFC3339 timestamp (idempotency key).
	TaskManualRunAnnotation = "cubepilot/manual-run"
)

func init() {
	SchemeBuilder.Register(&Task{}, &TaskList{})
}
