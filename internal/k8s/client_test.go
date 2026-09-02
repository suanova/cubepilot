package k8s

import "testing"

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
// deterministic and DNS-1123 (issue #19 Option B).
func TestUserKubeconfigSecretFor(t *testing.T) {
	cases := map[string]string{
		"zhang.wei": "zhang-wei-kubeconfig",
		"Zhang Wei": "zhang-wei-kubeconfig",
		"alice":     "alice-kubeconfig",
		"":          "user-kubeconfig",
	}
	for in, want := range cases {
		if got := UserKubeconfigSecretFor(in); got != want {
			t.Errorf("UserKubeconfigSecretFor(%q) = %q, want %q", in, got, want)
		}
	}
}
