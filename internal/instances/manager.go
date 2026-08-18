// Package instances implements the Instance Manager facade used by the
// assistant service. With the controller-runtime incarnation (design doc
// CubePilot-Cloud-for-Agents-Design.md §4.1: Instance Manager 控制器化), the
// AgentInstance controller owns the Pod/PVC/Service lifecycle; this package
// resolves the per-(user, agent) instance and waits for it to be Warm, and
// keeps the legacy in-process reconcile/GC for non-CRD deployments.
package instances

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/leader"
	"github.com/suanova/cubepilot/internal/metrics"
)

const (
	readyTimeout   = 60 * time.Second
	reconcileEvery = 30 * time.Second
	gcEvery        = 5 * time.Minute
)

// Manager resolves and warms per-(user, agent) instances. When the CRD path
// is enabled (crClient != nil), instances are AgentInstance CRs reconciled by
// the controller; otherwise it falls back to the legacy Pod/PVC manager.
type Manager struct {
	client  *kubernetes.Clientset
	cr      client.Client // controller-runtime client (CRD path); nil = legacy
	spec    k8s.AgentSpec
	cfg     config.Config
	ns      string
	agentNS string // namespace of platform CRs (cluster-scoped → unused)

	mu     sync.Mutex
	active map[string]time.Time // agentKey -> last activity
	ttl    time.Duration

	reclaimEnabled bool
	gcWindow       time.Duration
	gcWatermark    float64
	elector        *leader.Elector
}

// New constructs a Manager. cr is the controller-runtime client used to watch
// AgentInstance CRs (may be nil for the legacy path).
func New(client *kubernetes.Clientset, cr client.Client, cfg config.Config) *Manager {
	return &Manager{
		client: client,
		cr:     cr,
		cfg:    cfg,
		spec: k8s.AgentSpec{
			Namespace:    cfg.Namespace,
			Image:        cfg.AgentImage,
			GatewayToken: cfg.GatewayToken,
			Port:         int32(cfg.AgentPort),
		},
		ns:             cfg.Namespace,
		active:         map[string]time.Time{},
		ttl:            cfg.IdleTTL,
		reclaimEnabled: cfg.ReclaimEnabled,
		gcWindow:       cfg.GCWindow,
		gcWatermark:    cfg.GCWatermark,
	}
}

// SetElector attaches the shared leader elector (reconcile/GC leader-only).
func (m *Manager) SetElector(e *leader.Elector) { m.elector = e }

func (m *Manager) isLeader() bool {
	return m.elector == nil || m.elector.IsLeader()
}

// AgentKey is the instance key = user + agent (设计 §3.2).
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

// BaseURL returns the in-cluster gateway URL for the agent instance.
func (m *Manager) BaseURL(user string) string {
	return m.BaseURLFor(AgentKey{User: user, Agent: v1alpha1.DefaultAgentName})
}

// BaseURLFor returns the in-cluster gateway URL for an agentKey. The
// controller names the instance's Pod/Service `agent-<instanceName>`
// (design §3.2: podName: agent-zhang-wei-agent-for-cloud), so the URL uses
// that resource name.
func (m *Manager) BaseURLFor(k AgentKey) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", k8s.ResourceName("agent", k.InstanceName()), m.ns, m.spec.Port)
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
// (legacy signature; delegates to EnsureFor).
func (m *Manager) Ensure(ctx context.Context, user string) error {
	return m.EnsureFor(ctx, AgentKey{User: user, Agent: v1alpha1.DefaultAgentName})
}

