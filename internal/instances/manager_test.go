package instances

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

// okProbe is a reachability probe that always succeeds.
func okProbe(addr string, timeout time.Duration) error { return nil }

func testManager(t *testing.T, objs ...client.Object) *Manager {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.AgentInstance{}, &v1alpha1.AgentTemplate{}).
		WithObjects(objs...).
		Build()
	return New(cl, config.Config{DefaultUser: "zhang.wei"})
}

func instance(user, selected string) *v1alpha1.AgentInstance {
	return &v1alpha1.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: k8s.InstanceName(user, v1alpha1.DefaultAgentName)},
		Spec: v1alpha1.AgentInstanceSpec{
			TemplateRef:   v1alpha1.DefaultAgentName,
			Owner:         user,
			SelectedModel: selected,
		},
	}
}

// TestSelectedModelForResolvesDefault verifies that with no explicit
// instance selection there is no override: the runtime uses its configured
// primary (the deployer's provider config decides the default).
func TestSelectedModelForResolvesDefault(t *testing.T) {
	agent := &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.DefaultAgentName},
		Spec: v1alpha1.AgentTemplateSpec{
			DefaultModel: "deepseek-v4-flash",
			Models: []v1alpha1.TemplateModelSpec{
				{Name: "deepseek-v4-flash", Endpoint: "https://api.deepseek.com"},
			},
		},
	}
	m := testManager(t, agent, instance("li.ming", ""))

	got, err := m.SelectedModelFor(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("SelectedModelFor: %v", err)
	}
	if got != "" {
		t.Errorf("model = %q, want empty (no override for default)", got)
	}
}

// TestSelectedModelForExplicit verifies an explicit instance selection wins
// over the agent default (design §3.2: selectedModel overrides).
func TestSelectedModelForExplicit(t *testing.T) {
	agent := &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.DefaultAgentName},
		Spec: v1alpha1.AgentTemplateSpec{
			DefaultModel: "deepseek-v4-flash",
			Models: []v1alpha1.TemplateModelSpec{
				{Name: "deepseek-v4-flash", Endpoint: "https://api.deepseek.com"},
				{Name: "deepseek-chat", Endpoint: "https://api.deepseek.com"},
			},
		},
	}
	m := testManager(t, agent, instance("li.ming", "deepseek-chat"))

	got, err := m.SelectedModelFor(context.Background(), "li.ming")
	if err != nil {
		t.Fatalf("SelectedModelFor: %v", err)
	}
	if got != "deepseek-chat/deepseek-chat" {
		t.Errorf("model = %q, want deepseek-chat/deepseek-chat", got)
	}
}

// TestSelectedModelForOutsideAllowlist verifies fail-closed: a selection
// outside the agent's inline models is an error, never a silent fallback.
func TestSelectedModelForOutsideAllowlist(t *testing.T) {
	agent := &v1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.DefaultAgentName},
		Spec: v1alpha1.AgentTemplateSpec{
			DefaultModel: "deepseek-v4-flash",
			Models: []v1alpha1.TemplateModelSpec{
				{Name: "deepseek-v4-flash", Endpoint: "https://api.deepseek.com"},
			},
		},
	}
	m := testManager(t, agent, instance("li.ming", "glm-5.2"))

	if _, err := m.SelectedModelFor(context.Background(), "li.ming"); err == nil {
		t.Error("selection outside inline models should fail (fail-closed)")
	}
}

// TestSelectedModelForNoConfig verifies no instance / no selection / no
// default -> empty (runtime default), which is not an error.
func TestSelectedModelForNoConfig(t *testing.T) {
	m := testManager(t)
	if _, err := m.SelectedModelFor(context.Background(), "nobody"); err != nil {
		t.Errorf("no instance should be empty, not error: %v", err)
	}
}

// instanceWithPhase creates the user's AgentInstance CR with the given status
// phase. The fake client's status subresource strips .Status on Create, so the
// phase is applied through a Status().Update.
func instanceWithPhase(t *testing.T, m *Manager, user string, phase v1alpha1.InstancePhase) {
	t.Helper()
	inst := instance(user, "")
	if err := m.cr.Create(context.Background(), inst); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	inst.Status.Phase = phase
	if err := m.cr.Status().Update(context.Background(), inst); err != nil {
		t.Fatalf("update instance status: %v", err)
	}
}

