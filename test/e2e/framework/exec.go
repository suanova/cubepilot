package framework

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// Exec runs cmd in container of pod and returns its stdout, with stderr folded
// into the error. It exists so a test can observe and perturb what the supervisor
// installs inside a running agent pod: the pod-side ownership rules can only be
// verified against the real supervisor/gateway pair, not a fake.
func (f *Framework) Exec(ctx context.Context, pod, container string, cmd ...string) (string, error) {
	req := f.KubeClient.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(f.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(f.RestConfig, http.MethodPost, req.URL())
	if err != nil {
		return "", fmt.Errorf("exec setup: %w", err)
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return stdout.String(), fmt.Errorf("exec %s: %w (stderr: %s)",
			strings.Join(cmd, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
