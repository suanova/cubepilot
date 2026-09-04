package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/suanova/cubepilot/internal/openclaw"
)

var namespaceGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// HITL (issue #20): with the device master key configured, a ConfirmWrites
// chat turn that reaches a write operation must pause with a confirm_pending
// and a rejected write must not execute.
var _ = Describe("HITL gates a chat write until rejected", Label("chat"), func() {
	It("emits confirm_pending and does not run the rejected write", func() {
		if os.Getenv("CUBEPILOT_E2E_CHAT") != "1" {
			Skip("CUBEPILOT_E2E_CHAT != 1 (needs a real LLM key); skipping HITL chat e2e")
		}
		// HITL is on by default in the e2e deployment (chart api.hitl.enabled:
		// the API auto-generates the device master key and supervisors auto-pair),
		// so this spec exercises the real write gate. If a deployment disables
		// hitl, this spec would fail -- gate it back on with a deployment check
		// if such a profile ever needs to run the chat specs.
		By("waiting until the agent instance is Ready and its pod is stable")
		Eventually(func() error { return agentStabilityErr(context.Background(), fw.Users[0]) },
			4*time.Minute, 5*time.Second).Should(Succeed())

		nsName := "hitl-e2e-" + rand.String(6)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()

		By("asking the agent to run a write that must be rejected")
		prompt := fmt.Sprintf(
			"Execute exactly this command and show its raw output: kubectl create namespace %s （该命名空间当前不存在）", nsName)
		events, err := fw.ChatSSEWithDecision(ctx, fw.Users[0], "e2e-"+rand.String(6), prompt, "reject")
		Expect(err).NotTo(HaveOccurred())

		By("asserting the write paused for a decision and was rejected")
		sawPending, sawResolvedRejected, sawDone := false, false, false
		for _, ev := range events {
			switch ev.Event {
			case openclaw.EventConfirmPending:
				sawPending = true
			case openclaw.EventConfirmResolved:
				var r struct {
					Approved *bool `json:"approved"`
				}
				Expect(json.Unmarshal(ev.Data, &r)).To(Succeed())
				if r.Approved != nil && !*r.Approved {
					sawResolvedRejected = true
				}
			case openclaw.EventMessageDone:
				sawDone = true
			}
		}
		Expect(sawPending).To(BeTrue(), "a write must produce confirm_pending under HITL")
		Expect(sawResolvedRejected).To(BeTrue(), "confirm_resolved should carry approved:false for a rejected write")
		Expect(sawDone).To(BeTrue(), "message_done should terminate the turn after the rejection")

		By("asserting the rejected namespace was never created")
		Consistently(func() bool {
			_, err := fw.DynamicClient.Resource(namespaceGVR).Get(ctx, nsName, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, 10*time.Second, 1*time.Second).Should(BeTrue(),
			"the rejected kubectl create namespace must not have executed")
	})
})
