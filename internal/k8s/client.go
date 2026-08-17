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
	// ReadOnlyServiceAccountName is the identity used by inspection instances
	// (design §5.4 权限边界技术强制): a read-only RBAC that cannot write even
	// when the agent is prompt-injected.
	ReadOnlyServiceAccountName = "cubepilot-agent-inspect"
	// ReadOnlyKubeconfigSecretName carries the in-cluster kubeconfig for the
	// read-only identity, mounted by inspection instances.
	ReadOnlyKubeconfigSecretName = "agent-kubeconfig-inspect"
	AgentLabelApp                = "cubepilot-agent"
	AgentLabelUser               = "cubepilot/user"
	// AgentLabelInspect marks read-only inspection resources (design §5.4).
	// Kept separate from AgentLabelApp so reconcile/GC never touch them.
	AgentLabelInspect = "cubepilot-agent-inspect"
)

// NewClient returns a clientset using in-cluster config when available, otherwise
// the kubeconfig from CUBEPILOT_KUBECONFIG or ~/.kube/config (local dev).
func NewClient() (*kubernetes.Clientset, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.BuildConfigFromFlags("", KubeconfigPath())
		if err != nil {
			return nil, err
		}
	}
	return kubernetes.NewForConfig(cfg)
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
