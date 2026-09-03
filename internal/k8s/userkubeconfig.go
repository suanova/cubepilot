package k8s

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Per-user identity generation (issue #19): the platform mints a ServiceAccount
// per configured user and renders a kubeconfig from its token, so user-facing
// kubectl runs as that user (cluster `view` + ai.cubestack.io admin) without
// any operator/admin-supplied kubeconfig.

// UserIdentityHash is a stable 128-bit (32-hex) digest of the RAW identity,
// shared by every per-user object name so identities that sanitize the same
// (e.g. "zhang.wei" vs "Zhang Wei") never collide.
func UserIdentityHash(user string) string {
	sum := sha256.Sum256([]byte(user))
	return hex.EncodeToString(sum[:])[:32]
}

// UserServiceAccountName is the namespaced ServiceAccount the platform creates
// for a user. Collision-free (sanitized identity + identity hash) and distinct
// from the shared runtime SA (cubepilot-agent).
func UserServiceAccountName(user string) string {
	return "user-" + Sanitize(user) + "-" + UserIdentityHash(user)
}

// PerUserKubeconfigYAML renders an in-cluster kubeconfig authenticating as the
// per-user ServiceAccount token. The cluster server/CA mirror
// deploy/agent-kubeconfig.yaml: the CA path exists in every agent Pod because
// the Pod mounts its own ServiceAccount. The token is inlined (the per-user SA
// token is not projected into the Pod, so a tokenFile reference would be
// wrong).
func PerUserKubeconfigYAML(token string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
    server: https://kubernetes.default.svc
  name: in-cluster
contexts:
- context:
    cluster: in-cluster
    user: user
  name: in-cluster
current-context: in-cluster
users:
- name: user
  user:
    token: %s
`, token))
}
