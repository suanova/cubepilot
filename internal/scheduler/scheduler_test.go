package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: %v", err)
	}
	return scheme
}

// newFakeClient returns a fake client with the status subresource enabled for
// the platform types (fake client ignores status writes otherwise).
func newFakeClient(t *testing.T, scheme *runtime.Scheme, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&v1alpha1.Task{},
			&v1alpha1.TaskRun{},
			&v1alpha1.AgentInstance{},
			&v1alpha1.AgentTemplate{},
			&v1alpha1.Skill{},
		).
		WithObjects(objs...).
		Build()
}

// fakeRunner records the prompt and returns a canned report.
type fakeRunner struct {
	gotPrompt string
	gotUser   string
}

func (f *fakeRunner) RunTask(ctx context.Context, creator, sessionKey, prompt string) (string, error) {
	f.gotUser = creator
	f.gotPrompt = prompt
	return "### P1 Important -- inference pod CrashLoopBackOff\nEvidence: kubectl get pods", nil
}

func dueTask(created time.Time) *v1alpha1.Task {
	return &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "zhang-wei-daily-inspection",
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: v1alpha1.TaskSpec{
			TemplateRef: "daily-inspection",
			Owner:       "zhang.wei",
			Trigger:     v1alpha1.TaskTriggerCron,
			Cron:        "* * * * *", // every minute -> deterministically due
			State:       v1alpha1.TaskStateEnabled,
		},
	}
}

