// Package resolver merges the per-instance effective configuration into a
// single immutable artifact (design §3.2/§3.3): AgentTemplate + AgentInstance
// + Model catalog + Skills -> ResolvedAgentConfig. It is a pure function
// over CRs -- it never writes anything (no CRD updates, no ConfigMaps). The
// agent-side supervisor pulls ResolvedAgentConfig via the internal API and
// renders it into runtime form (skills etc.); the API and runner use the same
// artifact for the selectedModel override.
package resolver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/allowlist"
	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/gateway"
	"github.com/suanova/cubepilot/internal/grants"
	"github.com/suanova/cubepilot/internal/k8s"
)

// ResolvedSkill is one enabled skill's content reference (design §3.4: the
// content lives in the repository; the supervisor pulls the tar by name and
// extracts it into workspace/skills/<name>/).
type ResolvedSkill struct {
	Name     string `json:"name"`
	Path     string `json:"path,omitempty"`   // repo-relative tar path (skills/<name>/vN.tar.gz)
	Sha256   string `json:"sha256,omitempty"` // content fingerprint, when published/backfilled
	Revision string `json:"revision"`
}

// ResolvedAgentConfig is the immutable, fully-resolved configuration for one
// agent instance -- the single artifact the runtime depends on. Revision is a
// content hash of everything else: any CR change (agent template, instance,
// model, skill) produces a new revision, which is the supervisor's
// "reload needed" signal. It is pure data -- never persisted to CRDs or
// ConfigMaps.
type ResolvedAgentConfig struct {
	// Revision is the content fingerprint of the resolved config (12 hex).
	Revision string `json:"revision"`
	// Agent is the agent definition name (AgentTemplate).
	Agent string `json:"agent"`
	// Instance is the AgentInstance name.
	Instance string `json:"instance"`
	// Owner is the instance owner (user).
	Owner string `json:"owner"`
	// SelectedModel is the per-turn model override, set only when the instance
	// explicitly selected a model (empty = no override; the runtime uses its
	// configured primary).
	SelectedModel string `json:"selectedModel,omitempty"`
	// ModelName is the model ref backing SelectedModel, or the template
	// default model ref when nothing was explicitly selected (display only).
	ModelName string `json:"modelName,omitempty"`
	// ApprovalPolicy is the agent's effective confirmation intent (template
	// default unless the instance overrides it; issue #116).
	ApprovalPolicy v1alpha1.ApprovalPolicy `json:"approvalPolicy,omitempty"`
	// Allowlist is the agent's effective safe-command allowlist (issue #116):
	// the union of the platform builtin, the template's allowlist, the
	// instance's own hand-authored rules and the instance's learned grants. The
	// instance adds to it and cannot remove from it, so an empty instance list
	// means "adds nothing". Only enforced under Allowlist policy.
	Allowlist []v1alpha1.AllowlistRule `json:"allowlist,omitempty"`
	// DevicePublicKey is the platform's operator device public key for this
	// agent's gateway (gateway channel, issue #20). Transport-only: filled by
	// the API when serving the internal config (the supervisor uses it to
	// approve the device pairing), never part of resolver output.
	DevicePublicKey string `json:"devicePublicKey,omitempty"`
	// Instructions is the agent definition's default system prompt.
	Instructions string `json:"instructions,omitempty"`
	// Skills are the domain skills visible to this agent
	// (empty = no domain skills; atomic skills are overlays and do
	// not appear here).
	Skills []ResolvedSkill `json:"skills,omitempty"`
	// Credentials lists the model credential Secrets the supervisor must read
	// to deliver apiKeys to the gateway's file secret provider (design §6).
	Credentials []ResolvedCredential `json:"credentials,omitempty"`
}

// ResolvedCredential maps a rendered apiKey key name (k8s.EnvNameForProvider,
// the keys.json JSON key) to the Secret holding its value.
type ResolvedCredential struct {
	Env        string `json:"env"`
	SecretName string `json:"secretName"`
}

// MergeInstructions composes the managed AGENTS.md section's instructions from the
// template's default and the instance's user instructions: both trimmed, joined by a
// blank line. Resolve renders through it and the API's size checks validate through it.
func MergeInstructions(template, user string) string {
	t := strings.TrimSpace(template)
	u := strings.TrimSpace(user)
	switch {
	case t == "":
		return u
	case u == "":
		return t
	}
	return t + "\n\n" + u
}

