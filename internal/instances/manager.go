// Package instances implements the Instance Manager facade used by the
// assistant service. With the controller-runtime incarnation (design doc
// CubePilot-Cloud-for-Agents-Design.md §4.1: the Instance Manager is
// controller-based), the AgentInstance controller owns the Pod/PVC/Service
// lifecycle; this package resolves the per-(user, agent) instance and waits
// for it to be Warm and its gateway reachable.
package instances

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/resolver"
)

const (
	readyTimeout     = 60 * time.Second
	reachableTimeout = 30 * time.Second
)

// Poll cadences for warming waiters. Kept as vars (not consts) so tests can
// shorten them; production cadence is fixed at the values below.
var (
	crWarmTick    = 2 * time.Second
	reachableTick = time.Second
	reachableDial = 2 * time.Second
)

// Manager resolves and warms per-(user, agent) instances. Instances are
// AgentInstance CRs reconciled by the platform controllers; the manager
// observes the CR status and waits for the gateway to become reachable.
type Manager struct {
	cr   client.Client // controller-runtime client (CRD path)
	cfg  config.Config
	ns   string
	port int32

	resolve *resolver.Resolver

	// probe reports whether the gateway at addr accepts TCP connections. It is
	// a field so tests can stub reachability without a real Service to dial.
	probe func(addr string, timeout time.Duration) error

	mu     sync.Mutex
	active map[string]time.Time // agentKey -> last activity
}

// New constructs a Manager backed by the controller-runtime client (CRD path).
func New(cr client.Client, cfg config.Config) *Manager {
	return &Manager{
		cr:      cr,
		cfg:     cfg,
		ns:      cfg.Namespace,
		port:    int32(cfg.AgentPort),
		resolve: resolver.New(cr, cfg.Namespace),
		active:  map[string]time.Time{},
		probe:   probeTCP,
	}
}

// probeTCP dials addr and closes the connection on success; non-nil error
// means the gateway is not reachable yet.
func probeTCP(addr string, timeout time.Duration) error {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// AgentKey is the instance key = user + agent (design §3.2).
type AgentKey struct {
	User  string
	Agent string
}

func (k AgentKey) String() string {
	if k.Agent == "" {
		k.Agent = v1alpha1.DefaultAgentName
	}
	return k.User + "/" + k.Agent
}

// InstanceName returns the CR name for the key.
func (k AgentKey) InstanceName() string {
	agent := k.Agent
	if agent == "" {
		agent = v1alpha1.DefaultAgentName
	}
	return k8s.InstanceName(k.User, agent)
}

// BaseURL returns the in-cluster gateway URL for the default agent of user.
func (m *Manager) BaseURL(user string) string {
	return m.BaseURLFor(AgentKey{User: user, Agent: v1alpha1.DefaultAgentName})
}

// BaseURLFor returns the in-cluster gateway URL for an agentKey. The
// controller names the instance's Pod/Service `agent-<instanceName>`
// (design §3.2), so the URL uses that resource name.
func (m *Manager) BaseURLFor(k AgentKey) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", k8s.ResourceName("agent", k.InstanceName()), m.ns, m.port)
}

// Touch records activity for an agentKey (keeps the instance warm).
func (m *Manager) Touch(user string) {
	m.TouchFor(AgentKey{User: user, Agent: v1alpha1.DefaultAgentName})
}

// TouchFor records activity for an agentKey.
func (m *Manager) TouchFor(k AgentKey) {
	m.mu.Lock()
	m.active[k.String()] = time.Now()
	m.mu.Unlock()
}

// Ensure guarantees a healthy agent instance for the default agent of user
// (delegates to EnsureFor).
func (m *Manager) Ensure(ctx context.Context, user string) error {
	return m.EnsureFor(ctx, AgentKey{User: user, Agent: v1alpha1.DefaultAgentName})
}

// EnsureFor guarantees a healthy agent instance exists for the key, waiting
// for the gateway to become ready.
func (m *Manager) EnsureFor(ctx context.Context, k AgentKey) error {
	m.TouchFor(k)
	if err := m.waitCRWarm(ctx, k.InstanceName()); err != nil {
		return err
	}
	return m.waitReachableFor(ctx, k8s.ResourceName("agent", k.InstanceName()))
}

