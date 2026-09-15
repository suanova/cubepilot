package server

import (
	"context"
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
	"github.com/suanova/cubepilot/internal/gateway"
	"github.com/suanova/cubepilot/internal/k8s"
)

// llmRequest is the body shared by the add and edit handlers. Name is only
// read by add: a rename is a delete plus an add, because the name is the
// gateway provider key, the prefix of every model ref, the selection key and
// the credential Secret name all at once.
type llmRequest struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	APIKey   string `json:"apiKey"`
	// Models are the backend model ids this provider serves. Name is the
	// provider name -- the ref prefix and the credential Secret suffix -- and
	// has nothing to do with the ids, which are sent to the endpoint verbatim.
	Models []string `json:"models"`
	// Public declares that the endpoint requires no credentials. A provider
	// without a credential is only ever stored when the request says so:
	// otherwise a forgotten apiKey would save a provider that can be selected
	// and fails every turn with "No API key resolved" (gateway.PublicModelAPIKey).
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

// normalizeModels trims and de-duplicates the requested model ids. The grammar
// is deliberately not restated here: the handler validates the assembled
// TemplateProviderSpec with its own Validate, so the HTTP path and a
// hand-edited CR are held to exactly the same rules by one implementation.
func normalizeModels(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// handleAddLLM serves POST /api/llms -- the platform admin adds an LLM by
// giving a name, an OpenAI-compatible endpoint, the model ids the endpoint
// serves and (for non-public providers) an apiKey. The handler appends that
// provider to the builtin AgentTemplate and creates one credential Secret for
// it when keyed; the operator renders it into the gateway config (issue #6).
// No credentials are ever stored in the CR.
func (s *Server) handleAddLLM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	var body llmRequest
	if !decodeJSONBody(w, r, &body) {
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
	// The assembled provider is validated before anything is written, so a
	// request that would store a provider the operator then skips is refused.
	provider := v1alpha1.TemplateProviderSpec{
		Name:     name,
		Endpoint: endpoint,
		Models:   normalizeModels(body.Models),
	}
	if body.APIKey != "" {
		provider.CredentialRef = &corev1.LocalObjectReference{Name: llmCredentialName(name)}
	}
	// One validator for the HTTP path and the CRD: an empty model list, a bad id
	// or a bad provider name is refused with the same message either way.
	if err := provider.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
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
	for _, pr := range tmpl.Spec.Providers {
		if pr.Name == name {
			writeJSON(w, http.StatusConflict, map[string]any{"error": fmt.Sprintf("provider %q already exists", name)})
			return
		}
	}
	// spec.providers caps the list at 32 (MaxItems). The bound cannot live in
	// TemplateProviderSpec.Validate -- that validator is handed one provider --
	// so it is enforced here, before the write: the 33rd provider passes the
	// validation below and is refused by the API server inside s.cr.Update,
	// which the handler can only report as a 500.
	if len(tmpl.Spec.Providers) >= 32 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the template must carry at most 32 providers"})
		return
	}

	// Commit the provider to the template BEFORE creating the credential
	// Secret: a failed template update leaves no orphaned key Secret, and a
	// re-add with a new key never keeps the old one (the operator skips a
	// provider whose Secret is missing and re-renders once it appears).
	tmpl.Spec.Providers = append(tmpl.Spec.Providers, provider)
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
	writeJSON(w, http.StatusCreated, map[string]any{"provider": provider})
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
// platform admin edits or removes a provider it already added. Both act on the
// builtin AgentTemplate, matching handleAddLLM. {name} is the sanitized
// provider name, and it is not editable -- the name is the gateway provider
// key, the prefix of every model ref, the selection key and the credential
// Secret name all at once, so a rename is a delete plus an add.
func (s *Server) handleLLMByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeNotFound(w, "missing provider name")
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

// handleUpdateLLM applies an edit to an existing provider: its endpoint, its
// credential and the model ids it serves. The list is replaced wholesale, so
// adding or removing a single id is this same request. An empty apiKey means
// "keep the stored credential": the client never received the key, so it
// cannot echo it back, and keeping it is what makes a typo'd endpoint
// correctable without re-entering the credential. public=true is the other
// direction -- it clears the credential and deletes its Secret.
func (s *Server) handleUpdateLLM(w http.ResponseWriter, r *http.Request, name string) {
	var body llmRequest
	if !decodeJSONBody(w, r, &body) {
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
	for i := range tmpl.Spec.Providers {
		if tmpl.Spec.Providers[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("provider %q not found", name)})
		return
	}
	current := tmpl.Spec.Providers[idx]
	if body.APIKey == "" && !body.Public && current.CredentialRef == nil {
		// Neither a key to keep nor a declaration that none is wanted. Refuse,
		// exactly as an add would: a model with no credential fails every turn
		// with "No API key resolved".
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "apiKey is required unless the model is declared public (public=true)"})
		return
	}

	provider := current
	provider.Endpoint = endpoint
	// A PUT always carries the full list, so this replaces the model list
	// rather than merging into it: removing one id and adding another are the
	// same request. An absent or empty list is a client error -- "keep the
	// current ones" would be the one way to reach a provider with no ids, which
	// renders nothing and is unselectable.
	provider.Models = normalizeModels(body.Models)
	if body.APIKey != "" {
		// Reuse the provider's own reference so a credential Secret that was not
		// named by this API (a hand-edited CR) keeps working.
		if current.CredentialRef != nil && current.CredentialRef.Name != "" {
			provider.CredentialRef = current.CredentialRef
		} else {
			provider.CredentialRef = &corev1.LocalObjectReference{Name: llmCredentialName(name)}
		}
	} else if body.Public {
		provider.CredentialRef = nil
	}
	if err := provider.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// An edit can drop the very id defaultModel names, and the ref then has to
	// be cleared in this same write -- the delete path's rule, applied to the
	// ids this update removes. It is not that the ref would be merely stale:
	// the CEL XValidation on spec.defaultModel refuses a write whose default
	// names a model the provider no longer lists, so leaving it makes the whole
	// edit fail with a 500 instead of storing the new list. A default naming
	// another provider is left alone -- this edit says nothing about it.
	served, kept := providerModelRefs(current), providerModelRefs(provider)

	// The ids this edit removes can also be selected by an instance, the
	// situation the delete path refuses outright: SelectedModelFor is
	// fail-closed, so the affected user's next turn fails with `model "..."
	// is not available in template ...` instead of falling back. Dropping a
	// single id is a one-click action in the Portal, so the delete path's rule
	// is applied here -- to the refs this edit actually removes, before the
	// write, so a refusal leaves the catalog exactly as it was.
	if dropped := droppedModelRefs(tmpl.Spec.Providers, idx, served, kept); len(dropped) > 0 {
		selecting, err := s.instancesSelecting(r.Context(), dropped)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if len(selecting) > 0 {
			writeModelSelectionConflict(w, name, selecting)
			return
		}
	}
	if tmpl.Spec.DefaultModel != "" && served[tmpl.Spec.DefaultModel] && !kept[tmpl.Spec.DefaultModel] {
		tmpl.Spec.DefaultModel = ""
	}

	// Template first, then the Secret -- the same ordering as handleAddLLM:
	// the template decides whether the model exists at all, so a failure after
	// it leaves a recoverable state (an orphaned Secret) rather than a model
	// the operator skips for a missing credential.
	tmpl.Spec.Providers[idx] = provider
	if err := s.cr.Update(r.Context(), &tmpl); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("update template: %v", err)})
		return
	}
	warning := ""
	switch {
	case body.APIKey != "":
		if err := upsertLLMCredential(r.Context(), s, provider.CredentialRef.Name, body.APIKey); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("update credential Secret: %v", err)})
			return
		}
	case body.Public && current.CredentialRef != nil:
		// Demoted to public: the credential must not outlive the provider's
		// reference to it.
		if w := removeProviderCredential(r.Context(), s, name, current.CredentialRef); w != "" {
			warning = "provider updated, but its " + w
		}
	}
	resp := map[string]any{"provider": provider}
	if warning != "" {
		resp["warning"] = warning
	}
	writeJSON(w, http.StatusOK, resp)
}

