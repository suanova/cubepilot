package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add to scheme: %v", err)
	}
	return scheme
}

// testClientBuilder returns the fake-client builder the scheduler tests share:
// the status subresource is enabled for the platform types (the fake client
// drops status writes otherwise).
func testClientBuilder(scheme *runtime.Scheme, objs ...client.Object) *fake.ClientBuilder {
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&v1alpha1.Task{},
			&v1alpha1.TaskRun{},
			&v1alpha1.AgentInstance{},
			&v1alpha1.AgentTemplate{},
			&v1alpha1.Skill{},
		).
		WithObjects(objs...)
}

// newFakeClient returns a fake client with the status subresource enabled for
// the platform types (fake client ignores status writes otherwise).
func newFakeClient(t *testing.T, scheme *runtime.Scheme, objs ...client.Object) client.Client {
	t.Helper()
	return testClientBuilder(scheme, objs...).Build()
}

// fakeRunner records the prompt and returns a canned report. calls counts the
// turns actually run, so a test asserting "no turn was run" cannot pass just
// because the prompt it inspected happened to be empty.
type fakeRunner struct {
	gotPrompt string
	gotUser   string
	calls     int
}

func (f *fakeRunner) RunTask(ctx context.Context, creator, sessionKey, prompt string) (string, error) {
	f.calls++
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
			Cron:        "* * * * *", // every minute -> deterministically due
			State:       v1alpha1.TaskStateEnabled,
		},
	}
}

// readyInstance returns a Ready cubepilot instance for the owner -- the
// instance the scheduler looks up before firing a task.
func readyInstance(owner string) *v1alpha1.AgentInstance {
	return &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: k8s.InstanceName(owner, v1alpha1.DefaultAgentName)},
		Spec:       v1alpha1.AgentInstanceSpec{TemplateRef: v1alpha1.DefaultAgentName, Owner: owner},
		Status:     v1alpha1.AgentInstanceStatus{Phase: v1alpha1.InstanceReady},
	}
}

