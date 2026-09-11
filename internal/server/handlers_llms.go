package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
)

// llmRequest is the body shared by the add and edit handlers. Name is only
// read by add: a rename is a delete plus an add, because the name is the
// gateway provider key, the model id sent to the endpoint, the selection key
// and the credential Secret name all at once.
type llmRequest struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	APIKey   string `json:"apiKey"`
	// Public declares that the endpoint requires no credentials. A model
	// without a credential is only ever stored when the request says so:
	// otherwise a forgotten apiKey would save a model that can be selected and
	// fails every turn with "No API key resolved" (gateway.PublicModelAPIKey).
	Public bool `json:"public"`
}

// normalizeEndpoint validates an endpoint and reduces it to the API root the
// OpenAI SDK expects. The SDK appends /chat/completions to baseUrl itself, so
// an endpoint copied from a working curl command (a full request URL) would be
// doubled -- .../v1/chat/completions/chat/completions -- and 404. Only that one
// suffix is stripped, and a missing /v1 is never added: a root endpoint is
// correct for some providers (the platform default https://api.deepseek.com is
// one), so guessing a path prefix would break them.
func normalizeEndpoint(raw string) (string, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(raw), "/")
	const suffix = "/chat/completions"
	if len(endpoint) >= len(suffix) && strings.EqualFold(endpoint[len(endpoint)-len(suffix):], suffix) {
		endpoint = strings.TrimRight(endpoint[:len(endpoint)-len(suffix)], "/")
	}
	if u, err := url.Parse(endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("endpoint must be a valid URL")
	}
	return endpoint, nil
}

// credentialChoiceError validates the {apiKey, public} pair of an add, which
// has no stored credential to fall back on: exactly one of the two must be
// given. It returns the message to send, or "" when the pair is valid.
func credentialChoiceError(apiKey string, public bool) string {
	if public && apiKey != "" {
		return "apiKey and public are mutually exclusive: a public model has no credential"
	}
	if !public && apiKey == "" {
		return "apiKey is required unless the model is declared public (public=true)"
	}
	return ""
}

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
	var body llmRequest
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
	endpoint, err := normalizeEndpoint(body.Endpoint)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if msg := credentialChoiceError(body.APIKey, body.Public); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg})
		return
	}
	if s.cr == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "k8s client unavailable"})
		return
	}

	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(r.Context(), types.NamespacedName{Namespace: s.cfg.Namespace, Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
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
		model.CredentialRef = &corev1.LocalObjectReference{Name: llmCredentialName(name)}
	}
	tmpl.Spec.Models = append(tmpl.Spec.Models, model)
	if err := s.cr.Update(r.Context(), &tmpl); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("update template: %v", err)})
		return
	}
	if body.APIKey != "" {
		if err := upsertLLMCredential(r.Context(), s, llmCredentialName(name), body.APIKey); err != nil {
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

// handleLLMByName serves PUT and DELETE /api/llms/{name} (issue #170): the
// platform admin edits or removes a model it already added. Both act on the
// builtin AgentTemplate, matching handleAddLLM. {name} is the sanitized model
// name, and it is not editable -- the name is the gateway provider key, the
// model id sent to the endpoint, the selection key and the credential Secret
// name all at once, so a rename is a delete plus an add.
func (s *Server) handleLLMByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.NotFound(w, r)
		return
	}
	if s.cr == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "k8s client unavailable"})
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.handleUpdateLLM(w, r, name)
	case http.MethodDelete:
		s.handleDeleteLLM(w, r, name)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "PUT or DELETE required"})
	}
}

// handleUpdateLLM applies an edit to an existing model. An empty apiKey means
// "keep the stored credential": the client never received the key, so it
// cannot echo it back, and keeping it is what makes a typo'd endpoint
// correctable without re-entering the credential. public=true is the other
// direction -- it clears the credential and deletes its Secret.
func (s *Server) handleUpdateLLM(w http.ResponseWriter, r *http.Request, name string) {
	var body llmRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
		return
	}
	endpoint, err := normalizeEndpoint(body.Endpoint)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if body.Public && body.APIKey != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "apiKey and public are mutually exclusive: a public model has no credential"})
		return
	}

	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(r.Context(), types.NamespacedName{Namespace: s.cfg.Namespace, Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("builtin template: %v", err)})
		return
	}
	idx := -1
	for i := range tmpl.Spec.Models {
		if tmpl.Spec.Models[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("model %q not found", name)})
		return
	}
	current := tmpl.Spec.Models[idx]
	if body.APIKey == "" && !body.Public && current.CredentialRef == nil {
		// Neither a key to keep nor a declaration that none is wanted. Refuse,
		// exactly as an add would: a model with no credential fails every turn
		// with "No API key resolved".
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "apiKey is required unless the model is declared public (public=true)"})
		return
	}

	model := current
	model.Endpoint = endpoint
	if body.APIKey != "" {
		// Reuse the model's own reference so a credential Secret that was not
		// named by this API (a hand-edited CR) keeps working.
		if current.CredentialRef != nil && current.CredentialRef.Name != "" {
			model.CredentialRef = current.CredentialRef
		} else {
			model.CredentialRef = &corev1.LocalObjectReference{Name: llmCredentialName(name)}
		}
	} else if body.Public {
		model.CredentialRef = nil
	}

	// Template first, then the Secret -- the same ordering as handleAddLLM:
	// the template decides whether the model exists at all, so a failure after
	// it leaves a recoverable state (an orphaned Secret) rather than a model
	// the operator skips for a missing credential.
	tmpl.Spec.Models[idx] = model
	if err := s.cr.Update(r.Context(), &tmpl); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("update template: %v", err)})
		return
	}
	warning := ""
	switch {
	case body.APIKey != "":
		if err := upsertLLMCredential(r.Context(), s, model.CredentialRef.Name, body.APIKey); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("update credential Secret: %v", err)})
			return
		}
	case body.Public && current.CredentialRef != nil:
		// Demoted to public: the credential must not outlive the model's
		// reference to it. The model is already gone from the template, so a
		// failure here is reported as a warning rather than an error that would
		// read as "the edit failed".
		if err := deleteLLMCredential(r.Context(), s, current.CredentialRef.Name); err != nil {
			warning = fmt.Sprintf("model updated, but its credential Secret could not be removed: %v", err)
		}
	}
	resp := map[string]any{"model": model}
	if warning != "" {
		resp["warning"] = warning
	}
	writeJSON(w, http.StatusOK, resp)
}

