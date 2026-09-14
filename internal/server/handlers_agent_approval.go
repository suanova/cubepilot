package server

import (
	"context"
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
// approvalPolicy only "does something" while the channel that pauses writes can
// carry the turn. "up" = established/establishable now; "pairing" = first-time
// device pairing in flight (auto-approves shortly); "down" = the channel cannot
// be reached, so a gated turn fails closed; "unconfigured" = no HITL machinery
// (the API could not bring the channel up).
const (
	approvalChannelUp           = "up"
	approvalChannelPairing      = "pairing"
	approvalChannelDown         = "down"
	approvalChannelUnconfigured = "unconfigured"
)

// approvalView is the Portal's read of the confirmation configuration for the
// caller's default agent instance (issue #116). approvalPolicy/allowlist are
// the *effective* values (what the runtime enforces); override/allowlistOwned
// are the instance's own state (empty = inheriting the template default live).
type approvalView struct {
	Exists         bool                    `json:"exists"`
	ApprovalPolicy v1alpha1.ApprovalPolicy `json:"approvalPolicy"`
	Override       v1alpha1.ApprovalPolicy `json:"override"`
	TemplatePolicy v1alpha1.ApprovalPolicy `json:"templatePolicy"`
	Allowlist      []approvalRule          `json:"allowlist,omitempty"`
	AllowlistOwned []approvalRule          `json:"allowlistOwned,omitempty"`
	Channel        string                  `json:"channel"`
}

// approvalRule is one allowlist rule served to the Portal. Label is set by the
// server ONLY for rules that exactly match a platform builtin read-only rule,
// so the UI never guesses that a user-added rule (which may allow a write) is
// read-only.
type approvalRule struct {
	Pattern    string `json:"pattern"`
	ArgPattern string `json:"argPattern,omitempty"`
	Label      string `json:"label,omitempty"`
}

func toApprovalRules(rules []v1alpha1.AllowlistRule) []approvalRule {
	out := make([]approvalRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, approvalRule{
			Pattern:    r.Pattern,
			ArgPattern: r.ArgPattern,
			Label:      allowlist.BuiltinLabel(r),
		})
	}
	return out
}

// handleAgentApproval serves GET/PUT /api/agent/approval -- the instance owner's
// confirmation posture: an optional approvalPolicy override ("" = follow the
// template) and the instance-owned allowlist ([] = inherit the template's
// effective default live; a non-empty list is owned and authoritative).
func (s *Server) handleAgentApproval(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		view, err := s.approvalView(r.Context(), s.userOf(r))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		view.Channel = s.gatedChannel(r.Context(), s.userOf(r), view.ApprovalPolicy)
		writeJSON(w, http.StatusOK, view)
	case http.MethodPut:
		var body struct {
			ApprovalPolicy v1alpha1.ApprovalPolicy  `json:"approvalPolicy"`
			Allowlist      []v1alpha1.AllowlistRule `json:"allowlist"`
		}
		if !decodeJSONBody(w, r, &body) {
			return
		}
		switch body.ApprovalPolicy {
		case "", v1alpha1.ApprovalPolicyNone, v1alpha1.ApprovalPolicyAllowlist, v1alpha1.ApprovalPolicyAlwaysAsk:
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "approvalPolicy must be None, Allowlist, AlwaysAsk or empty"})
			return
		}
		if err := s.saveConfirm(r.Context(), s.userOf(r), body.ApprovalPolicy, body.Allowlist); err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, errNoInstance) {
				code = http.StatusConflict
			}
			writeJSON(w, code, map[string]any{"error": err.Error()})
			return
		}
		view, err := s.approvalView(r.Context(), s.userOf(r))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		view.Channel = s.gatedChannel(r.Context(), s.userOf(r), view.ApprovalPolicy)
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
func (s *Server) gatedChannel(ctx context.Context, user string, pol v1alpha1.ApprovalPolicy) string {
	switch pol {
	case v1alpha1.ApprovalPolicyAllowlist, v1alpha1.ApprovalPolicyAlwaysAsk:
	default:
		return ""
	}
	// With no HITL manager the channel is unconfigured (after EnableHITL this
	// only happens when the API could not bring the channel up -- a fatal
	// misconfig).
	if s.gatewayConns == nil {
		return approvalChannelUnconfigured
	}
	return s.gatewayConns.channelState(ctx, user)
}

// approvalView resolves the effective + owned confirmation state for a user's
// default instance.
func (s *Server) approvalView(ctx context.Context, user string) (approvalView, error) {
	var view approvalView
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
	view.Override = inst.Spec.ApprovalPolicy
	view.AllowlistOwned = toApprovalRules(inst.Spec.Allowlist)
	if inst.Spec.TemplateRef != "" {
		var def v1alpha1.AgentTemplate
		if err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.cfg.Namespace, Name: inst.Spec.TemplateRef}, &def); err == nil {
			view.TemplatePolicy = def.Spec.ApprovalPolicy
		}
	}
	if s.mgr != nil {
		if cfg, err := s.mgr.ResolvedConfigForUser(ctx, user); err == nil && cfg != nil && !cfg.Empty() {
			view.ApprovalPolicy = cfg.ApprovalPolicy
			view.Allowlist = toApprovalRules(cfg.Allowlist)
		}
	}
	return view, nil
}

// saveConfirm writes the instance's confirmation override and owned allowlist.
// An empty approvalPolicy clears the override (inherit the template); an empty
// allowlist clears ownership (inherit the template's effective default live).
func (s *Server) saveConfirm(ctx context.Context, user string, pol v1alpha1.ApprovalPolicy, al []v1alpha1.AllowlistRule) error {
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
	inst.Spec.ApprovalPolicy = pol
	inst.Spec.Allowlist = allowlist.Merge(nil, al) // sanitize: drop empty patterns, dedupe
	return s.cr.Update(ctx, &inst)
}

// errNoInstance reports a PUT /api/agent/approval against a user with no
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

// allowlistAlways appends an allow-always entry to the instance's own
// allowlist. The inherited defaults are deliberately not copied in: the
// resolver unions them in live, so the append only widens the instance's own
// rules. No-op (false) when the effective policy is not Allowlist (under
// AlwaysAsk everything asks anyway).
func (s *Server) allowlistAlways(ctx context.Context, user string, rule v1alpha1.AllowlistRule) (bool, error) {
	if s.cr == nil {
		return false, nil
	}
	if s.mgr != nil {
		if cfg, err := s.mgr.ResolvedConfigForUser(ctx, user); err == nil && cfg != nil && !cfg.Empty() {
			if cfg.ApprovalPolicy != v1alpha1.ApprovalPolicyAllowlist {
				return false, nil
			}
		}
	}
	name := k8s.InstanceName(user, v1alpha1.DefaultAgentName)
	var inst v1alpha1.AgentInstance
	if err := s.cr.Get(ctx, types.NamespacedName{Namespace: s.cfg.Namespace, Name: name}, &inst); err != nil {
		return false, err
	}
	// Append only to the instance's own rules. Deliberately NOT the resolved
	// effective list: copying that in would write the platform builtin into the
	// spec, and the union would then faithfully include the snapshot, so a
	// builtin later removed from Default() — a hardening — would keep
	// auto-passing here. The union supplies the builtin live instead.
	inst.Spec.Allowlist = allowlist.Merge(inst.Spec.Allowlist, []v1alpha1.AllowlistRule{rule})
	if err := s.cr.Update(ctx, &inst); err != nil {
		return false, err
	}
	return true, nil
}