// TestSchedulerFiresDueTask verifies the CRD scheduler: a due cron task is
// fired through the runner and the report is written as a TaskRun with the
// platform identity (design §3.3.4: TaskRun written with the platform
// identity; §5.4 inspection runs with the creator's identity). The owner's
// Ready instance is present, so the pre-fire availability check passes.
func TestSchedulerFiresDueTask(t *testing.T) {
	scheme := testScheme(t)
	runner := &fakeRunner{}
	cl := newFakeClient(t, scheme, readyInstance("zhang.wei"))

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
		Cfg:    config.Config{Namespace: ""},
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

	// TaskRun created with the platform identity, completed.
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
// TaskRun records the error and any partial content, and the Task's last run
// status is "failed".
func TestSchedulerRunFailed(t *testing.T) {
	scheme := testScheme(t)
	runner := &errorRunner{}
	cl := newFakeClient(t, scheme, readyInstance("zhang.wei"))

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
		Cfg:    config.Config{Namespace: ""},
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
	cl := newFakeClient(t, scheme, readyInstance("zhang.wei"))

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
		Cfg:    config.Config{Namespace: ""},
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
// annotation never fires: no TaskRun is created and the task records no next
// run (design §3.5: Paused never fires).
func TestPausedTaskDoesNotFire(t *testing.T) {
	scheme := testScheme(t)
	cl := newFakeClient(t, scheme)

	task := dueTask(time.Now().Add(-26 * time.Hour)) // would be long due if enabled
	task.Spec.State = v1alpha1.TaskStatePaused
	// The pre-state that makes the dedup meaningful: the task ran while
	// enabled, so its status carries a next run; pausing must clear it.
	task.Status.NextRunTime = &metav1.Time{Time: time.Now().Add(-25 * time.Hour)}
	if err := cl.Create(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := cl.Status().Update(context.Background(), task); err != nil {
		t.Fatalf("seed task status: %v", err)
	}

	r := &ReconcileScheduler{
		Client: cl,
		Cfg:    config.Config{Namespace: ""},
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
	if got.Status.NextRunTime != nil {
		t.Errorf("paused task still has nextRunTime = %v, want nil", got.Status.NextRunTime)
	}
}

// TestPausedTaskWritesStatusOnce verifies the scheduler's pause dedup: the
// first reconcile records that a paused task has no next run, and every
// requeue after that writes nothing. resourceVersion is the evidence -- the
// fake client bumps it on a status write. The first reconcile is asserted to
// have bumped it, so the follow-up assertion cannot pass vacuously.
func TestPausedTaskWritesStatusOnce(t *testing.T) {
	scheme := testScheme(t)
	cl := newFakeClient(t, scheme)

	task := dueTask(time.Now().Add(-26 * time.Hour))
	task.Spec.State = v1alpha1.TaskStatePaused
	// Stale next run from the enabled period: the first reconcile must clear it.
	task.Status.NextRunTime = &metav1.Time{Time: time.Now().Add(-25 * time.Hour)}
	if err := cl.Create(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := cl.Status().Update(context.Background(), task); err != nil {
		t.Fatalf("seed task status: %v", err)
	}

	r := &ReconcileScheduler{
		Client: cl,
		Cfg:    config.Config{Namespace: ""},
		Runner: &fakeRunner{}, // must not be invoked
	}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: task.Name}}

	var seeded v1alpha1.Task
	if err := cl.Get(context.Background(), req.NamespacedName, &seeded); err != nil {
		t.Fatal(err)
	}

	// First reconcile: the state changed (paused, no next run) -> exactly one
	// write.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var afterFirst v1alpha1.Task
	if err := cl.Get(context.Background(), req.NamespacedName, &afterFirst); err != nil {
		t.Fatal(err)
	}
	if afterFirst.Status.NextRunTime != nil {
		t.Fatalf("paused task still has nextRunTime = %v, want nil", afterFirst.Status.NextRunTime)
	}
	if afterFirst.ResourceVersion == seeded.ResourceVersion {
		t.Fatalf("resourceVersion = %q unchanged on the first reconcile; the dedup assertion below would be vacuous",
			afterFirst.ResourceVersion)
	}

	// Every requeue after that must stay silent: no status write, so
	// resourceVersion must not move again.
	for i := 1; i <= 3; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("requeue %d: %v", i, err)
		}
		var got v1alpha1.Task
		if err := cl.Get(context.Background(), req.NamespacedName, &got); err != nil {
			t.Fatal(err)
		}
		if got.ResourceVersion != afterFirst.ResourceVersion {
			t.Errorf("requeue %d wrote status: resourceVersion = %q, want %q (one write when the state changes, none after)",
				i, got.ResourceVersion, afterFirst.ResourceVersion)
		}
	}
}

// TestMalformedCronWritesStatusOnce verifies the patchNextRun guard: a cron the
// scheduler cannot parse yields no next run, so once the stale NextRunTime has
// been cleared the scheduler must stop writing status on every reconcile.
// resourceVersion is the evidence (the fake client bumps it on a status write).
// The first reconcile is asserted to have bumped it -- it is the one that
// clears the stale value -- so the guard assertion cannot pass vacuously.
func TestMalformedCronWritesStatusOnce(t *testing.T) {
	scheme := testScheme(t)
	cl := newFakeClient(t, scheme)

	task := dueTask(time.Now().Add(-26 * time.Hour))
	task.Spec.Cron = "not-a-cron" // unparseable -> nextDue returns nil
	// Stale next run from the period when the cron was still valid: the first
	// reconcile must clear it, and only it.
	task.Status.NextRunTime = &metav1.Time{Time: time.Now().Add(-25 * time.Hour)}
	if err := cl.Create(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := cl.Status().Update(context.Background(), task); err != nil {
		t.Fatalf("seed task status: %v", err)
	}

	r := &ReconcileScheduler{
		Client: cl,
		Cfg:    config.Config{Namespace: ""},
		Runner: &fakeRunner{}, // must not be invoked
	}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: task.Name}}

	var seeded v1alpha1.Task
	if err := cl.Get(context.Background(), req.NamespacedName, &seeded); err != nil {
		t.Fatal(err)
	}

	// First reconcile: the stale next run is cleared -> exactly one write.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var afterFirst v1alpha1.Task
	if err := cl.Get(context.Background(), req.NamespacedName, &afterFirst); err != nil {
		t.Fatal(err)
	}
	if afterFirst.Status.NextRunTime != nil {
		t.Fatalf("task with a malformed cron still has nextRunTime = %v, want nil", afterFirst.Status.NextRunTime)
	}
	if afterFirst.ResourceVersion == seeded.ResourceVersion {
		t.Fatalf("resourceVersion = %q unchanged on the first reconcile; the guard assertion below would be vacuous",
			afterFirst.ResourceVersion)
	}

	// Nothing left to clear: the guard must stop the write, so resourceVersion
	// must not move again.
	for i := 1; i <= 3; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("requeue %d: %v", i, err)
		}
		var got v1alpha1.Task
		if err := cl.Get(context.Background(), req.NamespacedName, &got); err != nil {
			t.Fatal(err)
		}
		if got.ResourceVersion != afterFirst.ResourceVersion {
			t.Errorf("requeue %d wrote status: resourceVersion = %q, want %q (one write when the state changes, none after)",
				i, got.ResourceVersion, afterFirst.ResourceVersion)
		}
	}

	// A malformed cron never fires.
	var runs v1alpha1.TaskRunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list taskruns: %v", err)
	}
	if len(runs.Items) != 0 {
		t.Errorf("taskruns = %d, want 0 for a malformed cron", len(runs.Items))
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
		Cfg:    config.Config{Namespace: ""},
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
	task.Spec.Cron = ""
	if err := cl.Create(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	r := &ReconcileScheduler{
		Client: cl,
		Cfg:    config.Config{Namespace: ""},
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

	run := NewTaskRun(task, v1alpha1.TaskTriggerCron)

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
	// The run records what the task was called when it ran, so the name
	// survives the Task being renamed or deleted. Without an annotation the CR
	// name is the fallback.
	if got := run.Annotations[v1alpha1.TaskDisplayNameAnnotation]; got != task.Name {
		t.Errorf("display-name annotation = %q, want the CR name %q as fallback", got, task.Name)
	}

	named := dueTask(time.Now())
	named.Annotations = map[string]string{v1alpha1.TaskDisplayNameAnnotation: "每日巡检"}
	namedRun := NewTaskRun(named, v1alpha1.TaskTriggerManual)
	if got := namedRun.Annotations[v1alpha1.TaskDisplayNameAnnotation]; got != "每日巡检" {
		t.Errorf("display-name annotation = %q, want the human name", got)
	}
}

// TestSchedulerSkipsRunWhenInstanceMissing verifies the pre-fire instance
// check: when the owner's cubepilot instance does not exist, the
// scheduler records a Failed TaskRun and does not invoke the runner (issue #26
// AC: failure writes a TaskRun and executes nothing).
func TestSchedulerSkipsRunWhenInstanceMissing(t *testing.T) {
	scheme := testScheme(t)
	runner := &fakeRunner{}
	cl := newFakeClient(t, scheme) // no AgentInstance for the owner

	task := dueTask(time.Now().Add(-2 * time.Minute)) // due if enabled
	if err := cl.Create(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	r := &ReconcileScheduler{
		Client: cl,
		Cfg:    config.Config{Namespace: ""},
		Runner: runner,
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: task.Name},
	}); err != nil {
		// Reconcile swallows fire() errors into logs; surfacing here would mean
		// the missing-instance path regressed.
		t.Fatalf("reconcile: %v", err)
	}

	var runs v1alpha1.TaskRunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list taskruns: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("taskruns = %d, want 1 (skipped run recorded)", len(runs.Items))
	}
	run := runs.Items[0]
	if run.Status.Phase != v1alpha1.TaskRunFailed {
		t.Errorf("phase = %s, want Failed", run.Status.Phase)
	}
	if !strings.Contains(run.Status.Error, "instance") {
		t.Errorf("error = %q, want instance-missing reason", run.Status.Error)
	}
	if runner.gotPrompt != "" {
		t.Errorf("runner invoked with %q despite missing instance", runner.gotPrompt)
	}

	// The due state must advance (LastRunTime/LastStatus) so a cron task does
	// not stay due and re-fire a Failed run every reconcile.
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

// TestSchedulerSkipsRunWhenPromptIsBlank verifies the fail-closed prompt check:
// a Task that resolves to a blank prompt must never hand the runner a turn.
// Both admitted-by-the-schema shapes are covered --
//
//   - a template-bound Task whose template has since been deleted, with no
//     stored instruction (the CEL rule requires one of templateRef/instruction
//     to be *set*, not to be non-blank); the API always stores a rendered
//     snapshot, but a hand-written CR need not -- and
//   - an instruction that is blank only to strings.TrimSpace: the CEL rules
//     match ^\s*$ with RE2's ASCII-only \s, which does not cover U+00A0 and the
//     other Unicode spaces the scheduler trims.
//
// In both cases the run is recorded as Failed with the reason and the runner is
// never invoked.
func TestSchedulerSkipsRunWhenPromptIsBlank(t *testing.T) {
	cases := []struct {
		name        string
		templateRef string
		instruction string
		wantReason  string
	}{
		{
			name:        "templateRef does not resolve and no instruction is stored",
			templateRef: "deleted-template",
			wantReason:  `template "deleted-template" did not resolve`,
		},
		{
			// U+00A0: blank to strings.TrimSpace, not to the CRD's \s.
			name:        "instruction is only a non-breaking space",
			instruction: "\u00a0", // non-breaking space
			wantReason:  "the inline instruction is blank",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := testScheme(t)
			runner := &fakeRunner{}
			cl := newFakeClient(t, scheme, readyInstance("zhang.wei"))

			task := dueTask(time.Now().Add(-2 * time.Minute)) // due if enabled
			task.Spec.TemplateRef = tc.templateRef
			task.Spec.Instruction = tc.instruction
			if err := cl.Create(ctx, task); err != nil {
				t.Fatalf("create task: %v", err)
			}

			r := &ReconcileScheduler{
				Client: cl,
				Cfg:    config.Config{Namespace: ""},
				Runner: runner,
			}
			if _, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: task.Name},
			}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			// No turn ran at all: counting the calls is what makes this
			// non-vacuous (an empty gotPrompt would pass even if the runner had
			// been invoked with the blank prompt).
			if runner.calls != 0 {
				t.Errorf("runner ran %d turn(s) with prompt %q, want 0", runner.calls, runner.gotPrompt)
			}

			var runs v1alpha1.TaskRunList
			if err := cl.List(ctx, &runs); err != nil {
				t.Fatalf("list taskruns: %v", err)
			}
			if len(runs.Items) != 1 {
				t.Fatalf("taskruns = %d, want 1 (skipped run recorded)", len(runs.Items))
			}
			run := runs.Items[0]
			if run.Status.Phase != v1alpha1.TaskRunFailed {
				t.Errorf("phase = %s, want Failed", run.Status.Phase)
			}
			if !strings.Contains(run.Status.Error, "no instruction to run") {
				t.Errorf("error = %q, want a missing-instruction reason", run.Status.Error)
			}
			if !strings.Contains(run.Status.Error, tc.wantReason) {
				t.Errorf("error = %q, want it to say %q", run.Status.Error, tc.wantReason)
			}

			// The skipped occurrence still advances the task's due state, so a
			// cron task does not re-fire (and re-fail) every reconcile.
			var got v1alpha1.Task
			if err := cl.Get(ctx, types.NamespacedName{Name: task.Name}, &got); err != nil {
				t.Fatal(err)
			}
			if got.Status.LastTaskRunName != run.Name {
				t.Errorf("task.lastTaskRunName = %q, want %q", got.Status.LastTaskRunName, run.Name)
			}
			if got.Status.LastStatus != "failed" {
				t.Errorf("task.lastStatus = %q, want failed", got.Status.LastStatus)
			}
		})
	}
}

