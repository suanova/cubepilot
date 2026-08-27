package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
)

// handleAddLLM serves POST /api/llms -- the platform admin adds an LLM by
// giving a name, an OpenAI-compatible endpoint and (for non-public models) an
// apiKey. The handler appends a model to the builtin AgentTemplate and creates
// a credential Secret when keyed; the operator renders it into the gateway
// config (issue #6). No credentials are ever stored in the CR.
func (s *Server) handleAddLLM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	var body struct {
		Name     string `json:"name"`
		Endpoint string `json:"endpoint"`
		APIKey   string `json:"apiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
		return
	}
	rawName := strings.TrimSpace(body.Name)
	if rawName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
		return
	}
	name := k8s.Sanitize(rawName)
	endpoint := strings.TrimSpace(body.Endpoint)
	if u, err := url.Parse(endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "endpoint must be a valid URL"})
		return
	}
	if s.cr == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "k8s client unavailable"})
		return
	}

	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(r.Context(), types.NamespacedName{Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("builtin template: %v", err)})
		return
	}
	for _, m := range tmpl.Spec.Models {
		if m.Name == name {
			writeJSON(w, http.StatusConflict, map[string]any{"error": fmt.Sprintf("model %q already exists", name)})
			return
		}
	}

	// Commit the model to the template BEFORE creating the credential Secret:
	// a failed template update leaves no orphaned key Secret, and a re-add with
	// a new key never keeps the old one (the operator skips a model whose
	// Secret is missing and re-renders once it appears).
	model := v1alpha1.TemplateModelSpec{Name: name, Endpoint: endpoint}
	if body.APIKey != "" {
		model.CredentialRef = &corev1.LocalObjectReference{Name: "llm-" + name}
	}
	tmpl.Spec.Models = append(tmpl.Spec.Models, model)
	if err := s.cr.Update(r.Context(), &tmpl); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("update template: %v", err)})
		return
	}
	if body.APIKey != "" {
		if err := upsertLLMCredential(r.Context(), s, "llm-"+name, body.APIKey); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("create credential Secret: %v", err)})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": model})
}

// upsertLLMCredential creates the credential Secret, or refreshes its apiKey
// if it already exists (so re-adding a model with a new key takes effect).
func upsertLLMCredential(ctx context.Context, s *Server, secretName, apiKey string) error {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: s.cfg.Namespace, Name: secretName},
		Data:       map[string][]byte{"apiKey": []byte(apiKey)},
	}
	if err := s.cr.Create(ctx, sec); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return err
	}
	var existing corev1.Secret
	if err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.cfg.Namespace, Name: secretName}, &existing); err != nil {
		return err
	}
	existing.Data["apiKey"] = []byte(apiKey)
	return s.cr.Update(ctx, &existing)
}
