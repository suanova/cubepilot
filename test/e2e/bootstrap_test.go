package e2e

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/controller"
	"github.com/suanova/cubepilot/internal/k8s"
	"github.com/suanova/cubepilot/internal/skill"
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

	It("bootstraps the builtin cubepilot template", func() {
		tpl := &v1alpha1.AgentTemplate{}
		Eventually(func() error {
			return fw.CtrlClient.Get(ctx, types.NamespacedName{Namespace: fw.Namespace, Name: controller.BuiltinAgentName}, tpl)
		}).Should(Succeed())
		Expect(tpl.Labels).To(HaveKeyWithValue("cubepilot/builtin", "true"))
		Expect(tpl.Spec.DefaultModel).NotTo(BeEmpty())
		Expect(tpl.Spec.Providers).NotTo(BeEmpty())
	})

	It("bootstraps builtin skills and the daily-inspection task template", func() {
		// The seeded set must match the embedded builtin set exactly (issue
		// #86: only cluster-inspection + kubectl-platform after the drop).
		want := map[string]bool{}
		for _, n := range skill.BuiltinSkillNames() {
			want[n] = true
		}

		var list v1alpha1.SkillList
		Eventually(func() error {
			if err := fw.CtrlClient.List(ctx, &list, client.InNamespace(fw.Namespace)); err != nil {
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
			got := map[string]bool{}
			for _, s := range list.Items {
				got[s.Name] = true
			}
			if !maps.Equal(got, want) {
				return fmt.Errorf("seeded skills %v, want %v", got, want)
			}
			return nil
		}).Should(Succeed())

		tt := &v1alpha1.TaskTemplate{}
		Eventually(func() error {
			return fw.CtrlClient.Get(ctx, types.NamespacedName{Namespace: fw.Namespace, Name: controller.BuiltinTaskTemplateName}, tt)
		}).Should(Succeed())
		Expect(tt.Labels).To(HaveKeyWithValue("cubepilot/builtin", "true"))
	})

	It("gives the assistant identity cluster-scoped reads, but never secrets", func() {
		// The per-user ServiceAccount is what the assistant runs kubectl as, so
		// this is the RBAC the assistant actually ends up with. Both halves are
		// worth asserting, and the refusals more than the grants: a missing read
		// surfaces downstream as a Forbidden that someone will notice, while a
		// widened one -- secrets above all -- is silent.
		for _, user := range fw.Users {
			// The identity exists once the user has an instance; the platform
			// creates none of its own.
			Expect(ensureAgentInstance(ctx, user)).To(Succeed())
			Eventually(func() error {
				_, err := fw.KubeClient.CoreV1().Secrets(fw.Namespace).Get(ctx, k8s.UserKubeconfigSecretFor(user), metav1.GetOptions{})
				return err
			}, 2*time.Minute, 5*time.Second).Should(Succeed(), "the instance's owner should get their credentials")

			subject := "system:serviceaccount:" + fw.Namespace + ":" + k8s.UserServiceAccountName(user)

			for _, allow := range []struct{ resource, verb string }{
				{"nodes", "list"},
				{"nodes/status", "get"},
				{"persistentvolumes", "list"},
				{"pods/log", "get"},
				{"events", "list"},
				{"deployments.apps", "list"},
				{"customresourcedefinitions.apiextensions.k8s.io", "list"},
				{"agenttemplates.ai.cubestack.io", "list"},
			} {
				Expect(subjectMay(ctx, subject, allow.resource, allow.verb)).To(BeTrue(),
					"%s should be allowed to %s %s", subject, allow.verb, allow.resource)
			}

			for _, deny := range []struct{ resource, verb, why string }{
				{"secrets", "get", "reading a secret is what this identity is defined not to do"},
				{"nodes/proxy", "get", "the kubelet API is node root"},
				{"pods/proxy", "get", "proxying to a Pod's own ports bypasses the app's auth"},
				{"pods/exec", "create", "a shell in someone else's container"},
				{"serviceaccounts/token", "create", "minting an identity is privilege escalation"},
			} {
				Expect(subjectMay(ctx, subject, deny.resource, deny.verb)).To(BeFalse(),
					"%s must not be allowed to %s %s: %s", subject, deny.verb, deny.resource, deny.why)
			}
		}
	})

	It("mints credentials for the owner of an instance, and only for them", func() {
		// The platform creates no instance (the Portal asks for one) and mints
		// the identity behind it; "no instance, no credentials" is asserted in
		// the unit tests, where the absence is deterministic.
		user := fw.Users[0]
		Expect(ensureAgentInstance(ctx, user)).To(Succeed())
		name := k8s.InstanceName(user, controller.BuiltinAgentName)
		Eventually(func() error {
			var inst v1alpha1.AgentInstance
			if err := fw.CtrlClient.Get(ctx, types.NamespacedName{Namespace: fw.Namespace, Name: name}, &inst); err != nil {
				return err
			}
			if inst.Spec.Owner != user || inst.Spec.TemplateRef != controller.BuiltinAgentName {
				return fmt.Errorf("instance %s = owner %q template %q", name, inst.Spec.Owner, inst.Spec.TemplateRef)
			}
			// The framework's cached client has no core types registered; the
			// typed one is what the rest of the suite reads Secrets with.
			_, err := fw.KubeClient.CoreV1().Secrets(fw.Namespace).Get(ctx, k8s.UserKubeconfigSecretFor(user), metav1.GetOptions{})
			return err
		}).Should(Succeed(), "the instance's owner should get their credentials")
	})
})

// subjectMay asks the API server whether the named identity may perform the
// verb, through a SubjectAccessReview, so the answer comes from the same
// authorizer the assistant's own requests pass through. resource is written
// "pods", "pods/log", or "deployments.apps"; the group is the suffix after the
// first dot (empty for the core group). Namespace is deliberately left unset --
// the per-user grants are ClusterRoleBindings, so the question is whether the
// permission exists anywhere, not in one namespace.
func subjectMay(ctx context.Context, subject, resource, verb string) bool {
	attrs := &authorizationv1.ResourceAttributes{Verb: verb}
	rest := resource
	if i := strings.Index(rest, "/"); i >= 0 {
		attrs.Subresource = rest[i+1:]
		rest = rest[:i]
	}
	if i := strings.Index(rest, "."); i >= 0 {
		attrs.Group = rest[i+1:]
		rest = rest[:i]
	}
	attrs.Resource = rest

	resp, err := fw.KubeClient.AuthorizationV1().SubjectAccessReviews().
		Create(ctx, &authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{User: subject, ResourceAttributes: attrs},
		}, metav1.CreateOptions{})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return resp.Status.Allowed
}
