// Package k8s provides a Kubernetes client and the resource builders used by
// the Instance Manager to manage per-user OpenClaw agent Pods.
package k8s

import (
	"crypto/sha256"
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

// Platform API endpoint the agent Pod's supervisor pulls its resolved config
// from (issue #172). The URL must be namespace-relative: the chart installs
// into ANY namespace, so the Pod derives it from its OWN namespace via the
// downward API rather than assuming one (a hardcoded `cubepilot` left every
// install elsewhere dialing a namespace it does not own, and the agent Pod
// never became Ready).
const (
	// APIServiceName is the in-namespace Service exposing the platform API --
	// the chart's api.name and the default api service name; web/nginx.conf
	// already pins the same name. Keep the three in sync.
	APIServiceName = "cubepilot-api"
	// APIServicePort is the port that Service exposes.
	APIServicePort = 8080
	// PodNamespaceEnv carries the Pod's own namespace into the supervisor
	// (downward API).
	PodNamespaceEnv = "POD_NAMESPACE"
	// APIURLEnv names the platform internal API base URL for the supervisor.
	// The value references $(POD_NAMESPACE), so the kubelet expands it into the
	// install's real namespace; POD_NAMESPACE must therefore be declared first.
	APIURLEnv = "CUBEPILOT_API_URL"
)

// UserKubeconfigSecretFor returns the per-user kubeconfig Secret name (key
// "config"). Provisioned per user (e.g. by Helm/setup) as part of the
// dual-kubeconfig model; when it does not exist the operator falls back to the
// shared agent-kubeconfig (see AgentInstance controller).
//
// The name is collision-resistant: sanitized identity + a stable 128-bit (32-hex) digest
// of the RAW identity, so identities that normalize the same (e.g. "foo.bar"
// and "foo_bar") still get distinct Secrets. Mirrored by the Helm chart
// (cubepilot.sanitize + sha256sum) and scripts/setup.sh so all provisioning
// paths agree.
func UserKubeconfigSecretFor(user string) string {
	return Sanitize(user) + "-kubeconfig-" + UserIdentityHash(user)
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

const (
	// MaxResourceNameLen is the longest name Kubernetes accepts for the
	// namespaced resources the platform generates (the DNS-1123 subdomain
	// limit): PVC and Pod.
	MaxResourceNameLen = 253
	// MaxServiceNameLen is the longest name Kubernetes accepts for a Service.
	// A Service name is a DNS-1035 label, not a subdomain: at most 63
	// characters, and it must start with a letter -- so the subdomain bound
	// above would still hand the API server an invalid Service name for a long
	// instance name.
	MaxServiceNameLen = 63
	// generatedNameHashLen is how many hex characters of the input digest a
	// truncated generated name carries.
	generatedNameHashLen = 10
)

// GeneratedName returns a name derived from prefix and name, bounded to
// Kubernetes' 253-character limit -- the DNS-1123 subdomain bound, which is
// what PVC and Pod names get. Inputs that fit are returned unchanged, so
// existing resources keep their names; longer ones are truncated and given a
// short hash of the full input, so distinct inputs stay distinct.
//
// The bound matters because callers derive these names from AgentInstance
// metadata.name, which Kubernetes itself accepts up to 253 characters: a
// 253-character instance name yields a 258-character "data-<name>", which the
// API server rejects -- and which the instance finalizer then tries to delete
// under that same impossible name. Truncating without the digest would instead
// collapse two long instance names onto one PVC/Pod.
func GeneratedName(prefix, name string) string {
	return generatedNameWithin(prefix, name, MaxResourceNameLen)
}

// GeneratedServiceName returns a name derived from prefix and name, bounded to
// Kubernetes' 63-character Service-name limit (DNS-1035 label). Use it for
// every Service the platform generates: the subdomain bound GeneratedName
// applies is 190 characters too generous for a Service.
func GeneratedServiceName(prefix, name string) string {
	return generatedNameWithin(prefix, name, MaxServiceNameLen)
}

// generatedNameWithin bounds ResourceName(prefix, name) to max characters. The
// prefix ("data"/"agent") starts with a letter and is never truncated away, so
// a bounded name stays a valid DNS-1035 label as well as a DNS-1123 subdomain.
func generatedNameWithin(prefix, name string, max int) string {
	generated := ResourceName(prefix, name)
	if len(generated) <= max {
		return generated
	}
	sum := sha256.Sum256([]byte(generated))
	digest := fmt.Sprintf("%x", sum[:generatedNameHashLen/2])
	// Keep the head, drop any separator the cut left dangling (the tail is
	// always alphanumeric), and append the digest of the full input.
	head := strings.TrimRight(generated[:max-len(digest)-1], "-")
	return head + "-" + digest
}

// InstanceName builds the AgentInstance name for (user, agent) -- the instance
// key is user + agent (design §3.2). Both segments are sanitized to DNS-1123.
func InstanceName(user, agent string) string {
	return Sanitize(user) + "-" + Sanitize(agent)
}

// EnvNameForProvider derives the stable identifier used for a provider
// credential's apiKey across the credential-delivery path: the file SecretRef
// id rendered into openclaw.json ("/"+name) and the matching key in the
// emptyDir keys.json the supervisor writes. The operator render, the
// resolver's credential list and the supervisor must all derive the same name.
//
// A short hash of the original provider name is appended so distinct names
// that sanitize identically (e.g. "foo-bar" vs "foo_bar", or case variants)
// still map to distinct keys instead of one provider's apiKey silently serving
// another.
func EnvNameForProvider(name string) string {
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
