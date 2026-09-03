package k8s

import (
	"strings"
	"testing"
)

// TestUserServiceAccountName verifies per-user SA names are deterministic,
// DNS-1123 and distinct from the shared runtime SA (cubepilot-agent).
func TestUserServiceAccountName(t *testing.T) {
	cases := map[string]string{
		"zhang.wei": "user-zhang-wei",
		"Zhang Wei": "user-zhang-wei",
		"alice":     "user-alice",
		"":          "user-user",
	}
	for in, want := range cases {
		if got := UserServiceAccountName(in); got != want {
			t.Errorf("UserServiceAccountName(%q) = %q, want %q", in, got, want)
		}
	}
	if UserServiceAccountName("zhang.wei") == ServiceAccountName {
		t.Error("per-user SA must not collide with the shared runtime SA")
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
