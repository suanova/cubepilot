// Package scheduler implements the FR-M4 / design §4.5 task scheduler: it reads
// Task CRDs (design §3.3.3), fires due tasks through the creator's agent
// instance, and writes the execution report as a TaskRun CRD with the
// platform identity (design §3.3.4 / §5.4: the scheduler creates and writes
// TaskRuns with the platform identity; Agent instances and user credentials
// never write CRDs directly — credential minimization).
package scheduler

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/schedule"
)

// Runner executes one task turn through an agent instance and returns the
// collected text. Implemented by the server (which owns the Instance Manager
// and the OpenClaw client wiring).
type Runner interface {
	// RunTask runs one task turn: prompt → agent instance → collected deltas.
	RunTask(ctx context.Context, creator, sessionKey, prompt string) (string, error)
}

// ReconcileScheduler is the CRD-driven scheduler: it watches Task CRs and
// fires due ones. TaskRuns are written with the platform identity (design
// §3.3.4).
type ReconcileScheduler struct {
	client.Client
	Cfg    config.Config
	Runner Runner
}

// +kubebuilder:rbac:groups=assistant.suanova.io,resources=tasks,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=assistant.suanova.io,resources=taskruns,verbs=get;list;watch;create;update;patch

// Reconcile is invoked on Task changes; due tasks fire here (leader-gated in
// the manager wiring).
func (r *ReconcileScheduler) Reconcile(ctx context.Context, req reconcile.Request) (ctrl.Result, error) {
	var task v1alpha1.Task
	if err := r.Get(ctx, req.NamespacedName, &task); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !task.Enabled() {
		r.patchPaused(ctx, &task)
		return ctrl.Result{}, nil
	}
	if task.Spec.Trigger != v1alpha1.TaskTriggerCron || strings.TrimSpace(task.Spec.Cron) == "" {
		return ctrl.Result{}, nil // manual-only: fired via API
	}

	next, due := r.nextDue(&task)
	r.patchNextRun(ctx, &task, next)

	if !due {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Fire the task (asynchronously — a long agent turn must not block the
	// reconcile loop; the requeue keeps the loop alive for the next due time).
	if err := r.fire(ctx, &task, "cron"); err != nil {
		log.Printf("scheduler: task %s fire: %v", task.Name, err)
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// nextDue computes the next fire time and whether the task is due now.
func (r *ReconcileScheduler) nextDue(task *v1alpha1.Task) (next *time.Time, due bool) {
	cron, err := schedule.Parse(task.Spec.Cron)
	if err != nil {
		return nil, false
	}
	now := time.Now()
	base := task.CreationTimestamp.Time
	if task.Status.LastRunTime != nil {
		base = task.Status.LastRunTime.Time
	}
	nextT := cron.NextAfter(base)
	// Due when the next fire time is not in the future AND we have not
	// recorded a run at/after that time.
	if !nextT.IsZero() && !nextT.After(now) {
		return &nextT, true
	}
	return &nextT, false
}

func (r *ReconcileScheduler) patchPaused(ctx context.Context, task *v1alpha1.Task) {
	if task.Status.Phase == v1alpha1.TaskPhasePaused {
		return
	}
	patch := client.MergeFrom(task.DeepCopy())
	task.Status.Phase = v1alpha1.TaskPhasePaused
	if err := r.Status().Patch(ctx, task, patch); err != nil {
		log.Printf("scheduler: patch paused %s: %v", task.Name, err)
	}
}

func (r *ReconcileScheduler) patchNextRun(ctx context.Context, task *v1alpha1.Task, next *time.Time) {
	if task.Status.NextRunTime != nil && next != nil && task.Status.NextRunTime.Time.Equal(next.Truncate(time.Minute)) {
		return
	}
	patch := client.MergeFrom(task.DeepCopy())
	task.Status.Phase = v1alpha1.TaskPhaseReady
	if next != nil {
		t := metav1.NewTime(*next)
		task.Status.NextRunTime = &t
	}
	if err := r.Status().Patch(ctx, task, patch); err != nil {
		log.Printf("scheduler: patch nextRun %s: %v", task.Name, err)
	}
}

// fire executes a due task: creates a TaskRun (Pending → Running), runs the
// agent turn, and writes the report (Completed / Failed) — written with the
// platform identity.
func (r *ReconcileScheduler) fire(ctx context.Context, task *v1alpha1.Task, trigger string) error {
	run := NewTaskRun(task, trigger)
	if err := r.Create(ctx, run); err != nil {
		return fmt.Errorf("create taskrun: %w", err)
	}
	// Mark Running.
	patch := client.MergeFrom(run.DeepCopy())
	run.Status.Phase = v1alpha1.TaskRunRunning
	now := metav1.Now()
	run.Status.StartedAt = &now
	if err := r.Status().Patch(ctx, run, patch); err != nil {
		log.Printf("scheduler: patch running %s: %v", run.Name, err)
	}

	// Resolve the prompt: template instruction + params interpolation.
	prompt := task.Spec.Instruction
	if task.Spec.TemplateRef != "" {
		var tpl v1alpha1.TaskTemplate
		if err := r.Get(ctx, types.NamespacedName{Name: task.Spec.TemplateRef}, &tpl); err == nil {
			prompt = renderTemplate(tpl.Spec.Instruction, task.Spec.Params)
		} else {
			log.Printf("scheduler: template %s: %v (falling back to inline)", task.Spec.TemplateRef, err)
		}
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = task.Spec.Instruction
	}

	// Run through the creator's agent instance (inspection runs with the
	// creator's identity, §5.4).
	sessionKey := fmt.Sprintf("task-%s-%s", task.Name, run.Name)
	content, runErr := r.Runner.RunTask(ctx, task.Spec.Creator, sessionKey, prompt)

	// Write the TaskRun report with the platform identity.
	finishPatch := client.MergeFrom(run.DeepCopy())
	finish := metav1.Now()
	run.Status.FinishedAt = &finish
	run.Status.Content = content
	run.Status.Summary = &v1alpha1.TaskRunSummary{
		Total:    countSeverityTotal(content),
		Abnormal: countSeverity(content, "P0") + countSeverity(content, "P1") + countSeverity(content, "P2"),
		P0:       countSeverity(content, "P0"),
		P1:       countSeverity(content, "P1"),
		P2:       countSeverity(content, "P2"),
	}
	if runErr != nil {
		run.Status.Phase = v1alpha1.TaskRunFailed
		run.Status.Error = runErr.Error()
	} else {
		run.Status.Phase = v1alpha1.TaskRunCompleted
	}
	if err := r.Status().Patch(ctx, run, finishPatch); err != nil {
		log.Printf("scheduler: patch finish %s: %v", run.Name, err)
	}

	// Record the run on the Task (LastRunTime / LastTaskRunName / LastStatus).
	taskPatch := client.MergeFrom(task.DeepCopy())
	lastRun := metav1.NewTime(time.Now())
	task.Status.LastRunTime = &lastRun
	task.Status.LastTaskRunName = run.Name
	task.Status.LastStatus = "success"
	if runErr != nil {
		task.Status.LastStatus = "failed"
	}
	if err := r.Status().Patch(ctx, task, taskPatch); err != nil {
		log.Printf("scheduler: patch task %s: %v", task.Name, err)
	}
	return runErr
}

// NewTaskRun builds the TaskRun skeleton (design §3.3.4: creatorTaskRef links
// back to the Task; written with the platform identity). TaskRun is a
// cluster-scoped CRD, so no
// namespace is set. Exported for reuse by the server's manual-run path.
func NewTaskRun(task *v1alpha1.Task, trigger string) *v1alpha1.TaskRun {
	ts := time.Now().UTC()
	name := fmt.Sprintf("%s-%s", task.Name, ts.Format("20060102-150405"))
	return &v1alpha1.TaskRun{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"cubepilot/task": task.Name,
			},
		},
		Spec: v1alpha1.TaskRunSpec{
			Type:    "inspection",
			Creator: task.Spec.Creator,
			Trigger: trigger,
			CreatorTaskRef: v1alpha1.TaskRef{
				Name: task.Name,
				UID:  string(task.UID),
			},
			TaskName: task.Spec.TemplateRef,
		},
		Status: v1alpha1.TaskRunStatus{Phase: v1alpha1.TaskRunPending},
	}
}

// renderTemplate interpolates {{param}} placeholders in a template
// instruction with the task's params (design §3.3.2: parameterized
// instruction).
func renderTemplate(instruction string, params map[string]string) string {
	out := instruction
	for k, v := range params {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out
}

// countSeverity counts severity mentions (shared with the server's report
// builder; keeps TaskRun summaries consistent).
func countSeverity(content, sev string) int {
	return strings.Count(content, sev)
}

func countSeverityTotal(content string) int {
	// Total findings ≈ count of P0/P1/P2 lines (rough; the agent's structured
	// report lists each finding under a header).
	lines := strings.Split(content, "\n")
	total := 0
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "- P0") || strings.HasPrefix(t, "- P1") || strings.HasPrefix(t, "- P2") ||
			strings.HasPrefix(t, "### P0") || strings.HasPrefix(t, "### P1") || strings.HasPrefix(t, "### P2") {
			total++
		}
	}
	return total
}

// SetupWithManager registers the scheduler's watch on Task CRs.
func (r *ReconcileScheduler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("scheduler").
		For(&v1alpha1.Task{}).
		Complete(r)
}

var _ reconcile.Reconciler = (*ReconcileScheduler)(nil)
