package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/schedule"
)

// taskDTO is the API shape of a task (kept wire-compatible with the pre-CRD
// JSON-store shape so the Portal needs no change). id = Task CR name; name =
// display-name annotation (the CR name is DNS-1123 sanitized).
type taskDTO struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Prompt      string     `json:"prompt"`
	Schedule    string     `json:"schedule"`
	TemplateRef string     `json:"templateRef,omitempty"` // bound TaskTemplate name ("" = free-form)
	State       string     `json:"state"`                 // Enabled | Paused
	Enabled     bool       `json:"enabled"`               // derived from State (wire compat)
	Creator     string     `json:"creator"`
	CreatedAt   time.Time  `json:"createdAt"`
	LastRunAt   *time.Time `json:"lastRunAt,omitempty"`
	LastStatus  string     `json:"lastStatus,omitempty"`
	NextRunAt   *time.Time `json:"nextRunAt,omitempty"`
}

// reportDTO is the API shape of a run report (wire-compatible with the old
// JSON-store Report). Backed by TaskRun CRs.
type reportDTO struct {
	ID         string    `json:"id"`
	TaskID     string    `json:"taskId"`
	TaskName   string    `json:"taskName"`
	Trigger    string    `json:"trigger"` // Manual | Cron | Inspect
	Status     string    `json:"status"`  // success | failed | running
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	Content    string    `json:"content"`
	P0         int       `json:"p0"`
	P1         int       `json:"p1"`
	P2         int       `json:"p2"`
}

func taskToDTO(t v1alpha1.Task) taskDTO {
	dto := taskDTO{
		ID:          t.Name,
		Name:        t.Annotations[v1alpha1.TaskDisplayNameAnnotation],
		Prompt:      t.Spec.Instruction,
		Schedule:    t.Spec.Cron,
		TemplateRef: t.Spec.TemplateRef,
		Enabled:     t.Enabled(),
		State:       string(t.Spec.State),
		Creator:     t.Spec.Owner,
		CreatedAt:   t.CreationTimestamp.Time,
		LastRunAt:   taskTimePtr(t.Status.LastRunTime),
		LastStatus:  t.Status.LastStatus,
	}
	if dto.Name == "" {
		dto.Name = t.Name
	}
	// Next fire time, mirroring the operator scheduler's computation (cron is
	// evaluated in UTC -- issue #95; the UI labels schedules "(UTC)").
	if dto.Enabled && dto.Schedule != "" {
		if cron, err := schedule.Parse(dto.Schedule); err == nil {
			base := t.CreationTimestamp.UTC()
			if t.Status.LastRunTime != nil {
				base = t.Status.LastRunTime.UTC()
			}
			if next := cron.NextAfter(base); !next.IsZero() {
				dto.NextRunAt = &next
			}
		}
	}
	return dto
}

func taskTimePtr(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.Time
	return &v
}

// reportRunStatus maps a TaskRun phase to the wire report status. A run that is
// still queued or executing (Pending/Running) must not be presented as a
// finished "success": it is reported as "running" until the scheduler writes
// Completed / Failed (issue #95).
func reportRunStatus(phase v1alpha1.TaskRunPhase) string {
	switch phase {
	case v1alpha1.TaskRunCompleted:
		return "success"
	case v1alpha1.TaskRunFailed, v1alpha1.TaskRunCancelled:
		return "failed"
	default: // TaskRunPending, TaskRunRunning, or an unset phase
		return "running"
	}
}

func taskRunToReport(taskName string, run v1alpha1.TaskRun) reportDTO {
	dto := reportDTO{
		ID:       run.Name,
		TaskID:   run.Spec.CreatorTaskRef.Name,
		TaskName: taskName,
		Trigger:  run.Spec.Trigger,
		Status:   reportRunStatus(run.Status.Phase),
		Content:  run.Status.Content,
	}
	if run.Status.StartedAt != nil {
		dto.StartedAt = run.Status.StartedAt.Time
	}
	if run.Status.FinishedAt != nil {
		dto.FinishedAt = run.Status.FinishedAt.Time
	}
	if run.Status.Summary != nil {
		dto.P0 = run.Status.Summary.P0
		dto.P1 = run.Status.Summary.P1
		dto.P2 = run.Status.Summary.P2
	}
	return dto
}