// llmCredentialName is the credential Secret name for a provider. The name is
// derived from the provider name, which is immutable, so it never drifts.
func llmCredentialName(providerName string) string {
	return "llm-" + providerName
}

// removeProviderCredential deletes the credential Secret a provider owns, and
// returns a warning for the response ("" when there is nothing to report). It is
// called after the provider change is already committed, so a failure is
// reported rather than raised -- an error response would read as "the change
// failed" when it did not.
//
// Only the Secret this API names after the provider (llm-<name>) is removed. A
// CR hand-edited to point a provider at a Secret it shares with another
// provider (the builtin's cubepilot-llm, say) must not lose that Secret when
// this provider goes: the other provider would silently lose its credential.
// Removing a single model id never reaches here -- that is an update, not a
// delete, so a provider's credential always outlives its model list.
func removeProviderCredential(ctx context.Context, s *Server, providerName string, ref *corev1.LocalObjectReference) string {
	if ref == nil || ref.Name == "" {
		return ""
	}
	owned := llmCredentialName(providerName)
	if ref.Name != owned {
		return fmt.Sprintf("credential Secret %q is not the platform-managed %q and was left in place", ref.Name, owned)
	}
	if err := deleteLLMCredential(ctx, s, ref.Name); err != nil {
		return fmt.Sprintf("credential Secret %q could not be deleted: %v", ref.Name, err)
	}
	return ""
}