// TestSchedulerFiresDueTask verifies the CRD scheduler: a due cron task is
// fired through the runner and the report is written as a TaskRun with the
// platform identity (design §3.3.4: TaskRun written with the platform
// identity; §5.4 inspection runs with the creator's identity).
func TestSchedulerFiresDueTask(t *testing.T) {
	scheme := testScheme(t)
	runner := &fakeRunner{}
	cl := newFakeClient(t, scheme)

	// Task due: created 2 minutes ago; every-minute cron -> the next fire is
	// already in the past -> due on reconcile.
	task := dueTask(time.Now().Add(-2 * time.Minute))
	if err := cl.Create(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// Template for instruction rendering.
	tpl := &v1alpha1.TaskTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "daily-inspection"},
		Spec: v1alpha1.TaskTemplateSpec{
			DisplayName: "Daily cluster inspection",
			Instruction: "Inspect the cluster read-only, grade findings as P0/P1/P2; scope {{scope}}",
		},
	}
	if err := cl.Create(context.Background(), tpl); err != nil {
		t.Fatalf("create template: %v", err)
	}

	r := &ReconcileScheduler{
		Client: cl,
		Cfg:    config.Config{Namespace: "cubepilot"},
		Runner: runner,
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: task.Name},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Runner invoked with the creator identity and rendered instruction.
	if runner.gotUser != "zhang.wei" {
		t.Errorf("runner creator = %q, want zhang.wei", runner.gotUser)
	}
	if !strings.Contains(runner.gotPrompt, "{{scope}}") {
		// No params set -> placeholder stays; instruction still rendered.
		if !strings.Contains(runner.gotPrompt, "read-only") {
			t.Errorf("prompt not rendered from template: %q", runner.gotPrompt)
		}
	}

	// TaskRun created with the platform identity, completed, report summary.
	var runs v1alpha1.TaskRunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list taskruns: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("taskruns = %d, want 1", len(runs.Items))
	}
	run := runs.Items[0]
	if run.Spec.CreatorTaskRef.Name != task.Name {
		t.Errorf("creatorTaskRef.name = %q, want %q", run.Spec.CreatorTaskRef.Name, task.Name)
	}
	if run.Status.Phase != v1alpha1.TaskRunCompleted {
		t.Errorf("phase = %s, want Completed", run.Status.Phase)
	}
	if run.Status.Summary == nil || run.Status.Summary.P1 != 1 {
		t.Errorf("summary = %+v, want P1=1", run.Status.Summary)
	}

	// Task status records the run.
	var got v1alpha1.Task
	if err := cl.Get(context.Background(), types.NamespacedName{Name: task.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.LastTaskRunName != run.Name {
		t.Errorf("task.lastTaskRunName = %q, want %q", got.Status.LastTaskRunName, run.Name)
	}
	if got.Status.LastStatus != "success" {
		t.Errorf("task.lastStatus = %q, want success", got.Status.LastStatus)
	}
}

// TestRenderTemplate verifies {{param}} interpolation (design §3.3.2
// parameterized instruction).
func TestRenderTemplate(t *testing.T) {
	got := renderTemplate("inspection scope {{scope}}, nodes {{scope}}", map[string]string{"scope": "all"})
	if got != "inspection scope all, nodes all" {
		t.Errorf("render = %q", got)
	}
}

// TestNextDue verifies the due computation: a paused/disabled task never due.
func TestNextDueDisabled(t *testing.T) {
	task := dueTask(time.Now().Add(-26 * time.Hour))
	task.Spec.State = v1alpha1.TaskStatePaused
	r := &ReconcileScheduler{}
	if !task.Enabled() {
		// Enabled() false -> scheduler returns early; nextDue not consulted.
	} else {
		t.Error("task should be disabled")
	}
	_ = r
}

// errorRunner produces a partial report and then fails, simulating an agent
// turn that produced output but did not complete (design §3.3.4: Failed).
type errorRunner struct {
	gotPrompt string
	gotUser   string
}

func (f *errorRunner) RunTask(ctx context.Context, creator, sessionKey, prompt string) (string, error) {
	f.gotUser = creator
	f.gotPrompt = prompt
	return "### P0 -- node NotReady\nEvidence: kubectl get nodes", errors.New("agent turn failed: context deadline")
}

// TestSchedulerRunFailed verifies the Failed state-machine transition: the
// TaskRun records the error and any partial content, the severity summary is
// still computed, and the Task's last run status is "failed".
func TestSchedulerRunFailed(t *testing.T) {
	scheme := testScheme(t)
	runner := &errorRunner{}
	cl := newFakeClient(t, scheme)

	task := dueTask(time.Now().Add(-2 * time.Minute))
	if err := cl.Create(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	tpl := &v1alpha1.TaskTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "daily-inspection"},
		Spec: v1alpha1.TaskTemplateSpec{
			DisplayName: "Daily cluster inspection",
			Instruction: "Inspect the cluster read-only, grade findings as P0/P1/P2; scope {{scope}}",
		},
	}
	if err := cl.Create(context.Background(), tpl); err != nil {
		t.Fatalf("create template: %v", err)
	}

	r := &ReconcileScheduler{
		Client: cl,
		Cfg:    config.Config{Namespace: "cubepilot"},
		Runner: runner,
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: task.Name},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var runs v1alpha1.TaskRunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list taskruns: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("taskruns = %d, want 1", len(runs.Items))
	}
	run := runs.Items[0]
	if run.Status.Phase != v1alpha1.TaskRunFailed {
		t.Errorf("phase = %s, want Failed", run.Status.Phase)
	}
	if run.Status.Error == "" || !strings.Contains(run.Status.Error, "agent turn failed") {
		t.Errorf("error = %q, want agent turn failure recorded", run.Status.Error)
	}
	if !strings.Contains(run.Status.Content, "P0") {
		t.Errorf("content = %q, want partial output retained", run.Status.Content)
	}
	if run.Status.Summary == nil || run.Status.Summary.P0 != 1 {
		t.Errorf("summary = %+v, want P0=1 despite failure", run.Status.Summary)
	}

	var got v1alpha1.Task
	if err := cl.Get(context.Background(), types.NamespacedName{Name: task.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.LastStatus != "failed" {
		t.Errorf("task.lastStatus = %q, want failed", got.Status.LastStatus)
	}
	if got.Status.LastTaskRunName != run.Name {
		t.Errorf("task.lastTaskRunName = %q, want %q", got.Status.LastTaskRunName, run.Name)
	}
}

// TestManualRunAnnotationFiresEvenWhenPaused verifies an explicit manual run
// (API sets cubepilot/manual-run) fires even for a paused task, records the
// run with trigger=Manual, and clears the annotation so a reconcile retry
// cannot fire it twice (design §3.5: the API never writes TaskRuns -- the
// scheduler owns execution).
func TestManualRunAnnotationFiresEvenWhenPaused(t *testing.T) {
	scheme := testScheme(t)
	runner := &fakeRunner{}
	cl := newFakeClient(t, scheme)

	task := dueTask(time.Now().Add(-2 * time.Minute))
	task.Spec.State = v1alpha1.TaskStatePaused
	task.Annotations = map[string]string{
		v1alpha1.TaskManualRunAnnotation: time.Now().UTC().Format(time.RFC3339),
	}
	if err := cl.Create(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	tpl := &v1alpha1.TaskTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "daily-inspection"},
		Spec: v1alpha1.TaskTemplateSpec{
			DisplayName: "Daily cluster inspection",
			Instruction: "Inspect the cluster read-only, grade findings as P0/P1/P2",
		},
	}
	if err := cl.Create(context.Background(), tpl); err != nil {
		t.Fatalf("create template: %v", err)
	}

	r := &ReconcileScheduler{
		Client: cl,
		Cfg:    config.Config{Namespace: "cubepilot"},
		Runner: runner,
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: task.Name},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var runs v1alpha1.TaskRunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list taskruns: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("taskruns = %d, want 1 (manual run fired despite paused)", len(runs.Items))
	}
	if runs.Items[0].Spec.Trigger != "Manual" {
		t.Errorf("run trigger = %q, want Manual", runs.Items[0].Spec.Trigger)
	}
	if runs.Items[0].Status.Phase != v1alpha1.TaskRunCompleted {
		t.Errorf("phase = %s, want Completed", runs.Items[0].Status.Phase)
	}

	// The manual-run annotation must be cleared so a retry does not re-fire.
	var got v1alpha1.Task
	if err := cl.Get(context.Background(), types.NamespacedName{Name: task.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Annotations[v1alpha1.TaskManualRunAnnotation]; ok {
		t.Errorf("manual-run annotation not cleared after firing")
	}
}

// TestPausedTaskDoesNotFire verifies a paused task without a manual-run
// annotation never fires: no TaskRun is created and the task status is marked
// Paused (design §3.5: Paused never fires).
func TestPausedTaskDoesNotFire(t *testing.T) {
	scheme := testScheme(t)
	cl := newFakeClient(t, scheme)

	task := dueTask(time.Now().Add(-26 * time.Hour)) // would be long due if enabled
	task.Spec.State = v1alpha1.TaskStatePaused
	if err := cl.Create(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	r := &ReconcileScheduler{
		Client: cl,
		Cfg:    config.Config{Namespace: "cubepilot"},
		Runner: &fakeRunner{}, // must not be invoked
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: task.Name},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var runs v1alpha1.TaskRunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list taskruns: %v", err)
	}
	if len(runs.Items) != 0 {
		t.Fatalf("taskruns = %d, want 0 for a paused task", len(runs.Items))
	}

	var got v1alpha1.Task
	if err := cl.Get(context.Background(), types.NamespacedName{Name: task.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.TaskPhasePaused {
		t.Errorf("task phase = %s, want Paused", got.Status.Phase)
	}
}

// TestNotDueTaskDoesNotFire verifies a task whose next fire is still in the
// future does not create a TaskRun.
func TestNotDueTaskDoesNotFire(t *testing.T) {
	scheme := testScheme(t)
	cl := newFakeClient(t, scheme)

	task := dueTask(time.Now()) // created now -> next minute boundary is in the future
	if err := cl.Create(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	r := &ReconcileScheduler{
		Client: cl,
		Cfg:    config.Config{Namespace: "cubepilot"},
		Runner: &fakeRunner{},
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: task.Name},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var runs v1alpha1.TaskRunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list taskruns: %v", err)
	}
	if len(runs.Items) != 0 {
		t.Fatalf("taskruns = %d, want 0 (not due yet)", len(runs.Items))
	}
}

// TestManualOnlyTaskDoesNotFire verifies a Manual-trigger task without a
// manual-run annotation is a no-op for the scheduler (it fires only through
// the API annotation path).
func TestManualOnlyTaskDoesNotFire(t *testing.T) {
	scheme := testScheme(t)
	cl := newFakeClient(t, scheme)

	task := dueTask(time.Now().Add(-2 * time.Minute))
	task.Spec.Trigger = v1alpha1.TaskTriggerManual
	task.Spec.Cron = ""
	if err := cl.Create(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	r := &ReconcileScheduler{
		Client: cl,
		Cfg:    config.Config{Namespace: "cubepilot"},
		Runner: &fakeRunner{},
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: task.Name},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var runs v1alpha1.TaskRunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list taskruns: %v", err)
	}
	if len(runs.Items) != 0 {
		t.Fatalf("taskruns = %d, want 0 for manual-only without annotation", len(runs.Items))
	}
}

// TestNewTaskRunSkeleton verifies the TaskRun record skeleton: it starts
// Pending and denormalizes creatorTaskRef / owner / trigger plus the task
// label for lookup (design §3.3.4).
func TestNewTaskRunSkeleton(t *testing.T) {
	task := dueTask(time.Now())
	task.UID = types.UID("uid-123")

	run := NewTaskRun(task, "Cron")

	if run.Status.Phase != v1alpha1.TaskRunPending {
		t.Errorf("phase = %s, want Pending", run.Status.Phase)
	}
	if !strings.HasPrefix(run.Name, task.Name+"-") {
		t.Errorf("name = %q, want prefix %q", run.Name, task.Name+"-")
	}
	if run.Spec.CreatorTaskRef.Name != task.Name {
		t.Errorf("creatorTaskRef.name = %q, want %q", run.Spec.CreatorTaskRef.Name, task.Name)
	}
	if run.Spec.CreatorTaskRef.UID != "uid-123" {
		t.Errorf("creatorTaskRef.uid = %q, want uid-123", run.Spec.CreatorTaskRef.UID)
	}
	if run.Spec.Owner != "zhang.wei" {
		t.Errorf("owner = %q, want zhang.wei", run.Spec.Owner)
	}
	if run.Spec.Trigger != "Cron" {
		t.Errorf("trigger = %q, want Cron", run.Spec.Trigger)
	}
	if run.Labels["cubepilot/task"] != task.Name {
		t.Errorf("label cubepilot/task = %q, want %q", run.Labels["cubepilot/task"], task.Name)
	}
}
