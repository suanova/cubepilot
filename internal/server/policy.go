package server

import "github.com/suanova/cubepilot/internal/openclaw/ws"

// Phase-1 HITL read allowlist (issue #20, design §3). A guarded session runs
// ask:on-miss: commands matching the allowlist pass silently; everything else
// pauses for a human decision. The allowlist therefore covers the reads the
// agent legitimately performs (kubectl read verbs + a few read-only shell
// tools); anything unknown asks, which is the safe default.
//
// The arg patterns are anchored and only allow a safe token charset to the end
// of the command, so an allowlisted "read" cannot smuggle shell separators or
// substitution through (e.g. `kubectl get pods; kubectl delete ...` or
// `ls -la; rm -rf ...` fail to match and therefore ask).
const (
	// kubectlReadArgPattern matches a kubectl command whose verb is a read and
	// whose remaining tokens (after optional leading global flags) are plain
	// words -- no separators/substitution.
	kubectlReadArgPattern = `^((--[A-Za-z0-9][A-Za-z0-9-]*(=[A-Za-z0-9_./:=,%+*?@~"#'-]+)?|-[a-zA-Z0-9](\s+[A-Za-z0-9_./:=,%+*?@~"#'-]+)?|--namespace\s+[A-Za-z0-9_./:=,%+*?@~"#'-]+|--context\s+[A-Za-z0-9_./:=,%+*?@~"#'-]+)\s+)*(get|list|watch|describe|logs|events|top|api-resources|api-versions|explain|version|diff|cluster-info)([A-Za-z0-9_./:=,%+*?@~"#'-]|\s)*$`

	// safeArgPattern matches only space-separated plain words (no separators).
	safeArgPattern = `^[A-Za-z0-9_./:=,%+*?@~"#'-]+(\s+[A-Za-z0-9_./:=,%+*?@~"#'-]+)*$`
)

// defaultReadAllowlist returns the entries merged into agents."main".
func defaultReadAllowlist() []ws.AllowlistEntry {
	kubectl := ws.AllowlistEntry{Pattern: "kubectl", ArgPattern: kubectlReadArgPattern}
	safeBins := []string{"ls", "cat", "pwd", "grep", "head", "tail", "wc", "jq", "date", "env", "echo", "printf", "which"}
	out := make([]ws.AllowlistEntry, 0, 1+len(safeBins))
	out = append(out, kubectl)
	for _, b := range safeBins {
		out = append(out, ws.AllowlistEntry{Pattern: b, ArgPattern: safeArgPattern})
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