// probeRunner reads the Task's status from inside the turn. The ordering under
// test -- the Task already naming the run that is executing -- must be observed
// while it is true: a read taken after the run completes cannot tell the two
// orderings apart.
type probeRunner struct {
	cl       client.Client
	taskName string
	calls    int
	observed string
	readErr  error
}

func (f *probeRunner) RunTask(ctx context.Context, creator, sessionKey, prompt string) (string, error) {
	f.calls++
	var task v1alpha1.Task
	if err := f.cl.Get(ctx, types.NamespacedName{Name: f.taskName}, &task); err != nil {
		f.readErr = err
		return "", err
	}
	f.observed = task.Status.LastTaskRunName
	return "### P2 -- checked during the run\n", nil
}

// TestTaskNamesTheRunInProgress verifies lastTaskRunName is set when the run is
// created, not when it finishes: the field is documented as "the most recent
// TaskRun created for this Task", and the Portal's report picker preselects
// lastRunId while the run is in flight -- a value only written at completion
// makes the picker open the *previous* report for the whole duration.
func TestTaskNamesTheRunInProgress(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	cl := newFakeClient(t, scheme, readyInstance("zhang.wei"))

	task := dueTask(time.Now().Add(-2 * time.Minute)) // due if enabled
	// The state the bug showed up in: a task that already ran, so the field
	// names an older run before this fire. That makes "names the run in
	// progress" distinguishable from "still names the previous run".
	const previousRun = "zhang-wei-daily-inspection-20200101-000000"
	task.Status.LastTaskRunName = previousRun
	if err := cl.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := cl.Status().Update(ctx, task); err != nil {
		t.Fatalf("seed task status: %v", err)
	}
	tpl := &v1alpha1.TaskTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "daily-inspection"},
		Spec: v1alpha1.TaskTemplateSpec{
			DisplayName: "Daily cluster inspection",
			Instruction: "Inspect the cluster read-only",
		},
	}
	if err := cl.Create(ctx, tpl); err != nil {
		t.Fatalf("create template: %v", err)
	}

	runner := &probeRunner{cl: cl, taskName: task.Name}
	r := &ReconcileScheduler{
		Client: cl,
		Cfg:    config.Config{Namespace: ""},
		Runner: runner,
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: task.Name},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
	if runner.readErr != nil {
		t.Fatalf("reading the task during the run: %v", runner.readErr)
	}

	var runs v1alpha1.TaskRunList
	if err := cl.List(ctx, &runs); err != nil {
		t.Fatalf("list taskruns: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("taskruns = %d, want 1", len(runs.Items))
	}
	run := runs.Items[0]
	if runner.observed != run.Name {
		t.Errorf("lastTaskRunName during the run = %q, want the run in progress %q (previous run was %q)",
			runner.observed, run.Name, previousRun)
	}
	if runner.observed == previousRun {
		t.Errorf("lastTaskRunName during the run still names the previous run %q", previousRun)
	}
}

