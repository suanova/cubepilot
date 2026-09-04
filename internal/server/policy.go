package server

import "github.com/suanova/cubepilot/internal/openclaw/ws"

// Phase-1 HITL read allowlist (issue #20, design §3). A guarded session runs
// ask:on-miss: commands matching the allowlist pass silently; everything else
// pauses for a human decision. The allowlist therefore covers the reads the
// agent legitimately performs (kubectl read verbs + a few read-only shell
// tools); anything unknown asks, which is the safe default.
const (
	kubectlReadArgPattern = `^((--kubeconfig|--context|--namespace|-n|--user|--cluster)=?\S*\s+)*(get|list|watch|describe|logs|events|top|api-resources|api-versions|explain|version|diff|cluster-info)(\s|$)`
)

// defaultReadAllowlist returns the entries merged into agents."main".
func defaultReadAllowlist() []ws.AllowlistEntry {
	kubectl := ws.AllowlistEntry{Pattern: "kubectl", ArgPattern: kubectlReadArgPattern}
	safeBins := []string{"ls", "cat", "pwd", "grep", "head", "tail", "wc", "jq", "date", "env", "echo", "printf", "which"}
	out := make([]ws.AllowlistEntry, 0, 1+len(safeBins))
	out = append(out, kubectl)
	for _, b := range safeBins {
		out = append(out, ws.AllowlistEntry{Pattern: b})
	}
	return out
}

// mergeAllowlists unions existing and desired entries, keyed by
// pattern|argPattern. Existing entries (e.g. allow-always grants) are kept.
func mergeAllowlists(existing, desired []ws.AllowlistEntry) []ws.AllowlistEntry {
	seen := map[string]bool{}
	out := make([]ws.AllowlistEntry, 0, len(existing)+len(desired))
	add := func(e ws.AllowlistEntry) {
		k := e.Pattern + "|" + e.ArgPattern
		if e.Pattern == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, e)
	}
	for _, e := range existing {
		add(e)
	}
	for _, e := range desired {
		add(e)
	}
	return out
}
