package framework

import (
	"context"
	"fmt"
	"io"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DumpDiagnostics writes pod state, events, and recent logs to w. The e2e suite
// calls it on failure so a red CI run explains itself without a rerun.
func DumpDiagnostics(ctx context.Context, f *Framework, w io.Writer) {
	pods, err := f.KubeClient.CoreV1().Pods(f.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintf(w, "diagnostics: list pods: %v\n", err)
		return
	}
	sort.Slice(pods.Items, func(i, j int) bool { return pods.Items[i].Name < pods.Items[j].Name })
	for _, pod := range pods.Items {
		fmt.Fprintf(w, "\n=== pod %s phase=%s node=%s\n", pod.Name, pod.Status.Phase, pod.Spec.NodeName)
		for _, cs := range pod.Status.ContainerStatuses {
			fmt.Fprintf(w, "  container %s ready=%v restarts=%d state=%s lastState=%s\n",
				cs.Name, cs.Ready, cs.RestartCount, stateOf(cs.State), stateOf(cs.LastTerminationState))
		}
		events, err := f.KubeClient.CoreV1().Events(f.Namespace).List(ctx, metav1.ListOptions{
			FieldSelector: "involvedObject.name=" + pod.Name,
		})
		if err == nil {
			for _, e := range events.Items {
				fmt.Fprintf(w, "  event %s %s: %s\n", e.Type, e.Reason, e.Message)
			}
		}
		for _, c := range pod.Spec.Containers {
			fmt.Fprintf(w, "  resources %s: requests=%v limits=%v\n", c.Name, c.Resources.Requests, c.Resources.Limits)
			for _, previous := range []bool{false, true} {
				if previous && restartsOf(pod, c.Name) == 0 {
					continue
				}
				label := "logs"
				if previous {
					label = "previous logs"
				}
				tail := int64(200)
				logs, err := f.KubeClient.CoreV1().Pods(f.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
					Container: c.Name,
					Previous:  previous,
					TailLines: &tail,
				}).DoRaw(ctx)
				if err != nil {
					fmt.Fprintf(w, "  --- %s %s: %v ---\n", label, c.Name, err)
					continue
				}
				fmt.Fprintf(w, "  --- %s %s ---\n%s\n", label, c.Name, logs)
			}
		}
	}
}

func restartsOf(pod corev1.Pod, container string) int32 {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == container {
			return cs.RestartCount
		}
	}
	return 0
}

func stateOf(s corev1.ContainerState) string {
	switch {
	case s.Running != nil:
		return "running"
	case s.Waiting != nil:
		return "waiting(" + s.Waiting.Reason + ")"
	case s.Terminated != nil:
		return fmt.Sprintf("terminated(%s, exit %d)", s.Terminated.Reason, s.Terminated.ExitCode)
	default:
		return "unknown"
	}
}