// handleTasks serves GET (list) and POST (create) for /api/tasks -- a thin
// CRD facade over Task CRs (design §3.5: Task = who + when; scheduling is
// owned by the operator's ReconcileScheduler, the API never writes TaskRuns).
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if s.cr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "CRD path disabled"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		var list v1alpha1.TaskList
		if err := s.cr.List(r.Context(), &list); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		// Owner-scoped listing (design §3.5: Task carries its owner; a user
		// sees only their own tasks -- same isolation as instances).
		me := s.userOf(r)
		out := make([]taskDTO, 0, len(list.Items))
		for _, t := range list.Items {
			if t.Spec.Owner == me {
				out = append(out, taskToDTO(t))
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
		writeJSON(w, http.StatusOK, map[string]any{"tasks": out})
	case http.MethodPost:
		var body struct {
			Name        string            `json:"name"`
			Prompt      string            `json:"prompt"`
			Schedule    *string           `json:"schedule"` // omitted == use template default; explicit "" == Manual
			TemplateRef string            `json:"templateRef"`
			Params      map[string]string `json:"params"`
			State       string            `json:"state"`
			Enabled     *bool             `json:"enabled"` // deprecated wire compat
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		body.Prompt = strings.TrimSpace(body.Prompt)
		body.TemplateRef = strings.TrimSpace(body.TemplateRef)
		cronExpr := ""
		scheduleProvided := body.Schedule != nil
		if body.Schedule != nil {
			cronExpr = strings.TrimSpace(*body.Schedule)
		}
		if body.Name == "" || (body.Prompt == "" && body.TemplateRef == "") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name and a prompt or template are required"})
			return
		}
		// Optional template binding (design §3.5): a Task either binds a
		// TaskTemplate (templateRef + params; at fire time the scheduler renders
		// the template's current instruction and records the revision used) or
		// is a free-form inline task (instruction only, no templateRef).
		templateRef := body.TemplateRef
		instruction := body.Prompt
		params := body.Params
		if templateRef != "" {
			var tpl v1alpha1.TaskTemplate
			if err := s.cr.Get(r.Context(), types.NamespacedName{Name: templateRef}, &tpl); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("unknown task template %q", templateRef)})
				return
			}
			merged, err := resolveTemplateParams(tpl.Spec.ParamsSchema, body.Params)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			params = merged
			// The stored instruction is a rendered snapshot (used for display
			// and as the fallback if the template is later deleted); the
			// operator re-renders from the template at fire time.
			instruction = renderTaskInstruction(tpl.Spec.Instruction, params)
			// Default the schedule from the template only when the caller
			// omitted it entirely. An explicit empty schedule means Manual and
			// must stay Manual; an explicit cron expression wins.
			if !scheduleProvided && tpl.Spec.DefaultCron != "" {
				cronExpr = tpl.Spec.DefaultCron
			}
		} else if len(body.Params) > 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "params require a template (templateRef)"})
			return
		}
		trigger := v1alpha1.TaskTriggerManual
		if cronExpr != "" {
			if _, err := schedule.Parse(cronExpr); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid cron expression: %v", err)})
				return
			}
			trigger = v1alpha1.TaskTriggerCron
		}
		enabled := true
		if body.Enabled != nil {
			enabled = *body.Enabled
		}
		state := v1alpha1.TaskStateEnabled
		if body.State != "" {
			state = v1alpha1.TaskState(body.State)
		} else if !enabled {
			state = v1alpha1.TaskStatePaused
		}
		task := &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				// CR name must be DNS-1123; the human name lives in the
				// display-name annotation (CJK input would otherwise be lost).
				Name: fmt.Sprintf("%s-task-%s", k8s.Sanitize(s.userOf(r)), uuid.NewString()[:8]),
				Annotations: map[string]string{
					v1alpha1.TaskDisplayNameAnnotation: body.Name,
				},
			},
			Spec: v1alpha1.TaskSpec{
				TemplateRef: templateRef,
				Instruction: instruction,
				Params:      params,
				Owner:       s.userOf(r),
				Trigger:     trigger,
				Cron:        cronExpr,
				State:       state,
			},
		}
		if err := s.cr.Create(r.Context(), task); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"task": taskToDTO(*task)})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET or POST required"})
	}
}

