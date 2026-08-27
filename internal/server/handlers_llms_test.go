package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
	s := addLLMTestServer(t, controller.BuiltinAgentTemplate("https://api.deepseek.com"))

	body := bytes.NewBufferString(`{"name":"My Qwen","endpoint":"https://api.example.com/v1","apiKey":"sk-2"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/llms", body)
	w := httptest.NewRecorder()
	s.handleAddLLM(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	// Model appended to the builtin template.
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(context.Background(), types.NamespacedName{Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
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
	s := addLLMTestServer(t, controller.BuiltinAgentTemplate("https://api.deepseek.com"))

	body := bytes.NewBufferString(`{"name":"local-ollama","endpoint":"http://localhost:11434/v1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/llms", body)
	w := httptest.NewRecorder()
	s.handleAddLLM(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(context.Background(), types.NamespacedName{Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
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
