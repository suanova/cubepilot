package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/suanova/cubepilot/internal/openclaw"
	"github.com/suanova/cubepilot/internal/schedule"
	"github.com/suanova/cubepilot/internal/store"
)

// taskDTO is the API shape of a task, with the computed next-run time.
type taskDTO struct {
	store.Task
	NextRunAt *time.Time `json:"nextRunAt,omitempty"`
}

func (s *Server) toDTO(t store.Task) taskDTO {
	dto := taskDTO{Task: t}
	base := time.Now()
	if t.LastRunAt != nil {
		base = *t.LastRunAt
	}
	if t.Enabled {
		if next := schedule.NextRun(t.Schedule, base); !next.IsZero() {
			dto.NextRunAt = &next
		}
	}
	return dto
}

// handleTasks serves GET (list) and POST (create) for /api/tasks.
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		tasks, err := s.store.ListTasks()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out := make([]taskDTO, 0, len(tasks))
		for _, t := range tasks {
			out = append(out, s.toDTO(t))
		}
		writeJSON(w, http.StatusOK, map[string]any{"tasks": out})
	case http.MethodPost:
		var body struct {
			Name     string `json:"name"`
			Prompt   string `json:"prompt"`
			Schedule string `json:"schedule"`
			Enabled  *bool  `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		body.Prompt = strings.TrimSpace(body.Prompt)
		body.Schedule = strings.TrimSpace(body.Schedule)
		if body.Name == "" || body.Prompt == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name 与 prompt 必填"})
			return
		}
		if body.Schedule != "" {
			if _, err := schedule.Parse(body.Schedule); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("cron 表达式无效: %v", err)})
				return
			}
		}
		enabled := true
		if body.Enabled != nil {
			enabled = *body.Enabled
		}
		task, err := s.store.CreateTask(store.Task{
			Name:     body.Name,
			Prompt:   body.Prompt,
			Schedule: body.Schedule,
			Enabled:  enabled,
			Creator:  s.userOf(r),
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"task": s.toDTO(task)})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET or POST required"})
	}
}

// handleTaskByID routes /api/tasks/{id}[/run|/toggle|/reports].
func (s *Server) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/tasks/"), "/")
	parts := strings.SplitN(rest, "/", 2)
	id, action := parts[0], ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch action {
	case "":
		if r.Method != http.MethodDelete {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "DELETE required"})
			return
		}
		if err := s.store.DeleteTask(id); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
	case "run":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
			return
		}
		task, err := s.store.GetTask(id)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if err := s.runTask(ctx, task, "manual"); err != nil {
				log.Printf("task %s manual run failed: %v", task.ID, err)
			}
		}()
		writeJSON(w, http.StatusAccepted, map[string]any{"started": true, "task": s.toDTO(task)})
	case "toggle":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
			return
		}
		var task store.Task
		err := s.store.UpdateTask(id, func(t *store.Task) {
			t.Enabled = !t.Enabled
			task = *t
		})
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"task": s.toDTO(task)})
	case "reports":
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
			return
		}
		reports, err := s.store.ListReports(id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"reports": reports})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown task action"})
	}
}

// RunTask implements scheduler.Runner (design §4.5 / §5.4): it runs one
// agent turn through the creator's instance and returns the collected text.
// The CRD scheduler calls this and writes the TaskRun report itself
// (平台身份写入).
func (s *Server) RunTask(ctx context.Context, creator, sessionKey, prompt string) (string, error) {
	return s.streamCollect(ctx, creator, sessionKey, prompt)
}

// runTask executes one task through the user's agent instance: streams the
// prompt, records tool calls for audit, and stores a severity-counted report.
func (s *Server) runTask(ctx context.Context, task store.Task, trigger string) error {
	started := time.Now()
	content, runErr := s.streamCollect(ctx, task.Creator, "task-"+task.ID+"-"+uuid.NewString()[:8], task.Prompt)
	report, reportErr := s.store.AddReport(storeReport(task.ID, task.Name, trigger, started, content, runErr))
	status := "success"
	if runErr != nil {
		status = "failed"
	}
	if uerr := s.store.UpdateTask(task.ID, func(t *store.Task) {
		now := started
		t.LastRunAt = &now
		t.LastStatus = status
	}); uerr != nil {
		return uerr
	}
	if runErr != nil {
		return runErr
	}
	_ = report
	return reportErr
}

// streamCollect runs one agent turn and returns the concatenated deltas.
// Tool calls are not visible on the stream, so afterwards it replays the
// session transcript to record them for audit (M5).
func (s *Server) streamCollect(ctx context.Context, user, sessionKey, prompt string) (string, error) {
	if err := s.mgr.Ensure(ctx, user); err != nil {
		return "", fmt.Errorf("instance warming failed: %w", err)
	}
	var buf strings.Builder
	var doneErr string
	err := s.clientFor(user).StreamChat(ctx, openclaw.ChatParams{
		SessionKey: sessionKey,
		Messages:   []openclaw.ChatMessage{{Role: "user", Content: prompt}},
	}, func(ev openclaw.Event) error {
		switch ev.Type {
		case openclaw.EventMessageDelta:
			buf.WriteString(ev.Delta)
		case openclaw.EventMessageDone:
			doneErr = ev.Error
		}
		return nil
	})
	s.extractToolEvents(ctx, user, sessionKey)
	if err != nil {
		return buf.String(), err
	}
	if doneErr != "" {
		return buf.String(), fmt.Errorf("%s", doneErr)
	}
	return buf.String(), nil
}

// storeReport builds a severity-counted report from one run's output.
func storeReport(taskID, taskName, trigger string, started time.Time, content string, runErr error) store.Report {
	status := "success"
	if runErr != nil {
		status = "failed"
		content = content + "\n\n[执行错误] " + runErr.Error()
	}
	return store.Report{
		TaskID:     taskID,
		TaskName:   taskName,
		Trigger:    trigger,
		Status:     status,
		StartedAt:  started,
		FinishedAt: time.Now(),
		Content:    content,
		P0:         countSeverity(content, "P0"),
		P1:         countSeverity(content, "P1"),
		P2:         countSeverity(content, "P2"),
	}
}

// countSeverity counts distinct severity findings in a report. Structured
// reports list each finding under a header like "### P1 重要 — …", so count
// those first; fall back to counting bare mentions for free-text reports.
func countSeverity(content, sev string) int {
	header := regexp.MustCompile(`(?m)^#{1,4}\s*` + sev + `\b`)
	if n := len(header.FindAllString(content, -1)); n > 0 {
		return n
	}
	return strings.Count(content, sev)
}

