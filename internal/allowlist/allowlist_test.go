package allowlist

import (
	"regexp"
	"testing"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// TestKubectlReadArgPattern exercises the kubectl read-verb matcher (design
// §3 / issue #20). The pattern anchors on a read subcommand and treats the
// remaining argv as data handed to kubectl, so output templates (jsonpath /
// go-template / custom-columns, which embed `{ } ( ) [ ]` and `\n`) match.
// Separator/substitution/redirect smuggling is not this rule's job: the
// gateway evaluates the rule against one parsed command and refuses
// unanalyzable constructs (a second command, `$(...)`, backticks, redirects)
// before this pattern is consulted. Only the verb gates here.
func TestKubectlReadArgPattern(t *testing.T) {
	re := regexp.MustCompile(kubectlReadArgPattern)
	pass := []string{
		"get pods",
		"list pods -n default",
		"watch pods",
		"describe pod foo",
		"api-resources",
		"--kubeconfig=/etc/crd-kubeconfig get crd deployments",
		"get crd",
		"logs -f deploy/app",
		"events -n default",
		"top pods",
		"-n default get pods",
		"get pods -o wide -l app=foo,env=prod",
		// -o output templates are argv data; their chars are not shell syntax.
		"get devenvironment dev-4c8g -n default -o jsonpath=phase={.status.phase.name}{range .status.conditions[*]}{.type}={.status} ({.reason}){end}{range .status.endpoints[*]}{.name}: {.address}{end}",
		`get devenvironment dev-4c8g -n default -o jsonpath=phase={.status.phase.name}{"\n"}{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}{range .status.endpoints[*]}{.name}: {.address}{"\n"}{end}`,
		`get pods -o go-template={{range .items}}{{.metadata.name}}{{"\n"}}{{end}}`,
		`get secret app -o custom-columns=DATA:.data.password`,
	}
	ask := []string{
		// write subcommands must never match a read-verb rule
		"delete pod foo",
		"create -f pod.yaml",
		"apply -f pod.yaml",
		"scale deploy/app --replicas=0",
		"delete ns staging",
		"exec -it pod -- sh",
		"edit deploy/app",
		"-n default delete pod foo",
		"label pod foo tier=frontend",
	}
	for _, c := range pass {
		if !re.MatchString(c) {
			t.Errorf("expected %q to match the read allowlist", c)
		}
	}
	for _, c := range ask {
		if re.MatchString(c) {
			t.Errorf("expected %q to MISS the read allowlist", c)
		}
	}

	// Read-only shell tools are allowlisted only for plain argument lists; a
	// separator turns the same "read" into an ask.
	binRe := regexp.MustCompile(safeArgPattern)
	for _, c := range []string{"ls -la", "cat /etc/resolv.conf", "grep -i error /var/log/app.log"} {
		if !binRe.MatchString(c) {
			t.Errorf("expected %q to match the safe-bin arg pattern", c)
		}
	}
	for _, c := range []string{"cat /etc/passwd; rm -rf /", "ls -la && whoami", "echo '$(id)'"} {
		if binRe.MatchString(c) {
			t.Errorf("expected %q to MISS the safe-bin arg pattern", c)
		}
	}
}

// TestDefaultExcludesCommandWrappers ensures command-wrapper bins that can exec
// a following command (e.g. `env kubectl delete ...`), bins with mutating flags
// (`date -s`), and writers (`curl`) are never in the builtin allowlist.
func TestDefaultExcludesCommandWrappers(t *testing.T) {
	for _, e := range Default() {
		for _, banned := range []string{"env", "xargs", "sh", "bash", "nohup", "timeout", "date", "curl"} {
			if e.Pattern == banned {
				t.Errorf("allowlist must not include unsafe bin %q", banned)
			}
		}
	}
}

func TestEffectiveInheritsTemplateDefault(t *testing.T) {
	// Empty owned list -> platform builtin ∪ template allowlist.
	base := v1alpha1.AllowlistRule{Pattern: "helm", ArgPattern: `^list`}
	got := Effective(nil, []v1alpha1.AllowlistRule{base})
	if len(got) == 0 {
		t.Fatal("effective empty; want platform builtin + template entries")
	}
	if got[0].Pattern != "kubectl" {
		t.Errorf("effective[0].pattern = %q, want builtin kubectl first", got[0].Pattern)
	}
	found := false
	for _, e := range got {
		if e.Pattern == "helm" && e.ArgPattern == `^list` {
			found = true
		}
	}
	if !found {
		t.Errorf("template allowlist entry not in effective: %+v", got)
	}
}

func TestEffectiveOwnedIsAuthoritative(t *testing.T) {
	// A non-empty owned list replaces the default entirely (the user may have
	// dropped builtin reads; that only makes those commands ask again).
	owned := []v1alpha1.AllowlistRule{{Pattern: "git", ArgPattern: `^(log|show|status|diff)(\s|$)`}}
	got := Effective(owned, []v1alpha1.AllowlistRule{{Pattern: "helm"}})
	if len(got) != 1 || got[0].Pattern != "git" {
		t.Errorf("owned list not authoritative: %+v", got)
	}
}

func TestMergeDedupesAndSkipsEmpty(t *testing.T) {
	dupe := v1alpha1.AllowlistRule{Pattern: "kubectl", ArgPattern: `x`}
	in := []v1alpha1.AllowlistRule{
		dupe,
		{Pattern: "ls", ArgPattern: safeArgPattern},
		{Pattern: ""}, // dropped
	}
	got := Merge([]v1alpha1.AllowlistRule{dupe}, in)
	var count int
	for _, e := range got {
		if e.Pattern == "kubectl" && e.ArgPattern == "x" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("dedupe failed, kubectl|x appears %d times: %+v", count, got)
	}
	for _, e := range got {
		if e.Pattern == "" {
			t.Errorf("empty-pattern entry kept: %+v", got)
		}
	}
}

func TestDefaultHasKubectlReadVerbsAndSafeBins(t *testing.T) {
	d := Default()
	if len(d) == 0 {
		t.Fatal("empty builtin default")
	}
	if d[0].Pattern != "kubectl" || d[0].ArgPattern == "" {
		t.Errorf("kubectl entry malformed: %+v", d[0])
	}
}

func TestBuiltinLabelOnlyForBuiltinRules(t *testing.T) {
	if got := BuiltinLabel(Default()[0]); got == "" {
		t.Error("builtin kubectl rule should carry a read-only label")
	}
	// A user rule whose pattern is kubectl but which allows a WRITE must NOT be
	// presented as read-only (issue #123 UI mislabel).
	custom := v1alpha1.AllowlistRule{Pattern: "kubectl", ArgPattern: `^create namespace hitl-create-107$`}
	if got := BuiltinLabel(custom); got != "" {
		t.Errorf("custom kubectl write rule wrongly labelled %q", got)
	}
	if got := BuiltinLabel(v1alpha1.AllowlistRule{Pattern: "ls", ArgPattern: safeArgPattern}); got == "" {
		t.Error("builtin ls rule should carry a read-only label")
	}
}
