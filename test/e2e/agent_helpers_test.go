package e2e

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/k8s"
)

// stableAgentPodAge is how long the agent pod must have been up before a chat
// turn may start. On a fresh provision the operator recreates the pod once,
// ~60s after creation, when the mounted per-user kubeconfig Secret's
// resourceVersion settles (a security-fingerprint drift, issue #98). A chat
// started inside that window is cut when the pod is replaced, so chat specs
// wait until the current pod has been up past the drift window.
const stableAgentPodAge = 90 * time.Second

// agentStabilityErr reports why the user's agent instance is not yet safe to
// chat with, or nil once the instance is Ready and its pod has been up for
// stableAgentPodAge (i.e. the initial provisioning recreate has already
// happened). Callers wrap it in an Eventually.
func agentStabilityErr(ctx context.Context, user string) error {
	name := k8s.InstanceName(user, v1alpha1.DefaultAgentName)
	var inst v1alpha1.AgentInstance
	if err := fw.CtrlClient.Get(ctx, types.NamespacedName{Name: name, Namespace: fw.Namespace}, &inst); err != nil {
		return fmt.Errorf("get instance %s: %w", name, err)
	}
	if inst.Status.Phase != v1alpha1.InstanceReady {
		return fmt.Errorf("instance %s not Ready (phase %s)", name, inst.Status.Phase)
	}
	if inst.Status.PodName == "" {
		return fmt.Errorf("instance %s has no pod yet", name)
	}
	// The framework's controller-runtime client only registers the cubepilot
	// v1alpha1 scheme, so read the Pod through the typed kubernetes client.
	pod, err := fw.KubeClient.CoreV1().Pods(fw.Namespace).Get(ctx, inst.Status.PodName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get agent pod %s: %w", inst.Status.PodName, err)
	}
	if !podReady(*pod) {
		return fmt.Errorf("agent pod %s not Ready", inst.Status.PodName)
	}
	if pod.Status.StartTime == nil {
		return fmt.Errorf("agent pod %s has no start time", inst.Status.PodName)
	}
	if age := time.Since(pod.Status.StartTime.Time); age < stableAgentPodAge {
		return fmt.Errorf("agent pod %s up %s, waiting for stability (%s)", inst.Status.PodName, age.Round(time.Second), stableAgentPodAge)
	}
	return nil
}

func podReady(pod corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
