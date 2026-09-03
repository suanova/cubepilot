package e2e

import (
	"context"
	"encoding/json"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/suanova/cubepilot/internal/openclaw"
)

var _ = Describe("Chat (SSE)", Label("chat"), func() {
	It("streams message_delta then message_done with no error", func() {
		if os.Getenv("CUBEPILOT_E2E_CHAT") != "1" {
			Skip("CUBEPILOT_E2E_CHAT != 1 (needs a real LLM key); skipping chat e2e")
		}
		// On a fresh provision the agent pod is recreated once ~60s in (per-user
		// kubeconfig fingerprint drift, issue #98); wait until it is Ready and
		// stable so the turn is not cut mid-stream by that swap. The chat
		// deadline is created AFTER the wait so the gate does not eat the budget.
		By("waiting until the agent instance is Ready and its pod is stable")
		Eventually(func() error { return agentStabilityErr(context.Background(), fw.Users[0]) },
			4*time.Minute, 5*time.Second).Should(Succeed())

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		events, err := fw.ChatSSE(ctx, fw.Users[0], "e2e-"+rand.String(6),
			"你是 CubePilot 平台助手。请用一句话回复:你好。")
		Expect(err).NotTo(HaveOccurred())

		var types []string
		var deltaIdx, doneIdx = -1, -1
		for i, ev := range events {
			types = append(types, ev.Event)
			switch ev.Event {
			case openclaw.EventMessageDelta:
				if deltaIdx < 0 {
					deltaIdx = i
				}
			case openclaw.EventMessageDone:
				if doneIdx < 0 {
					doneIdx = i
				}
			}
		}
		Expect(types).To(ContainElement(openclaw.EventMessageDelta))
		Expect(types).To(ContainElement(openclaw.EventMessageDone))
		// A reply streams message_delta before the terminal message_done.
		Expect(deltaIdx).To(BeNumerically(">=", 0))
		Expect(doneIdx).To(BeNumerically(">=", 0))
		Expect(deltaIdx).To(BeNumerically("<", doneIdx))

		var done map[string]any
		for _, ev := range events {
			if ev.Event == openclaw.EventMessageDone {
				Expect(json.Unmarshal(ev.Data, &done)).To(Succeed())
			}
		}
		Expect(done).NotTo(BeNil(), "message_done event should carry a data payload")
		// A successful turn omits the error key entirely (Event.Marshal uses
		// json:",omitempty" on the empty error), so done["error"] is nil; a real
		// error would be a non-empty string. Accept either clean form.
		Expect(done["error"]).To(SatisfyAny(BeNil(), BeEmpty()), "message_done should carry no error")
	})
})
