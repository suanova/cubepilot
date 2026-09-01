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

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
)

// ResolvedSkill is one enabled skill's content reference (design §3.4: the
// content lives in the repository; the supervisor pulls the tar by name and
// extracts it into workspace/skills/<name>/).
type ResolvedSkill struct {
	Name     string `json:"name"`
	Path     string `json:"path,omitempty"` // repo-relative tar path (skills/<name>/vN.tar.gz)
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
	// ModelName is the catalog model name backing SelectedModel, or the
	// template default model name when nothing was explicitly selected
	// (display only).
	ModelName string `json:"modelName,omitempty"`
	// ConfirmPolicy is the agent-level write confirmation policy.
	ConfirmPolicy v1alpha1.ConfirmPolicy `json:"confirmPolicy,omitempty"`
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

// ResolvedCredential maps a rendered apiKey key name (k8s.EnvNameForModel, the
// keys.json JSON key) to the Secret holding its value.
type ResolvedCredential struct {
	Env        string `json:"env"`
	SecretName string `json:"secretName"`
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
	cr client.Client
}

// New returns a Resolver backed by the controller-runtime client.
func New(cr client.Client) *Resolver {
	return &Resolver{cr: cr}
}

// ResolveForUser resolves the default agent instance config for a user. A
// missing instance yields an empty config (runtime default), never an error.
func (r *Resolver) ResolveForUser(ctx context.Context, user string) (*ResolvedAgentConfig, error) {
	return r.Resolve(ctx, user, v1alpha1.DefaultAgentName)
}

// Resolve merges AgentTemplate + AgentInstance + Model catalog + Skills
// for (user, agent). Fail-closed: an explicit selection that is outside the
// agent's availableModels, missing from the catalog, or Unreachable is an
// error -- never a silent fallback. An empty selection (no instance, no
// explicit selection, no agent default) is not an error.
func (r *Resolver) Resolve(ctx context.Context, user, agent string) (*ResolvedAgentConfig, error) {
	instanceName := k8s.InstanceName(user, agent)

	var inst v1alpha1.AgentInstance
	err := r.cr.Get(ctx, types.NamespacedName{Name: instanceName}, &inst)
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

	// Template constraints (defaultModel / models / confirmPolicy /
	// instructions). A missing template contributes no constraints
	// (phase-one compatibility).
	if inst.Spec.TemplateRef != "" {
		cfg.Agent = inst.Spec.TemplateRef
		var def v1alpha1.AgentTemplate
		if err := r.cr.Get(ctx, types.NamespacedName{Name: inst.Spec.TemplateRef}, &def); err == nil {
			cfg.ConfirmPolicy = def.Spec.ConfirmPolicy
			cfg.Instructions = def.Spec.Instructions
			// Credential mapping for the gateway's file secret provider: the
			// supervisor reads these Secrets and writes keys.json into the
			// pod's emptyDir (design §6).
			for _, m := range def.Spec.Models {
				// Match the renderer's eligibility rule: models with an empty
				// endpoint are dropped from the rendered config, so their
				// credentials must not appear either (a missing Secret on an
				// ineligible model would otherwise block valid ones).
				if m.Endpoint == "" || m.CredentialRef == nil || m.CredentialRef.Name == "" {
					continue
				}
				cfg.Credentials = append(cfg.Credentials, ResolvedCredential{
					Env:        k8s.EnvNameForModel(m.Name),
					SecretName: m.CredentialRef.Name,
				})
			}
			// User instructions append after the template instructions
			// (design §3.2: final instructions = platform safety & execution
			// constraints + template instructions + user instructions, combined
			// in order; user instructions cannot remove or weaken the safety
			// boundary).
			if ui := strings.TrimSpace(inst.Spec.UserInstructions); ui != "" {
				if cfg.Instructions != "" {
					cfg.Instructions += "\n\n"
				}
				cfg.Instructions += ui
			}
			// Model selection: only an explicitly chosen model
			// (instance.selectedModel) is sent as the per-turn override, so the
			// agent's default model is whatever the runtime's configured primary
			// is -- no provider-key naming convention is required. The template
			// default is kept as the display name only. Fail-closed still
			// applies to explicit selections (outside the template models ->
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

	// Domain skills visible to this agent (empty Agents = visible to
	// all; atomic skills are overlays, not skills). The instance may
	// further restrict to an explicit enabledSkills subset (design §3.2);
	// empty = all declared/all visible.
	var skills v1alpha1.SkillList
	if err := r.cr.List(ctx, &skills); err != nil {
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
		if len(restrict) > 0 && !restrict[skill.Name] {
			continue
		}
		cfg.Skills = append(cfg.Skills, ResolvedSkill{
			Name:     skill.Name,
			Path:     skill.Spec.Source.Path,
			Revision: skill.Revision(),
		})
	}

	cfg.Revision = cfg.fingerprint()
	return cfg, nil
}

// resolveModel validates the selection against the template's inline models
// list and returns the effective override ref. Fail-closed: not in models
// list -> error. The returned ref is the full gateway ref `<name>/<name>` --
// the same string the gateway renderer puts in the allowlist, so the override
// always matches (the coupling issue #6 removes).
func (r *Resolver) resolveModel(selected string, def v1alpha1.AgentTemplate) (string, error) {
	for _, m := range def.Spec.Models {
		if m.Name == selected {
			return m.Name + "/" + m.Name, nil
		}
	}
	return "", fmt.Errorf("model %q is not available in template %q (add it under Agent config -> LLM Config, then select it again)", selected, def.Name)
}
