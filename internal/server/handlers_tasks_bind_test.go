package server

import (
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// inspectionTemplate returns the builtin daily-inspection preset shape used in
// binding tests (paramsSchema scope + defaultCron).
func inspectionTemplate() *v1alpha1.TaskTemplate {
	return &v1alpha1.TaskTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "daily-inspection"},
		Spec: v1alpha1.TaskTemplateSpec{
			DisplayName: "Daily Cluster Inspection",
			Instruction: "Inspect the cluster read-only, scope {{scope}}",
			DefaultCron: "0 2 * * *",
			ParamsSchema: []v1alpha1.ParamSchema{
				{Name: "scope", Type: "string", Default: "all", Enum: []string{"all", "node-pool", "project"}},
			},
		},
	}
}

// TestHandleTaskTemplatesList verifies GET /api/tasktemplates returns the
// TaskTemplate registry (no owner scoping -- cluster-scoped catalog).
func TestHandleTaskTemplatesList(t *testing.T) {
	s := platformTestServer(t, inspectionTemplate())

	rec := doReq(t, s.Handler(), http.MethodGet, "/api/tasktemplates", "zhang.wei", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/tasktemplates = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	got := decode[struct {
		TaskTemplates []v1alpha1.TaskTemplate `json:"taskTemplates"`
	}](t, rec)
	if len(got.TaskTemplates) != 1 || got.TaskTemplates[0].Name != "daily-inspection" {
		t.Fatalf("taskTemplates = %+v, want [daily-inspection]", got.TaskTemplates)
	}
}

// TestCreateTaskBindsTemplate verifies POST /api/tasks with templateRef stores
// the binding: params merged/rendered into the instruction snapshot, schedule
// defaulted from the template's defaultCron, trigger Cron.
func TestCreateTaskBindsTemplate(t *testing.T) {
	s := platformTestServer(t, inspectionTemplate())

	rec := doReq(t, s.Handler(), http.MethodPost, "/api/tasks", "zhang.wei", map[string]any{
		"name":        "My Daily Inspection",
		"templateRef": "daily-inspection",
		"params":      map[string]string{"scope": "node-pool"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	got := decode[struct {
		Task taskDTO `json:"task"`
	}](t, rec).Task
	if got.TemplateRef != "daily-inspection" {
		t.Errorf("templateRef = %q, want daily-inspection", got.TemplateRef)
	}
	if !strings.Contains(got.Prompt, "node-pool") || strings.Contains(got.Prompt, "{{scope}}") {
		t.Errorf("prompt = %q, want rendered scope=node-pool", got.Prompt)
	}
	if got.Schedule != "0 2 * * *" {
		t.Errorf("schedule = %q, want template default 0 2 * * *", got.Schedule)
	}
}

// TestCreateTaskBindsTemplateDefaults verifies that with no explicit params the
// template's paramSchema defaults are applied, and an explicit schedule wins
// over the template's defaultCron.
func TestCreateTaskBindsTemplateDefaults(t *testing.T) {
	s := platformTestServer(t, inspectionTemplate())

	// No params -> scope defaults to "all"; explicit schedule wins.
	rec := doReq(t, s.Handler(), http.MethodPost, "/api/tasks", "zhang.wei", map[string]any{
		"name":        "Defaulted Inspection",
		"templateRef": "daily-inspection",
		"schedule":    "30 6 * * 1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	got := decode[struct {
		Task taskDTO `json:"task"`
	}](t, rec).Task
	if !strings.Contains(got.Prompt, "scope all") {
		t.Errorf("prompt = %q, want defaulted scope=all", got.Prompt)
	}
	if got.Schedule != "30 6 * * 1" {
		t.Errorf("schedule = %q, want explicit 30 6 * * 1", got.Schedule)
	}
}

// TestCreateTaskManualTemplateNotDefaulted verifies that an explicit empty
// schedule (Manual) on a template-bound task stays Manual -- only an omitted
// schedule receives the template's defaultCron fallback.
func TestCreateTaskManualTemplateNotDefaulted(t *testing.T) {
	s := platformTestServer(t, inspectionTemplate())

	rec := doReq(t, s.Handler(), http.MethodPost, "/api/tasks", "zhang.wei", map[string]any{
		"name":        "Manual inspection",
		"templateRef": "daily-inspection",
		"schedule":    "", // explicit empty == Manual
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	got := decode[struct {
		Task taskDTO `json:"task"`
	}](t, rec).Task
	if got.TemplateRef != "daily-inspection" {
		t.Errorf("templateRef = %q, want daily-inspection", got.TemplateRef)
	}
	if got.Schedule != "" {
		t.Errorf("schedule = %q, want empty (Manual must not inherit defaultCron)", got.Schedule)
	}
}

// TestCreateTaskFreeFormStillWorks verifies the no-template path is unchanged:
// name + prompt creates an inline Manual task with no templateRef.
func TestCreateTaskFreeFormStillWorks(t *testing.T) {
	s := platformTestServer(t)

	rec := doReq(t, s.Handler(), http.MethodPost, "/api/tasks", "zhang.wei", map[string]any{
		"name":   "Freeform task",
		"prompt": "Check pod health",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	got := decode[struct {
		Task taskDTO `json:"task"`
	}](t, rec).Task
	if got.TemplateRef != "" {
		t.Errorf("templateRef = %q, want empty for free-form", got.TemplateRef)
	}
	if got.Prompt != "Check pod health" {
		t.Errorf("prompt = %q, want free-form text preserved", got.Prompt)
	}
}

// TestCreateTaskRejectsBadBindings verifies the templateRef validation paths:
// unknown template, unknown param key, and params without a template.
func TestCreateTaskRejectsBadBindings(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{name: "unknown template", body: map[string]any{"name": "x", "templateRef": "nope"}},
		{name: "unknown param key", body: map[string]any{"name": "x", "templateRef": "daily-inspection", "params": map[string]string{"bogus": "1"}}},
		{name: "params without template", body: map[string]any{"name": "x", "prompt": "p", "params": map[string]string{"scope": "all"}}},
		{name: "whitespace-only templateRef", body: map[string]any{"name": "x", "templateRef": "   "}},
		{name: "non-enum param value", body: map[string]any{"name": "x", "templateRef": "daily-inspection", "params": map[string]string{"scope": "unapproved"}}},
	}
	s := platformTestServer(t, inspectionTemplate())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, s.Handler(), http.MethodPost, "/api/tasks", "zhang.wei", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
		})
	}
}
