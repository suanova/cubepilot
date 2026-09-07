package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/suanova/cubepilot/internal/allowlist"
	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
)

// confirmView is the Portal's read of the confirmation configuration for the
// caller's default agent instance (issue #116). confirmPolicy/allowlist are
// the *effective* values (what the runtime enforces); override/allowlistOwned
// are the instance's own state (empty = inheriting the template default live).
type confirmView struct {
	Exists         bool                     `json:"exists"`
	ConfirmPolicy  v1alpha1.ConfirmPolicy   `json:"confirmPolicy"`
	Override       v1alpha1.ConfirmPolicy   `json:"override"`
	TemplatePolicy v1alpha1.ConfirmPolicy   `json:"templatePolicy"`
	Allowlist      []v1alpha1.AllowlistRule `json:"allowlist,omitempty"`
	AllowlistOwned []v1alpha1.AllowlistRule `json:"allowlistOwned,omitempty"`
}

// handleAgentConfirm serves GET/PUT /api/agent/confirm -- the instance owner's
// confirmation posture: an optional confirmPolicy override ("" = follow the
// template) and the instance-owned allowlist ([] = inherit the template's
// effective default live; a non-empty list is owned and authoritative).
func (s *Server) handleAgentConfirm(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		view, err := s.confirmView(r.Context(), s.userOf(r))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, view)
	case http.MethodPut:
		var body struct {
			ConfirmPolicy v1alpha1.ConfirmPolicy   `json:"confirmPolicy"`
			Allowlist     []v1alpha1.AllowlistRule `json:"allowlist"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON body"})
			return
		}
		switch body.ConfirmPolicy {
		case "", v1alpha1.ConfirmPolicyNone, v1alpha1.ConfirmPolicyAllowlist, v1alpha1.ConfirmPolicyAlwaysAsk:
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "confirmPolicy must be None, Allowlist, AlwaysAsk or empty"})
			return
		}
		if err := s.saveConfirm(r.Context(), s.userOf(r), body.ConfirmPolicy, body.Allowlist); err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, errNoInstance) {
				code = http.StatusConflict
			}
			writeJSON(w, code, map[string]any{"error": err.Error()})
			return
		}
		view, err := s.confirmView(r.Context(), s.userOf(r))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, view)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET or PUT required"})
	}
}

// confirmView resolves the effective + owned confirmation state for a user's
// default instance.
func (s *Server) confirmView(ctx context.Context, user string) (confirmView, error) {
	var view confirmView
	if s.cr == nil {
		return view, nil
	}
	name := k8s.InstanceName(user, v1alpha1.DefaultAgentName)
	var inst v1alpha1.AgentInstance
	err := s.cr.Get(ctx, types.NamespacedName{Name: name}, &inst)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return view, nil // not provisioned
		}
		return view, err
	}
	view.Exists = true
	view.Override = inst.Spec.ConfirmPolicy
	view.AllowlistOwned = inst.Spec.Allowlist
	if inst.Spec.TemplateRef != "" {
		var def v1alpha1.AgentTemplate
		if err := s.cr.Get(ctx, types.NamespacedName{Name: inst.Spec.TemplateRef}, &def); err == nil {
			view.TemplatePolicy = def.Spec.ConfirmPolicy
		}
	}
	if s.mgr != nil {
		if cfg, err := s.mgr.ResolvedConfigForUser(ctx, user); err == nil && cfg != nil && !cfg.Empty() {
			view.ConfirmPolicy = cfg.ConfirmPolicy
			view.Allowlist = cfg.Allowlist
		}
	}
	return view, nil
}

// saveConfirm writes the instance's confirmation override and owned allowlist.
// An empty confirmPolicy clears the override (inherit the template); an empty
// allowlist clears ownership (inherit the template's effective default live).
func (s *Server) saveConfirm(ctx context.Context, user string, pol v1alpha1.ConfirmPolicy, al []v1alpha1.AllowlistRule) error {
	if s.cr == nil {
		return nil
	}
	name := k8s.InstanceName(user, v1alpha1.DefaultAgentName)
	var inst v1alpha1.AgentInstance
	if err := s.cr.Get(ctx, types.NamespacedName{Name: name}, &inst); err != nil {
		if apierrors.IsNotFound(err) {
			return errNoInstance
		}
		return err
	}
	inst.Spec.ConfirmPolicy = pol
	inst.Spec.Allowlist = allowlist.Merge(nil, al) // sanitize: drop empty patterns, dedupe
	return s.cr.Update(ctx, &inst)
}

// errNoInstance reports a PUT /api/agent/confirm against a user with no
// provisioned instance (provision on the Agent Config page first).
var errNoInstance = errors.New("no agent instance yet — provision it on the Agent Config page first")

// deriveAllowAlwaysRule turns a pending command into a conservative allowlist
// entry that matches exactly this invocation: the executable as the pattern and
// the remaining argv anchored verbatim (no separator smuggling is possible).
func deriveAllowAlwaysRule(command string) (v1alpha1.AllowlistRule, bool) {
	f := strings.Fields(command)
	if len(f) == 0 {
		return v1alpha1.AllowlistRule{}, false
	}
	rule := v1alpha1.AllowlistRule{Pattern: f[0]}
	if len(f) == 1 {
		rule.ArgPattern = `^$`
	} else {
		rule.ArgPattern = `^` + regexp.QuoteMeta(strings.Join(f[1:], " ")) + `$`
	}
	return rule, true
}

// allowlistAlways appends an allow-always entry to the user's instance
// allowlist. When the instance was still inheriting (empty owned list) the
// current effective list is materialized first, so appending preserves the
// inherited defaults and only widens. No-op (false) when the effective policy
// is not Allowlist (under AlwaysAsk everything asks anyway).
func (s *Server) allowlistAlways(ctx context.Context, user string, rule v1alpha1.AllowlistRule) (bool, error) {
	if s.cr == nil {
		return false, nil
	}
	if s.mgr != nil {
		if cfg, err := s.mgr.ResolvedConfigForUser(ctx, user); err == nil && cfg != nil && !cfg.Empty() {
			if cfg.ConfirmPolicy != v1alpha1.ConfirmPolicyAllowlist {
				return false, nil
			}
		}
	}
	name := k8s.InstanceName(user, v1alpha1.DefaultAgentName)
	var inst v1alpha1.AgentInstance
	if err := s.cr.Get(ctx, types.NamespacedName{Name: name}, &inst); err != nil {
		return false, err
	}
	base := inst.Spec.Allowlist
	if len(base) == 0 && s.mgr != nil {
		if cfg, err := s.mgr.ResolvedConfigForUser(ctx, user); err == nil && cfg != nil {
			base = cfg.Allowlist // materialize the inherited default on first ownership
		}
	}
	inst.Spec.Allowlist = allowlist.Merge(base, []v1alpha1.AllowlistRule{rule})
	if err := s.cr.Update(ctx, &inst); err != nil {
		return false, err
	}
	return true, nil
}
