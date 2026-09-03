package server

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// TestReportRunStatus verifies the TaskRun phase -> wire report status mapping
// (issue #95): only Completed reports as "success", Failed/Cancelled report as
// "failed", and a run that is still queued or executing reports as "running"
// instead of a premature "success".
func TestReportRunStatus(t *testing.T) {
	cases := []struct {
		name  string
		phase v1alpha1.TaskRunPhase
		want  string
	}{
		{"completed", v1alpha1.TaskRunCompleted, "success"},
		{"failed", v1alpha1.TaskRunFailed, "failed"},
		{"cancelled", v1alpha1.TaskRunCancelled, "failed"},
		{"running", v1alpha1.TaskRunRunning, "running"},
		{"pending", v1alpha1.TaskRunPending, "running"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reportRunStatus(c.phase); got != c.want {
				t.Errorf("reportRunStatus(%q) = %q, want %q", c.phase, got, c.want)
			}
		})
	}
}

// TestTaskRunToReportRunningNotFinished verifies an in-flight TaskRun is exposed
// as a "running" report whose FinishedAt is not fabricated from the creation
// timestamp (previously any non-Failed run looked Completed with a ~0 duration).
func TestTaskRunToReportRunningNotFinished(t *testing.T) {
	created := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	started := created.Add(time.Second)
	run := v1alpha1.TaskRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "t-r-20260814-100000",
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: v1alpha1.TaskRunSpec{
			CreatorTaskRef: v1alpha1.TaskRef{Name: "t"},
			Trigger:        "Cron",
		},
		Status: v1alpha1.TaskRunStatus{
			Phase:     v1alpha1.TaskRunRunning,
			StartedAt: &metav1.Time{Time: started},
			// FinishedAt is nil: the run is still in flight.
		},
	}

	rep := taskRunToReport("T", run)
	if rep.Status != "running" {
		t.Errorf("status = %q, want running", rep.Status)
	}
	if !rep.FinishedAt.IsZero() {
		t.Errorf("finishedAt = %v, want zero while running (not the creation timestamp)", rep.FinishedAt)
	}
	if !rep.StartedAt.Equal(started) {
		t.Errorf("startedAt = %v, want %v", rep.StartedAt, started)
	}
}

// TestTaskToDTONextRunUTC verifies the task-list next-run computation is pinned
// to UTC (issue #95): "0 2 * * *" always shows the next 02:00 UTC, never the
// operator process's local 02:00.
func TestTaskToDTONextRunUTC(t *testing.T) {
	task := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "zhang-task",
			CreationTimestamp: metav1.NewTime(time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)),
		},
		Spec: v1alpha1.TaskSpec{
			Owner:   "zhang.wei",
			Trigger: v1alpha1.TaskTriggerCron,
			Cron:    "0 2 * * *",
			State:   v1alpha1.TaskStateEnabled,
		},
	}

	dto := taskToDTO(*task)
	want := time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC) // created 10:00 UTC -> next 02:00 UTC
	if dto.NextRunAt == nil {
		t.Fatal("NextRunAt = nil, want set")
	}
	if !dto.NextRunAt.Equal(want) {
		t.Errorf("NextRunAt = %v, want %v", *dto.NextRunAt, want)
	}
	if dto.NextRunAt.Location() != time.UTC {
		t.Errorf("NextRunAt location = %v, want UTC", dto.NextRunAt.Location())
	}
}
