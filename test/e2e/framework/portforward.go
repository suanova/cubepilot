package framework

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// PortForward tunnels one port of a namespaced Service to a random local port
// and returns the local base URL plus an idempotent cleanup function. Like
// kubectl, it resolves the Service to a backing Pod and forwards to the
// service's targetPort (the apiserver has no services/portforward subresource).
// Uses client-go's SPDY port-forward, so the test binary needs no kubectl on
// PATH and never guesses a free port ("0").
func (f *Framework) PortForward(ctx context.Context, svcName string, servicePort int) (string, func(), error) {
	podName, podPort, err := f.podAndPortForService(ctx, svcName, servicePort)
	if err != nil {
		return "", nil, err
	}
	req := f.KubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(f.Namespace).
		Name(podName).
		SubResource("portforward").URL()

	rt, upgrader, err := spdy.RoundTripperFor(f.RestConfig)
	if err != nil {
		return "", nil, fmt.Errorf("spdy roundtripper: %w", err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: rt}, http.MethodPost, req)

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	pf, err := portforward.New(dialer, []string{fmt.Sprintf("0:%d", podPort)}, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return "", nil, fmt.Errorf("port-forward %s: %w", svcName, err)
	}

	runErr := make(chan error, 1)
	go func() { runErr <- pf.ForwardPorts() }()

	select {
	case <-readyCh:
	case err := <-runErr:
		close(stopCh)
		return "", nil, fmt.Errorf("port-forward %s failed: %w", svcName, err)
	case <-time.After(30 * time.Second):
		// The forwarder may still be running -- stop it so it doesn't leak a
		// listener after we return.
		close(stopCh)
		return "", nil, fmt.Errorf("port-forward %s timed out", svcName)
	}
	ports, err := pf.GetPorts()
	if err != nil {
		close(stopCh)
		return "", nil, fmt.Errorf("read port-forward port: %w", err)
	}
	if len(ports) != 1 {
		return "", nil, fmt.Errorf("unexpected forwarded ports: %v", ports)
	}

	var once sync.Once
	cleanup := func() { once.Do(func() { close(stopCh) }) }
	return fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local), cleanup, nil
}

// podAndPortForService returns the name of a running Pod backing the Service
// and the pod port to forward to (the Service's targetPort for the given
// service port, resolving named ports against the pod's container ports).
func (f *Framework) podAndPortForService(ctx context.Context, svcName string, servicePort int) (string, int, error) {
	svc, err := f.KubeClient.CoreV1().Services(f.Namespace).Get(ctx, svcName, metav1.GetOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("get service %s: %w", svcName, err)
	}
	podPort := servicePort
	for _, p := range svc.Spec.Ports {
		if int(p.Port) == servicePort {
			if p.TargetPort.Type == intstr.Int {
				podPort = p.TargetPort.IntValue()
			}
			break
		}
	}

	selector := labels.Set(svc.Spec.Selector).AsSelector()
	pods, err := f.KubeClient.CoreV1().Pods(f.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return "", 0, fmt.Errorf("list pods for service %s: %w", svcName, err)
	}
	var pod *corev1.Pod
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			pod = &pods.Items[i]
			break
		}
	}
	if pod == nil && len(pods.Items) > 0 {
		pod = &pods.Items[0]
	}
	if pod == nil {
		return "", 0, fmt.Errorf("no pods backing service %s", svcName)
	}

	// Resolve a named targetPort (e.g. "http") against the pod's container ports.
	if podPort == servicePort {
		for _, p := range svc.Spec.Ports {
			if int(p.Port) == servicePort && p.TargetPort.Type == intstr.String {
				if resolved := findContainerPort(pod, p.TargetPort.StrVal); resolved != 0 {
					podPort = resolved
				}
			}
		}
	}
	return pod.Name, podPort, nil
}

func findContainerPort(pod *corev1.Pod, name string) int {
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.Name == name {
				return int(p.ContainerPort)
			}
		}
	}
	return 0
}
