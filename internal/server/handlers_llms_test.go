package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/controller"
)

func addLLMTestServer(t *testing.T, objs ...client.Object) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add platform types: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core types: %v", err)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Server{cfg: config.Config{Namespace: "cubepilot"}, cr: cl}
}

func TestHandleAddLLM(t *testing.T) {
	builtin := controller.BuiltinAgentTemplate("https://api.deepseek.com", "deepseek-v4-flash")
	builtin.Namespace = "cubepilot"
	s := addLLMTestServer(t, builtin)

	body := bytes.NewBufferString(`{"name":"My Qwen","endpoint":"https://api.example.com/v1","apiKey":"sk-2"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/llms", body)
	w := httptest.NewRecorder()
	s.handleAddLLM(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	// Model appended to the builtin template.
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if len(tmpl.Spec.Models) != 2 || tmpl.Spec.Models[1].Name != "my-qwen" {
		t.Fatalf("models = %+v", tmpl.Spec.Models)
	}
	if tmpl.Spec.Models[1].CredentialRef.Name != "llm-my-qwen" {
		t.Errorf("credentialRef = %q", tmpl.Spec.Models[1].CredentialRef.Name)
	}
	// Credential Secret created.
	var sec corev1.Secret
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "llm-my-qwen"}, &sec); err != nil {
		t.Fatalf("credential Secret: %v", err)
	}
	if string(sec.Data["apiKey"]) != "sk-2" {
		t.Errorf("apiKey = %q", sec.Data["apiKey"])
	}
}

func TestHandleAddLLMPublicNoKey(t *testing.T) {
	builtin := controller.BuiltinAgentTemplate("https://api.deepseek.com", "deepseek-v4-flash")
	builtin.Namespace = "cubepilot"
	s := addLLMTestServer(t, builtin)

	body := bytes.NewBufferString(`{"name":"local-ollama","endpoint":"http://localhost:11434/v1","public":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/llms", body)
	w := httptest.NewRecorder()
	s.handleAddLLM(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		t.Fatalf("get template: %v", err)
	}
	m := tmpl.Spec.Models[len(tmpl.Spec.Models)-1]
	if m.CredentialRef != nil {
		t.Errorf("public model should carry no credentialRef: %+v", m)
	}
	// No Secret created for a public model.
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "llm-local-ollama"}, &corev1.Secret{}); err == nil {
		t.Error("public model should not create a Secret")
	}
}