// flipToReadyClient serves the underlying fake but reports the instance as
// Ready from the n-th Get on, letting a test drive a Creating -> Ready
// transition deterministically (no goroutine racing the poller).
type flipToReadyClient struct {
	client.Client
	gets      int
	readyFrom int
}

func (c *flipToReadyClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	c.gets++
	if c.gets >= c.readyFrom {
		if inst, ok := obj.(*v1alpha1.AgentInstance); ok {
			inst.Status.Phase = v1alpha1.InstanceReady
		}
	}
	return nil
}

// shortenTicks shrinks the warming poll cadences so tests exercise the
// wait-loops (not just the fast paths) without real 2s/1s sleeps.
func shortenTicks(t *testing.T) {
	t.Helper()
	oldCR, oldR := crWarmTick, reachableTick
	crWarmTick, reachableTick = time.Millisecond, time.Millisecond
	t.Cleanup(func() { crWarmTick, reachableTick = oldCR, oldR })
}

// TestEnsureWarmFastPath locks the warm path latency: an instance that is
// already Ready with a reachable gateway must return without waiting out the
// 2s CR + 1s reachability poll cadence. Regression for the ~3s per-request
// warming delay that made every runtime-touching request (session list,
// history, chat) stall on page load.
func TestEnsureWarmFastPath(t *testing.T) {
	m := testManager(t)
	instanceWithPhase(t, m, "li.ming", v1alpha1.InstanceReady)
	m.probe = okProbe

	start := time.Now()
	if err := m.EnsureFor(context.Background(), AgentKey{User: "li.ming", Agent: v1alpha1.DefaultAgentName}); err != nil {
		t.Fatalf("EnsureFor: %v", err)
	}
	if d := time.Since(start); d >= time.Second {
		t.Errorf("warm EnsureFor took %v; want the fast path (Ready + reachable) without ticker delay", d)
	}
}

// TestEnsureWaitsUntilWarm verifies the polling semantics are preserved: when
// the CR is not yet Ready, EnsureFor keeps waiting and returns as soon as it
// flips to Ready (it must not fail fast or return while still Creating).
func TestEnsureWaitsUntilWarm(t *testing.T) {
	shortenTicks(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.AgentInstance{}, &v1alpha1.AgentTemplate{}).
		Build()
	inst := instance("li.ming", "")
	if err := base.Create(context.Background(), inst); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	inst.Status.Phase = v1alpha1.InstanceCreating
	if err := base.Status().Update(context.Background(), inst); err != nil {
		t.Fatalf("update instance status: %v", err)
	}
	flip := &flipToReadyClient{Client: base, readyFrom: 2}
	m := New(flip, config.Config{DefaultUser: "zhang.wei"})
	m.probe = okProbe

	if err := m.EnsureFor(context.Background(), AgentKey{User: "li.ming", Agent: v1alpha1.DefaultAgentName}); err != nil {
		t.Fatalf("EnsureFor: %v", err)
	}
	if flip.gets != 2 {
		t.Errorf("observed %d CR Gets; want 2 (fast pre-check sees Creating, one poll sees Ready)", flip.gets)
	}
}

// TestEnsureWaitsUntilReachable verifies the reachability poller still waits
// when the gateway is not yet accepting connections.
func TestEnsureWaitsUntilReachable(t *testing.T) {
	shortenTicks(t)
	m := testManager(t)
	instanceWithPhase(t, m, "li.ming", v1alpha1.InstanceReady)
	calls := 0
	m.probe = func(addr string, timeout time.Duration) error {
		calls++
		if calls < 2 {
			return context.DeadlineExceeded // not reachable yet
		}
		return nil
	}

	if err := m.EnsureFor(context.Background(), AgentKey{User: "li.ming", Agent: v1alpha1.DefaultAgentName}); err != nil {
		t.Fatalf("EnsureFor: %v", err)
	}
	if calls != 2 {
		t.Errorf("made %d reachability probes; want 2 (fast probe fails, one poll succeeds)", calls)
	}
}