// EnsureFor guarantees a healthy agent instance exists for the key, waiting
// for the gateway to become ready.
func (m *Manager) EnsureFor(ctx context.Context, k AgentKey) error {
	m.TouchFor(k)
	instanceName := k.InstanceName()

	if m.cr != nil {
		// CRD path: the AgentInstance controller owns the lifecycle; wait for
		// the instance to be Warm and the gateway reachable.
		if err := m.waitCRWarm(ctx, instanceName); err != nil {
			return err
		}
		return m.waitReachableFor(ctx, k8s.ResourceName("agent", instanceName))
	}

	// Legacy path: in-process Pod management (kept for non-CRD deployments).
	return m.ensureLegacy(ctx, k.User)
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

// InstanceStatus reports whether the user's agent instance exists and its
// phase (Portal 实例状态 card).
func (m *Manager) InstanceStatus(ctx context.Context, user string) (exists bool, phase string, startedAt time.Time) {
	return m.InstanceStatusFor(ctx, AgentKey{User: user, Agent: v1alpha1.DefaultAgentName})
}

// InstanceStatusFor reports the live state of an agent instance.
func (m *Manager) InstanceStatusFor(ctx context.Context, k AgentKey) (exists bool, phase string, startedAt time.Time) {
	instanceName := k.InstanceName()
	if m.cr != nil {
		var inst v1alpha1.AgentInstance
		if err := m.cr.Get(ctx, types.NamespacedName{Name: instanceName}, &inst); err != nil {
			return false, "未拉起（常驻策略）", time.Time{}
		}
		p := string(inst.Status.Phase)
		if p == "" {
			p = "Creating"
		}
		return true, p, time.Time{}
	}
	pod, err := m.client.CoreV1().Pods(m.ns).Get(ctx, k8s.ResourceName("agent", k.User), metav1.GetOptions{})
	if err != nil {
		if m.reclaimEnabled {
			return false, "回收中（按需拉起）", time.Time{}
		}
		return false, "未拉起（常驻策略）", time.Time{}
	}
	if pod.Status.StartTime != nil {
		startedAt = pod.Status.StartTime.Time
	}
	return true, string(pod.Status.Phase), startedAt
}

// Run starts the background reconciliation loop (legacy path only; the CRD
// controller replaces it when enabled).
func (m *Manager) Run(ctx context.Context) {
	if m.cr != nil {
		return // CRD path: the controller owns reconcile/GC.
	}
	go func() {
		t := time.NewTicker(reconcileEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !m.isLeader() {
					continue
				}
				if err := m.reconcile(ctx); err != nil {
					log.Printf("instances: reconcile: %v", err)
				}
			}
		}
	}()
	go func() {
		t := time.NewTicker(gcEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !m.isLeader() {
					continue
				}
				if err := m.gcDataDirs(ctx); err != nil {
					log.Printf("instances: data-dir GC: %v", err)
				}
			}
		}
	}()
}

// ---- legacy Pod manager (non-CRD deployments) ----

func (m *Manager) ensureLegacy(ctx context.Context, user string) error {
	podName := k8s.ResourceName("agent", user)
	existing, err := m.client.CoreV1().Pods(m.ns).Get(ctx, podName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if err := m.createResources(ctx, user); err != nil {
			return err
		}
	case err != nil:
		return err
	case isCrashLoop(existing):
		log.Printf("instances: recreating crashed pod %s", podName)
		if err := m.client.CoreV1().Pods(m.ns).Delete(ctx, podName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err := m.createResources(ctx, user); err != nil {
			return err
		}
	}
	if err := m.waitReady(ctx, user); err != nil {
		return err
	}
	return m.waitReachable(ctx, user)
}

func (m *Manager) createResources(ctx context.Context, user string) error {
	pvc := m.spec.DataPVC(user)
	if _, err := m.client.CoreV1().PersistentVolumeClaims(m.ns).Get(ctx, pvc.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := m.client.CoreV1().PersistentVolumeClaims(m.ns).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create pvc: %w", err)
		}
	}
	svc := m.spec.Service(user)
	if _, err := m.client.CoreV1().Services(m.ns).Get(ctx, svc.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := m.client.CoreV1().Services(m.ns).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create service: %w", err)
		}
	}
	pod := m.spec.Pod(user)
	if _, err := m.client.CoreV1().Pods(m.ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create pod: %w", err)
	}
	return nil
}

func (m *Manager) waitReady(ctx context.Context, user string) error {
	deadline := time.After(readyTimeout)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errors.New("agent instance not ready within timeout")
		case <-tick.C:
			pod, err := m.client.CoreV1().Pods(m.ns).Get(ctx, k8s.ResourceName("agent", user), metav1.GetOptions{})
			if err != nil {
				continue
			}
			if podReady(pod) {
				return nil
			}
		}
	}
}

func (m *Manager) waitReachable(ctx context.Context, user string) error {
	return m.waitReachableFor(ctx, k8s.ResourceName("agent", user))
}

