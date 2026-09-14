package e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
)

// Task CRD enum validation: spec.state (Enabled | Paused) and TaskRun's
// spec.trigger (Manual | Cron) carry kubebuilder validation:Enum on the shipped
// CRD, so the API server rejects out-of-enum values (design §3.5: string enums,
// not bool flags). Mirrors the Skill CEL validation suite.
//
// The trigger enum lives on the TaskRun, not the Task: a Task's schedule is its
// cron expression (empty = manual), while a run's trigger records how it was
// started, which the Task cannot express.
var _ = Describe("Task CRD enum validation", func() {
	ctx := context.Background()

	It("rejects an out-of-enum spec.state", func() {
		bad := &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-task-state", Namespace: fw.Namespace},
			Spec: v1alpha1.TaskSpec{
				Owner: "zhang.wei",
				State: "Anything",
			},
		}
		err := fw.CtrlClient.Create(ctx, bad)
		Expect(err).To(HaveOccurred(), "state=Anything must be rejected by the API server enum")
	})

	It("rejects an out-of-enum spec.trigger on a TaskRun", func() {
		bad := &v1alpha1.TaskRun{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-run-trigger", Namespace: fw.Namespace},
			Spec: v1alpha1.TaskRunSpec{
				Owner:          "zhang.wei",
				CreatorTaskRef: v1alpha1.TaskRef{Name: "some-task"},
				Trigger:        "Whenever",
			},
		}
		err := fw.CtrlClient.Create(ctx, bad)
		Expect(err).To(HaveOccurred(), "trigger=Whenever must be rejected by the API server enum")
	})

	It("accepts valid enums (state Enabled)", func() {
		good := &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "good-task-enums", Namespace: fw.Namespace},
			Spec: v1alpha1.TaskSpec{
				Owner: "zhang.wei",
				State: v1alpha1.TaskStateEnabled,
			},
		}
		Expect(fw.CtrlClient.Create(ctx, good)).To(Succeed(), "valid enum values must be accepted")
		Expect(fw.CtrlClient.Delete(ctx, good)).To(Succeed())
	})
})
