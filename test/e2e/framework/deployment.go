package framework

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CheckDeploymentReady reports a deployment as ready only once the controller
// has observed the current generation and the ready/updated replica counts
// match the desired replicas (the same signal `kubectl rollout status` waits
// on). Comparing against Status.Replicas would report success while both the
// ready and observed counts are still 0, before any Pod exists.
func (f *Framework) CheckDeploymentReady(ctx context.Context, name string) error {
	dep, err := f.KubeClient.AppsV1().Deployments(f.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get deployment %s: %w", name, err)
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	if dep.Status.ObservedGeneration < dep.Generation {
		return fmt.Errorf("deployment %s: not yet observed (generation %d < %d)",
			name, dep.Status.ObservedGeneration, dep.Generation)
	}
	if dep.Status.ReadyReplicas != desired || dep.Status.UpdatedReplicas != desired {
		return fmt.Errorf("deployment %s not ready: ready=%d updated=%d desired=%d",
			name, dep.Status.ReadyReplicas, dep.Status.UpdatedReplicas, desired)
	}
	return nil
}
