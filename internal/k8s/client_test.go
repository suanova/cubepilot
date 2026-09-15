package k8s

import (
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

// TestGeneratedServiceNameBounded pins the Service-name bound: a Service name
// is a DNS-1035 label (63 characters, must start with a letter), not the
// DNS-1123 subdomain GeneratedName bounds to (253). A 253-character
// AgentInstance name therefore still produced an invalid Service name through
// GeneratedName alone. Short names must be untouched -- they are the names of
// every existing Service -- and long ones must be bounded, valid and distinct.
func TestGeneratedServiceNameBounded(t *testing.T) {
	// Inputs that fit are returned unchanged, and the Service/Pod names agree
	// while there is room for both.
	short := "zhang-wei-cubepilot"
	if got := GeneratedServiceName("agent", short); got != "agent-"+short {
		t.Errorf("GeneratedServiceName(agent, %s) = %q, want %q", short, got, "agent-"+short)
	}
	if GeneratedServiceName("agent", short) != GeneratedName("agent", short) {
		t.Errorf("GeneratedServiceName and GeneratedName disagree on a short name")
	}

	long := strings.Repeat("a", 253) // the longest metadata.name Kubernetes accepts
	svc := GeneratedServiceName("agent", long)
	if len(svc) > MaxServiceNameLen {
		t.Errorf("GeneratedServiceName(agent, 253-char name) = %d characters, want <= %d", len(svc), MaxServiceNameLen)
	}
	if errs := validation.IsDNS1035Label(svc); len(errs) > 0 {
		t.Errorf("GeneratedServiceName(agent, 253-char name) = %q is not a valid DNS-1035 label: %v", svc, errs)
	}
	if svc != GeneratedServiceName("agent", long) {
		t.Errorf("GeneratedServiceName(agent, 253-char name) is not deterministic")
	}
	// Distinct long inputs must not collapse onto one Service name.
	if other := GeneratedServiceName("agent", strings.Repeat("a", 252)+"b"); other == svc {
		t.Errorf("two distinct 253-char names both produced the service name %q", svc)
	}
	// The readable head survives the cut.
	if !strings.HasPrefix(svc, "agent-aaa") {
		t.Errorf("GeneratedServiceName(agent, 253-char name) = %q, want it to keep the input's head", svc)
	}
	// The whole point of the second helper: the subdomain bound is too generous
	// for a Service, so the two must not agree here.
	if svc == GeneratedName("agent", long) {
		t.Errorf("the Service name is still bounded to %d characters", MaxResourceNameLen)
	}
}

// TestEnvNameForProviderNoCollision verifies distinct provider names that
// sanitize to the same readable form (differing only in separator/case) still
// map to distinct credential identifiers, so one provider's apiKey can never be
// served to another provider's endpoint.
func TestEnvNameForProviderNoCollision(t *testing.T) {
	a := EnvNameForProvider("foo-bar")
	b := EnvNameForProvider("foo_bar")
	c := EnvNameForProvider("FOO-BAR")
	if a == b || a == c || b == c {
		t.Fatalf("EnvNameForProvider collisions: %q %q %q", a, b, c)
	}
	first := EnvNameForProvider("deepseek-v4-flash")
	second := EnvNameForProvider("deepseek-v4-flash")
	if first != second {
		t.Errorf("same provider mapped differently: %q vs %q", first, second)
	}
}

// TestUserKubeconfigSecretFor verifies the per-user kubeconfig Secret name is
// deterministic, DNS-1123, and collision-resistant across identities that
// sanitize the same (issue #19 Option B): sanitized identity + 32-hex digest of
// the raw identity.
func TestUserKubeconfigSecretFor(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?-kubeconfig-[0-9a-f]{32}$`)
	for _, in := range []string{"zhang.wei", "zhang_wei", "Zhang Wei", "alice", ""} {
		got := UserKubeconfigSecretFor(in)
		if !re.MatchString(got) {
			t.Errorf("UserKubeconfigSecretFor(%q) = %q, want sanitize-kubeconfig-<32hex>", in, got)
		}
		// Deterministic.
		if again := UserKubeconfigSecretFor(in); again != got {
			t.Errorf("UserKubeconfigSecretFor(%q) not deterministic: %q vs %q", in, got, again)
		}
	}
	// Identities that sanitize to the same segment still get distinct Secrets.
	a := UserKubeconfigSecretFor("foo.bar")
	b := UserKubeconfigSecretFor("foo_bar")
	if a == b {
		t.Errorf("collision: %q vs %q must differ", a, b)
	}
}
