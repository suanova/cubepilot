package e2e

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/controller"
)

var _ = Describe("Builtin bootstrap", func() {
	ctx := context.Background()

	It("installs the six ai.cubestack.io CRDs", func() {
		for _, name := range []string{"agenttemplates", "agentinstances", "skills", "tasktemplates", "tasks", "taskruns"} {
			_, err := fw.ApiExtClient.ApiextensionsV1().CustomResourceDefinitions().
				Get(ctx, name+".ai.cubestack.io", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "CRD %s.ai.cubestack.io should exist", name)
		}
	})

	It("creates the shared secrets", func() {
		sec, err := fw.KubeClient.CoreV1().Secrets(fw.Namespace).Get(ctx, "cubepilot-llm", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(sec.Data["apiKey"])).NotTo(BeEmpty())
		_, err = fw.KubeClient.CoreV1().Secrets(fw.Namespace).Get(ctx, "agent-kubeconfig", metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
	})

	It("bootstraps the builtin agent-for-cloud template", func() {
		tpl := &v1alpha1.AgentTemplate{}
		Eventually(func() error {
			return fw.CtrlClient.Get(ctx, types.NamespacedName{Name: controller.BuiltinAgentName}, tpl)
		}).Should(Succeed())
		Expect(tpl.Labels).To(HaveKeyWithValue("cubepilot/builtin", "true"))
		Expect(tpl.Spec.DefaultModel).NotTo(BeEmpty())
		Expect(tpl.Spec.Models).NotTo(BeEmpty())
	})

	It("bootstraps builtin skills and the daily-inspection task template", func() {
		var list v1alpha1.SkillList
		Eventually(func() error {
			if err := fw.CtrlClient.List(ctx, &list); err != nil {
				return err
			}
			if len(list.Items) == 0 {
				return fmt.Errorf("no builtin skills yet")
			}
			for _, s := range list.Items {
				if s.Labels["cubepilot/builtin"] != "true" {
					return fmt.Errorf("skill %s missing builtin label", s.Name)
				}
			}
			return nil
		}).Should(Succeed())

		tt := &v1alpha1.TaskTemplate{}
		Eventually(func() error {
			return fw.CtrlClient.Get(ctx, types.NamespacedName{Name: controller.BuiltinTaskTemplateName}, tt)
		}).Should(Succeed())
		Expect(tt.Labels).To(HaveKeyWithValue("cubepilot/builtin", "true"))
	})

	It("instantiates one agent per configured user", func() {
		for _, user := range fw.Users {
			name := controller.InstanceNameFor(user, controller.BuiltinAgentName)
			inst := &v1alpha1.AgentInstance{}
			Eventually(func() error {
				return fw.CtrlClient.Get(ctx, types.NamespacedName{Name: name}, inst)
			}).Should(Succeed(), "builtin instance %s for user %s should exist", name, user)
			Expect(inst.Spec.TemplateRef).To(Equal(controller.BuiltinAgentName))
			Expect(inst.Spec.Identity.PrincipalRef.UserRef).To(Equal(user))
			Expect(inst.Labels).To(HaveKeyWithValue("cubepilot/builtin", "true"))
		}
	})
})
