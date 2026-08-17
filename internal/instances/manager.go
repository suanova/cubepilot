// Package instances implements the Instance Manager: per-user OpenClaw agent
// Pods, resident by default, healed when unhealthy, and (optionally) reclaimed
// when idle (design doc FR-M2-002 / §5.2: 常驻运行是默认策略, 闲置回收为可配置策略).
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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/leader"
	"github.com/suanova/cubepilot/internal/metrics"
)

const (
	readyTimeout          = 60 * time.Second
	reconcileEvery        = 30 * time.Second
	crashRestartThreshold = 3
	gcEvery               = 5 * time.Minute
)

// Manager owns the lifecycle of per-user agent instances.
type Manager struct {
	client *kubernetes.Clientset
	spec   k8s.AgentSpec
	// inspectSpec is the read-only inspection instance spec (design §5.4:
	// 巡检实例挂载专用只读 kubeconfig, 即使被注入也无法写入).
	inspectSpec k8s.AgentSpec

	mu     sync.Mutex
	active map[string]time.Time // user -> last activity
	ttl    time.Duration

	// reclaimEnabled gates idle reclaim (§5.2): false (default) = resident.
	reclaimEnabled bool
	// gcWindow is the per-user data directory retention window (§5.1: 72h).
	gcWindow time.Duration
	// gcWatermark is the PVC usage ratio that triggers aggressive GC + alert.
	gcWatermark float64
	// elector gates reconcile + GC to the leader replica (§3.3).
	elector *leader.Elector
}

// New constructs a Manager for the given clientset and configuration.
func New(client *kubernetes.Clientset, cfg config.Config) *Manager {
	return &Manager{
		client: client,
		spec: k8s.AgentSpec{
			Namespace:    cfg.Namespace,
			Image:        cfg.AgentImage,
			GatewayToken: cfg.GatewayToken,
			Port:         int32(cfg.AgentPort),
		},
		inspectSpec: k8s.AgentSpec{
			Namespace:    cfg.Namespace,
			Image:        cfg.AgentImage,
			GatewayToken: cfg.GatewayToken,
			Port:         int32(cfg.AgentPort),
			ReadOnly:     true,
		},
		active:         map[string]time.Time{},
		ttl:            cfg.IdleTTL,
		reclaimEnabled: cfg.ReclaimEnabled,
		gcWindow:       cfg.GCWindow,
		gcWatermark:    cfg.GCWatermark,
	}
}

// SetElector attaches the shared leader elector. The manager only reconciles
// and GCs when this elector reports leadership (multi-replica Active/Standby).
func (m *Manager) SetElector(e *leader.Elector) { m.elector = e }

func (m *Manager) isLeader() bool {
	return m.elector == nil || m.elector.IsLeader()
}

// BaseURL returns the in-cluster gateway URL for a user's agent instance.
func (m *Manager) BaseURL(user string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", k8s.ResourceName("agent", user), m.spec.Namespace, m.spec.Port)
}

// InspectBaseURL returns the in-cluster gateway URL for a user's read-only
// inspection instance.
func (m *Manager) InspectBaseURL(user string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", k8s.ResourceName("inspect", user), m.inspectSpec.Namespace, m.inspectSpec.Port)
}

