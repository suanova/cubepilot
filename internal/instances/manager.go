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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

const (
	readyTimeout     = 60 * time.Second
	reachableTimeout = 30 * time.Second
)

// Manager resolves and warms per-(user, agent) instances. Instances are
// AgentInstance CRs reconciled by the platform controllers; the manager
// observes the CR status and waits for the gateway to become reachable.
type Manager struct {
	cr   client.Client // controller-runtime client (CRD path)
	cfg  config.Config
	ns   string
	port int32

	mu     sync.Mutex
	active map[string]time.Time // agentKey -> last activity
}

// New constructs a Manager backed by the controller-runtime client (CRD path).
func New(cr client.Client, cfg config.Config) *Manager {
	return &Manager{
		cr:     cr,
		cfg:    cfg,
		ns:     cfg.Namespace,
		port:   int32(cfg.AgentPort),
		active: map[string]time.Time{},
	}
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

// waitCRWarm polls the AgentInstance CR until phase == Warm (or the deadline).
func (m *Manager) waitCRWarm(ctx context.Context, instanceName string) error {
	deadline := time.After(readyTimeout)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("agent instance %s not warm within timeout", instanceName)
		case <-tick.C:
			var inst v1alpha1.AgentInstance
			err := m.cr.Get(ctx, types.NamespacedName{Name: instanceName}, &inst)
			if err != nil {
				if apierrors.IsNotFound(err) {
					continue // controller still creating
				}
				continue
			}
			switch inst.Status.Phase {
			case v1alpha1.InstanceWarm:
				return nil
			case v1alpha1.InstanceFailed:
				// Let the controller heal; keep waiting (transient).
				log.Printf("instances: %s failed (%s), waiting for heal", instanceName, inst.Status.Message)
			}
		}
	}
}

// SelectedModelFor resolves the effective backend model id for a user's
// agent instance (design §3.2/§3.3): instance.spec.selectedModel → Model
// catalog → spec.modelId. Empty means "no override — use the runtime's
// normal configured model". An explicitly selected model that is missing
// from the catalog or Unreachable is an error (fail-closed, never a silent
// fallback); an empty selection is not an error.
func (m *Manager) SelectedModelFor(ctx context.Context, user string) (string, error) {
	var inst v1alpha1.AgentInstance
	if err := m.cr.Get(ctx, types.NamespacedName{Name: k8s.InstanceName(user, v1alpha1.DefaultAgentName)}, &inst); err != nil {
		return "", nil // not provisioned yet — runtime default
	}
	selected := inst.Spec.SelectedModel
	if selected == "" {
		return "", nil // no explicit selection — runtime default
	}
	var model v1alpha1.Model
	if err := m.cr.Get(ctx, types.NamespacedName{Name: selected}, &model); err != nil {
		return "", fmt.Errorf("model %q not in catalog: %v", selected, err)
	}
	if model.Status.Phase == v1alpha1.ModelUnreachable {
		return "", fmt.Errorf("model %q unavailable: %s", selected, model.Status.Message)
	}
	if model.Spec.ModelID == "" {
		return "", nil // platform default, no override
	}
	return model.Spec.ModelID, nil
}

// InstanceStatus reports whether the user's agent instance exists and its
// phase (Portal instance status card).
func (m *Manager) InstanceStatus(ctx context.Context, user string) (exists bool, phase string, startedAt time.Time) {
	return m.InstanceStatusFor(ctx, AgentKey{User: user, Agent: v1alpha1.DefaultAgentName})
}

// InstanceStatusFor reports the live state of an agent instance.
func (m *Manager) InstanceStatusFor(ctx context.Context, k AgentKey) (exists bool, phase string, startedAt time.Time) {
	var inst v1alpha1.AgentInstance
	if err := m.cr.Get(ctx, types.NamespacedName{Name: k.InstanceName()}, &inst); err != nil {
		return false, "not provisioned (resident policy)", time.Time{}
	}
	p := string(inst.Status.Phase)
	if p == "" {
		p = "Creating"
	}
	return true, p, time.Time{}
}

// waitReachableFor polls the instance gateway until the TCP port accepts
// connections (the gateway is up and the Service routes to it).
func (m *Manager) waitReachableFor(ctx context.Context, podName string) error {
	addr := fmt.Sprintf("%s.%s.svc:%d", podName, m.ns, m.port)
	deadline := time.After(reachableTimeout)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("agent service %s not reachable within timeout", addr)
		case <-tick.C:
			conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err == nil {
				_ = conn.Close()
				return nil
			}
		}
	}
}

var _ = metav1.Now