func (m *Manager) waitReachableFor(ctx context.Context, podName string) error {
	addr := fmt.Sprintf("%s.%s.svc:%d", podName, m.ns, m.spec.Port)
	deadline := time.After(30 * time.Second)
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

func (m *Manager) reconcile(ctx context.Context) error {
	pods, err := m.client.CoreV1().Pods(m.ns).List(ctx, metav1.ListOptions{
		LabelSelector: k8s.AgentLabelApp + "=true",
	})
	if err != nil {
		return err
	}
	now := time.Now()
	metrics.SetGauge("cubepilot_pool_instances", int64(len(pods.Items)))

	existing := map[string]bool{}
	for _, pod := range pods.Items {
		user := pod.Labels[k8s.AgentLabelUser]
		existing[user] = true
		if m.reclaimEnabled {
			m.mu.Lock()
			last, ok := m.active[user]
			if !ok {
				last = pod.CreationTimestamp.Time
				m.active[user] = last
			}
			m.mu.Unlock()
			if now.Sub(last) > m.ttl {
				log.Printf("instances: reclaiming idle instance for %s", user)
				metrics.Inc("cubepilot_pool_reclaims_total", "", 1)
				if err := m.client.CoreV1().Pods(m.ns).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
					return err
				}
				continue
			}
		}
		if isCrashLoop(&pod) {
			log.Printf("instances: healing crashed instance for %s", user)
			metrics.Inc("cubepilot_pool_rebuilds_total", "", 1)
			if err := m.client.CoreV1().Pods(m.ns).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			_ = m.createResources(ctx, user)
		}
	}
	if !m.reclaimEnabled {
		pvcs, err := m.client.CoreV1().PersistentVolumeClaims(m.ns).List(ctx, metav1.ListOptions{
			LabelSelector: k8s.AgentLabelApp + "=true",
		})
		if err != nil {
			return err
		}
		for _, pvc := range pvcs.Items {
			user := pvc.Labels[k8s.AgentLabelUser]
			if user == "" || existing[user] {
				continue
			}
			log.Printf("instances: re-creating missing resident instance for %s", user)
			metrics.Inc("cubepilot_pool_rebuilds_total", "", 1)
			if err := m.createResources(ctx, user); err != nil {
				log.Printf("instances: recreate %s: %v", user, err)
			}
		}
	}
	return nil
}

func (m *Manager) gcDataDirs(ctx context.Context) error {
	pvcs, err := m.client.CoreV1().PersistentVolumeClaims(m.ns).List(ctx, metav1.ListOptions{
		LabelSelector: k8s.AgentLabelApp + "=true",
	})
	if err != nil {
		return fmt.Errorf("list pvcs: %w", err)
	}
	for _, pvc := range pvcs.Items {
		user := pvc.Labels[k8s.AgentLabelUser]
		if user == "" {
			continue
		}
		if err := m.gcUserData(ctx, user); err != nil {
			log.Printf("instances: gc %s: %v", user, err)
		}
	}
	return nil
}

func (m *Manager) gcUserData(ctx context.Context, user string) error {
	podName := k8s.ResourceName("agent", user)
	pod, err := m.client.CoreV1().Pods(m.ns).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get pod: %w", err)
	}
	if !podReady(pod) {
		return fmt.Errorf("pod not ready, skip")
	}
	hours := int64(m.gcWindow.Hours())
	if hours <= 0 {
		hours = 72
	}
	cmd := fmt.Sprintf(
		`find /home/node/.openclaw -type f \( -name '*.jsonl' -o -name 'sessions.json' -o -name '*.json' \) -mmin +%d -delete 2>/dev/null; echo gc-done`, hours*60)
	if _, err := m.execInPod(ctx, podName, "gateway", cmd); err != nil {
		return fmt.Errorf("exec gc: %w", err)
	}
	df, err := m.execInPod(ctx, podName, "gateway",
		`df -P /home/node/.openclaw | awk 'NR==2 {print $5}' | tr -d '%'`)
	if err != nil {
		return fmt.Errorf("df: %w", err)
	}
	pct, err := strconv.Atoi(strings.TrimSpace(df))
	if err == nil && float64(pct)/100 > m.gcWatermark {
		log.Printf("instances: PVC watermark %d%% > %.0f%% for %s, aggressive GC advised",
			pct, m.gcWatermark*100, user)
	}
	return nil
}

// execInPod runs cmd in the named container of pod via the Kubernetes exec
// API and returns combined output (or an error carrying it).
func (m *Manager) execInPod(ctx context.Context, pod, container, cmd string) (string, error) {
	req := m.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(m.ns).
		SubResource("exec").
		Param("container", container).
		VersionedParams(&corev1.PodExecOptions{
			Command: []string{"/bin/sh", "-c", cmd},
			Stdout:  true,
			Stderr:  true,
		}, scheme.ParameterCodec)

	cfg, err := k8s.NewRestConfig()
	if err != nil {
		return "", fmt.Errorf("rest config: %w", err)
	}
	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("spdy executor: %w", err)
	}
	var buf bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &buf,
		Stderr: &buf,
	})
	if err != nil {
		return buf.String(), fmt.Errorf("exec: %w", err)
	}
	return buf.String(), nil
}

func isCrashLoop(pod *corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodFailed {
		return true
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready && cs.RestartCount >= 3 {
			return true
		}
	}
	return false
}

func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
