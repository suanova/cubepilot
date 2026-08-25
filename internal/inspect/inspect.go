// Package inspect drives the minimal M4 scheduled-task flow: a fixed inspection
// prompt run through the agent loop, producing a natural-language report.
package inspect

import (
	"context"
	"strings"

	"github.com/suanova/cubepilot/internal/openclaw"
)

const prompt = `Run a basic health inspection of the current Kubernetes cluster:
1. Check node status (kubectl get nodes);
2. Find abnormal Pods in all namespaces (not Running, e.g. CrashLoopBackOff / Pending / ImagePullBackOff / OOMKilled);
3. Check recent cluster events (kubectl get events -A).
Classify findings by severity: P0 critical / P1 important / P2 minor, and output a structured inspection report in Simplified Chinese (with evidence for each item).

[Credibility constraints]
- Every finding must include an evidence chain: executed command + excerpt of raw output + timestamp.
- Suspected findings that cannot be confirmed must be marked "AI suspicion, manual review required", and must not be treated as established facts.
- Do not report the same issue repeatedly; filter out noise (occasional restarts, non-critical events) or classify it as P2.
- This inspection is read-only: only run query commands; any write operation (apply/delete/scale/create) is forbidden.
[Read-only note] If your credentials are rejected by RBAC, state the actual permission scope and do not retry rejected operations.`

// Run executes the inspection prompt against the given agent instance and
// returns the agent's final natural-language report text. onEvent (optional)
// receives every stream event so callers can record tool calls for audit.
func Run(ctx context.Context, oc openclaw.AgentRuntime, sessionKey string, onEvent func(openclaw.Event)) (string, error) {
	var buf strings.Builder
	err := oc.StreamChat(ctx, openclaw.ChatParams{
		SessionKey: sessionKey,
		Messages:   []openclaw.ChatMessage{{Role: "user", Content: prompt}},
	}, func(ev openclaw.Event) error {
		if onEvent != nil {
			onEvent(ev)
		}
		if ev.Type == openclaw.EventMessageDelta {
			buf.WriteString(ev.Delta)
		}
		return nil
	})
	return buf.String(), err
}

// Prompt returns the fixed inspection prompt (also used as the preset for the
// cluster-health-inspection task template on the tasks page).
func Prompt() string { return prompt }