// waitCRWarm waits until the AgentInstance CR reaches the Ready phase (or the
// deadline). The warm case is the common one -- every runtime-touching request
// warms on the way in -- so it probes once before starting the ticker instead
// of paying a full tick for an instance that is already Ready.
func (m *Manager) waitCRWarm(ctx context.Context, instanceName string) error {
	if m.crReady(ctx, instanceName) {
		return nil
	}
	deadline := time.After(readyTimeout)
	tick := time.NewTicker(crWarmTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("agent instance %s not warm within timeout", instanceName)
		case <-tick.C:
			if m.crReady(ctx, instanceName) {
				return nil
			}
		}
	}
}

// crReady reports whether the AgentInstance CR is in the Ready phase.
func (m *Manager) crReady(ctx context.Context, instanceName string) bool {
	var inst v1alpha1.AgentInstance
	err := m.cr.Get(ctx, types.NamespacedName{Namespace: m.ns, Name: instanceName}, &inst)
	if err != nil {
		return false // controller still creating / transient read error
	}
	switch inst.Status.Phase {
	case v1alpha1.InstanceReady:
		return true
	case v1alpha1.InstanceFailed:
		// Let the controller heal; keep waiting (transient).
		log.Printf("instances: %s failed (%s), waiting for heal", instanceName, inst.Status.Message)
	}
	return false
}

// SelectedModelFor resolves the per-turn model override for a user's agent
// instance (design §3.2/§3.3). It is non-empty only when the instance
// explicitly set selectedModel (instance.spec.selectedModel -> Agent
// definition models -> spec.modelId). Empty means "no override -- use the
// runtime's configured primary model", so the deployer's provider config
// decides the default and no provider-key naming convention is required.
// An explicitly selected model that is missing from the catalog or outside
// the agent's availableModels is an error (fail-closed, never a silent
// fallback); an empty selection is not an error.
func (m *Manager) SelectedModelFor(ctx context.Context, user string) (string, error) {
	cfg, err := m.resolve.ResolveForUser(ctx, user)
	if err != nil {
		return "", err
	}
	return cfg.SelectedModel, nil
}

// ResolvedConfigForUser returns the fully-resolved agent configuration for a
// user's default instance (AgentTemplate + AgentInstance + Model catalog +
// Skills merged). It is the artifact the agent-side supervisor pulls
// via the internal API to render runtime skills and detect reloads.
func (m *Manager) ResolvedConfigForUser(ctx context.Context, user string) (*resolver.ResolvedAgentConfig, error) {
	return m.resolve.ResolveForUser(ctx, user)
}

// InstanceStatus reports whether the user's agent instance exists and its
// phase (Portal instance status card).
func (m *Manager) InstanceStatus(ctx context.Context, user string) (exists bool, phase string, startedAt time.Time) {
	return m.InstanceStatusFor(ctx, AgentKey{User: user, Agent: v1alpha1.DefaultAgentName})
}

// InstanceStatusFor reports the live state of an agent instance.
func (m *Manager) InstanceStatusFor(ctx context.Context, k AgentKey) (exists bool, phase string, startedAt time.Time) {
	var inst v1alpha1.AgentInstance
	if err := m.cr.Get(ctx, types.NamespacedName{Namespace: m.ns, Name: k.InstanceName()}, &inst); err != nil {
		return false, "not provisioned (resident policy)", time.Time{}
	}
	p := string(inst.Status.Phase)
	if p == "" {
		p = "Creating"
	}
	return true, p, time.Time{}
}

// waitReachableFor waits until the instance gateway accepts TCP connections
// (the gateway is up and the Service routes to it). Like waitCRWarm it probes
// once up front so an already-reachable gateway does not cost a full tick.
func (m *Manager) waitReachableFor(ctx context.Context, podName string) error {
	addr := fmt.Sprintf("%s.%s.svc:%d", podName, m.ns, m.port)
	if m.reachable(addr) {
		return nil
	}
	deadline := time.After(reachableTimeout)
	tick := time.NewTicker(reachableTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("agent service %s not reachable within timeout", addr)
		case <-tick.C:
			if m.reachable(addr) {
				return nil
			}
		}
	}
}

// reachable reports whether the gateway at addr accepts a TCP connection.
func (m *Manager) reachable(addr string) bool {
	return m.probe(addr, reachableDial) == nil
}

var _ = metav1.Now
