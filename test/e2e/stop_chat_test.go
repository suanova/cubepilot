package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/suanova/cubepilot/internal/openclaw"
	"github.com/suanova/cubepilot/test/e2e/framework"
)

// Stop (issue #166). POST /api/sessions/{key}/abort ends the session's live
// turn; because the endpoint answers only once the session has settled, the
// very next send for that session is not refused as a concurrent turn (409) and
// can start a turn of its own. That "stop, then immediately send" ordering is
// what these specs exercise -- the assertion that matters is on the follow-up
// send, not on the abort's own response.
//
// ChatSSE blocks until the turn finishes, so a turn that must still be running
// when Stop lands is started in a goroutine and the spec goroutine drives the
// abort. Either turn may finish on its own before the abort arrives (a fast
// model, a slow cold start); that is a legitimate outcome, so the specs assert
// the observable end state -- the session reports no active turn and the
// follow-up send is accepted -- rather than racing the abort against the turn.
// Where the outcome would have been a race, the specs only record which way it
// went (GinkgoWriter) and never assert on it.

// stopLongPrompt asks for a reply long enough that the turn is still streaming
// when the spec posts Stop. It is deliberately a pure writing task (no tool
// use) so the abort lands on a token stream, not on a paused write gate.
const stopLongPrompt = "请写一篇约 800 字的说明文，介绍 Kubernetes 中 Pod、Deployment 和 Service 的关系。直接输出正文，不要使用工具。"

// stopFollowUpPrompt is the short message sent after Stop: it only has to prove
// the send was accepted (not a 409) and produced a turn of its own.
const stopFollowUpPrompt = "请用一句话回复：收到。"

// stopTurnObserveTimeout bounds the wait for the background turn to become
// observable (or to end on its own). The agent instance is already Ready when a
// spec starts, so the only things inside this window are opening the gateway
// connection and the first token.
const stopTurnObserveTimeout = 3 * time.Minute

// stopTurnFinishTimeout bounds the wait for a stopped turn's SSE stream to
// terminate on the client side. The stream ends when the server-side handler
// returns, which the abort has already waited for, so this only covers the
// remaining bytes in flight.
const stopTurnFinishTimeout = time.Minute

// stopTurnResult is the outcome of a chat turn driven in the background.
type stopTurnResult struct {
	events []framework.SSEEvent
	err    error
}

// stopTurn is a chat turn running in its own goroutine, so the spec goroutine
// stays free to post the abort while the turn is still streaming.
type stopTurn struct {
	done chan struct{}
	res  stopTurnResult
}

func startStopTurn(ctx context.Context, user, sessionID, content string) *stopTurn {
	t := &stopTurn{done: make(chan struct{})}
	go func() {
		defer close(t.done)
		t.res.events, t.res.err = fw.ChatSSE(ctx, user, sessionID, content)
	}()
	return t
}

// finished reports whether the turn's stream has terminated (the turn ended,
// either on its own or because it was stopped).
func (t *stopTurn) finished() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// stopTurnObservedLive waits until the session reports an active run or the
// background turn has finished, whichever comes first, and reports which one it
// saw. A turn that ended before the abort could land is not a failure: the specs
// then still assert the end state the abort promises, they just cannot claim the
// abort landed mid-turn.
func stopTurnObservedLive(ctx context.Context, user, sessionID string, turn *stopTurn) bool {
	live := false
	Eventually(func() bool {
		if live || turn.finished() {
			return true
		}
		// The first turn is what dials the user's gateway connection, and an
		// idle session answers a plain 200 {active:false} either way, so an
		// unreadable status only means "not yet" here.
		active, code, err := fw.SessionTurnActive(ctx, user, sessionID)
		if err != nil || code != http.StatusOK {
			return false
		}
		live = active
		return live
	}, stopTurnObserveTimeout, time.Second).Should(BeTrue(),
		"the background turn neither reported an active run nor finished")
	return live
}

