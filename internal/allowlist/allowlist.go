// Package allowlist holds the platform's safe-command allowlist model (issue
// #116). Entries are public-API AllowlistRule values: Pattern is the command;
// ArgPattern optionally constrains argv. The builtin default covers the read
// verbs and read-only shell tools the platform vets; templates and instances
// layer on top of it.
package allowlist

import "github.com/suanova/cubepilot/internal/api/v1alpha1"

// kubectlReadArgPattern matches a kubectl command whose verb is a read and
// whose remaining tokens (after optional leading global flags) are plain
// words -- no separators/substitution.
const kubectlReadArgPattern = `^((--[A-Za-z0-9][A-Za-z0-9-]*(=[A-Za-z0-9_./:=,%+*?@~"#'-]+)?|-[a-zA-Z0-9](\s+[A-Za-z0-9_./:=,%+*?@~"#'-]+)?|--namespace\s+[A-Za-z0-9_./:=,%+*?@~"#'-]+|--context\s+[A-Za-z0-9_./:=,%+*?@~"#'-]+)\s+)*(get|list|watch|describe|logs|events|top|api-resources|api-versions|explain|version|diff|cluster-info)([A-Za-z0-9_./:=,%+*?@~"#'-]|\s)*$`

// safeArgPattern matches only space-separated plain words (no separators).
const safeArgPattern = `^[A-Za-z0-9_./:=,%+*?@~"#'-]+(\s+[A-Za-z0-9_./:=,%+*?@~"#'-]+)*$`

// Default returns the platform builtin safe-read allowlist. The arg patterns
// are anchored and only allow a safe token charset to the end of the command,
// so an allowlisted "read" cannot smuggle shell separators or substitution
// through. Deliberately no command-wrapper bins (env, xargs, sh, ...) that can
// exec a following command, no `date` (`date -s` changes the clock) and no
// `curl` (can write).
func Default() []v1alpha1.AllowlistRule {
	kubectl := v1alpha1.AllowlistRule{Pattern: "kubectl", ArgPattern: kubectlReadArgPattern}
	safeBins := []string{"ls", "cat", "pwd", "grep", "head", "tail", "wc", "jq", "echo", "printf", "which"}
	out := make([]v1alpha1.AllowlistRule, 0, 1+len(safeBins))
	out = append(out, kubectl)
	for _, b := range safeBins {
		out = append(out, v1alpha1.AllowlistRule{Pattern: b, ArgPattern: safeArgPattern})
	}
	return out
}

// Merge unions existing and desired entries, preserving existing order, keyed
// by pattern|argPattern. Entries with an empty pattern are dropped.
func Merge(existing, desired []v1alpha1.AllowlistRule) []v1alpha1.AllowlistRule {
	seen := map[string]bool{}
	out := make([]v1alpha1.AllowlistRule, 0, len(existing)+len(desired))
	add := func(e v1alpha1.AllowlistRule) {
		if e.Pattern == "" {
			return
		}
		k := e.Pattern + "|" + e.ArgPattern
		if seen[k] {
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

// Effective returns the effective allowlist for an instance (issue #116): the
// instance's owned list when it has taken ownership (non-empty), else the
// template's effective default (the platform builtin ∪ the template's own
// allowlist). An owned list is authoritative -- it may drop builtin entries
// (the result is only that those commands ask again; the safe direction).
func Effective(owned, templateAllowlist []v1alpha1.AllowlistRule) []v1alpha1.AllowlistRule {
	if len(owned) > 0 {
		return owned
	}
	return Merge(Default(), templateAllowlist)
}
