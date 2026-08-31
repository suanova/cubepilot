package framework

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CheckDeploymentReady reports a deployment as ready only once its rollout is
// complete, using the same predicate the Deployment controller applies
// (pkg/controller/deployment/util.DeploymentComplete): the controller has
// observed the current generation and updated, total and available replicas all
// match the desired count. Requiring total Replicas excludes the maxSurge
// window, and AvailableReplicas adds the minReadySeconds availability wait --
// comparing against Status.Replicas alone would report success while all
// counts are still 0, before any Pod exists.
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
	if dep.Status.UpdatedReplicas != desired || dep.Status.Replicas != desired || dep.Status.AvailableReplicas != desired {
		return fmt.Errorf("deployment %s not ready: updated=%d replicas=%d available=%d desired=%d",
			name, dep.Status.UpdatedReplicas, dep.Status.Replicas, dep.Status.AvailableReplicas, desired)
	}
	return nil
}
