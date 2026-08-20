// Package resolver merges the per-instance effective configuration into a
// single immutable artifact (design §3.2/§3.3): AgentTemplate + AgentInstance
// + Model catalog + Capabilities → ResolvedAgentConfig. It is a pure function
// over CRs — it never writes anything (no CRD updates, no ConfigMaps). The
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

// ResolvedCapability is the domain capability content an agent may use —
// the skill source. The supervisor renders it into workspace/skills/<name>/.
type ResolvedCapability struct {
	Name         string                    `json:"name"`
	Title        string                    `json:"title,omitempty"`
	Description  string                    `json:"description,omitempty"`
	Instructions string                    `json:"instructions,omitempty"`
	Uses         []string                  `json:"uses,omitempty"`
	Files        []v1alpha1.CapabilityFile `json:"files,omitempty"`
	Revision     string                    `json:"revision"`
}

// ResolvedAgentConfig is the immutable, fully-resolved configuration for one
// agent instance — the single artifact the runtime depends on. Revision is a
// content hash of everything else: any CR change (agent template, instance,
// model, capability) produces a new revision, which is the supervisor's
// "reload needed" signal. It is pure data — never persisted to CRDs or
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
	// SelectedModel is the effective backend model id (empty = runtime
	// default; only set when an explicit/default selection resolved).
	SelectedModel string `json:"selectedModel,omitempty"`
	// ModelName is the catalog entry name backing SelectedModel.
	ModelName string `json:"modelName,omitempty"`
	// ConfirmPolicy is the agent-level write confirmation policy.
	ConfirmPolicy v1alpha1.ConfirmPolicy `json:"confirmPolicy,omitempty"`
	// Instructions is the agent definition's default system prompt.
	Instructions string `json:"instructions,omitempty"`
	// Capabilities are the domain capabilities visible to this agent
	// (empty = no domain skills; atomic capabilities are overlays and do
	// not appear here).
	Capabilities []ResolvedCapability `json:"capabilities,omitempty"`
}

// Empty reports whether the config is the zero default (no instance — the
// runtime keeps its normal configured model and skills).
func (c *ResolvedAgentConfig) Empty() bool {
	return c.Instance == "" && c.Agent == "" && c.SelectedModel == ""
}

// fingerprint hashes the config contents (Revision excluded) — 12 hex, the
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

// Resolve merges AgentTemplate + AgentInstance + Model catalog + Capabilities
// for (user, agent). Fail-closed: an explicit selection that is outside the
// agent's availableModels, missing from the catalog, or Unreachable is an
// error — never a silent fallback. An empty selection (no instance, no
// explicit selection, no agent default) is not an error.
func (r *Resolver) Resolve(ctx context.Context, user, agent string) (*ResolvedAgentConfig, error) {
	instanceName := k8s.InstanceName(user, agent)

	var inst v1alpha1.AgentInstance
	err := r.cr.Get(ctx, types.NamespacedName{Name: instanceName}, &inst)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return &ResolvedAgentConfig{}, nil // not provisioned — runtime default
		}
		return nil, fmt.Errorf("get instance %s: %w", instanceName, err)
	}

	cfg := &ResolvedAgentConfig{
		Agent:    agent,
		Instance: instanceName,
		Owner:    inst.Spec.Owner,
	}

	// Agent definition constraints (defaultModel / availableModels /
	// confirmPolicy / instructions). A missing definition contributes no
	// constraints (phase-one compatibility).
	if inst.Spec.AgentRef != "" {
		cfg.Agent = inst.Spec.AgentRef
		var def v1alpha1.Agent
		if err := r.cr.Get(ctx, types.NamespacedName{Name: inst.Spec.AgentRef}, &def); err == nil {
			cfg.ConfirmPolicy = def.Spec.ConfirmPolicy
			cfg.Instructions = def.Spec.Instructions
			// Model selection: instance override → agent default → none.
			selected := inst.Spec.SelectedModel
			if selected == "" {
				selected = def.Spec.DefaultModel
			}
			if selected != "" {
				modelID, err := r.resolveModel(ctx, selected, def)
				if err != nil {
					return nil, err
				}
				cfg.SelectedModel = modelID
				cfg.ModelName = selected
			}
		} else if !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get agent %s: %w", inst.Spec.AgentRef, err)
		}
	}

	// Domain capabilities visible to this agent (empty Agents = visible to
	// all; atomic capabilities are overlays, not skills).
	var caps v1alpha1.CapabilityList
	if err := r.cr.List(ctx, &caps); err != nil {
		return nil, fmt.Errorf("list capabilities: %w", err)
	}
	for i := range caps.Items {
		cap := &caps.Items[i]
		if cap.Spec.Type != v1alpha1.CapabilityDomain {
			continue
		}
		if len(cap.Spec.Agents) > 0 && !contains(cap.Spec.Agents, cfg.Agent) {
			continue
		}
		cfg.Capabilities = append(cfg.Capabilities, ResolvedCapability{
			Name:         cap.Name,
			Title:        cap.Spec.Title,
			Description:  cap.Spec.Description,
			Instructions: cap.Spec.Instructions,
			Uses:         cap.Spec.Uses,
			Files:        cap.Spec.Files,
			Revision:     cap.Revision(),
		})
	}

	cfg.Revision = cfg.fingerprint()
	return cfg, nil
}

// resolveModel validates the selection against the agent's availableModels
// allowlist and the Model catalog, returning the effective backend model id.
// Fail-closed: outside allowlist / not in catalog / Unreachable → error.
func (r *Resolver) resolveModel(ctx context.Context, selected string, def v1alpha1.Agent) (string, error) {
	if len(def.Spec.AvailableModels) > 0 && !contains(def.Spec.AvailableModels, selected) {
		return "", fmt.Errorf("model %q not in agent %q availableModels", selected, def.Name)
	}
	var model v1alpha1.Model
	if err := r.cr.Get(ctx, types.NamespacedName{Name: selected}, &model); err != nil {
		return "", fmt.Errorf("model %q not in catalog: %v", selected, err)
	}
	if model.Status.Phase == v1alpha1.ModelUnreachable {
		return "", fmt.Errorf("model %q unavailable: %s", selected, model.Status.Message)
	}
	if model.Spec.ModelID == "" {
		return "", nil // platform default, no override
	}
	return model.Spec.ModelID, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// RenderSkill converts a resolved domain capability into an OpenClaw
// SKILL.md (design §3.3.1: the skill name equals the Capability name so
// platform accounting and runtime skills share one identity). The body is
// the capability instructions; description + uses feed the frontmatter.
func RenderSkill(cap ResolvedCapability) (string, error) {
	if cap.Name == "" {
		return "", fmt.Errorf("capability has no name")
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "name: %s\n", cap.Name)
	if cap.Description != "" {
		fmt.Fprintf(&b, "description: %s\n", cap.Description)
	}
	if len(cap.Uses) > 0 {
		b.WriteString("metadata:\n  openclaw:\n    requires:\n")
		for _, u := range cap.Uses {
			fmt.Fprintf(&b, "      - %s\n", u)
		}
	}
	b.WriteString("---\n\n")
	if cap.Title != "" {
		fmt.Fprintf(&b, "# %s\n\n", cap.Title)
	}
	if cap.Instructions != "" {
		b.WriteString(strings.TrimSpace(cap.Instructions))
		b.WriteString("\n")
	}
	return b.String(), nil
}
