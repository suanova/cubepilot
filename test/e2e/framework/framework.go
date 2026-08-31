// Package framework provides the clients and helpers the CubePilot e2e suite
// uses to talk to a deployed stack (kind + helm). It mirrors the pattern of
// the OCM project's test/e2e/framework helpers, adapted to CubePilot: clients
// are built once in BeforeSuite from the target cluster's kubeconfig, and the
// specs assert against the live operator / api / portal.
package framework

import (
	"fmt"
	"os"
	"strings"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// Framework holds the clients and shared state for the e2e suite. It is
// created once in BeforeSuite from the target cluster's kubeconfig.
type Framework struct {
	RestConfig   *rest.Config
	KubeClient   kubernetes.Interface
	ApiExtClient apiextensionsclient.Interface
	CtrlClient   crclient.Client

	// Namespace / Users / DefaultUser mirror internal/config.Load() defaults so
	// the expectations match the deployed operator's config.
	Namespace   string
	Users       []string
	DefaultUser string

	// APIBase / PortalBase are the http://127.0.0.1:<port> port-forwards to
	// the cubepilot-api and portal services, opened in BeforeSuite.
	APIBase    string
	PortalBase string
}

// New builds a Framework against the cluster named by kubeconfig.
func New(kubeconfig string) (*Framework, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube client: %w", err)
	}
	ac, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("apiextensions client: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("v1alpha1 scheme: %w", err)
	}
	cc, err := crclient.New(cfg, crclient.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("controller-runtime client: %w", err)
	}
	// Validate the users list: a value such as "," trims to zero users, and the
	// specs index Users[0] -- fail with a config error instead of panicking.
	users := splitUsers(getenv("CUBEPILOT_USERS", "zhang.wei,li.ming"))
	if len(users) == 0 {
		return nil, fmt.Errorf("CUBEPILOT_USERS must contain at least one user")
	}
	return &Framework{
		RestConfig:   cfg,
		KubeClient:   kc,
		ApiExtClient: ac,
		CtrlClient:   cc,
		Namespace:    getenv("CUBEPILOT_NAMESPACE", "cubepilot"),
		DefaultUser:  getenv("CUBEPILOT_DEFAULT_USER", "zhang.wei"),
		Users:        users,
	}, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitUsers(s string) []string {
	var users []string
	for _, u := range strings.Split(s, ",") {
		if u = strings.TrimSpace(u); u != "" {
			users = append(users, u)
		}
	}
	return users
}
