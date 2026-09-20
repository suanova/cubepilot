package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TaskRunPhase is the lifecycle phase of a TaskRun:
// Pending / Running / Completed / Failed / Cancelled.
type TaskRunPhase string

const (
	// TaskRunPending means the run is queued.
	TaskRunPending TaskRunPhase = "Pending"
	// TaskRunRunning means the run is in progress.
	TaskRunRunning TaskRunPhase = "Running"
	// TaskRunCompleted means the run finished successfully.
	TaskRunCompleted TaskRunPhase = "Completed"
	// TaskRunFailed means the run failed.
	TaskRunFailed TaskRunPhase = "Failed"
	// TaskRunCancelled means the run was cancelled.
	TaskRunCancelled TaskRunPhase = "Cancelled"
)

// TaskRef links a TaskRun back to its Task (creatorTaskRef).
type TaskRef struct {
	Name string `json:"name"`
	UID  string `json:"uid,omitempty"`
}

// TaskRunStatus is the execution report written by the scheduler with the
// platform identity: Agent instances and user credentials never write CRDs
// directly.
type TaskRunStatus struct {
	// Phase is Pending / Running / Completed / Failed / Cancelled.
	Phase TaskRunPhase `json:"phase,omitempty"`
	// Content is the full natural-language report text.
	// +optional
	Content string `json:"content,omitempty"`
	// TemplateRevision is the TaskTemplate revision actually used for this run
	// (resolved at run time, recorded for audit/rollback).
	// +optional
	TemplateRevision string `json:"templateRevision,omitempty"`
	// SkillRevision is the skill revision actually used for this run
	// (resolved at run time, recorded for audit/rollback).
	// +optional
	SkillRevision string `json:"skillRevision,omitempty"`
	// StartedAt / FinishedAt bound the run.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// Error carries the failure detail.
	// +optional
	Error string `json:"error,omitempty"`
}

// TaskRunSpec is the execution report of a task. It is
// created and written by the scheduler with the platform identity.
type TaskRunSpec struct {
	// CreatorTaskRef links back to the owning Task.
	CreatorTaskRef TaskRef `json:"creatorTaskRef"`
	// Owner is the task owner (execution identity; derived from the Task's
	// owner).
	Owner string `json:"owner,omitempty"`
	// Trigger records how this run was started -- the same Task can be fired by
	// cron and by hand, so this is provenance about the run, not something
	// derivable from the Task it came from.
	// +optional
	Trigger TaskTriggerKind `json:"trigger,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.trigger"
// +kubebuilder:printcolumn:name="Task",type="string",JSONPath=".spec.creatorTaskRef.name"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// TaskRun is the execution report -- written by the scheduler
// with the platform identity, completing the template -> task -> run report
// loop.
type TaskRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TaskRunSpec   `json:"spec,omitempty"`
	Status TaskRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TaskRunList contains a list of TaskRun.
type TaskRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TaskRun `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TaskRun{}, &TaskRunList{})
}
