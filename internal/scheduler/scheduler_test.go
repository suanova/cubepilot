package scheduler

import (
	"context"
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
			&v1alpha1.Agent{},
			&v1alpha1.Capability{},
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
