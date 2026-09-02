package k8s

import (
	"regexp"
	"testing"
)

// TestEnvNameForModelNoCollision verifies distinct model names that sanitize to
// the same readable form (differing only in separator/case) still map to
// distinct credential identifiers, so one model's apiKey can never be served to
// another model's provider.
func TestEnvNameForModelNoCollision(t *testing.T) {
	a := EnvNameForModel("foo-bar")
	b := EnvNameForModel("foo_bar")
	c := EnvNameForModel("FOO-BAR")
	if a == b || a == c || b == c {
		t.Fatalf("EnvNameForModel collisions: %q %q %q", a, b, c)
	}
	first := EnvNameForModel("deepseek-v4-flash")
	second := EnvNameForModel("deepseek-v4-flash")
	if first != second {
		t.Errorf("same model mapped differently: %q vs %q", first, second)
	}
}

// TestUserKubeconfigSecretFor verifies the per-user kubeconfig Secret name is
// deterministic, DNS-1123, and collision-resistant across identities that
// sanitize the same (issue #19 Option B): sanitized identity + 8-hex digest of
// the raw identity.
func TestUserKubeconfigSecretFor(t *testing.T) {
	re := regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?-kubeconfig-[0-9a-f]{8}$`)
	for _, in := range []string{"zhang.wei", "zhang_wei", "Zhang Wei", "alice", ""} {
		got := UserKubeconfigSecretFor(in)
		if !re.MatchString(got) {
			t.Errorf("UserKubeconfigSecretFor(%q) = %q, want sanitize-kubeconfig-<8hex>", in, got)
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
