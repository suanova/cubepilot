package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TaskTriggerKind is how a task is triggered (design §3.3.3).
type TaskTriggerKind string

const (
	// TaskTriggerManual means the task runs on demand only.
	TaskTriggerManual TaskTriggerKind = "manual"
	// TaskTriggerCron means the task runs on a cron schedule.
	TaskTriggerCron TaskTriggerKind = "cron"
)

// ParamSchema describes one task parameter (design §3.3.2 paramsSchema).
type ParamSchema struct {
	Name    string   `json:"name"`
	Type    string   `json:"type,omitempty"`
	Default string   `json:"default,omitempty"`
	Enum    []string `json:"enum,omitempty"`
}

// RequiredPermissions is the permission hint of a task template
// (design §3.3.2: full-cluster inspection requires the creator to hold
// cluster-level read permission).
type RequiredPermissions struct {
	Level string `json:"level,omitempty"`
	Note  string `json:"note,omitempty"`
}

// TaskTemplateDefaults are the default trigger settings of a template.
type TaskTemplateDefaults struct {
	Trigger TaskTriggerKind `json:"trigger,omitempty"`
	Cron    string          `json:"cron,omitempty"`
}

// TaskTemplateSpec is a parameterized task template (design §3.3.2) — the
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
	// Defaults are the default trigger settings.
	// +optional
	Defaults *TaskTemplateDefaults `json:"defaults,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="DisplayName",type="string",JSONPath=".spec.displayName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// TaskTemplate is a parameterized task template (design §3.3.2) — template ≠
// instance ≠ run: TaskTemplate (what to do) ≠ Task (whose task, when it runs)
// ≠ TaskRun (run report).
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