// stopEventNames lists the SSE event types of a turn, for compact assertions.
func stopEventNames(events []framework.SSEEvent) []string {
	names := make([]string, 0, len(events))
	for _, ev := range events {
		names = append(names, ev.Event)
	}
	return names
}

// stopTerminal is the terminal chat frame as the Portal reads it. A stopped turn
// is a third outcome of message_done: not a failure, not a normal completion.
type stopTerminal struct {
	Stopped bool   `json:"stopped"`
	Error   string `json:"error"`
}

// stopTerminalOf returns the message_done payload of a turn, and whether the
// stream carried one at all. The payload is read raw, not through the event's
// type: the fields are what the wire contract is, and the type would only
// restate them.
func stopTerminalOf(events []framework.SSEEvent) (stopTerminal, bool) {
	for _, ev := range events {
		if ev.Event != openclaw.EventMessageDone {
			continue
		}
		var terminal stopTerminal
		if err := json.Unmarshal(ev.Data, &terminal); err != nil {
			GinkgoWriter.Printf("stop e2e: undecodable message_done payload %s: %v\n", ev.Data, err)
			return stopTerminal{}, false
		}
		return terminal, true
	}
	return stopTerminal{}, false
}

// assertStoppedTerminal asserts the turn ended as a request-initiated stop.
//
// This is the feature's central wire claim, and the one thing the other
// assertions here cannot see: a 200 from Stop plus an accepted follow-up send
// would also hold if the gateway's aborted frame were classified as a *failure*,
// in which case pressing Stop would paint the user a red failed turn and throw
// away the partial reply's standing as a stopped (not finished) answer.
//
// Only meaningful when the abort was observed to land while the turn was still
// running. A turn that completed on its own before Stop arrived ends as an
// ordinary completion -- correctly -- so asserting a stopped terminal for it
// would be asserting on a race the specs deliberately do not run.
func assertStoppedTerminal(events []framework.SSEEvent, observedLive bool) {
	if !observedLive {
		GinkgoWriter.Printf("stop e2e: the turn had already ended when Stop landed; not asserting the stopped terminal\n")
		return
	}
	terminal, ok := stopTerminalOf(events)
	Expect(ok).To(BeTrue(), "the stopped turn's stream carried no message_done terminal")
	Expect(terminal.Stopped).To(BeTrue(),
		"a turn stopped by request must end as message_done{stopped:true}, not as a failure: %+v", terminal)
	Expect(terminal.Error).To(BeEmpty(),
		"stopped and error are mutually exclusive; a stopped turn must not carry a failure: %+v", terminal)
}

