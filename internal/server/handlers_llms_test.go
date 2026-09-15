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
	"github.com/suanova/cubepilot/internal/k8s"
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

	body := bytes.NewBufferString(`{"name":"My Qwen","endpoint":"https://api.example.com/v1","apiKey":"sk-2","models":["qwen3-32b"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/llms", body)
	w := httptest.NewRecorder()
	s.handleAddLLM(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", w.Code, w.Body.String())
	}

	// Provider appended to the builtin template.
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if len(tmpl.Spec.Providers) != 2 || tmpl.Spec.Providers[1].Name != "my-qwen" {
		t.Fatalf("providers = %+v", tmpl.Spec.Providers)
	}
	if tmpl.Spec.Providers[1].CredentialRef.Name != "llm-my-qwen" {
		t.Errorf("credentialRef = %q", tmpl.Spec.Providers[1].CredentialRef.Name)
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

	body := bytes.NewBufferString(`{"name":"local-ollama","endpoint":"http://localhost:11434/v1","public":true,"models":["llama3"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/llms", body)
	w := httptest.NewRecorder()
	s.handleAddLLM(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", w.Code, w.Body.String())
	}
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		t.Fatalf("get template: %v", err)
	}
	m := tmpl.Spec.Providers[len(tmpl.Spec.Providers)-1]
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

	body := bytes.NewBufferString(`{"name":"qwen","endpoint":"https://api.example.com/v1/chat/completions/","apiKey":"sk-1","models":["qwen3-32b"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/llms", body)
	w := httptest.NewRecorder()
	s.handleAddLLM(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", w.Code, w.Body.String())
	}
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if got := tmpl.Spec.Providers[len(tmpl.Spec.Providers)-1].Endpoint; got != "https://api.example.com/v1" {
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
	req := httptest.NewRequest(http.MethodPost, "/api/v1/llms", body)
	w := httptest.NewRecorder()
	s.handleAddLLM(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		t.Fatalf("get template: %v", err)
	}
	if len(tmpl.Spec.Providers) != 1 {
		t.Errorf("nothing should be written on a rejected add: %+v", tmpl.Spec.Providers)
	}
}

// TestHandleAddLLMRejectsPublicWithKey rejects a contradictory request rather
// than silently picking one of the two meanings.
func TestHandleAddLLMRejectsPublicWithKey(t *testing.T) {
	builtin := controller.BuiltinAgentTemplate("https://api.deepseek.com", "deepseek-v4-flash")
	builtin.Namespace = "cubepilot"
	s := addLLMTestServer(t, builtin)

	body := bytes.NewBufferString(`{"name":"both","endpoint":"https://api.example.com/v1","apiKey":"sk-1","public":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/llms", body)
	w := httptest.NewRecorder()
	s.handleAddLLM(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
}

// TestHandleAddLLMProviderWithModels: one endpoint and one key serving several
// model ids is the case the flat list could not express. The credential is
// created once, for the provider, not once per id.
func TestHandleAddLLMProviderWithModels(t *testing.T) {
	s := llmTestServer(t)
	body := map[string]any{
		"name":     "vllm",
		"endpoint": "http://vllm.ai.svc:8000/v1",
		"apiKey":   "sk-vllm",
		"models":   []string{"qwen3-32b", "deepseek-v4-flash"},
	}
	rec := doReq(t, s.Handler(), http.MethodPost, "/api/v1/llms", "", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	providers := templateProviders(t, s)
	last := providers[len(providers)-1]
	if last.Name != "vllm" || len(last.Models) != 2 {
		t.Fatalf("provider = %+v, want vllm with two ids", last)
	}
	var sec corev1.Secret
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "llm-vllm"}, &sec); err != nil {
		t.Fatalf("credential Secret: %v", err)
	}
	// One Secret for the provider, none per model.
	for _, id := range last.Models {
		if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "llm-" + id}, &corev1.Secret{}); err == nil {
			t.Errorf("model %q should not get its own credential Secret", id)
		}
	}
}

// llmTestServer builds a server whose builtin template carries the given extra
// providers on top of the platform default.
func llmTestServer(t *testing.T, providers ...v1alpha1.TemplateProviderSpec) *Server {
	t.Helper()
	builtin := controller.BuiltinAgentTemplate("https://api.deepseek.com", "deepseek-v4-flash")
	builtin.Namespace = "cubepilot"
	builtin.Spec.Providers = append(builtin.Spec.Providers, providers...)
	return addLLMTestServer(t, builtin)
}

// keyedModel is a one-model provider with a credential, the shape this task's
// write API produces.
func keyedModel(name, endpoint string) v1alpha1.TemplateProviderSpec {
	return v1alpha1.TemplateProviderSpec{
		Name:          name,
		Endpoint:      endpoint,
		CredentialRef: &corev1.LocalObjectReference{Name: "llm-" + name},
		Models:        []string{name},
	}
}

func putLLM(t *testing.T, s *Server, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/llms/"+name, bytes.NewBufferString(body))
	req.SetPathValue("name", name)
	w := httptest.NewRecorder()
	s.handleLLMByName(w, req)
	return w
}

func templateProviders(t *testing.T, s *Server) []v1alpha1.TemplateProviderSpec {
	t.Helper()
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		t.Fatalf("get template: %v", err)
	}
	return tmpl.Spec.Providers
}