// Empty reports whether the config is the zero default (no instance -- the
// runtime keeps its normal configured model and skills).
func (c *ResolvedAgentConfig) Empty() bool {
	return c.Instance == "" && c.Agent == "" && c.SelectedModel == ""
}

// fingerprint hashes the config contents (Revision excluded) -- 12 hex, the
// same scheme as v1alpha1 spec revisions, so revision strings are comparable
// across objects and runs.
func (c *ResolvedAgentConfig) fingerprint() string {
	clone := *c
	clone.Revision = ""
	b, err := json.Marshal(clone)
	if err != nil {
		return "unknown"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// Resolver resolves ResolvedAgentConfig from CRs.
type Resolver struct {
	cr     client.Client
	ns     string
	grants *grants.Store
}

// New returns a Resolver backed by the controller-runtime client, reading
// platform CRs from namespace (namespaced CRD scope, issue #146).
func New(cr client.Client, namespace string) *Resolver {
	return &Resolver{cr: cr, ns: namespace, grants: grants.New(cr, namespace)}
}

// ResolveForUser resolves the default agent instance config for a user. A
// missing instance yields an empty config (runtime default), never an error.
func (r *Resolver) ResolveForUser(ctx context.Context, user string) (*ResolvedAgentConfig, error) {
	return r.Resolve(ctx, user, v1alpha1.DefaultAgentName)
}

// Resolve merges AgentTemplate + AgentInstance + Skills for (user, agent).
// Fail-closed: an explicit selection that no provider of the agent definition
// serves is an error -- never a silent fallback. An empty selection (no
// instance, no explicit selection, no agent default) is not an error.
func (r *Resolver) Resolve(ctx context.Context, user, agent string) (*ResolvedAgentConfig, error) {
	instanceName := k8s.InstanceName(user, agent)

	var inst v1alpha1.AgentInstance
	err := r.cr.Get(ctx, types.NamespacedName{Namespace: r.ns, Name: instanceName}, &inst)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &ResolvedAgentConfig{}, nil // not provisioned -- runtime default
		}
		return nil, fmt.Errorf("get instance %s: %w", instanceName, err)
	}

	cfg := &ResolvedAgentConfig{
		Agent:    agent,
		Instance: instanceName,
		Owner:    inst.Spec.Owner,
	}

	// Template constraints (defaultModel / models / approvalPolicy /
	// instructions). A missing template contributes no constraints
	// (phase-one compatibility).
	var tmplAllowlist []v1alpha1.AllowlistRule
	if inst.Spec.TemplateRef != "" {
		cfg.Agent = inst.Spec.TemplateRef
		var def v1alpha1.AgentTemplate
		if err := r.cr.Get(ctx, types.NamespacedName{Namespace: r.ns, Name: inst.Spec.TemplateRef}, &def); err == nil {
			cfg.ApprovalPolicy = def.Spec.ApprovalPolicy
			tmplAllowlist = def.Spec.Allowlist
			cfg.Instructions = def.Spec.Instructions
			// Credential mapping for the gateway's file secret provider:
			// the supervisor reads these Secrets and writes keys.json into
			// the pod's emptyDir (design §6). One entry per provider -- the
			// credential is the provider's, not the model's.
			for _, pr := range def.Spec.Providers {
				// Match the renderer's eligibility rule: providers with an
				// empty endpoint (or no models) are dropped from the
				// rendered config, so their credentials must not appear
				// either (a missing Secret on an ineligible provider would
				// otherwise block valid ones).
				if pr.Endpoint == "" || len(pr.Models) == 0 ||
					pr.CredentialRef == nil || pr.CredentialRef.Name == "" {
					continue
				}
				cfg.Credentials = append(cfg.Credentials, ResolvedCredential{
					Env:        k8s.EnvNameForProvider(pr.Name),
					SecretName: pr.CredentialRef.Name,
				})
			}
			// Final instructions are the template's with the user's appended (design
			// §3.2), composed by MergeInstructions. There is no platform-level text
			// layer: the safety boundary is mechanism, not prompt text.
			cfg.Instructions = MergeInstructions(def.Spec.Instructions, inst.Spec.UserInstructions)
			// Model selection: only an explicitly chosen model
			// (instance.selectedModel) is sent as the per-turn override, so the
			// agent's default model is whatever the runtime's configured primary
			// is -- no provider-key naming convention is required. The template
			// default is kept as the display name only. Fail-closed still
			// applies to explicit selections (outside the template providers ->
			// error).
			if selected := strings.TrimSpace(inst.Spec.SelectedModel); selected != "" {
				modelID, err := r.resolveModel(selected, def)
				if err != nil {
					return nil, err
				}
				cfg.SelectedModel = modelID
				cfg.ModelName = selected
			} else {
				cfg.ModelName = def.Spec.DefaultModel
			}
		} else if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get template %s: %w", inst.Spec.TemplateRef, err)
		}
	}

	// Confirmation intent & allowlist (issue #185): an instance override wins
	// over the template default; the effective allowlist is the union of the
	// platform builtin, the template's additions, the instance's own
	// additions and the user's learned grants.
	if inst.Spec.ApprovalPolicy != "" {
		cfg.ApprovalPolicy = inst.Spec.ApprovalPolicy
	}
	// A failed grants read is fatal on purpose, not an error to degrade past.
	// Resolve backs ResolvedConfigForUser (the policy push) but also
	// SelectedModelFor, which the interactive turn, the one-shot path, the
	// gateway-config endpoint and every scheduled task call -- so a ConfigMap
	// fault fails model selection and scheduled runs too. Unioning nothing on a
	// failed read would be cheaper, but it would enforce a list different from
	// the one the API shows the user, which is the fail-open direction.
	records, err := r.grants.List(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("list grants for %s: %w", user, err)
	}
	learnedRules := make([]v1alpha1.AllowlistRule, 0, len(records))
	for _, rec := range records {
		learnedRules = append(learnedRules, rec.Rule())
	}
	cfg.Allowlist = allowlist.Effective(tmplAllowlist, inst.Spec.Allowlist, learnedRules)

	// Domain skills visible to this agent (empty Agents = visible to
	// all; atomic skills are overlays, not skills). The instance may
	// further restrict to an explicit enabledSkills subset (design §3.2);
	// empty = all declared/all visible.
	var skills v1alpha1.SkillList
	if err := r.cr.List(ctx, &skills, client.InNamespace(r.ns)); err != nil {
		return nil, fmt.Errorf("list skills: %w", err)
	}
	restrict := map[string]bool{}
	if len(inst.Spec.EnabledSkills) > 0 {
		for _, name := range inst.Spec.EnabledSkills {
			restrict[name] = true
		}
	}
	for i := range skills.Items {
		skill := &skills.Items[i]
		// Unreachable content (missing/moved tar) is excluded; empty phase
		// (e.g. a manually applied skill) is treated as available.
		if skill.Status.Phase == v1alpha1.SkillPhaseUnreachable {
			continue
		}
		if len(restrict) > 0 && !restrict[skill.Name] {
			continue
		}
		cfg.Skills = append(cfg.Skills, ResolvedSkill{
			Name:     skill.Name,
			Path:     skill.Spec.Source.Path,
			Sha256:   skill.Spec.Source.Sha256,
			Revision: skill.Revision(),
		})
	}

	cfg.Revision = cfg.fingerprint()
	return cfg, nil
}

// resolveModel validates the selection ref against the template's providers and
// returns the effective override ref. Fail-closed: an unknown provider, or an
// id the named provider does not serve, is an error. The returned ref is the
// same <provider>/<modelId> string the renderer puts in the allowlist, so the
// override always matches.
func (r *Resolver) resolveModel(selected string, def v1alpha1.AgentTemplate) (string, error) {
	for _, pr := range def.Spec.Providers {
		for _, id := range pr.Models {
			if ref := gateway.ModelKey(pr.Name, id); ref == selected {
				return ref, nil
			}
		}
	}
	return "", fmt.Errorf("model %q is not available in template %q (add it under Agent config -> LLM Config, then select it again)", selected, def.Name)
}
