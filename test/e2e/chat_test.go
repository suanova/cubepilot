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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		events, err := fw.ChatSSE(ctx, fw.Users[0], "e2e-"+rand.String(6),
			"你是 CubePilot 平台助手。请用一句话回复:你好。")
		Expect(err).NotTo(HaveOccurred())

		var types []string
		for _, ev := range events {
			types = append(types, ev.Event)
		}
		Expect(types).To(ContainElement(openclaw.EventMessageDelta))
		Expect(types).To(ContainElement(openclaw.EventMessageDone))

		var done map[string]any
		for _, ev := range events {
			if ev.Event == openclaw.EventMessageDone {
				Expect(json.Unmarshal(ev.Data, &done)).To(Succeed())
			}
		}
		Expect(done).NotTo(BeNil(), "message_done event should carry a data payload")
		Expect(done["error"]).To(BeEmpty())
	})
})