// llmCredentialName is the credential Secret name for a model. The name is
// derived from the model name, which is immutable, so it never drifts.
func llmCredentialName(modelName string) string {
	return "llm-" + modelName
}

// deleteLLMCredential removes a model's credential Secret. A missing Secret is
// success: the goal is that it does not exist.
func deleteLLMCredential(ctx context.Context, s *Server, secretName string) error {
	if secretName == "" {
		return nil
	}
	err := s.cr.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.cfg.Namespace, Name: secretName}})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// modelInstanceRef names an instance that selects a model, for the refusal
// body of a delete.
type modelInstanceRef struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

// instancesSelecting lists the AgentInstances of the builtin template that
// explicitly select the given model. Instances bound to another template are
// ignored: their selection resolves against that template, so this one cannot
// break it.
func (s *Server) instancesSelecting(ctx context.Context, model string) ([]modelInstanceRef, error) {
	var list v1alpha1.AgentInstanceList
	if err := s.cr.List(ctx, &list, client.InNamespace(s.cfg.Namespace)); err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	out := []modelInstanceRef{}
	for _, inst := range list.Items {
		if inst.Spec.TemplateRef == v1alpha1.DefaultAgentName && inst.Spec.SelectedModel == model {
			out = append(out, modelInstanceRef{Name: inst.Name, Owner: inst.Spec.Owner})
		}
	}
	return out, nil
}

// handleDeleteLLM removes a model and its credential Secret (issue #170). It
// refuses while an instance selects the model: SelectedModelFor is fail-closed,
// so that user's turns would start failing with a resolver error instead of
// falling back. The response names the instances, so the admin (or the user)
// can re-point the selection first.
func (s *Server) handleDeleteLLM(w http.ResponseWriter, r *http.Request, name string) {
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(r.Context(), types.NamespacedName{Namespace: s.cfg.Namespace, Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("builtin template: %v", err)})
		return
	}
	idx := -1
	for i := range tmpl.Spec.Models {
		if tmpl.Spec.Models[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("model %q not found", name)})
		return
	}

	selecting, err := s.instancesSelecting(r.Context(), name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if len(selecting) > 0 {
		// The Portal shows this message verbatim, so it names who is blocking
		// the delete rather than only counting them.
		who := make([]string, 0, len(selecting))
		for _, sel := range selecting {
			if sel.Owner != "" {
				who = append(who, sel.Owner)
			} else {
				who = append(who, sel.Name)
			}
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("model %q is selected by %s; select another model there first",
				name, strings.Join(who, ", ")),
			"instances": selecting,
		})
		return
	}

	model := tmpl.Spec.Models[idx]
	tmpl.Spec.Models = append(tmpl.Spec.Models[:idx], tmpl.Spec.Models[idx+1:]...)
	if tmpl.Spec.DefaultModel == name {
		// The deleted model may be the gateway's primary. Clearing the name is
		// defined: the renderer falls back to the first remaining provider. A
		// dangling name would leave the CR referencing a model that is gone.
		tmpl.Spec.DefaultModel = ""
	}
	if err := s.cr.Update(r.Context(), &tmpl); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("update template: %v", err)})
		return
	}
	warning := ""
	if model.CredentialRef != nil {
		// The model is already gone from the template; a failure here is a
		// warning, not an error -- reporting "the delete failed" would be a lie.
		if err := deleteLLMCredential(r.Context(), s, model.CredentialRef.Name); err != nil {
			warning = fmt.Sprintf("model removed, but its credential Secret could not be deleted: %v", err)
		}
	}
	resp := map[string]any{"removed": name}
	if warning != "" {
		resp["warning"] = warning
	}
	writeJSON(w, http.StatusOK, resp)
}