// deleteLLMCredential removes a credential Secret. A missing Secret is success:
// the goal is that it does not exist.
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

// providerModelRefs is the set of <provider>/<modelId> refs a provider serves.
// A stored selection and spec.defaultModel are both such refs, so both paths
// that have to compare against them build the set the same way -- with
// gateway.ModelKey, which leaves an already-prefixed id alone. A ref built by
// concatenation would miss the id the renderer treats as that provider's.
func providerModelRefs(p v1alpha1.TemplateProviderSpec) map[string]bool {
	refs := make(map[string]bool, len(p.Models))
	for _, id := range p.Models {
		refs[gateway.ModelKey(p.Name, id)] = true
	}
	return refs
}

// droppedModelRefs is the set of refs an edit to providers[edited] takes out of
// the catalog: a ref that provider served before (served), that its new list no
// longer serves (kept), and that no other provider of the same template serves
// either. A dropped ref another provider still serves is left out of the set:
// resolveModel scans every provider of the template, so a user selecting that
// ref keeps resolving and the edit strands nobody. Only a ref that is nowhere
// left to be found can break a selection, so only that one is ever worth
// refusing.
//
// The "another provider still serves it" exclusion is unreachable through this
// API: a ref's first segment is the lowercased provider name, provider names
// are unique within the template and are DNS-1123 labels, so no two providers
// of one template can serve the same ref. The branch is kept anyway, because a
// hand-written object in that state (two entries sharing a name, which the
// listMapKey on spec.providers forbids) must not have a harmless edit turned
// into a refusal by it. The test that covers the branch seeds that state
// directly -- the API server cannot produce it.
func droppedModelRefs(providers []v1alpha1.TemplateProviderSpec, edited int, served, kept map[string]bool) map[string]bool {
	other := map[string]bool{}
	for i := range providers {
		if i == edited {
			continue
		}
		for ref := range providerModelRefs(providers[i]) {
			other[ref] = true
		}
	}
	dropped := make(map[string]bool, len(served))
	for ref := range served {
		if !kept[ref] && !other[ref] {
			dropped[ref] = true
		}
	}
	return dropped
}

