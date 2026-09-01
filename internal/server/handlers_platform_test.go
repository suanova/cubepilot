package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/instances"
	"github.com/suanova/cubepilot/internal/store"
)

// platformTestServer builds a Server with a fake client (status subresources
// enabled for Model/TaskRun so patchStatus-style writes behave) and the
// platform scheme registered.
func platformTestServer(t *testing.T, objs ...client.Object) *Server {
	t.Helper()
	return platformTestServerStore(t, nil, objs...)
}

// platformTestServerStore is platformTestServer with a JSON store attached, so
// endpoints that persist agent config (PUT /api/agent/config) can be tested.
func platformTestServerStore(t *testing.T, st *store.Store, objs ...client.Object) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add platform types: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.TaskRun{}, &v1alpha1.AgentInstance{}, &v1alpha1.Task{}).
		WithObjects(objs...).
		Build()
	cfg := config.Config{DefaultUser: "zhang.wei"}
	mgr := instances.New(cl, cfg)
	return New(cfg, mgr, st, nil, cl)
}

// platformTestServerSkillsDir is platformTestServerStore with a skill repo
// dir configured (for the internal skill tar endpoint).
func platformTestServerSkillsDir(t *testing.T, skillsDir string, objs ...client.Object) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add platform types: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.TaskRun{}, &v1alpha1.AgentInstance{}, &v1alpha1.Task{}, &v1alpha1.Skill{}).
		WithObjects(objs...).
		Build()
	cfg := config.Config{DefaultUser: "zhang.wei", SkillsDir: skillsDir}
	mgr := instances.New(cl, cfg)
	return New(cfg, mgr, nil, nil, cl)
}

func doReq(t *testing.T, h http.Handler, method, path, user string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if user != "" {
		req.Header.Set("X-CubePilot-User", user)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return v
}

// TestHandleInstancesOwnerScoped verifies GET /api/instances returns only the
// caller's own instances -- even with a ?user= filter naming someone else.
func TestHandleInstancesOwnerScoped(t *testing.T) {
	li := &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "li-ming-agent-for-cloud"},
		Spec:       v1alpha1.AgentInstanceSpec{Owner: "li.ming", TemplateRef: "agent-for-cloud"},
	}
	wang := &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "wang-wu-agent-for-cloud"},
		Spec:       v1alpha1.AgentInstanceSpec{Owner: "wang.wu", TemplateRef: "agent-for-cloud"},
	}
	s := platformTestServer(t, li, wang)

	// Filter naming another user must still return only the caller's own.
	rec := doReq(t, s.Handler(), http.MethodGet, "/api/instances?user=wang.wu", "li.ming", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := decode[struct {
		Instances []v1alpha1.AgentInstance `json:"instances"`
	}](t, rec)
	if len(got.Instances) != 1 || got.Instances[0].Name != "li-ming-agent-for-cloud" {
		t.Errorf("li.ming sees %+v, want only own instance", got.Instances)
	}

	// No filter: same result.
	rec = doReq(t, s.Handler(), http.MethodGet, "/api/instances", "li.ming", nil)
	got = decode[struct {
		Instances []v1alpha1.AgentInstance `json:"instances"`
	}](t, rec)
	if len(got.Instances) != 1 || got.Instances[0].Name != "li-ming-agent-for-cloud" {
		t.Errorf("li.ming sees %+v, want only own instance", got.Instances)
	}
}

