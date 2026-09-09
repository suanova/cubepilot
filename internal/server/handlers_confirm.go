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

// Approval-channel state surfaced on the confirm view (issue #127): a gated
// confirmPolicy only "does something" while the channel that pauses writes can
// carry the turn. "up" = established/establishable now; "pairing" = first-time
// device pairing in flight (auto-approves shortly); "down" = the channel cannot
// be reached, so a gated turn fails closed; "unconfigured" = no HITL machinery
// (the API could not bring the channel up).
const (
	confirmChannelUp           = "up"
	confirmChannelPairing      = "pairing"
	confirmChannelDown         = "down"
	confirmChannelUnconfigured = "unconfigured"
)

// confirmView is the Portal's read of the confirmation configuration for the
// caller's default agent instance (issue #116). confirmPolicy/allowlist are
// the *effective* values (what the runtime enforces); override/allowlistOwned
// are the instance's own state (empty = inheriting the template default live).
type confirmView struct {
	Exists         bool                   `json:"exists"`
	ConfirmPolicy  v1alpha1.ConfirmPolicy `json:"confirmPolicy"`
	Override       v1alpha1.ConfirmPolicy `json:"override"`
	TemplatePolicy v1alpha1.ConfirmPolicy `json:"templatePolicy"`
	Allowlist      []confirmRule          `json:"allowlist,omitempty"`
	AllowlistOwned []confirmRule          `json:"allowlistOwned,omitempty"`
	Channel        string                 `json:"channel"`
}

// confirmRule is one allowlist rule served to the Portal. Label is set by the
// server ONLY for rules that exactly match a platform builtin read-only rule,
// so the UI never guesses that a user-added rule (which may allow a write) is
// read-only.
type confirmRule struct {
	Pattern    string `json:"pattern"`
	ArgPattern string `json:"argPattern,omitempty"`
	Label      string `json:"label,omitempty"`
}

func toConfirmRules(rules []v1alpha1.AllowlistRule) []confirmRule {
	out := make([]confirmRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, confirmRule{
			Pattern:    r.Pattern,
			ArgPattern: r.ArgPattern,
			Label:      allowlist.BuiltinLabel(r),
		})
	}
	return out
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
		view.Channel = s.gatedChannel(r.Context(), s.userOf(r), view.ConfirmPolicy)
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
		view.Channel = s.gatedChannel(r.Context(), s.userOf(r), view.ConfirmPolicy)
		writeJSON(w, http.StatusOK, view)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET or PUT required"})
	}
}

// gatedChannel reports the approval-channel state for the confirm view, but only
// when the effective policy is actually gated: a None (or not-yet-provisioned)
// instance has no gating to enforce, so probing would only add a needless
// gateway dial (and seed a device pairing) for users who will never be gated.
// It is empty otherwise.
func (s *Server) gatedChannel(ctx context.Context, user string, pol v1alpha1.ConfirmPolicy) string {
	switch pol {
	case v1alpha1.ConfirmPolicyAllowlist, v1alpha1.ConfirmPolicyAlwaysAsk:
	default:
		return ""
	}
	// With no HITL manager the channel is unconfigured (after EnableHITL this
	// only happens when the API could not bring the channel up -- a fatal
	// misconfig).
	if s.hitl == nil {
		return confirmChannelUnconfigured
	}
	return s.hitl.channelState(ctx, user)
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
	err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.cfg.Namespace, Name: name}, &inst)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return view, nil // not provisioned
		}
		return view, err
	}
	view.Exists = true
	view.Override = inst.Spec.ConfirmPolicy
	view.AllowlistOwned = toConfirmRules(inst.Spec.Allowlist)
	if inst.Spec.TemplateRef != "" {
		var def v1alpha1.AgentTemplate
		if err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.cfg.Namespace, Name: inst.Spec.TemplateRef}, &def); err == nil {
			view.TemplatePolicy = def.Spec.ConfirmPolicy
		}
	}
	if s.mgr != nil {
		if cfg, err := s.mgr.ResolvedConfigForUser(ctx, user); err == nil && cfg != nil && !cfg.Empty() {
			view.ConfirmPolicy = cfg.ConfirmPolicy
			view.Allowlist = toConfirmRules(cfg.Allowlist)
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
	if err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.cfg.Namespace, Name: name}, &inst); err != nil {
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
	if err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.cfg.Namespace, Name: name}, &inst); err != nil {
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