// TestHandleAddLLMNormalizesRequestURL covers the reported incident: an endpoint
// copied from a working curl command ends in /chat/completions, but the OpenAI
// SDK treats baseUrl as the API root and appends that suffix itself, so the
// request 404s on a doubled path. Only that suffix is stripped -- a root
// endpoint is correct for some providers, so /v1 is never added.
func TestHandleAddLLMNormalizesRequestURL(t *testing.T) {
	builtin := controller.BuiltinAgentTemplate("https://api.deepseek.com", "deepseek-v4-flash")
	builtin.Namespace = "cubepilot"
	s := addLLMTestServer(t, builtin)

	body := bytes.NewBufferString(`{"name":"qwen","endpoint":"https://api.example.com/v1/chat/completions/","apiKey":"sk-1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/llms", body)
	w := httptest.NewRecorder()
	s.handleAddLLM(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if got := tmpl.Spec.Models[len(tmpl.Spec.Models)-1].Endpoint; got != "https://api.example.com/v1" {
		t.Errorf("endpoint = %q, want https://api.example.com/v1", got)
	}
}

// TestHandleAddLLMRejectsKeylessWithoutPublicFlag pins the guard that the
// reported public-model failure needed: a model with no credential is stored
// only when the request declares it public. Otherwise it would be saved, be
// selectable, and fail every turn with "No API key resolved".
func TestHandleAddLLMRejectsKeylessWithoutPublicFlag(t *testing.T) {
	builtin := controller.BuiltinAgentTemplate("https://api.deepseek.com", "deepseek-v4-flash")
	builtin.Namespace = "cubepilot"
	s := addLLMTestServer(t, builtin)

	body := bytes.NewBufferString(`{"name":"forgot-the-key","endpoint":"https://api.example.com/v1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/llms", body)
	w := httptest.NewRecorder()
	s.handleAddLLM(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if len(tmpl.Spec.Models) != 1 {
		t.Errorf("nothing should be written on a rejected add: %+v", tmpl.Spec.Models)
	}
}

// TestHandleAddLLMRejectsPublicWithKey rejects a contradictory request rather
// than silently picking one of the two meanings.
func TestHandleAddLLMRejectsPublicWithKey(t *testing.T) {
	builtin := controller.BuiltinAgentTemplate("https://api.deepseek.com", "deepseek-v4-flash")
	builtin.Namespace = "cubepilot"
	s := addLLMTestServer(t, builtin)

	body := bytes.NewBufferString(`{"name":"both","endpoint":"https://api.example.com/v1","apiKey":"sk-1","public":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/llms", body)
	w := httptest.NewRecorder()
	s.handleAddLLM(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
}

// llmTestServer builds a server whose builtin template carries the given extra
// models on top of the platform default.
func llmTestServer(t *testing.T, models ...v1alpha1.TemplateModelSpec) *Server {
	t.Helper()
	builtin := controller.BuiltinAgentTemplate("https://api.deepseek.com", "deepseek-v4-flash")
	builtin.Namespace = "cubepilot"
	builtin.Spec.Models = append(builtin.Spec.Models, models...)
	return addLLMTestServer(t, builtin)
}

func keyedModel(name, endpoint string) v1alpha1.TemplateModelSpec {
	return v1alpha1.TemplateModelSpec{
		Name:          name,
		Endpoint:      endpoint,
		CredentialRef: &corev1.LocalObjectReference{Name: "llm-" + name},
	}
}

func putLLM(t *testing.T, s *Server, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/llms/"+name, bytes.NewBufferString(body))
	req.SetPathValue("name", name)
	w := httptest.NewRecorder()
	s.handleLLMByName(w, req)
	return w
}

func templateModels(t *testing.T, s *Server) []v1alpha1.TemplateModelSpec {
	t.Helper()
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		t.Fatalf("get template: %v", err)
	}
	return tmpl.Spec.Models
}

// TestHandleUpdateLLMEndpointKeepsKey is the "fix a typo'd endpoint" journey:
// the client never receives the apiKey, so a blank one must keep the stored
// credential rather than wipe or replace it.
func TestHandleUpdateLLMEndpointKeepsKey(t *testing.T) {
	s := llmTestServer(t, keyedModel("my-qwen", "https://api.example.com/v1"))
	if err := upsertLLMCredential(context.Background(), s, "llm-my-qwen", "sk-old"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	w := putLLM(t, s, "my-qwen", `{"endpoint":"https://other.example.com/v1/chat/completions"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	models := templateModels(t, s)
	m := models[len(models)-1]
	if m.Endpoint != "https://other.example.com/v1" {
		t.Errorf("endpoint = %q, want the normalized root", m.Endpoint)
	}
	if m.CredentialRef == nil || m.CredentialRef.Name != "llm-my-qwen" {
		t.Fatalf("credentialRef should be kept: %+v", m.CredentialRef)
	}
	var sec corev1.Secret
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "llm-my-qwen"}, &sec); err != nil {
		t.Fatalf("credential Secret: %v", err)
	}
	if string(sec.Data["apiKey"]) != "sk-old" {
		t.Errorf("apiKey = %q, want the unchanged sk-old", sec.Data["apiKey"])
	}
}

// TestHandleUpdateLLMRotatesKey covers routine credential rotation.
func TestHandleUpdateLLMRotatesKey(t *testing.T) {
	s := llmTestServer(t, keyedModel("my-qwen", "https://api.example.com/v1"))
	if err := upsertLLMCredential(context.Background(), s, "llm-my-qwen", "sk-old"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	w := putLLM(t, s, "my-qwen", `{"endpoint":"https://api.example.com/v1","apiKey":"sk-new"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var sec corev1.Secret
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "llm-my-qwen"}, &sec); err != nil {
		t.Fatalf("credential Secret: %v", err)
	}
	if string(sec.Data["apiKey"]) != "sk-new" {
		t.Errorf("apiKey = %q, want sk-new", sec.Data["apiKey"])
	}
}

// TestHandleUpdateLLMPublicToKeyed promotes a public model once its endpoint
// turns out to need a key.
func TestHandleUpdateLLMPublicToKeyed(t *testing.T) {
	s := llmTestServer(t, v1alpha1.TemplateModelSpec{Name: "pub", Endpoint: "https://api.example.com/v1"})

	w := putLLM(t, s, "pub", `{"endpoint":"https://api.example.com/v1","apiKey":"sk-1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	models := templateModels(t, s)
	m := models[len(models)-1]
	if m.CredentialRef == nil || m.CredentialRef.Name != "llm-pub" {
		t.Fatalf("credentialRef should be set: %+v", m.CredentialRef)
	}
	var sec corev1.Secret
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "llm-pub"}, &sec); err != nil {
		t.Fatalf("credential Secret: %v", err)
	}
}

// TestHandleUpdateLLMKeyedToPublic demotes a model and must not leave its
// credential Secret orphaned.
func TestHandleUpdateLLMKeyedToPublic(t *testing.T) {
	s := llmTestServer(t, keyedModel("my-qwen", "https://api.example.com/v1"))
	if err := upsertLLMCredential(context.Background(), s, "llm-my-qwen", "sk-old"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	w := putLLM(t, s, "my-qwen", `{"endpoint":"https://api.example.com/v1","public":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	models := templateModels(t, s)
	if m := models[len(models)-1]; m.CredentialRef != nil {
		t.Errorf("credentialRef should be cleared: %+v", m.CredentialRef)
	}
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "llm-my-qwen"}, &corev1.Secret{}); err == nil {
		t.Error("credential Secret should be deleted with the credential")
	}
}

// TestHandleUpdateLLMKeylessWithoutFlag keeps the add-time guard on the edit
// path too: a public model cannot be left public by accident.
func TestHandleUpdateLLMKeylessWithoutFlag(t *testing.T) {
	s := llmTestServer(t, v1alpha1.TemplateModelSpec{Name: "pub", Endpoint: "https://api.example.com/v1"})

	w := putLLM(t, s, "pub", `{"endpoint":"https://api.example.com/v1"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateLLMPublicWithKey(t *testing.T) {
	s := llmTestServer(t, keyedModel("my-qwen", "https://api.example.com/v1"))

	w := putLLM(t, s, "my-qwen", `{"endpoint":"https://api.example.com/v1","apiKey":"sk-1","public":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateLLMUnknownModel(t *testing.T) {
	s := llmTestServer(t)

	w := putLLM(t, s, "nope", `{"endpoint":"https://api.example.com/v1","apiKey":"sk-1"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateLLMRejectsBadEndpoint(t *testing.T) {
	s := llmTestServer(t, keyedModel("my-qwen", "https://api.example.com/v1"))

	w := putLLM(t, s, "my-qwen", `{"endpoint":"not-a-url","apiKey":"sk-1"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateLLMRejectsGet(t *testing.T) {
	s := llmTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/llms/my-qwen", nil)
	req.SetPathValue("name", "my-qwen")
	w := httptest.NewRecorder()
	s.handleLLMByName(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func deleteLLM(t *testing.T, s *Server, name string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/llms/"+name, nil)
	req.SetPathValue("name", name)
	w := httptest.NewRecorder()
	s.handleLLMByName(w, req)
	return w
}

func builtinTemplate(t *testing.T, s *Server) v1alpha1.AgentTemplate {
	t.Helper()
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		t.Fatalf("get template: %v", err)
	}
	return tmpl
}

// TestHandleDeleteLLM removes the model and its credential: neither should
// survive, or the Secret leaks a live key nobody references.
func TestHandleDeleteLLM(t *testing.T) {
	s := llmTestServer(t, keyedModel("my-qwen", "https://api.example.com/v1"))
	if err := upsertLLMCredential(context.Background(), s, "llm-my-qwen", "sk-old"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	w := deleteLLM(t, s, "my-qwen")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	for _, m := range builtinTemplate(t, s).Spec.Models {
		if m.Name == "my-qwen" {
			t.Errorf("model should be gone: %+v", m)
		}
	}
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "llm-my-qwen"}, &corev1.Secret{}); err == nil {
		t.Error("credential Secret should be deleted with the model")
	}
}

// TestHandleDeleteLLMPublicModel covers a model that never had a Secret.
func TestHandleDeleteLLMPublicModel(t *testing.T) {
	s := llmTestServer(t, v1alpha1.TemplateModelSpec{Name: "pub", Endpoint: "https://api.example.com/v1"})

	if w := deleteLLM(t, s, "pub"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestHandleDeleteLLMUnknownModel(t *testing.T) {
	s := llmTestServer(t)

	if w := deleteLLM(t, s, "nope"); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", w.Code, w.Body.String())
	}
}

// TestHandleDeleteLLMRefusesSelectedModel pins the fail-closed rule: removing a
// model an instance selects would make that user's turns fail, so the delete is
// refused and the response names the instance to re-point.
func TestHandleDeleteLLMRefusesSelectedModel(t *testing.T) {
	s := llmTestServer(t, keyedModel("my-qwen", "https://api.example.com/v1"))
	inst := &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "bob-cubepilot", Namespace: "cubepilot"},
		Spec: v1alpha1.AgentInstanceSpec{
			TemplateRef:   v1alpha1.DefaultAgentName,
			Owner:         "bob",
			SelectedModel: "my-qwen",
		},
	}
	if err := s.cr.Create(context.Background(), inst); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	w := deleteLLM(t, s, "my-qwen")
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Error     string                         `json:"error"`
		Instances []struct{ Name, Owner string } `json:"instances"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 409 body: %v", err)
	}
	if len(resp.Instances) != 1 || resp.Instances[0].Owner != "bob" {
		t.Errorf("409 should name the selecting instance, got %+v", resp.Instances)
	}
	// The Portal surfaces the message verbatim, so the blocker has to be in it
	// -- a bare count sends the admin hunting through instances.
	if !strings.Contains(resp.Error, "bob") {
		t.Errorf("409 message should name the owner, got %q", resp.Error)
	}
	// The model must survive the refusal.
	found := false
	for _, m := range builtinTemplate(t, s).Spec.Models {
		if m.Name == "my-qwen" {
			found = true
		}
	}
	if !found {
		t.Error("a refused delete must not remove the model")
	}
}

// TestHandleDeleteLLMIgnoresOtherTemplateSelection: the selection only matters
// for the builtin template, so an instance pointing elsewhere must not block.
func TestHandleDeleteLLMIgnoresOtherTemplateSelection(t *testing.T) {
	s := llmTestServer(t, keyedModel("my-qwen", "https://api.example.com/v1"))
	inst := &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "bob-other", Namespace: "cubepilot"},
		Spec: v1alpha1.AgentInstanceSpec{
			TemplateRef:   "some-other-template",
			Owner:         "bob",
			SelectedModel: "my-qwen",
		},
	}
	if err := s.cr.Create(context.Background(), inst); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	if w := deleteLLM(t, s, "my-qwen"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", w.Code, w.Body.String())
	}
}

// TestHandleDeleteLLMClearsDefaultModel: the deleted model may be the gateway's
// primary. The renderer falls back to the first remaining provider, so the
// dangling name must be cleared rather than left in the CR.
func TestHandleDeleteLLMClearsDefaultModel(t *testing.T) {
	s := llmTestServer(t)
	if got := builtinTemplate(t, s).Spec.DefaultModel; got != "deepseek-v4-flash" {
		t.Fatalf("fixture defaultModel = %q", got)
	}

	if w := deleteLLM(t, s, "deepseek-v4-flash"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := builtinTemplate(t, s).Spec.DefaultModel; got != "" {
		t.Errorf("defaultModel = %q, want cleared", got)
	}
}

// TestHandleDeleteLLMLastModel: an empty catalog is a state the platform can be
// in, and the Portal already says so -- it is not worth a special case.
func TestHandleDeleteLLMLastModel(t *testing.T) {
	s := llmTestServer(t)

	if w := deleteLLM(t, s, "deepseek-v4-flash"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if models := builtinTemplate(t, s).Spec.Models; len(models) != 0 {
		t.Errorf("models = %+v, want empty", models)
	}
}

// TestLLMRoutesAreWired exercises the real mux. The route is the only way a
// client reaches the edit and delete handlers, and a handler-level test that
// calls them directly cannot catch a missing registration.
func TestLLMRoutesAreWired(t *testing.T) {
	s := platformTestServer(t, controller.BuiltinAgentTemplate("https://api.deepseek.com", "deepseek-v4-flash"))

	rec := doReq(t, s.Handler(), http.MethodPut, "/api/llms/deepseek-v4-flash", "admin",
		map[string]any{"endpoint": "https://api.deepseek.com", "apiKey": "sk-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/llms/{name} = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, s.Handler(), http.MethodDelete, "/api/llms/deepseek-v4-flash", "admin", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/llms/{name} = %d, body = %s", rec.Code, rec.Body.String())
	}
}
