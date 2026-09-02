package e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// Task CRD enum validation: spec.state (Enabled | Paused) and spec.trigger
// (Manual | Cron) carry kubebuilder validation:Enum on the shipped CRD, so the
// API server rejects out-of-enum values (design §3.5: string enums, not bool
// flags). Mirrors the Skill CEL validation suite.
var _ = Describe("Task CRD enum validation", func() {
	ctx := context.Background()

	It("rejects an out-of-enum spec.state", func() {
		bad := &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-task-state"},
			Spec: v1alpha1.TaskSpec{
				Owner:   "zhang.wei",
				Trigger: v1alpha1.TaskTriggerCron,
				State:   "Anything",
			},
		}
		err := fw.CtrlClient.Create(ctx, bad)
		Expect(err).To(HaveOccurred(), "state=Anything must be rejected by the API server enum")
	})

	It("rejects an out-of-enum spec.trigger", func() {
		bad := &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-task-trigger"},
			Spec: v1alpha1.TaskSpec{
				Owner:   "zhang.wei",
				Trigger: "Whenever",
			},
		}
		err := fw.CtrlClient.Create(ctx, bad)
		Expect(err).To(HaveOccurred(), "trigger=Whenever must be rejected by the API server enum")
	})

	It("accepts valid enums (state Enabled, trigger Manual)", func() {
		good := &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "good-task-enums"},
			Spec: v1alpha1.TaskSpec{
				Owner:   "zhang.wei",
				Trigger: v1alpha1.TaskTriggerManual,
				State:   v1alpha1.TaskStateEnabled,
			},
		}
		Expect(fw.CtrlClient.Create(ctx, good)).To(Succeed(), "valid enum values must be accepted")
		Expect(fw.CtrlClient.Delete(ctx, good)).To(Succeed())
	})
})
