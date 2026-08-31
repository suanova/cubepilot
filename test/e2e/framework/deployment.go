package framework

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CheckDeploymentReady returns nil when the deployment's ReadyReplicas equal
// its desired Replicas (the same signal `kubectl rollout status` waits on).
func (f *Framework) CheckDeploymentReady(ctx context.Context, name string) error {
	dep, err := f.KubeClient.AppsV1().Deployments(f.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get deployment %s: %w", name, err)
	}
	if dep.Status.ReadyReplicas != dep.Status.Replicas {
		return fmt.Errorf("deployment %s not ready: %d/%d replicas",
			name, dep.Status.ReadyReplicas, dep.Status.Replicas)
	}
	return nil
}
