package k8s

import (
	"regexp"
	"strings"
	"testing"
)

// TestUserServiceAccountName verifies per-user SA names are deterministic,
// DNS-1123 and distinct from the shared runtime SA (cubepilot-agent).
func TestUserServiceAccountName(t *testing.T) {
	re := regexp.MustCompile(`^user-[a-z0-9-]+-[0-9a-f]{32}$`)
	for _, in := range []string{"zhang.wei", "Zhang Wei", "alice", ""} {
		got := UserServiceAccountName(in)
		if !re.MatchString(got) {
			t.Errorf("UserServiceAccountName(%q) = %q, want user-<sanitize>-<32hex>", in, got)
		}
		if again := UserServiceAccountName(in); again != got {
			t.Errorf("UserServiceAccountName(%q) not deterministic", in)
		}
	}
	if UserServiceAccountName("zhang.wei") == ServiceAccountName {
		t.Error("per-user SA must not collide with the shared runtime SA")
	}
	// Distinct identities that sanitize the same never share an SA.
	if UserServiceAccountName("foo.bar") == UserServiceAccountName("foo_bar") {
		t.Error("sanitize-colliding identities share an SA")
	}
}

// TestPerUserKubeconfigYAML verifies the generated kubeconfig targets the
// in-cluster server with the shared CA path and an inline per-user token (no
// tokenFile -- the per-user SA token is not projected into the Pod).
func TestPerUserKubeconfigYAML(t *testing.T) {
	yaml := string(PerUserKubeconfigYAML("tok.abc123"))
	for _, want := range []string{
		"server: https://kubernetes.default.svc",
		"certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
		"token: tok.abc123",
		"current-context: in-cluster",
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("generated kubeconfig missing %q:\n%s", want, yaml)
		}
	}
	if strings.Contains(yaml, "tokenFile") {
		t.Error("generated kubeconfig must inline the token, not use tokenFile")
	}
}
