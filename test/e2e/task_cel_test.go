package e2e

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

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
//
// Each Task below also carries an instruction: the Task CEL rules reject a task
// with neither a templateRef nor a non-blank instruction, so a spec that
// tripped that rule too would pass these specs for the wrong reason.
var _ = Describe("Task CRD enum validation", func() {
	ctx := context.Background()

	It("rejects an out-of-enum spec.state", func() {
		bad := &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-task-state", Namespace: fw.Namespace},
			Spec: v1alpha1.TaskSpec{
				Owner:       "zhang.wei",
				Instruction: "list the pods",
				State:       "Anything",
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
				Owner:       "zhang.wei",
				Instruction: "list the pods",
				State:       v1alpha1.TaskStateEnabled,
			},
		}
		Expect(fw.CtrlClient.Create(ctx, good)).To(Succeed(), "valid enum values must be accepted")
		Expect(fw.CtrlClient.Delete(ctx, good)).To(Succeed())
	})
})

// Task CEL validation: the deployed tasks CRD carries two x-kubernetes-
// validations on spec, mirroring what the API handler enforces. A Task needs a
// templateRef or a non-blank instruction, and params need a templateRef.
//
// The rules test values, not presence -- has() is true for an explicitly empty
// string -- so the empty-string cases go through the dynamic client. The typed
// TaskSpec is omitempty, which would drop the key and quietly reduce
// instruction: "" to "no instruction key at all", leaving the value test
// unexercised.
var taskGVR = schema.GroupVersionResource{
	Group:    "ai.cubestack.io",
	Version:  "v1alpha1",
	Resource: "tasks",
}

// rawTask builds a Task from an explicit spec map, keeping empty-string keys on
// the wire; owner is filled in because spec.owner is required.
func rawTask(name string, spec map[string]any) *unstructured.Unstructured {
	spec["owner"] = "zhang.wei"
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ai.cubestack.io/v1alpha1",
		"kind":       "Task",
		"metadata":   map[string]any{"name": name, "namespace": fw.Namespace},
		"spec":       spec,
	}}
}

// taskTemplateForCEL creates the TaskTemplate the templateRef cases point at,
// so they do not assert that a dangling reference is admissible.
func taskTemplateForCEL(ctx context.Context) string {
	const name = "e2e-task-cel-template"
	tpl := &v1alpha1.TaskTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: fw.Namespace},
		Spec:       v1alpha1.TaskTemplateSpec{DisplayName: "E2E task CEL", Instruction: "list the pods"},
	}
	Expect(fw.CtrlClient.Create(ctx, tpl)).To(Succeed())
	DeferCleanup(func() { _ = fw.CtrlClient.Delete(context.Background(), tpl) })
	return name
}

var _ = Describe("Task CEL validation", func() {
	ctx := context.Background()

	It("rejects a task with neither a templateRef nor an instruction", func() {
		_, err := fw.DynamicClient.Resource(taskGVR).Namespace(fw.Namespace).
			Create(ctx, rawTask("bad-task-neither", map[string]any{}), metav1.CreateOptions{})
		Expect(err).To(HaveOccurred(), "a task with no templateRef and no instruction must be rejected")
	})

	It("rejects an explicitly empty instruction", func() {
		_, err := fw.DynamicClient.Resource(taskGVR).Namespace(fw.Namespace).
			Create(ctx, rawTask("bad-task-empty-instruction", map[string]any{"instruction": ""}), metav1.CreateOptions{})
		Expect(err).To(HaveOccurred(), `instruction: "" is a present key, so the value test must reject it`)
	})

	It("rejects a whitespace-only instruction", func() {
		_, err := fw.DynamicClient.Resource(taskGVR).Namespace(fw.Namespace).
			Create(ctx, rawTask("bad-task-blank-instruction", map[string]any{"instruction": "   "}), metav1.CreateOptions{})
		Expect(err).To(HaveOccurred(), "a whitespace-only instruction is blank after trimming, so it must be rejected")
	})

	It("rejects an instruction of only newlines and tabs", func() {
		_, err := fw.DynamicClient.Resource(taskGVR).Namespace(fw.Namespace).
			Create(ctx, rawTask("bad-task-newline-instruction", map[string]any{"instruction": "\n\t"}), metav1.CreateOptions{})
		Expect(err).To(HaveOccurred(), "\\s covers newlines and tabs, so this instruction is blank")
	})

	It("rejects an explicitly empty templateRef", func() {
		_, err := fw.DynamicClient.Resource(taskGVR).Namespace(fw.Namespace).
			Create(ctx, rawTask("bad-task-empty-templateref", map[string]any{"templateRef": ""}), metav1.CreateOptions{})
		Expect(err).To(HaveOccurred(), `templateRef: "" names no template, so it must be rejected`)
	})

	It("rejects params with no templateRef", func() {
		_, err := fw.DynamicClient.Resource(taskGVR).Namespace(fw.Namespace).
			Create(ctx, rawTask("bad-task-params", map[string]any{
				"instruction": "list the pods",
				"params":      map[string]any{"service": "checkout"},
			}), metav1.CreateOptions{})
		Expect(err).To(HaveOccurred(), "params only mean something with a template, so they must be rejected")
	})

	It("accepts a real instruction", func() {
		good := &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "good-task-instruction", Namespace: fw.Namespace},
			Spec:       v1alpha1.TaskSpec{Owner: "zhang.wei", Instruction: "list the pods"},
		}
		Expect(fw.CtrlClient.Create(ctx, good)).To(Succeed(), "a free-form task must be accepted")
		Expect(fw.CtrlClient.Delete(ctx, good)).To(Succeed())
	})

	It("accepts a real templateRef", func() {
		name := taskTemplateForCEL(ctx)
		good := &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "good-task-templateref", Namespace: fw.Namespace},
			Spec:       v1alpha1.TaskSpec{Owner: "zhang.wei", TemplateRef: name},
		}
		Expect(fw.CtrlClient.Create(ctx, good)).To(Succeed(), "a template-bound task must be accepted")
		Expect(fw.CtrlClient.Delete(ctx, good)).To(Succeed())
	})

	It("accepts both (the stored instruction is the rendered snapshot)", func() {
		name := taskTemplateForCEL(ctx)
		good := &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "good-task-both", Namespace: fw.Namespace},
			Spec: v1alpha1.TaskSpec{
				Owner:       "zhang.wei",
				TemplateRef: name,
				Instruction: "list the pods",
			},
		}
		Expect(fw.CtrlClient.Create(ctx, good)).To(Succeed(),
			"the rule is at least one, not exactly one: a template-bound task keeps its rendered snapshot")
		Expect(fw.CtrlClient.Delete(ctx, good)).To(Succeed())
	})

	It("accepts params with a real templateRef", func() {
		name := taskTemplateForCEL(ctx)
		good := &v1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "good-task-params", Namespace: fw.Namespace},
			Spec: v1alpha1.TaskSpec{
				Owner:       "zhang.wei",
				TemplateRef: name,
				Instruction: "list the pods",
				Params:      map[string]string{"service": "checkout"},
			},
		}
		Expect(fw.CtrlClient.Create(ctx, good)).To(Succeed(), "params with a templateRef must be accepted")
		Expect(fw.CtrlClient.Delete(ctx, good)).To(Succeed())
	})
})
