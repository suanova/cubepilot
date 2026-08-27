package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TaskRunPhase is the lifecycle phase of a TaskRun (design §3.3.4:
// Pending / Running / Completed / Failed / Cancelled).
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

// TaskRef links a TaskRun back to its Task (design §3.3.4 creatorTaskRef).
type TaskRef struct {
	Name string `json:"name"`
	UID  string `json:"uid,omitempty"`
}

// FindingItem is one structured finding of an inspection-style report
// (design §3.3.4: category / level / finding / evidence chain).
type FindingItem struct {
	// Category is the finding category (pod / node / gpu / storage / ...).
	Category string `json:"category,omitempty"`
	// Level is P0 / P1 / P2.
	Level string `json:"level,omitempty"`
	// Finding is the human-readable finding text.
	Finding string `json:"finding,omitempty"`
	// Evidence is the evidence chain (command + output excerpt + ts).
	// +optional
	Evidence []EvidenceEntry `json:"evidence,omitempty"`
}

// EvidenceEntry is one piece of evidence (design §3.3.4: evidence chain).
type EvidenceEntry struct {
	Command string `json:"command,omitempty"`
	Output  string `json:"output,omitempty"`
	TS      string `json:"ts,omitempty"`
}

// TaskRunStatus is the execution report written by the scheduler with the
// platform identity (design §3.3.4: written with the platform identity;
// Agent instances and user credentials never write CRDs directly).
type TaskRunStatus struct {
	// Phase is Pending / Running / Completed / Failed / Cancelled.
	Phase TaskRunPhase `json:"phase,omitempty"`
	// Summary carries the severity counts.
	// +optional
	Summary *TaskRunSummary `json:"summary,omitempty"`
	// Items are the structured findings.
	// +optional
	Items []FindingItem `json:"items,omitempty"`
	// Content is the full natural-language report text.
	// +optional
	Content string `json:"content,omitempty"`
	// TemplateRevision is the TaskTemplate revision actually used for this run
	// (design §3.5: resolved at run time, recorded for audit/rollback).
	// +optional
	TemplateRevision string `json:"templateRevision,omitempty"`
	// SkillRevision is the skill revision actually used for this run
	// (design §3.5: resolved at run time, recorded for audit/rollback).
	// +optional
	SkillRevision string `json:"skillRevision,omitempty"`
	// Conditions carries detail (Inspected=True etc).
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// StartedAt / FinishedAt bound the run.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
	// Error carries the failure detail.
	// +optional
	Error string `json:"error,omitempty"`
}

// TaskRunSummary is the severity summary of a run (design §3.3.4).
type TaskRunSummary struct {
	Total    int `json:"total,omitempty"`
	Abnormal int `json:"abnormal,omitempty"`
	P0       int `json:"p0,omitempty"`
	P1       int `json:"p1,omitempty"`
	P2       int `json:"p2,omitempty"`
}

// TaskRunSpec is the execution report of a task (design §3.3.4). It is
// created and written by the scheduler with the platform identity.
type TaskRunSpec struct {
	// Type is inspection | verification | ...
	Type string `json:"type,omitempty"`
	// Scope is the task scope (e.g. all).
	// +optional
	Scope string `json:"scope,omitempty"`
	// CreatorTaskRef links back to the owning Task.
	CreatorTaskRef TaskRef `json:"creatorTaskRef"`
	// TaskName is the display name of the task (denormalized).
	// +optional
	TaskName string `json:"taskName,omitempty"`
	// Owner is the task owner (execution identity; derived from the Task's
	// owner -- design §3.5).
	Owner string `json:"owner,omitempty"`
	// Trigger is Manual | Cron.
	// +optional
	Trigger string `json:"trigger,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Type",type="string",JSONPath=".spec.type"
// +kubebuilder:printcolumn:name="Task",type="string",JSONPath=".spec.creatorTaskRef.name"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="P0",type="integer",JSONPath=".status.summary.p0"
// +kubebuilder:printcolumn:name="P1",type="integer",JSONPath=".status.summary.p1"
// +kubebuilder:printcolumn:name="P2",type="integer",JSONPath=".status.summary.p2"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// TaskRun is the execution report (design §3.3.4) -- written by the scheduler
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
