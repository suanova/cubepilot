// Package instances implements the Instance Manager: per-user OpenClaw agent
// Pods, launched on demand, reclaimed when idle, and healed when unhealthy
// (design doc FR-M2-002, FR-M2-004).
package instances

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/k8s"
)

const (
	readyTimeout          = 60 * time.Second
	reconcileEvery        = 30 * time.Second
	crashRestartThreshold = 3
)

// Manager owns the lifecycle of per-user agent instances.
type Manager struct {
	client *kubernetes.Clientset
	spec   k8s.AgentSpec

	mu     sync.Mutex
	active map[string]time.Time // user -> last activity
	ttl    time.Duration
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
		active: map[string]time.Time{},
		ttl:    cfg.IdleTTL,
	}
}

// BaseURL returns the in-cluster gateway URL for a user's agent instance.
func (m *Manager) BaseURL(user string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", k8s.ResourceName("agent", user), m.spec.Namespace, m.spec.Port)
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
	addr := fmt.Sprintf("%s.%s.svc:%d", k8s.ResourceName("agent", user), m.spec.Namespace, m.spec.Port)
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
	podName := k8s.ResourceName("agent", user)
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
		return false, "回收中（按需拉起）", time.Time{}
	}
	if pod.Status.StartTime != nil {
		startedAt = pod.Status.StartTime.Time
	}
	return true, string(pod.Status.Phase), startedAt
}

// Run starts the background reconciliation loop (idle reclaim + crash heal).
func (m *Manager) Run(ctx context.Context) {
	go func() {
		t := time.NewTicker(reconcileEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := m.reconcile(ctx); err != nil {
					log.Printf("instances: reconcile: %v", err)
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

	for _, pod := range pods.Items {
		user := pod.Labels[k8s.AgentLabelUser]
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
			if err := m.client.CoreV1().Pods(m.spec.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			continue
		}
		if isCrashLoop(&pod) {
			log.Printf("instances: healing crashed instance for %s", user)
			if err := m.client.CoreV1().Pods(m.spec.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			_ = m.createResources(ctx, user)
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

func podReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