// resolveTemplateParams merges the template's declared param defaults with the
// caller's params and rejects values the template does not allow: an unknown
// key, or a non-empty value outside a declared ParamSchema.Enum (design §3.3.2:
// paramsSchema is the parameter contract). An empty caller value falls back to
// the schema default.
func resolveTemplateParams(schema []v1alpha1.ParamSchema, params map[string]string) (map[string]string, error) {
	merged := map[string]string{}
	for _, p := range schema {
		if p.Default != "" {
			merged[p.Name] = p.Default
		}
	}
	for k, v := range params {
		p, ok := schemaParam(schema, k)
		if !ok {
			return nil, fmt.Errorf("unknown template param %q", k)
		}
		if v == "" {
			continue // falls back to the schema default already merged in
		}
		if len(p.Enum) > 0 && !containsString(p.Enum, v) {
			return nil, fmt.Errorf("invalid value %q for template param %q (allowed: %s)", v, k, strings.Join(p.Enum, ", "))
		}
		merged[k] = v
	}
	return merged, nil
}

func schemaParam(schema []v1alpha1.ParamSchema, name string) (v1alpha1.ParamSchema, bool) {
	for _, p := range schema {
		if p.Name == name {
			return p, true
		}
	}
	return v1alpha1.ParamSchema{}, false
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// renderTaskInstruction interpolates {{param}} placeholders, mirroring the
// scheduler's renderTemplate (internal/scheduler/scheduler.go) so a
// template-bound Task's stored instruction matches what the operator renders
// at fire time. Duplicated here rather than importing the controller-runtime
// scheduler package into the API binary.
func renderTaskInstruction(instruction string, params map[string]string) string {
	out := instruction
	for k, v := range params {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out
}

// handleTaskByID routes /api/tasks/{id}[/run|/toggle|/reports].
func (s *Server) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	if s.cr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "CRD path disabled"})
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/tasks/"), "/")
	parts := strings.SplitN(rest, "/", 2)
	id, action := parts[0], ""
	if len(parts) == 2 {
		action = parts[1]
	}
	if id == "" {
		http.NotFound(w, r)
		return
	}
	key := types.NamespacedName{Name: id}

	switch action {
	case "":
		if r.Method != http.MethodDelete {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "DELETE required"})
			return
		}
		var existing v1alpha1.Task
		if err := s.cr.Get(r.Context(), key, &existing); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		if existing.Spec.Owner != s.userOf(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "not your task"})
			return
		}
		task := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: id}}
		if err := s.cr.Delete(r.Context(), task); err != nil {
			if apierrors.IsNotFound(err) {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
	case "run":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
			return
		}
		var task v1alpha1.Task
		if err := s.cr.Get(r.Context(), key, &task); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		if task.Spec.Owner != s.userOf(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "not your task"})
			return
		}
		// The operator's scheduler fires manual-run annotations (design §3.5:
		// the API never writes TaskRuns; the scheduler owns execution). The
		// timestamp value is the idempotency key.
		patch := client.MergeFrom(task.DeepCopy())
		if task.Annotations == nil {
			task.Annotations = map[string]string{}
		}
		task.Annotations[v1alpha1.TaskManualRunAnnotation] = time.Now().UTC().Format(time.RFC3339)
		if err := s.cr.Patch(r.Context(), &task, patch); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"started": true, "task": taskToDTO(task)})
	case "toggle":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
			return
		}
		var task v1alpha1.Task
		if err := s.cr.Get(r.Context(), key, &task); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		if task.Spec.Owner != s.userOf(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "not your task"})
			return
		}
		patch := client.MergeFrom(task.DeepCopy())
		if task.Spec.State == v1alpha1.TaskStatePaused {
			task.Spec.State = v1alpha1.TaskStateEnabled
		} else {
			task.Spec.State = v1alpha1.TaskStatePaused
		}
		if err := s.cr.Patch(r.Context(), &task, patch); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"task": taskToDTO(task)})
	case "reports":
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
			return
		}
		var task v1alpha1.Task
		if err := s.cr.Get(r.Context(), key, &task); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		if task.Spec.Owner != s.userOf(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "not your task"})
			return
		}
		taskName := id
		if n := task.Annotations[v1alpha1.TaskDisplayNameAnnotation]; n != "" {
			taskName = n
		}
		// TaskRun CRs are labelled with the owning task; newest first.
		var list v1alpha1.TaskRunList
		if err := s.cr.List(r.Context(), &list, client.MatchingLabels{"cubepilot/task": id}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out := make([]reportDTO, 0, len(list.Items))
		for _, run := range list.Items {
			out = append(out, taskRunToReport(taskName, run))
		}
		sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
		writeJSON(w, http.StatusOK, map[string]any{"reports": out})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown task action"})
	}
}
