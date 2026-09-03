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
	// The CubeStack CRDs (ai.cubestack.io) are provisioned by scripts/setup.sh
	// when the environment is brought up (idempotent, kept after setup), so this
	// spec only cleans up the DevEnvironment resource the chat creates.

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
		if os.Getenv("CUBEPILOT_E2E_CHAT") != "1" {
			Skip("CUBEPILOT_E2E_CHAT != 1 (needs a real LLM key); skipping chat e2e")
		}
		ctx := context.Background()
		chatCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		defer cancel()

		// On a fresh provision the agent pod is recreated once ~60s in (per-user
		// kubeconfig fingerprint drift, issue #98); wait until it is Ready and
		// stable so the turn is not cut mid-stream by that swap.
		By("waiting until the agent instance is Ready and its pod is stable")
		Eventually(func() error { return agentStabilityErr(ctx, fw.Users[0]) },
			4*time.Minute, 5*time.Second).Should(Succeed())

		// Natural user request, exactly as a platform user would type it. No
		// mention of the CRD kind / kubectl / discovery — the agent is expected
		// to produce the DevEnvironment via the builtin cubestack-platform skill
		// (or, for kinds it does not cover, kubectl-platform's discovery recipe).
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

		// Informational only: whether any tool_call referenced the
		// DevEnvironment kind. Not a gate — with the per-CRD skills dropped,
		// the CR existing below is the proof of generic discovery, and
		// tool_call reconstruction from the transcript can be lossy across
		// providers (CI e2e observed a clean turn with no tool_call surfaced).
		var referenced bool
		for _, ev := range events {
			if ev.Event == openclaw.EventToolCall &&
				strings.Contains(strings.ToLower(string(ev.Data)), "devenvironment") {
				referenced = true
			}
		}
		if !referenced {
			GinkgoWriter.Printf("note: no tool_call referenced the devenvironment kind (transcript reconstruction may be lossy); the CR assertion below is the gate\n")
		}

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
