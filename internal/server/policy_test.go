package server

import (
	"regexp"
	"testing"

	"github.com/suanova/cubepilot/internal/openclaw/ws"
)

// TestReadAllowlistArgPattern exercises the kubectl read-verb matcher against
// representative commands (design §3 / issue #20).
func TestReadAllowlistArgPattern(t *testing.T) {
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

func TestMergeAllowlistsDedupesAndKeepsExisting(t *testing.T) {
	existing := []ws.AllowlistEntry{
		{Pattern: "kubectl", ArgPattern: "^get "},
		{Pattern: "kubectl", ArgPattern: "allow-always-hash", Source: "allow-always"},
	}
	desired := []ws.AllowlistEntry{
		{Pattern: "kubectl", ArgPattern: "^get "}, // dup
		{Pattern: "ls"},
	}
	out := mergeAllowlists(existing, desired)
	if len(out) != 3 {
		t.Fatalf("merged len = %d, want 3: %+v", len(out), out)
	}
	// The allow-always grant must be preserved.
	hasGrant := false
	for _, e := range out {
		if e.Source == "allow-always" {
			hasGrant = true
		}
	}
	if !hasGrant {
		t.Error("allow-always grant dropped by merge")
	}
}

func TestDefaultReadAllowlistShape(t *testing.T) {
	al := defaultReadAllowlist()
	if len(al) < 3 {
		t.Fatalf("default allowlist too small: %d", len(al))
	}
	if al[0].Pattern != "kubectl" || al[0].ArgPattern == "" {
		t.Errorf("first entry should be the kubectl argv rule: %+v", al[0])
	}
	// Command-wrapper bins that can exec a following command (e.g. `env kubectl
	// delete ...`) must never be allowlisted (issue #20 code review).
	for _, e := range al {
		for _, banned := range []string{"env", "xargs", "sh", "bash", "nohup", "timeout"} {
			if e.Pattern == banned {
				t.Errorf("allowlist must not include wrapper bin %q", banned)
			}
		}
	}
}