// writeModelSelectionConflict is the 409 shared by both paths that remove model
// refs: a delete removes every id of a provider, an update removes the ids its
// new list drops, and either strands the users still selecting one of them. The
// Portal renders the message verbatim, so it names who is blocking rather than
// only counting them, and the structured instances array is what lets the card
// offer to re-point each selection.
func writeModelSelectionConflict(w http.ResponseWriter, providerName string, selecting []modelInstanceRef) {
	who := make([]string, 0, len(selecting))
	for _, sel := range selecting {
		if sel.Owner != "" {
			who = append(who, sel.Owner)
		} else {
			who = append(who, sel.Name)
		}
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"error": fmt.Sprintf("provider %q serves a model selected by %s; select another model there first",
			providerName, strings.Join(who, ", ")),
		"instances": selecting,
	})
}

// modelInstanceRef names an instance that selects a model, for the refusal
// body both removal paths answer with.
type modelInstanceRef struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

// instancesSelecting lists the AgentInstances of the builtin template that
// explicitly select one of the given model refs. Instances bound to another
// template are ignored: their selection resolves against that template, so this
// one cannot break it.
func (s *Server) instancesSelecting(ctx context.Context, refs map[string]bool) ([]modelInstanceRef, error) {
	var list v1alpha1.AgentInstanceList
	if err := s.cr.List(ctx, &list, client.InNamespace(s.cfg.Namespace)); err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	out := []modelInstanceRef{}
	for _, inst := range list.Items {
		if inst.Spec.TemplateRef == v1alpha1.DefaultAgentName && refs[inst.Spec.SelectedModel] {
			out = append(out, modelInstanceRef{Name: inst.Name, Owner: inst.Spec.Owner})
		}
	}
	return out, nil
}

// handleDeleteLLM removes a provider and its credential Secret (issue #170),
// and with it every model id the provider serves. It refuses while an instance
// selects any of those ids: SelectedModelFor is fail-closed, so that user's
// turns would start failing with a resolver error instead of falling back. The
// response names the instances, so the admin (or the user) can re-point the
// selection first.
func (s *Server) handleDeleteLLM(w http.ResponseWriter, r *http.Request, name string) {
	var tmpl v1alpha1.AgentTemplate
	if err := s.cr.Get(r.Context(), types.NamespacedName{Namespace: s.cfg.Namespace, Name: v1alpha1.DefaultAgentName}, &tmpl); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("builtin template: %v", err)})
		return
	}
	idx := -1
	for i := range tmpl.Spec.Providers {
		if tmpl.Spec.Providers[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("provider %q not found", name)})
		return
	}

	// The refusal covers every id the provider serves: deleting the provider
	// deletes all of them, and SelectedModelFor is fail-closed, so a user still
	// selecting any one of them would start failing with a resolver error
	// instead of falling back. The DefaultModel check below tests the same set:
	// a selection is stored as a <provider>/<modelId> ref, never as a bare
	// provider name.
	removed := tmpl.Spec.Providers[idx]
	refs := providerModelRefs(removed)

	selecting, err := s.instancesSelecting(r.Context(), refs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if len(selecting) > 0 {
		writeModelSelectionConflict(w, name, selecting)
		return
	}

	tmpl.Spec.Providers = append(tmpl.Spec.Providers[:idx], tmpl.Spec.Providers[idx+1:]...)
	if tmpl.Spec.DefaultModel != "" && refs[tmpl.Spec.DefaultModel] {
		// The removed provider may have served the gateway's primary. Clearing
		// the ref is defined: the renderer falls back to the first remaining
		// provider. A dangling ref would leave the CR referencing a model that
		// is gone.
		tmpl.Spec.DefaultModel = ""
	}
	if err := s.cr.Update(r.Context(), &tmpl); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fmt.Sprintf("update template: %v", err)})
		return
	}
	warning := ""
	if w := removeProviderCredential(r.Context(), s, removed.Name, removed.CredentialRef); w != "" {
		warning = "provider removed, but its " + w
	}
	resp := map[string]any{"removed": removed.Name}
	if warning != "" {
		resp["warning"] = warning
	}
	writeJSON(w, http.StatusOK, resp)
}
