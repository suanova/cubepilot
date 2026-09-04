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
	}
	ask := []string{
		"delete pod foo",
		"create -f pod.yaml",
		"apply -f pod.yaml",
		"scale deploy/app --replicas=0",
		"delete ns staging",
		"exec -it pod -- sh",
		"edit deploy/app",
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
}