// StartScheduler launches the FR-M4 cron loop: every 30s it runs due tasks.
// Multi-replica deployments only run tasks on the leader (design §3.3);
// standby replicas observe but do not fire.
func (s *Server) StartScheduler(ctx context.Context) {
	if s.schedulerLeader != nil && !s.schedulerLeader.IsLeader() {
		log.Printf("scheduler: standby replica, waiting for leadership")
	}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if s.schedulerLeader != nil && !s.schedulerLeader.IsLeader() {
				continue // standby: no firing
			}
			s.runDue(ctx)
		}
	}
}

func (s *Server) runDue(ctx context.Context) {
	tasks, err := s.store.ListTasks()
	if err != nil {
		log.Printf("scheduler: list tasks: %v", err)
		return
	}
	now := time.Now()
	for _, t := range tasks {
		if !t.Enabled || strings.TrimSpace(t.Schedule) == "" {
			continue
		}
		cron, err := schedule.Parse(t.Schedule)
		if err != nil {
			continue
		}
		base := t.CreatedAt
		if t.LastRunAt != nil {
			base = *t.LastRunAt
		}
		if next := cron.NextAfter(base); !next.IsZero() && !next.After(now) {
			log.Printf("scheduler: running due task %s (%s)", t.ID, t.Name)
			runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			if err := s.runTask(runCtx, t, "cron"); err != nil {
				log.Printf("scheduler: task %s failed: %v", t.ID, err)
			}
			cancel()
		}
	}
}
