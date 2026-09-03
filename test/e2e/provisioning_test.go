package e2e

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/controller"
	"github.com/suanova/cubepilot/internal/k8s"
)

var _ = Describe("Instance provisioning", func() {
	ctx := context.Background()
	instName := "e2e-" + rand.String(8) + "-agent-for-cloud"
	// Expected child names come from the production builders so the assertion
	// can never drift from what the controller actually creates.
	podName := k8s.ResourceName("agent", instName)
	svcName := podName
	pvcName := k8s.ResourceName("data", instName)

	BeforeEach(func() {
		// The per-user kubeconfig Secret is a hard prerequisite for provisioning
		// (issue #100): the controller waits for the owner's identity before it
		// creates the pod. The builtin bootstrap mints identities only for
		// configured users, so seed one for this spec's ad-hoc owner (the content
		// is opaque to the controller -- only the Secret's existence matters).
		if _, err := fw.KubeClient.CoreV1().Secrets(fw.Namespace).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: k8s.UserKubeconfigSecretFor("e2e.user"), Namespace: fw.Namespace},
			Data:       map[string][]byte{"config": []byte("e2e user kubeconfig")},
		}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}

		inst := &v1alpha1.AgentInstance{
			ObjectMeta: metav1.ObjectMeta{Name: instName},
			Spec: v1alpha1.AgentInstanceSpec{
				TemplateRef: controller.BuiltinAgentName,
				Owner:       "e2e.user",
				Identity: v1alpha1.IdentitySpec{
					Mode: v1alpha1.IdentityModeUser,
					PrincipalRef: v1alpha1.PrincipalRef{
						UserRef: "e2e.user",
					},
				},
				Lifecycle: &v1alpha1.LifecycleSpec{Strategy: "resident"},
			},
		}
		err := fw.CtrlClient.Create(ctx, inst)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	})

	AfterEach(func() {
		inst := &v1alpha1.AgentInstance{}
		if err := fw.CtrlClient.Get(ctx, types.NamespacedName{Name: instName}, inst); err == nil {
			Expect(fw.CtrlClient.Delete(ctx, inst)).To(Succeed())
		} else if !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}
		if err := fw.KubeClient.CoreV1().Secrets(fw.Namespace).Delete(ctx,
			k8s.UserKubeconfigSecretFor("e2e.user"), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			Expect(err).NotTo(HaveOccurred())
		}
		// The ai.cubestack.io/agentinstance finalizer removes the PVC, Pod and
		// Service; wait until everything is gone.
		Eventually(func() error {
			if err := fw.CtrlClient.Get(ctx, types.NamespacedName{Name: instName}, &v1alpha1.AgentInstance{}); err == nil {
				return fmt.Errorf("agentinstance %s not gone yet", instName)
			} else if !apierrors.IsNotFound(err) {
				return err
			}
			core := fw.KubeClient.CoreV1()
			if _, err := core.Pods(fw.Namespace).Get(ctx, podName, metav1.GetOptions{}); err == nil {
				return fmt.Errorf("pod %s not gone yet", podName)
			} else if !apierrors.IsNotFound(err) {
				return err
			}
			if _, err := core.Services(fw.Namespace).Get(ctx, svcName, metav1.GetOptions{}); err == nil {
				return fmt.Errorf("service %s not gone yet", svcName)
			} else if !apierrors.IsNotFound(err) {
				return err
			}
			if _, err := core.PersistentVolumeClaims(fw.Namespace).Get(ctx, pvcName, metav1.GetOptions{}); err == nil {
				return fmt.Errorf("pvc %s not gone yet", pvcName)
			} else if !apierrors.IsNotFound(err) {
				return err
			}
			if _, err := core.Secrets(fw.Namespace).Get(ctx, k8s.UserKubeconfigSecretFor("e2e.user"), metav1.GetOptions{}); err == nil {
				return fmt.Errorf("per-user kubeconfig secret %s not gone yet", k8s.UserKubeconfigSecretFor("e2e.user"))
			} else if !apierrors.IsNotFound(err) {
				return err
			}
			return nil
		}).Should(Succeed())
	})

	It("provisions the per-instance PVC, Service and Pod", func() {
		// The agent Pod may never reach Ready with a placeholder LLM key (the
		// gateway stays unready), so accept Creating/Ready; a transient Failed is
		// healed by the controller, so Eventually retries across it.
		Eventually(func() error {
			inst := &v1alpha1.AgentInstance{}
			if err := fw.CtrlClient.Get(ctx, types.NamespacedName{Name: instName}, inst); err != nil {
				return err
			}
			if inst.Status.Phase != v1alpha1.InstanceCreating && inst.Status.Phase != v1alpha1.InstanceReady {
				return fmt.Errorf("phase not Creating/Ready: %q (%s)", inst.Status.Phase, inst.Status.Message)
			}
			if inst.Status.PodName != podName || inst.Status.ServiceName != svcName || inst.Status.PVCName != pvcName {
				return fmt.Errorf("status names not set: %+v", inst.Status)
			}
			return nil
		}).Should(Succeed())

		_, err := fw.KubeClient.CoreV1().Pods(fw.Namespace).Get(ctx, podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "pod %s should exist", podName)
		_, err = fw.KubeClient.CoreV1().Services(fw.Namespace).Get(ctx, svcName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "service %s should exist", svcName)
		_, err = fw.KubeClient.CoreV1().PersistentVolumeClaims(fw.Namespace).Get(ctx, pvcName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred(), "pvc %s should exist", pvcName)

		pod, err := fw.KubeClient.CoreV1().Pods(fw.Namespace).Get(ctx, podName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		var supervisor *corev1.Container
		for i := range pod.Spec.Containers {
			if pod.Spec.Containers[i].Name == "supervisor" {
				supervisor = &pod.Spec.Containers[i]
			}
		}
		Expect(supervisor).NotTo(BeNil(), "pod %s should carry the supervisor container", podName)
		Expect(supervisor.ReadinessProbe).NotTo(BeNil())
		Expect(supervisor.ReadinessProbe.ProbeHandler.HTTPGet.Path).To(Equal("/healthz"))
	})
})