// TestHandleUpdateLLMEndpointKeepsKey is the "fix a typo'd endpoint" journey:
// the client never receives the apiKey, so a blank one must keep the stored
// credential rather than wipe or replace it.
func TestHandleUpdateLLMEndpointKeepsKey(t *testing.T) {
	s := llmTestServer(t, keyedModel("my-qwen", "https://api.example.com/v1"))
	if err := upsertLLMCredential(context.Background(), s, "llm-my-qwen", "sk-old"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	w := putLLM(t, s, "my-qwen", `{"endpoint":"https://other.example.com/v1/chat/completions","models":["my-qwen"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	providers := templateProviders(t, s)
	m := providers[len(providers)-1]
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

	w := putLLM(t, s, "my-qwen", `{"endpoint":"https://api.example.com/v1","apiKey":"sk-new","models":["my-qwen"]}`)
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
	s := llmTestServer(t, v1alpha1.TemplateProviderSpec{Name: "pub", Endpoint: "https://api.example.com/v1", Models: []string{"pub"}})

	w := putLLM(t, s, "pub", `{"endpoint":"https://api.example.com/v1","apiKey":"sk-1","models":["pub"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	providers := templateProviders(t, s)
	m := providers[len(providers)-1]
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

	w := putLLM(t, s, "my-qwen", `{"endpoint":"https://api.example.com/v1","public":true,"models":["my-qwen"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	providers := templateProviders(t, s)
	if m := providers[len(providers)-1]; m.CredentialRef != nil {
		t.Errorf("credentialRef should be cleared: %+v", m.CredentialRef)
	}
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "llm-my-qwen"}, &corev1.Secret{}); err == nil {
		t.Error("credential Secret should be deleted with the credential")
	}
}

// TestHandleUpdateLLMKeylessWithoutFlag keeps the add-time guard on the edit
// path too: a public model cannot be left public by accident.
func TestHandleUpdateLLMKeylessWithoutFlag(t *testing.T) {
	s := llmTestServer(t, v1alpha1.TemplateProviderSpec{Name: "pub", Endpoint: "https://api.example.com/v1", Models: []string{"pub"}})

	w := putLLM(t, s, "pub", `{"endpoint":"https://api.example.com/v1","models":["pub"]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateLLMPublicWithKey(t *testing.T) {
	s := llmTestServer(t, keyedModel("my-qwen", "https://api.example.com/v1"))

	w := putLLM(t, s, "my-qwen", `{"endpoint":"https://api.example.com/v1","apiKey":"sk-1","public":true,"models":["my-qwen"]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateLLMUnknownModel(t *testing.T) {
	s := llmTestServer(t)

	w := putLLM(t, s, "nope", `{"endpoint":"https://api.example.com/v1","apiKey":"sk-1","models":["nope"]}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", w.Code, w.Body.String())
	}
}

func TestHandleUpdateLLMRejectsBadEndpoint(t *testing.T) {
	s := llmTestServer(t, keyedModel("my-qwen", "https://api.example.com/v1"))

	w := putLLM(t, s, "my-qwen", `{"endpoint":"not-a-url","apiKey":"sk-1","models":["my-qwen"]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
}

// TestHandleUpdateLLMReplacesModels: adding and removing a single id is a PUT
// with the full list, which keeps the route surface unchanged.
func TestHandleUpdateLLMReplacesModels(t *testing.T) {
	s := llmTestServer(t, v1alpha1.TemplateProviderSpec{
		Name: "vllm", Endpoint: "http://vllm.ai.svc:8000/v1",
		CredentialRef: &corev1.LocalObjectReference{Name: "llm-vllm"},
		Models:        []string{"qwen3-32b"},
	})
	rec := doReq(t, s.Handler(), http.MethodPut, "/api/v1/llms/vllm", "", map[string]any{
		"endpoint": "http://vllm.ai.svc:8000/v1",
		"models":   []string{"qwen3-32b", "qwen3-8b"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	for _, p := range templateProviders(t, s) {
		if p.Name == "vllm" && (len(p.Models) != 2 || p.Models[1] != "qwen3-8b") {
			t.Errorf("models = %v, want [qwen3-32b qwen3-8b]", p.Models)
		}
	}
}

// TestHandleUpdateLLMRefusesEmptyModels: a provider with no model ids renders
// nothing and is unselectable, so the list can never be emptied.
func TestHandleUpdateLLMRefusesEmptyModels(t *testing.T) {
	s := llmTestServer(t, v1alpha1.TemplateProviderSpec{
		Name: "vllm", Endpoint: "http://vllm.ai.svc:8000/v1",
		CredentialRef: &corev1.LocalObjectReference{Name: "llm-vllm"},
		Models:        []string{"qwen3-32b"},
	})
	rec := doReq(t, s.Handler(), http.MethodPut, "/api/v1/llms/vllm", "", map[string]any{
		"endpoint": "http://vllm.ai.svc:8000/v1",
		"models":   []string{},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleUpdateLLMRejectsGet(t *testing.T) {
	s := llmTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/llms/my-qwen", nil)
	req.SetPathValue("name", "my-qwen")
	w := httptest.NewRecorder()
	s.handleLLMByName(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func deleteLLM(t *testing.T, s *Server, name string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/llms/"+name, nil)
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
	for _, p := range builtinTemplate(t, s).Spec.Providers {
		if p.Name == "my-qwen" {
			t.Errorf("provider should be gone: %+v", p)
		}
	}
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "llm-my-qwen"}, &corev1.Secret{}); err == nil {
		t.Error("credential Secret should be deleted with the model")
	}
}

// TestHandleDeleteLLMPublicModel covers a model that never had a Secret.
func TestHandleDeleteLLMPublicModel(t *testing.T) {
	s := llmTestServer(t, v1alpha1.TemplateProviderSpec{Name: "pub", Endpoint: "https://api.example.com/v1", Models: []string{"pub"}})

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

// seedInstanceSelecting creates the AgentInstance of the builtin template that
// belongs to zhang.wei and explicitly selects the given model ref.
func seedInstanceSelecting(t *testing.T, s *Server, ref string) {
	t.Helper()
	inst := &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      k8s.InstanceName("zhang.wei", v1alpha1.DefaultAgentName),
			Namespace: "cubepilot",
		},
		Spec: v1alpha1.AgentInstanceSpec{
			TemplateRef:   v1alpha1.DefaultAgentName,
			Owner:         "zhang.wei",
			SelectedModel: ref,
		},
	}
	if err := s.cr.Create(context.Background(), inst); err != nil {
		t.Fatalf("create instance: %v", err)
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
			TemplateRef: v1alpha1.DefaultAgentName,
			Owner:       "bob",
			// A stored selection is the provider's model ref, not the bare
			// provider name: keyedModel serves the single id "my-qwen" under
			// provider "my-qwen".
			SelectedModel: "my-qwen/my-qwen",
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
	for _, p := range builtinTemplate(t, s).Spec.Providers {
		if p.Name == "my-qwen" {
			found = true
		}
	}
	if !found {
		t.Error("a refused delete must not remove the model")
	}
}

// TestHandleDeleteLLMRefusesSelectedProvider: the 409 guard compares the
// instance's selectedModel, which is a <provider>/<modelId> ref, against the
// refs the provider serves. Comparing a provider name instead never matches,
// which would make a provider an instance still selects deletable.
func TestHandleDeleteLLMRefusesSelectedProvider(t *testing.T) {
	s := llmTestServer(t, v1alpha1.TemplateProviderSpec{
		Name: "vllm", Endpoint: "http://vllm.ai.svc:8000/v1",
		CredentialRef: &corev1.LocalObjectReference{Name: "llm-vllm"},
		Models:        []string{"qwen3-32b"},
	})
	seedInstanceSelecting(t, s, "vllm/qwen3-32b")
	rec := doReq(t, s.Handler(), http.MethodDelete, "/api/v1/llms/vllm", "", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleDeleteLLMRefusesWhileAnyModelIsSelected: the refusal covers every id
// the provider serves, not just one.
func TestHandleDeleteLLMRefusesWhileAnyModelIsSelected(t *testing.T) {
	s := llmTestServer(t, v1alpha1.TemplateProviderSpec{
		Name: "vllm", Endpoint: "http://vllm.ai.svc:8000/v1",
		CredentialRef: &corev1.LocalObjectReference{Name: "llm-vllm"},
		Models:        []string{"qwen3-32b", "qwen3-8b"},
	})
	seedInstanceSelecting(t, s, "vllm/qwen3-8b") // the second id, not the first
	rec := doReq(t, s.Handler(), http.MethodDelete, "/api/v1/llms/vllm", "", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleDeleteLLMIgnoresOtherTemplateSelection: the selection only matters
// for the builtin template, so an instance pointing elsewhere must not block --
// even when it selects a ref this provider serves.
func TestHandleDeleteLLMIgnoresOtherTemplateSelection(t *testing.T) {
	s := llmTestServer(t, keyedModel("my-qwen", "https://api.example.com/v1"))
	inst := &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "bob-other", Namespace: "cubepilot"},
		Spec: v1alpha1.AgentInstanceSpec{
			TemplateRef:   "some-other-template",
			Owner:         "bob",
			SelectedModel: "my-qwen/my-qwen",
		},
	}
	if err := s.cr.Create(context.Background(), inst); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	if w := deleteLLM(t, s, "my-qwen"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", w.Code, w.Body.String())
	}
}

// TestHandleDeleteLLMClearsDefaultModel: the deleted provider may serve the
// gateway's primary. The renderer falls back to the first remaining provider,
// so the dangling ref must be cleared rather than left in the CR.
func TestHandleDeleteLLMClearsDefaultModel(t *testing.T) {
	s := llmTestServer(t)
	if got := builtinTemplate(t, s).Spec.DefaultModel; got != "platform/deepseek-v4-flash" {
		t.Fatalf("fixture defaultModel = %q", got)
	}

	if w := deleteLLM(t, s, controller.BuiltinProviderName); w.Code != http.StatusOK {
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

	if w := deleteLLM(t, s, controller.BuiltinProviderName); w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if providers := builtinTemplate(t, s).Spec.Providers; len(providers) != 0 {
		t.Errorf("providers = %+v, want empty", providers)
	}
}

// TestLLMRoutesAreWired exercises the real mux. The route is the only way a
// client reaches the edit and delete handlers, and a handler-level test that
// calls them directly cannot catch a missing registration.
func TestLLMRoutesAreWired(t *testing.T) {
	s := platformTestServer(t, controller.BuiltinAgentTemplate("https://api.deepseek.com", "deepseek-v4-flash"))

	rec := doReq(t, s.Handler(), http.MethodPut, "/api/v1/llms/"+controller.BuiltinProviderName, "admin",
		map[string]any{"endpoint": "https://api.deepseek.com", "apiKey": "sk-1", "models": []string{"deepseek-v4-flash"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/llms/{name} = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, s.Handler(), http.MethodDelete, "/api/v1/llms/"+controller.BuiltinProviderName, "admin", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE /api/llms/{name} = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// shareCredentialWith points one of the template's providers at an existing
// credential Secret instead of the one this API would have named for it -- the
// state a hand-edited CR can be in.
func shareCredentialWith(t *testing.T, s *Server, provider, secret string) {
	t.Helper()
	tmpl := builtinTemplate(t, s)
	for i := range tmpl.Spec.Providers {
		if tmpl.Spec.Providers[i].Name == provider {
			tmpl.Spec.Providers[i].CredentialRef = &corev1.LocalObjectReference{Name: secret}
		}
	}
	if err := s.cr.Update(context.Background(), &tmpl); err != nil {
		t.Fatalf("update template: %v", err)
	}
}

// TestHandleDeleteLLMKeepsSharedCredential: a credentialRef that is not the
// Secret this API names after the model may be shared with another model (the
// builtin's cubepilot-llm is), so deleting the model must not take it along --
// the other model would silently lose its credential.
func TestHandleDeleteLLMKeepsSharedCredential(t *testing.T) {
	s := llmTestServer(t, keyedModel("my-qwen", "https://api.example.com/v1"))
	shareCredentialWith(t, s, "my-qwen", "cubepilot-llm")
	if err := upsertLLMCredential(context.Background(), s, "cubepilot-llm", "sk-shared"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	w := deleteLLM(t, s, "my-qwen")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "cubepilot-llm"}, &corev1.Secret{}); err != nil {
		t.Errorf("a Secret this model does not own must be left in place: %v", err)
	}
	// The model is gone either way; the leftover is reported, not silently kept.
	if !strings.Contains(w.Body.String(), "cubepilot-llm") {
		t.Errorf("the response should say the shared Secret was left: %s", w.Body.String())
	}
}

// TestHandleUpdateLLMKeepsSharedCredential is the same guard on the demote
// path: clearing a hand-set credentialRef must not delete the Secret it names.
func TestHandleUpdateLLMKeepsSharedCredential(t *testing.T) {
	s := llmTestServer(t, keyedModel("my-qwen", "https://api.example.com/v1"))
	shareCredentialWith(t, s, "my-qwen", "cubepilot-llm")
	if err := upsertLLMCredential(context.Background(), s, "cubepilot-llm", "sk-shared"); err != nil {
		t.Fatalf("seed credential: %v", err)
	}

	w := putLLM(t, s, "my-qwen", `{"endpoint":"https://api.example.com/v1","public":true,"models":["my-qwen"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if err := s.cr.Get(context.Background(), types.NamespacedName{Namespace: "cubepilot", Name: "cubepilot-llm"}, &corev1.Secret{}); err != nil {
		t.Errorf("a Secret this model does not own must be left in place: %v", err)
	}
}