// TestHandleInstancesCreate verifies POST /api/instances provisions the
// caller's own instance (owner forced server-side), is idempotent, and
// rejects unknown agent refs.
func TestHandleInstancesCreate(t *testing.T) {
	agent := &v1alpha1.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "agent-for-cloud"}}
	s := platformTestServer(t, agent)
	h := s.Handler()

	// Create.
	rec := doReq(t, h, http.MethodPost, "/api/instances", "wang.wu", map[string]any{"templateRef": "agent-for-cloud"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	created := decode[struct {
		Instance v1alpha1.AgentInstance `json:"instance"`
	}](t, rec)
	if created.Instance.Spec.Owner != "wang.wu" {
		t.Errorf("owner = %q, want wang.wu (forced from caller)", created.Instance.Spec.Owner)
	}

	// Idempotent repeat.
	rec = doReq(t, h, http.MethodPost, "/api/instances", "wang.wu", map[string]any{"templateRef": "agent-for-cloud"})
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := decode[struct {
		AlreadyExists bool `json:"alreadyExists"`
	}](t, rec)
	if !got.AlreadyExists {
		t.Error("repeat create did not report alreadyExists")
	}

	// Unknown template ref is rejected (no permanently-Failed instance).
	rec = doReq(t, h, http.MethodPost, "/api/instances", "li.ming", map[string]any{"templateRef": "no-such-template"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown template status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleTasksOwnerScoped verifies task list and mutations are scoped to
// the owner (design §3.5: Task carries its owner; same isolation as
// instances).
func TestHandleTasksOwnerScoped(t *testing.T) {
	liTask := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "li-ming-task-abc"},
		Spec: v1alpha1.TaskSpec{
			Owner:   "li.ming",
			Trigger: v1alpha1.TaskTriggerManual,
			State:   v1alpha1.TaskStateEnabled,
		},
	}
	wangTask := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "wang-wu-task-def"},
		Spec: v1alpha1.TaskSpec{
			Owner:   "wang.wu",
			Trigger: v1alpha1.TaskTriggerManual,
			State:   v1alpha1.TaskStateEnabled,
		},
	}
	s := platformTestServer(t, liTask, wangTask)
	h := s.Handler()

	rec := doReq(t, h, http.MethodGet, "/api/tasks", "li.ming", nil)
	got := decode[struct {
		Tasks []taskDTO `json:"tasks"`
	}](t, rec)
	if len(got.Tasks) != 1 || got.Tasks[0].ID != "li-ming-task-abc" {
		t.Errorf("li.ming sees %d tasks (%v), want only own", len(got.Tasks), got.Tasks)
	}

	// Delete another user's task -> 403.
	rec = doReq(t, h, http.MethodDelete, "/api/tasks/wang-wu-task-def", "li.ming", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-user delete status = %d, want 403: %s", rec.Code, rec.Body.String())
	}

	// Run another user's task -> 403.
	rec = doReq(t, h, http.MethodPost, "/api/tasks/wang-wu-task-def/run", "li.ming", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-user run status = %d, want 403: %s", rec.Code, rec.Body.String())
	}

	// Reports of another user's task -> 403.
	rec = doReq(t, h, http.MethodGet, "/api/tasks/wang-wu-task-def/reports", "li.ming", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-user reports status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleTaskRunsOwnerScoped verifies taskrun listing excludes other
// users' runs.
func TestHandleTaskRunsOwnerScoped(t *testing.T) {
	liRun := &v1alpha1.TaskRun{
		ObjectMeta: metav1.ObjectMeta{Name: "li-ming-task-abc-20260820-020000"},
		Spec:       v1alpha1.TaskRunSpec{Owner: "li.ming", CreatorTaskRef: v1alpha1.TaskRef{Name: "li-ming-task-abc"}},
	}
	wangRun := &v1alpha1.TaskRun{
		ObjectMeta: metav1.ObjectMeta{Name: "wang-wu-task-def-20260820-020000"},
		Spec:       v1alpha1.TaskRunSpec{Owner: "wang.wu", CreatorTaskRef: v1alpha1.TaskRef{Name: "wang-wu-task-def"}},
	}
	s := platformTestServer(t, liRun, wangRun)

	rec := doReq(t, s.Handler(), http.MethodGet, "/api/taskruns", "li.ming", nil)
	got := decode[struct {
		TaskRuns []v1alpha1.TaskRun `json:"taskruns"`
	}](t, rec)
	if len(got.TaskRuns) != 1 || got.TaskRuns[0].Name != "li-ming-task-abc-20260820-020000" {
		t.Errorf("li.ming sees %d runs (%v), want only own", len(got.TaskRuns), got.TaskRuns)
	}

	// Single-run fetch of another user's run -> 403.
	rec = doReq(t, s.Handler(), http.MethodGet, "/api/taskruns/wang-wu-task-def-20260820-020000", "li.ming", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-user taskrun status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}

// TestInstallSkill verifies POST /api/skills/{name}/install: appends on a
// non-empty set, is a no-op on the all-enabled baseline, and rejects unknown /
// unreachable skills and unprovisioned users.
func TestInstallSkill(t *testing.T) {
	li := internalTestInstance("li.ming", v1alpha1.DefaultAgentName)
	li.Spec.EnabledSkills = []string{"harbor"}
	s := platformTestServer(t, li,
		internalTestCap("harbor", "skills/harbor/v1.tar.gz"),
		internalTestCap("scan", "skills/scan/v1.tar.gz"))

	// Append to a non-empty set.
	rec := doReq(t, s.Handler(), http.MethodPost, "/api/skills/scan/install", "li.ming", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("install: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	got := decode[struct {
		EnabledSkills []string `json:"enabledSkills"`
	}](t, rec)
	if len(got.EnabledSkills) != 2 || got.EnabledSkills[0] != "harbor" || got.EnabledSkills[1] != "scan" {
		t.Errorf("enabledSkills = %v, want [harbor scan]", got.EnabledSkills)
	}
	var inst v1alpha1.AgentInstance
	if err := s.cr.Get(t.Context(), client.ObjectKey{Name: li.Name}, &inst); err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if len(inst.Spec.EnabledSkills) != 2 || inst.Spec.EnabledSkills[1] != "scan" {
		t.Errorf("instance enabledSkills = %v, want [harbor scan]", inst.Spec.EnabledSkills)
	}

	// Idempotent re-install.
	rec = doReq(t, s.Handler(), http.MethodPost, "/api/skills/scan/install", "li.ming", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-install: status = %d", rec.Code)
	}
	again := decode[struct {
		EnabledSkills []string `json:"enabledSkills"`
	}](t, rec)
	if len(again.EnabledSkills) != 2 {
		t.Errorf("re-install changed the set: %v", again.EnabledSkills)
	}

	// All-enabled baseline (empty set): installing is a no-op that stays empty.
	s2 := platformTestServer(t, internalTestInstance("zhang.wei", v1alpha1.DefaultAgentName),
		internalTestCap("harbor", "skills/harbor/v1.tar.gz"))
	rec = doReq(t, s2.Handler(), http.MethodPost, "/api/skills/harbor/install", "zhang.wei", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("baseline install: status = %d", rec.Code)
	}
	base := decode[struct {
		EnabledSkills []string `json:"enabledSkills"`
	}](t, rec)
	if len(base.EnabledSkills) != 0 {
		t.Errorf("baseline install should stay empty (all-enabled): %v", base.EnabledSkills)
	}

	// Unknown skill -> 404.
	rec = doReq(t, s.Handler(), http.MethodPost, "/api/skills/nope/install", "li.ming", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown skill status = %d, want 404", rec.Code)
	}

	// A case-variant identity resolves the same (sanitized/lowercased) instance
	// name but does not own it -> 403.
	liUpper := internalTestInstance("li.ming", v1alpha1.DefaultAgentName)
	liUpper.Spec.Owner = "LI.MING"
	sUpper := platformTestServer(t, liUpper, internalTestCap("harbor", "skills/harbor/v1.tar.gz"))
	rec = doReq(t, sUpper.Handler(), http.MethodPost, "/api/skills/harbor/install", "li.ming", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("case-variant owner status = %d, want 403", rec.Code)
	}

	// Unreachable skill -> 409.
	unreach := internalTestCap("broken", "skills/broken/v1.tar.gz")
	unreach.Status.Phase = v1alpha1.SkillPhaseUnreachable
	s3 := platformTestServer(t, li, unreach)
	rec = doReq(t, s3.Handler(), http.MethodPost, "/api/skills/broken/install", "li.ming", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("unreachable skill status = %d, want 409", rec.Code)
	}

	// No instance -> 409.
	s4 := platformTestServer(t, internalTestCap("harbor", "skills/harbor/v1.tar.gz"))
	rec = doReq(t, s4.Handler(), http.MethodPost, "/api/skills/harbor/install", "nobody", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("no-instance status = %d, want 409", rec.Code)
	}
}

// TestUninstallSkill verifies POST /api/skills/{name}/uninstall: removes from a
// non-empty set, materializes the allow-list from the all-enabled baseline, is
// idempotent, and rejects unprovisioned users.
func TestUninstallSkill(t *testing.T) {
	li := internalTestInstance("li.ming", v1alpha1.DefaultAgentName)
	li.Spec.EnabledSkills = []string{"harbor", "scan"}
	s := platformTestServer(t, li,
		internalTestCap("harbor", "skills/harbor/v1.tar.gz"),
		internalTestCap("scan", "skills/scan/v1.tar.gz"))

	// Remove one from a non-empty set.
	rec := doReq(t, s.Handler(), http.MethodPost, "/api/skills/scan/uninstall", "li.ming", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("uninstall: status = %d", rec.Code)
	}
	got := decode[struct {
		EnabledSkills []string `json:"enabledSkills"`
	}](t, rec)
	if len(got.EnabledSkills) != 1 || got.EnabledSkills[0] != "harbor" {
		t.Errorf("enabledSkills = %v, want [harbor]", got.EnabledSkills)
	}

	// Idempotent re-uninstall of an absent skill.
	rec = doReq(t, s.Handler(), http.MethodPost, "/api/skills/scan/uninstall", "li.ming", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-uninstall: status = %d", rec.Code)
	}

	// All-enabled baseline: uninstall materializes the visible skills minus the
	// one, so a later publish does not re-enable it.
	s2 := platformTestServer(t, internalTestInstance("zhang.wei", v1alpha1.DefaultAgentName),
		internalTestCap("harbor", "skills/harbor/v1.tar.gz"),
		internalTestCap("scan", "skills/scan/v1.tar.gz"))
	rec = doReq(t, s2.Handler(), http.MethodPost, "/api/skills/harbor/uninstall", "zhang.wei", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("baseline uninstall: status = %d", rec.Code)
	}
	base := decode[struct {
		EnabledSkills []string `json:"enabledSkills"`
	}](t, rec)
	if len(base.EnabledSkills) != 1 || base.EnabledSkills[0] != "scan" {
		t.Errorf("materialized = %v, want [scan]", base.EnabledSkills)
	}

	// Unknown skill -> 404 (rejected before any allow-list materialization).
	rec = doReq(t, s.Handler(), http.MethodPost, "/api/skills/nope/uninstall", "li.ming", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown skill uninstall status = %d, want 404", rec.Code)
	}

	// No instance -> 409.
	s3 := platformTestServer(t, internalTestCap("harbor", "skills/harbor/v1.tar.gz"))
	rec = doReq(t, s3.Handler(), http.MethodPost, "/api/skills/harbor/uninstall", "nobody", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("no-instance status = %d, want 409", rec.Code)
	}
}