var _ = Describe("Stop ends a chat turn (SSE)", Label("chat"), func() {
	It("ends the running turn and leaves the session idle for the next send", func() {
		if os.Getenv("CUBEPILOT_E2E_CHAT") != "1" {
			Skip("CUBEPILOT_E2E_CHAT != 1 (needs a real LLM key); skipping stop chat e2e")
		}
		// The operator waits for the per-user identity before first provisioning,
		// so the fresh agent pod is stable from the start; wait only until it is
		// Ready before the turn (same gate as the other chat specs).
		By("waiting until the agent instance is Ready and its pod is stable")
		Eventually(func() error { return agentStabilityErr(context.Background(), fw.Users[0]) },
			4*time.Minute, 5*time.Second).Should(Succeed())

		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()

		sessionID := "e2e-" + rand.String(6)

		By("starting a turn that is still streaming when Stop is posted")
		turn := startStopTurn(ctx, fw.Users[0], sessionID, stopLongPrompt)
		observedLive := stopTurnObservedLive(ctx, fw.Users[0], sessionID, turn)

		By("posting Stop, which answers only once the session has settled")
		body, code, err := fw.AbortSession(ctx, fw.Users[0], sessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusOK),
			"Stop should settle the session for %s: %s", sessionID, string(body))

		// Read before the follow-up send: nothing else can start a turn on this
		// session, so a single read is the abort's own promise, not a poll.
		By("asserting the session reports no active turn")
		active, code, err := fw.SessionTurnActive(ctx, fw.Users[0], sessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusOK), "turn status should be readable after Stop")
		Expect(active).To(BeFalse(), "the stopped turn must leave the session idle")

		// The whole point of the endpoint's settle-before-answering contract: the
		// next send is accepted (HTTP 200 with a stream), not refused as a
		// concurrent turn. ChatSSE reports a refusal as "chat POST returned 409".
		By("sending again for the same session, which Stop must have made safe")
		follow, err := fw.ChatSSE(ctx, fw.Users[0], sessionID, stopFollowUpPrompt)
		Expect(err).NotTo(HaveOccurred(),
			"a send right after Stop must not be refused as a concurrent turn")
		Expect(stopEventNames(follow)).To(ContainElement(openclaw.EventMessageDone),
			"the follow-up send should run a turn of its own")

		By("waiting for the stopped turn's stream to terminate")
		Eventually(turn.finished, stopTurnFinishTimeout, time.Second).Should(BeTrue(),
			"the stopped turn's stream should end")

		By("asserting the stopped turn ends as a stop, not as a failure")
		assertStoppedTerminal(turn.res.events, observedLive)

		GinkgoWriter.Printf("stop e2e: turn was still live when Stop was posted: %v\n", observedLive)
	})

	It("stops the first turn and runs the second when a send redirects mid-turn", func() {
		if os.Getenv("CUBEPILOT_E2E_CHAT") != "1" {
			Skip("CUBEPILOT_E2E_CHAT != 1 (needs a real LLM key); skipping stop chat e2e")
		}
		By("waiting until the agent instance is Ready and its pod is stable")
		Eventually(func() error { return agentStabilityErr(context.Background(), fw.Users[0]) },
			4*time.Minute, 5*time.Second).Should(Succeed())

		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()

		sessionID := "e2e-" + rand.String(6)

		By("starting a turn that is still streaming when the redirect is sent")
		first := startStopTurn(ctx, fw.Users[0], sessionID, stopLongPrompt)
		observedLive := stopTurnObservedLive(ctx, fw.Users[0], sessionID, first)

		// Exactly the UI's redirect sequence: stop the running turn first, then
		// send. The abort's 200 is what makes the send below safe.
		By("posting Stop while the first turn is still streaming")
		body, code, err := fw.AbortSession(ctx, fw.Users[0], sessionID)
		Expect(err).NotTo(HaveOccurred())
		Expect(code).To(Equal(http.StatusOK),
			"Stop should settle the session for %s: %s", sessionID, string(body))

		By("sending the replacement message for the same session")
		second, err := fw.ChatSSE(ctx, fw.Users[0], sessionID, stopFollowUpPrompt)
		Expect(err).NotTo(HaveOccurred(),
			"the redirecting send must be accepted once Stop has settled the session")
		secondEvents := stopEventNames(second)
		Expect(secondEvents).To(ContainElement(openclaw.EventMessageDelta),
			"the second turn should stream its own output")
		Expect(secondEvents).To(ContainElement(openclaw.EventMessageDone),
			"the second turn should complete")

		// A 200 on the send above already proves the session settled (the hub
		// refuses a second concurrent stream with a 409), so the first turn's
		// handler must have returned and its stream must end. Wait for the client
		// side to observe it rather than assuming it.
		By("asserting the first turn's stream ended")
		Eventually(first.finished, stopTurnFinishTimeout, time.Second).Should(BeTrue(),
			"the first turn's stream should end after the redirect")

		// The redirect is the same stop: the first turn must not come back to a
		// waiting client as a failed (or a completed) turn.
		By("asserting the redirected turn ends as a stop, not as a failure")
		assertStoppedTerminal(first.res.events, observedLive)

		By("asserting the session is idle again once the second turn completes")
		Eventually(func() bool {
			active, code, err := fw.SessionTurnActive(ctx, fw.Users[0], sessionID)
			return err == nil && code == http.StatusOK && !active
		}, time.Minute, time.Second).Should(BeTrue(), "the redirected session should settle idle")

		GinkgoWriter.Printf("stop e2e: first turn was still live when Stop was posted: %v\n", observedLive)
	})
})
