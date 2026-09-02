package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/suanova/cubepilot/internal/openclaw"
)

const (
	devEnvName      = "dev-cuda-e2e"
	devEnvNamespace = "default"
	devEnvImage     = "pytorch/pytorch:2.3.1-cuda12.1-cudnn8-runtime"
	devEnvCPU       = "4"
	devEnvMemory    = "16Gi"
)

// devEnvGVR is the DevEnvironment kind under the cubestack ai.cubestack.io
// group (not part of the platform's v1alpha1 scheme, so asserted via the
// dynamic client).
var devEnvGVR = schema.GroupVersionResource{
	Group: "ai.cubestack.io", Version: "v1alpha1", Resource: "devenvironments",
}

var _ = Describe("Chat creates a DevEnvironment via generic CRD discovery", Label("chat"), func() {
	ctx := context.Background()

	BeforeEach(func() {
		if os.Getenv("CUBEPILOT_E2E_CHAT") != "1" {
			Skip("CUBEPILOT_E2E_CHAT != 1 (needs a real LLM key); skipping chat e2e")
		}
		// The cubestack CRDs are installed at bring-up by scripts/setup.sh
		// (vendored under deploy/cubestack/crds), not by this spec. Guard that
		// they're present so a mis-configured cluster fails loudly here.
		By("checking the cubestack CRDs are installed")
		Eventually(func() error {
			_, err := fw.ApiExtClient.ApiextensionsV1().CustomResourceDefinitions().
				Get(ctx, "devenvironments.ai.cubestack.io", metav1.GetOptions{})
			return err
		}).Should(Succeed(), "devenvironments.ai.cubestack.io should be installed (scripts/setup.sh)")
	})

	AfterEach(func() {
		deleteCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		By("deleting the DevEnvironment created by the chat")
		err := fw.DynamicClient.Resource(devEnvGVR).Namespace(devEnvNamespace).
			Delete(deleteCtx, devEnvName, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			Fail(fmt.Sprintf("delete DevEnvironment: %v", err))
		}
		Eventually(func() bool {
			_, err := fw.DynamicClient.Resource(devEnvGVR).Namespace(devEnvNamespace).
				Get(deleteCtx, devEnvName, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}).Should(BeTrue(), "DevEnvironment should be gone after delete")
	})

	It("creates a DevEnvironment from natural language via generic discovery", func() {
		chatCtx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()

		// Natural user request, exactly as a platform user would type it. No
		// mention of the CRD kind / kubectl / discovery — the agent must work
		// those out from the kubectl-platform skill's schema-discovery recipe.
		prompt := fmt.Sprintf(
			"帮我创建一个开发机：名字叫 %s，放在 %s 命名空间，%s 核 CPU、%s 内存，镜像用 %s。",
			devEnvName, devEnvNamespace, devEnvCPU, devEnvMemory, devEnvImage)

		events, err := fw.ChatSSE(chatCtx, fw.Users[0], "e2e-"+rand.String(6), prompt)
		Expect(err).NotTo(HaveOccurred())

		// The turn must finish cleanly (message_done with no error).
		var done map[string]any
		foundDone := false
		for _, ev := range events {
			if ev.Event == openclaw.EventMessageDone {
				foundDone = true
				Expect(json.Unmarshal(ev.Data, &done)).To(Succeed())
			}
		}
		Expect(foundDone).To(BeTrue(), "message_done should terminate the turn")
		Expect(done["error"]).To(SatisfyAny(BeNil(), BeEmpty()), "message_done should carry no error")

		// Evidence the agent referenced the *discovered* kind (generic
		// discovery, not a per-CRD skill).
		var referenced bool
		for _, ev := range events {
			if ev.Event == openclaw.EventToolCall &&
				strings.Contains(strings.ToLower(string(ev.Data)), "devenvironment") {
				referenced = true
			}
		}
		Expect(referenced).To(BeTrue(), "the agent should have referenced the DevEnvironment kind (discovery evidence)")

		// The CR must exist with the requested spec.
		Eventually(func() error {
			_, err := fw.DynamicClient.Resource(devEnvGVR).Namespace(devEnvNamespace).
				Get(ctx, devEnvName, metav1.GetOptions{})
			return err
		}).Should(Succeed(), "DevEnvironment %s/%s should exist", devEnvNamespace, devEnvName)

		obj, err := fw.DynamicClient.Resource(devEnvGVR).Namespace(devEnvNamespace).
			Get(ctx, devEnvName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		spec, ok := obj.Object["spec"].(map[string]any)
		Expect(ok).To(BeTrue(), "DevEnvironment should carry a spec")
		Expect(spec["image"]).To(Equal(devEnvImage))
		res, ok := spec["resources"].(map[string]any)
		Expect(ok).To(BeTrue(), "DevEnvironment spec.resources should be an object")
		Expect(res["cpu"]).To(Equal(devEnvCPU))
		Expect(res["memory"]).To(Equal(devEnvMemory))
	})
})