// TestFailedStartPatchStillNamesTheRunAtTheEnd covers the failure path of the
// start patch: if the status write that records lastTaskRunName fails (a
// transient conflict, say) while execution continues, the Task must still end
// the run naming it.
//
// The bug it pins is the baseline of the finish patch. Taking that baseline from
// the local Task -- which the start patch already mutated -- leaves the field
// out of the finish patch's diff, so nothing ever writes it and the Task keeps
// pointing at the previous run even after the new run completed: the symptom the
// start patch exists to remove, reintroduced by a failure path. Sharing one
// baseline (taken before the mutation) makes the finish patch re-carry the field
// and repair the failed start.
func TestFailedStartPatchStillNamesTheRunAtTheEnd(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)

	// The state the bug showed up in: a Task that already ran, so the field
	// names an older run before this fire -- which makes "the finish patch
	// repaired it" distinguishable from "it was never wrong".
	const previousRun = "zhang-wei-daily-inspection-20200101-000000"

	task := dueTask(time.Now().Add(-2 * time.Minute)) // due if enabled
	task.Status.LastTaskRunName = previousRun
	cl := testClientBuilder(scheme, readyInstance("zhang.wei")).Build()
	if err := cl.Create(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := cl.Status().Update(ctx, task); err != nil {
		t.Fatalf("seed task status: %v", err)
	}
	tpl := &v1alpha1.TaskTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "daily-inspection"},
		Spec: v1alpha1.TaskTemplateSpec{
			DisplayName: "Daily cluster inspection",
			Instruction: "Inspect the cluster read-only",
		},
	}
	if err := cl.Create(ctx, tpl); err != nil {
		t.Fatalf("create template: %v", err)
	}

	// Fail the first Task status patch that writes the run name -- the start
	// patch -- and let everything else through. The name is what identifies it:
	// the scheduler's other Task status writes (nextRunTime, the outcome) never
	// carry the field.
	failedStartPatches := 0
	intercepted := interceptor.NewClient(cl, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if subResource != "status" {
				return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
			}
			task, isTask := obj.(*v1alpha1.Task)
			if !isTask {
				return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
			}
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			if failedStartPatches == 0 && strings.Contains(string(data), "lastTaskRunName") {
				failedStartPatches++
				return apierrors.NewConflict(schema.GroupResource{Group: "ai.cubestack.io", Resource: "tasks"},
					task.Name, errors.New("simulated transient conflict on the start patch"))
			}
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	})

	// The runner observes the Task from inside the turn: with the start patch
	// failed, that read must still show the previous run -- which is what makes
	// the final assertion evidence that the finish patch repaired the field,
	// rather than a start patch that quietly worked.
	runner := &probeRunner{cl: intercepted, taskName: task.Name}
	r := &ReconcileScheduler{
		Client: intercepted,
		Cfg:    config.Config{Namespace: ""},
		Runner: runner,
	}
	if _, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: task.Name},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if failedStartPatches != 1 {
		t.Fatalf("start patches failed = %d, want exactly 1 injected: the failure path under test was not exercised", failedStartPatches)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1 (a failed status patch must not stop execution)", runner.calls)
	}
	if runner.readErr != nil {
		t.Fatalf("reading the task during the run: %v", runner.readErr)
	}
	if runner.observed != previousRun {
		t.Errorf("lastTaskRunName during the run = %q, want the previous run %q: the start patch was supposed to fail", runner.observed, previousRun)
	}

	var runs v1alpha1.TaskRunList
	if err := cl.List(ctx, &runs); err != nil {
		t.Fatalf("list taskruns: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("taskruns = %d, want 1", len(runs.Items))
	}
	run := runs.Items[0]
	if run.Status.Phase != v1alpha1.TaskRunCompleted {
		t.Fatalf("phase = %s, want Completed (the turn itself succeeded)", run.Status.Phase)
	}

	var got v1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Name: task.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.LastTaskRunName != run.Name {
		t.Errorf("task.lastTaskRunName = %q after the run, want %q (previous run was %q): the finish patch did not re-carry the field, so a failed start patch left it stale",
			got.Status.LastTaskRunName, run.Name, previousRun)
	}
	if got.Status.LastStatus != "success" {
		t.Errorf("task.lastStatus = %q, want success", got.Status.LastStatus)
	}
	if got.Status.LastRunTime == nil {
		t.Error("task.lastRunTime not recorded: the finish patch did not land")
	}
}
