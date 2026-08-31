// Package k8s provides a Kubernetes client and the resource builders used by
// the Instance Manager to manage per-user OpenClaw agent Pods.
package k8s

import (
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

// EnvNameForModel derives the env var name that carries a model credential's
// apiKey into the agent Pod. The operator renders openclaw.json with an env
// reference ($ENV_NAME) in place of a literal key, and the instance reconciler
// injects the matching secretKeyRef; both must derive the same name, and the
// result must match OpenClaw's $UPPER env shorthand ([A-Z][A-Z0-9_]{0,127}).
func EnvNameForModel(name string) string {
	const prefix = "CUBEPILOT_LLM_"
	var b strings.Builder
	b.Grow(len(name) + len(prefix))
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
	return b.String()
}
