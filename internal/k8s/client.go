// Package k8s provides a Kubernetes client and the resource builders used by
// the Instance Manager to manage per-user OpenClaw agent Pods.
package k8s

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Shared resource names (one-time infra created by scripts/setup.sh).
const (
	KubeconfigSecretName = "agent-kubeconfig"
	ConfigSecretName     = "openclaw-config"
	ServiceAccountName   = "cubepilot-agent"
	AgentLabelApp        = "cubepilot-agent"
	AgentLabelUser       = "cubepilot/user"
)

// Kubeconfig layout inside the agent Pod (design §5.3 / issue #19 Option B:
// two kubeconfigs). The user's own kubeconfig is the DEFAULT so every
// user-facing kubectl operation runs with the user's least-privilege
// credentials; the platform (cubepilot-agent SA) kubeconfig lives on a
// non-default path and is used explicitly only for CRD/kind schema discovery
// via `kubectl --kubeconfig=$CUBEPILOT_PLATFORM_KUBECONFIG ...`.
const (
	// UserKubeconfigPath is the per-user kubeconfig file at kubectl's default
	// location ($HOME/.kube/config); mounted from the per-user Secret.
	UserKubeconfigPath = "/home/node/.kube/config"
	// PlatformKubeconfigPath is the second kubeconfig (cubepilot-agent SA /
	// agent-kubeconfig), mounted for schema discovery.
	PlatformKubeconfigPath = "/home/node/.kube/platform/config"
	// PlatformKubeconfigEnv names the platform kubeconfig path for the skill
	// recipe to reference.
	PlatformKubeconfigEnv = "CUBEPILOT_PLATFORM_KUBECONFIG"
	// UserKubeconfigEnv names the default (user) kubeconfig path.
	UserKubeconfigEnv = "CUBEPILOT_USER_KUBECONFIG"
	// KubeconfigRevisionAnnotation is set by the controller on the agent Pod to
	// the resourceVersions of the mounted kubeconfig Secrets, so an in-place
	// Secret content change (same name) recreates the Pod -- SubPath mounts do
	// not refresh on Secret update.
	KubeconfigRevisionAnnotation = "cubepilot.io/kubeconfig-rev"
)

// UserKubeconfigSecretFor returns the per-user kubeconfig Secret name (key
// "config"). Provisioned per user (e.g. by Helm/setup) as part of the
// dual-kubeconfig model; when it does not exist the operator falls back to the
// shared agent-kubeconfig (see AgentInstance controller).
//
// The name is collision-resistant: sanitized identity + a stable 8-hex digest
// of the RAW identity, so identities that normalize the same (e.g. "foo.bar"
// and "foo_bar") still get distinct Secrets. Mirrored by the Helm chart
// (cubepilot.sanitize + sha256sum) and scripts/setup.sh so all provisioning
// paths agree.
func UserKubeconfigSecretFor(user string) string {
	sum := sha256.Sum256([]byte(user))
	return Sanitize(user) + "-kubeconfig-" + hex.EncodeToString(sum[:])[:8]
}

// Credential key delivery (design §6): the operator renders each model's
// apiKey in openclaw.json as a file SecretRef pointing at a JSON file the
// supervisor writes into an emptyDir (never onto the PVC or in the network
// response). These constants are shared by the renderer (which produces the
// ref + the secrets.providers entry), the pod builder (which mounts the
// emptyDir) and the supervisor (which writes the file).
const (
	// CredProviderName is the OpenClaw file-secret provider name in
	// openclaw.json; mode "json", path CredentialsPath.
	CredProviderName = "cubepilot-keys"
	// CredentialsDir is the emptyDir mount path inside the agent pod.
	CredentialsDir = "/mnt/cubepilot-keys"
	// CredentialsFile is the JSON file the supervisor writes (all model keys).
	CredentialsFile = "keys.json"
	// CredentialsPath is the full path the provider reads.
	CredentialsPath = CredentialsDir + "/" + CredentialsFile
)

// NewClient returns a clientset using in-cluster config when available, otherwise
// the kubeconfig from CUBEPILOT_KUBECONFIG or ~/.kube/config (local dev).
func NewClient() (*kubernetes.Clientset, error) {
	cfg, err := NewRestConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// NewRestConfig resolves the cluster REST config (in-cluster first, then
// CUBEPILOT_KUBECONFIG or ~/.kube/config). Shared by the clientset and the
// controller-runtime manager.
func NewRestConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.BuildConfigFromFlags("", KubeconfigPath())
		if err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// KubeconfigPath returns the kubeconfig used by NewClient for out-of-cluster
// fallback (CUBEPILOT_KUBECONFIG or ~/.kube/config).
func KubeconfigPath() string {
	if p := os.Getenv("CUBEPILOT_KUBECONFIG"); p != "" {
		return p
	}
	return clientcmd.RecommendedHomeFile
}

var invalidName = regexp.MustCompile(`[^a-z0-9-]+`)

// Sanitize converts a user identity into a valid DNS-1123 resource-name segment.
func Sanitize(s string) string {
	s = strings.ToLower(s)
	s = invalidName.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "user"
	}
	return s
}

// ResourceName builds a per-user resource name from a prefix.
func ResourceName(prefix, user string) string {
	return prefix + "-" + Sanitize(user)
}

// InstanceName builds the AgentInstance name for (user, agent) -- the instance
// key is user + agent (design §3.2). Both segments are sanitized to DNS-1123.
func InstanceName(user, agent string) string {
	return Sanitize(user) + "-" + Sanitize(agent)
}

// EnvNameForModel derives the stable identifier used for a model credential's
// apiKey across the credential-delivery path: the file SecretRef id rendered
// into openclaw.json ("/"+name) and the matching key in the emptyDir keys.json
// the supervisor writes. The operator render, the resolver's credential list
// and the supervisor must all derive the same name.
//
// A short hash of the original model name is appended so distinct names that
// sanitize identically (e.g. "foo-bar" vs "foo_bar", or case variants) still
// map to distinct keys instead of one model's apiKey silently serving another.
func EnvNameForModel(name string) string {
	const prefix = "CUBEPILOT_LLM_"
	var b strings.Builder
	b.Grow(len(name) + len(prefix) + 5)
	b.WriteString(prefix)
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
			b.WriteByte(c - 'a' + 'A')
		case (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'):
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	sum := sha256.Sum256([]byte(name))
	fmt.Fprintf(&b, "_%x", sum[:2])
	return b.String()
}
