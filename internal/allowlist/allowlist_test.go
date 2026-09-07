package allowlist

import (
	"regexp"
	"testing"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// TestKubectlReadArgPattern exercises the kubectl read-verb matcher against
// representative commands (design §3 / issue #20, ported from policy_test).
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
	}
	ask := []string{
		"delete pod foo",
		"create -f pod.yaml",
		"apply -f pod.yaml",
		"scale deploy/app --replicas=0",
		"delete ns staging",
		"exec -it pod -- sh",
		"edit deploy/app",
		// separator / substitution smuggling must NOT match (issue #20 review).
		"get pods; kubectl delete pod foo",
		"get pods && kubectl delete ns staging",
		"get pods | grep Running",
		"get pods $(kubectl delete)",
		"get pods `id`",
		"get pods > /tmp/out",
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
