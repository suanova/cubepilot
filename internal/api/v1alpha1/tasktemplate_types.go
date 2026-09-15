package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TaskTriggerKind is how a task is triggered (design §3.3.3).
// +kubebuilder:validation:Enum=Manual;Cron
type TaskTriggerKind string

const (
	// TaskTriggerManual means the task runs on demand only.
	TaskTriggerManual TaskTriggerKind = "Manual"
	// TaskTriggerCron means the task runs on a cron schedule.
	TaskTriggerCron TaskTriggerKind = "Cron"
)

// ParamSchema describes one task parameter (design §3.3.2 paramsSchema).
type ParamSchema struct {
	Name string `json:"name"`
	// Default is the value used when the Task does not override this parameter.
	// +optional
	Default string `json:"default,omitempty"`
	// Enum, when set, restricts the accepted values.
	// +optional
	Enum []string `json:"enum,omitempty"`
}

// RequiredPermissions is the permission hint of a task template
// (design §3.3.2: full-cluster inspection requires the creator to hold
// cluster-level read permission).
type RequiredPermissions struct {
	Level string `json:"level,omitempty"`
	Note  string `json:"note,omitempty"`
}

// TaskTemplateSpec is a parameterized task template (design §3.3.2) -- the
// template (what to do), the "class" of tasks. Preloaded: daily-inspection.
type TaskTemplateSpec struct {
	// DisplayName is the human-facing template name.
	DisplayName string `json:"displayName,omitempty"`
	// Description explains what the template does.
	Description string `json:"description,omitempty"`
	// Instruction is the parameterized prompt ({{param}} interpolation).
	Instruction string `json:"instruction,omitempty"`
	// ParamsSchema declares the parameters.
	// +optional
	ParamsSchema []ParamSchema `json:"paramsSchema,omitempty"`
	// RequiredPermissions is the permission hint.
	// +optional
	RequiredPermissions *RequiredPermissions `json:"requiredPermissions,omitempty"`
	// Skills declares the skills the task needs (design §3.5: resolved at
	// execution time against the current versions; the actual revisions used
	// are recorded on the TaskRun).
	// +optional
	Skills []string `json:"skills,omitempty"`
	// DefaultCron is the creation-wizard default schedule hint (design §3.5:
	// the Task's own cron wins).
	// +optional
	DefaultCron string `json:"defaultCron,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="DisplayName",type="string",JSONPath=".spec.displayName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// TaskTemplate is a parameterized task template (design §3.3.2) -- template !=
// instance != run: TaskTemplate (what to do) != Task (whose task, when it runs)
// != TaskRun (run report).
type TaskTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec TaskTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// TaskTemplateList contains a list of TaskTemplate.
type TaskTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TaskTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TaskTemplate{}, &TaskTemplateList{})
}

// Revision returns an immutable content fingerprint of the template spec
// (design §3.5: the TaskRun records the template revision actually used at
// run time). A content hash is deterministic, survives object re-creation,
// and lets an auditor compare "what ran" across runs.
func (t *TaskTemplate) Revision() string {
	return specRevision(t.Spec)
}
