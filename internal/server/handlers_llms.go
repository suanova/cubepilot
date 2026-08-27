package server

import (
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
	name := k8s.Sanitize(strings.TrimSpace(body.Name))
	endpoint := strings.TrimSpace(body.Endpoint)
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
		return
	}
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

	model := v1alpha1.TemplateModelSpec{Name: name, Endpoint: endpoint}
	if body.APIKey != "" {
		secretName := "llm-" + name
		sec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: s.cfg.Namespace, Name: secretName},
			Data:       map[string][]byte{"apiKey": []byte(body.APIKey)},
		}
		if err := s.cr.Create(r.Context(), sec); err != nil && !apierrors.IsAlreadyExists(err) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("create credential Secret: %v", err)})
			return
		}
		model.CredentialRef = corev1.LocalObjectReference{Name: secretName}
	}

	tmpl.Spec.Models = append(tmpl.Spec.Models, model)
	if err := s.cr.Update(r.Context(), &tmpl); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("update template: %v", err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": model})
}