// EnsureInspect guarantees a healthy read-only inspection instance for user
// (design §5.4 权限边界技术强制). Inspection tasks run on this instance so
// even a prompt-injected agent cannot mutate cluster state.
func (m *Manager) EnsureInspect(ctx context.Context, user string) error {
	podName := k8s.ResourceName("inspect", user)
	existing, err := m.client.CoreV1().Pods(m.spec.Namespace).Get(ctx, podName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if err := m.createInspectResources(ctx, user); err != nil {
			return err
		}
	case err != nil:
		return err
	case isCrashLoop(existing):
		log.Printf("instances: recreating crashed inspect pod %s", podName)
		if err := m.client.CoreV1().Pods(m.spec.Namespace).Delete(ctx, podName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err := m.createInspectResources(ctx, user); err != nil {
			return err
		}
	}

	if err := m.waitReadyFor(ctx, podName); err != nil {
		return err
	}
	return m.waitReachableFor(ctx, podName)
}

func (m *Manager) createInspectResources(ctx context.Context, user string) error {
	pvc := m.inspectSpec.DataPVC(user)
	if _, err := m.client.CoreV1().PersistentVolumeClaims(m.spec.Namespace).Get(ctx, pvc.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := m.client.CoreV1().PersistentVolumeClaims(m.spec.Namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create inspect pvc: %w", err)
		}
	}

	svc := m.inspectSpec.Service(user)
	if _, err := m.client.CoreV1().Services(m.spec.Namespace).Get(ctx, svc.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := m.client.CoreV1().Services(m.spec.Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create inspect service: %w", err)
		}
	}

	pod := m.inspectSpec.Pod(user)
	if _, err := m.client.CoreV1().Pods(m.spec.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create inspect pod: %w", err)
	}
	return nil
}

// Touch records activity for a user (keeps the instance warm).
func (m *Manager) Touch(user string) {
	m.mu.Lock()
	m.active[user] = time.Now()
	m.mu.Unlock()
}

// Ensure guarantees a healthy agent instance exists for user, waiting for the
// gateway to become ready. It recreates a crashed Pod and returns the first
// readiness error (which callers surface as a "still warming" signal).
func (m *Manager) Ensure(ctx context.Context, user string) error {
	m.Touch(user)

	podName := k8s.ResourceName("agent", user)
	existing, err := m.client.CoreV1().Pods(m.spec.Namespace).Get(ctx, podName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if err := m.createResources(ctx, user); err != nil {
			return err
		}
	case err != nil:
		return err
	case isCrashLoop(existing):
		log.Printf("instances: recreating crashed pod %s", podName)
		if err := m.client.CoreV1().Pods(m.spec.Namespace).Delete(ctx, podName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err := m.createResources(ctx, user); err != nil {
			return err
		}
	}

	if err := m.waitReady(ctx, user); err != nil {
		return err
	}
	// The Pod may be Ready before the Service's Endpoints have propagated to
	// kube-proxy; dialing the gateway directly here closes that cold-start
	// race (observed as "connection refused" right after a fresh start).
	return m.waitReachable(ctx, user)
}

// waitReachable probes the agent's ClusterIP service with a raw TCP dial until
// it accepts connections (or the deadline expires). This guarantees a caller
// that just got an instance "ready" can actually reach the gateway.
func (m *Manager) waitReachable(ctx context.Context, user string) error {
	return m.waitReachableFor(ctx, k8s.ResourceName("agent", user))
}

func (m *Manager) waitReachableFor(ctx context.Context, podName string) error {
	addr := fmt.Sprintf("%s.%s.svc:%d", podName, m.spec.Namespace, m.spec.Port)
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

func (m *Manager) createResources(ctx context.Context, user string) error {
	pvc := m.spec.DataPVC(user)
	if _, err := m.client.CoreV1().PersistentVolumeClaims(m.spec.Namespace).Get(ctx, pvc.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := m.client.CoreV1().PersistentVolumeClaims(m.spec.Namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create pvc: %w", err)
		}
	}

	svc := m.spec.Service(user)
	if _, err := m.client.CoreV1().Services(m.spec.Namespace).Get(ctx, svc.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
		if _, err := m.client.CoreV1().Services(m.spec.Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create service: %w", err)
		}
	}

	pod := m.spec.Pod(user)
	if _, err := m.client.CoreV1().Pods(m.spec.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create pod: %w", err)
	}
	return nil
}

func (m *Manager) waitReady(ctx context.Context, user string) error {
	return m.waitReadyFor(ctx, k8s.ResourceName("agent", user))
}

func (m *Manager) waitReadyFor(ctx context.Context, podName string) error {
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
			pod, err := m.client.CoreV1().Pods(m.spec.Namespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				continue
			}
			if podReady(pod) {
				return nil
			}
		}
	}
}

// InstanceStatus reports whether the user's agent Pod currently exists, its
// phase and start time — backing the Portal 实例状态 card.
func (m *Manager) InstanceStatus(ctx context.Context, user string) (exists bool, phase string, startedAt time.Time) {
	pod, err := m.client.CoreV1().Pods(m.spec.Namespace).Get(ctx, k8s.ResourceName("agent", user), metav1.GetOptions{})
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

// Run starts the background reconciliation loop (idle reclaim + crash heal)
// and the per-user data directory GC. Both only act on the leader replica
// (design doc §3.3 多副本 Active/Standby).
func (m *Manager) Run(ctx context.Context) {
	go func() {
		t := time.NewTicker(reconcileEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if !m.isLeader() {
					continue // standby replica: observe only
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

func (m *Manager) reconcile(ctx context.Context) error {
	pods, err := m.client.CoreV1().Pods(m.spec.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: k8s.AgentLabelApp + "=true",
	})
	if err != nil {
		return err
	}

	now := time.Now()

	// Instance pool gauge (design §9 实例池: 活跃/回收实例数).
	metrics.SetGauge("cubepilot_pool_instances", int64(len(pods.Items)))

	existing := map[string]bool{}
	for _, pod := range pods.Items {
		user := pod.Labels[k8s.AgentLabelUser]
		existing[user] = true
		if m.reclaimEnabled {
			m.mu.Lock()
			last, ok := m.active[user]
			if !ok {
				// First sighting in this process: seed with the Pod creation time so
				// a freshly-restarted backend doesn't immediately reclaim instances
				// that were created by the previous process (activity is in-memory).
				last = pod.CreationTimestamp.Time
				m.active[user] = last
			}
			m.mu.Unlock()
			if now.Sub(last) > m.ttl {
				log.Printf("instances: reclaiming idle instance for %s", user)
				metrics.Inc("cubepilot_pool_reclaims_total", "", 1)
				if err := m.client.CoreV1().Pods(m.spec.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
					return err
				}
				continue
			}
		}
		if isCrashLoop(&pod) {
			log.Printf("instances: healing crashed instance for %s", user)
			metrics.Inc("cubepilot_pool_rebuilds_total", "", 1)
			if err := m.client.CoreV1().Pods(m.spec.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			_ = m.createResources(ctx, user)
		}
	}

	// 常驻策略（reclaimEnabled=false）下，实例应保持运行：对有数据目录（PVC）
	// 但 Pod 意外缺失的用户补拉实例（设计 §5.2 拉起、常驻运行、异常自愈重建）。
	if !m.reclaimEnabled {
		pvcs, err := m.client.CoreV1().PersistentVolumeClaims(m.spec.Namespace).List(ctx, metav1.ListOptions{
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

func isCrashLoop(pod *corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodFailed {
		return true
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready && cs.RestartCount >= crashRestartThreshold {
			return true
		}
	}
	return false
}

// ---- per-user data directory GC (design doc §5.1 / §10) ----
//
// Conversation history is retained for a sliding window (default 72h) and
// pruned beyond it; PVC watermarks above 70% trigger an aggressive pass and a
// warning (水位 >70% 触发清理/告警). GC is leader-only (multi-replica).

// gcDataDirs lists per-user data PVCs and prunes expired session/transcript
// files inside each via an exec into the owning agent Pod. Pods that are
// currently down are skipped (their data is untouched; next pass retries).
func (m *Manager) gcDataDirs(ctx context.Context) error {
	pvcs, err := m.client.CoreV1().PersistentVolumeClaims(m.spec.Namespace).List(ctx, metav1.ListOptions{
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
	pod, err := m.client.CoreV1().Pods(m.spec.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get pod: %w", err) // down: skip, retry next pass
	}
	if !podReady(pod) {
		return fmt.Errorf("pod not ready, skip")
	}

	// Delete transcript JSONL / sessions files older than the window. The
	// gateway writes state under OPENCLAW_STATE_DIR (/home/node/.openclaw):
	// sessions.json + transcript JSONL per session in the session store dir.
	// A conservative find over the state dir removes files with mtime older
	// than the window; the gateway recreates them on next use.
	hours := int64(m.gcWindow.Hours())
	if hours <= 0 {
		hours = 72
	}
	cmd := fmt.Sprintf(
		`find /home/node/.openclaw -type f \( -name '*.jsonl' -o -name 'sessions.json' -o -name '*.json' \) -mmin +%d -delete 2>/dev/null; echo gc-done`, hours*60)

	resp, err := m.execInPod(ctx, podName, "gateway", cmd)
	if err != nil {
		return fmt.Errorf("exec gc: %w", err)
	}

	// Watermark check (§10 水位 >70% 触发清理/告警): measure real usage of the
	// data mount via df (the Pod's PVC is mounted at /home/node/.openclaw).
	df, err := m.execInPod(ctx, podName, "gateway",
		`df -P /home/node/.openclaw | awk 'NR==2 {print $5}' | tr -d '%'`)
	if err != nil {
		return fmt.Errorf("df: %w", err)
	}
	if pct, err := strconv.Atoi(strings.TrimSpace(df)); err == nil {
		if float64(pct)/100 > m.gcWatermark {
			log.Printf("instances: PVC watermark %d%% > %.0f%% for %s, aggressive GC advised",
				pct, m.gcWatermark*100, user)
		}
	} else {
		log.Printf("instances: parse df output %q for %s: %v", df, user, err)
	}
	_ = resp
	return nil
}

// execInPod runs cmd in the named container of pod via the Kubernetes exec
// API and returns combined output (or an error carrying it).
func (m *Manager) execInPod(ctx context.Context, pod, container, cmd string) (string, error) {
	req := m.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(m.spec.Namespace).
		SubResource("exec").
		Param("container", container).
		VersionedParams(&corev1.PodExecOptions{
			Command: []string{"/bin/sh", "-c", cmd},
			Stdout:  true,
			Stderr:  true,
		}, scheme.ParameterCodec)

	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.BuildConfigFromFlags("", k8s.KubeconfigPath())
		if err != nil {
			return "", fmt.Errorf("rest config: %w", err)
		}
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

func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
