// Package allowlist holds the platform's safe-command allowlist model (issue
// #116). Entries are public-API AllowlistRule values: Pattern is the command;
// ArgPattern optionally constrains argv. The builtin default covers the read
// verbs and read-only shell tools the platform vets; templates and instances
// layer on top of it.
package allowlist

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// kubectlGlobalFlag matches one optional kubectl global flag that may precede
// the subcommand (--kubeconfig=/x, --context prod, -n default, ...). Global
// flag values are plain literal tokens, so their charset stays tight.
const kubectlGlobalFlag = `--[A-Za-z0-9][A-Za-z0-9-]*(=[A-Za-z0-9_./:=,%+*?@~"#'-]+)?|-[a-zA-Z0-9](\s+[A-Za-z0-9_./:=,%+*?@~"#'-]+)?|--namespace\s+[A-Za-z0-9_./:=,%+*?@~"#'-]+|--context\s+[A-Za-z0-9_./:=,%+*?@~"#'-]+`

// kubectlReadVerbs are the kubectl subcommands that only read cluster state.
const kubectlReadVerbs = `get|list|watch|describe|logs|events|top|api-resources|api-versions|explain|version|diff|cluster-info`

// kubectlReadArgPattern matches a kubectl command whose subcommand is a read
// verb, after optional leading global flags. Everything after the verb is argv
// handed to kubectl: resource names, selectors and -o jsonpath / go-template /
// custom-columns output templates, which embed `{ } [ ] ( )` and `\n` escapes.
// The tail is deliberately permissive because the gateway evaluates this rule
// against a single parsed command: a separator (`;` `|` `&&`), a substitution
// (`$(...)`/backtick) or a redirect splits the line into more commands or is
// refused before this rule is consulted, so a permissive tail cannot smuggle a
// second command. The verb stays anchored so a write subcommand
// (delete/apply/exec/scale/...) never matches.
const kubectlReadArgPattern = `^((` + kubectlGlobalFlag + `)\s+)*(` + kubectlReadVerbs + `)(\b[\s\S]*)?$`

// safeArgPattern matches only space-separated plain words (no separators).
const safeArgPattern = `^[A-Za-z0-9_./:=,%+*?@~"#'-]+(\s+[A-Za-z0-9_./:=,%+*?@~"#'-]+)*$`

// Default returns the platform builtin safe-read allowlist. The kubectl rule
// anchors on a read verb -- the gateway matches each rule against one parsed
// command, so verb-gating, not a token charset, is the security boundary. The
// read-only shell tools keep a plain-word-only arg charset instead: they run
// through a real shell, where an allowed separator would be interpreted.
// Deliberately no command-wrapper bins (env, xargs, sh, ...) that can exec a
// following command, no `date` (`date -s` changes the clock) and no `curl`
// (can write).
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

// BuiltinLabel returns a human meaning for a rule ONLY when it exactly matches
// one of the platform builtin read-only rules. Anything else -- user/template
// additions, allow-always grants -- returns "" so callers must NOT present it as
// read-only: a user-added "kubectl" rule can allow a write (e.g. argPattern
// matching create/delete).
func BuiltinLabel(e v1alpha1.AllowlistRule) string {
	switch e.Pattern {
	case "kubectl":
		if e.ArgPattern == kubectlReadArgPattern {
			return "kubectl — read-only operations (get/list/watch/describe/logs/events/top/…)"
		}
	case "ls", "cat", "pwd", "grep", "head", "tail", "wc", "jq", "echo", "printf", "which":
		if e.ArgPattern == safeArgPattern {
			return e.Pattern + " — read-only, plain args"
		}
	}
	return ""
}

// Effective returns the effective allowlist for an instance (issue #185): the
// platform builtin, the template's additions, the instance's hand-authored
// additions and the instance's learned grants, unioned.
//
// There is deliberately no "the instance owns its list" override. The previous
// design returned the instance list *instead of* the union whenever that list
// was non-empty, so the first edit of any kind -- including a removal --
// materialized the then-current builtin into the instance and froze it there.
// A later hardening of Default() then could not reach that instance, which is
// the fail-open direction. A union cannot freeze, for today's writers or any
// added later.
//
// Consequence, accepted deliberately: a builtin entry can no longer be removed
// per instance. AlwaysAsk is the strict posture.
func Effective(templateAllowlist, instanceAllowlist, grants []v1alpha1.AllowlistRule) []v1alpha1.AllowlistRule {
	all := make([]v1alpha1.AllowlistRule, 0, len(templateAllowlist)+len(instanceAllowlist)+len(grants))
	all = append(all, templateAllowlist...)
	all = append(all, instanceAllowlist...)
	all = append(all, grants...)
	return Merge(Default(), all)
}

// Validate reports whether a rule is well formed. Pattern is a command name
// rather than a regular expression, so it is only checked for emptiness;
// ArgPattern is a regular expression compiled by the gateway at match time, so
// it is compiled here to reject it while the user is still looking at the form.
func Validate(r v1alpha1.AllowlistRule) error {
	if strings.TrimSpace(r.Pattern) == "" {
		return errors.New("pattern is required")
	}
	if r.ArgPattern == "" {
		return nil
	}
	if _, err := regexp.Compile(r.ArgPattern); err != nil {
		return fmt.Errorf("argPattern is not a valid regular expression: %w", err)
	}
	return nil
}
