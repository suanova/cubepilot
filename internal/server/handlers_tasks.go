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
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prompt     string     `json:"prompt"`
	Schedule   string     `json:"schedule"`
	State      string     `json:"state"`   // Enabled | Paused
	Enabled    bool       `json:"enabled"` // derived from State (wire compat)
	Creator    string     `json:"creator"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastRunAt  *time.Time `json:"lastRunAt,omitempty"`
	LastStatus string     `json:"lastStatus,omitempty"`
	NextRunAt  *time.Time `json:"nextRunAt,omitempty"`
}

// reportDTO is the API shape of a run report (wire-compatible with the old
// JSON-store Report). Backed by TaskRun CRs.
type reportDTO struct {
	ID         string    `json:"id"`
	TaskID     string    `json:"taskId"`
	TaskName   string    `json:"taskName"`
	Trigger    string    `json:"trigger"` // Manual | Cron | Inspect
	Status     string    `json:"status"`  // success | failed
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
	Content    string    `json:"content"`
	P0         int       `json:"p0"`
	P1         int       `json:"p1"`
	P2         int       `json:"p2"`
}

func taskToDTO(t v1alpha1.Task) taskDTO {
	dto := taskDTO{
		ID:         t.Name,
		Name:       t.Annotations[v1alpha1.TaskDisplayNameAnnotation],
		Prompt:     t.Spec.Instruction,
		Schedule:   t.Spec.Cron,
		Enabled:    t.Enabled(),
		State:      string(t.Spec.State),
		Creator:    t.Spec.Owner,
		CreatedAt:  t.CreationTimestamp.Time,
		LastRunAt:  taskTimePtr(t.Status.LastRunTime),
		LastStatus: t.Status.LastStatus,
	}
	if dto.Name == "" {
		dto.Name = t.Name
	}
	// Next fire time, mirroring the operator scheduler's computation.
	if dto.Enabled && dto.Schedule != "" {
		if cron, err := schedule.Parse(dto.Schedule); err == nil {
			base := t.CreationTimestamp.Time
			if t.Status.LastRunTime != nil {
				base = t.Status.LastRunTime.Time
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

func taskRunToReport(taskName string, run v1alpha1.TaskRun) reportDTO {
	status := "success"
	if run.Status.Phase == v1alpha1.TaskRunFailed {
		status = "failed"
	}
	dto := reportDTO{
		ID:         run.Name,
		TaskID:     run.Spec.CreatorTaskRef.Name,
		TaskName:   taskName,
		Trigger:    run.Spec.Trigger,
		Status:     status,
		Content:    run.Status.Content,
		FinishedAt: run.CreationTimestamp.Time,
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
			Name     string `json:"name"`
			Prompt   string `json:"prompt"`
			Schedule string `json:"schedule"`
			State    string `json:"state"`
			Enabled  *bool  `json:"enabled"` // deprecated wire compat
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		body.Prompt = strings.TrimSpace(body.Prompt)
		body.Schedule = strings.TrimSpace(body.Schedule)
		if body.Name == "" || body.Prompt == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name and prompt are required"})
			return
		}
		trigger := v1alpha1.TaskTriggerManual
		if body.Schedule != "" {
			if _, err := schedule.Parse(body.Schedule); err != nil {
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
				Instruction: body.Prompt,
				Owner:       s.userOf(r),
				Trigger:     trigger,
				Cron:        body.Schedule,
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
