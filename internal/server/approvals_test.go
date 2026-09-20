package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/suanova/cubepilot/internal/config"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
	"github.com/suanova/cubepilot/internal/store"
)

// approvalRecord builds one gateway approval record. The stamps are real
// milliseconds so ordering assertions mean something: createdAt orders a
// session's cards, and expiresAt is what the gateway itself holds the approval
// until.
func approvalRecord(id, sessionKey, command string, createdAtMs int64) ws.ApprovalRequested {
	return ws.ApprovalRequested{
		Kind:        "exec",
		ID:          id,
		Request:     ws.ExecApprovalRequest{Command: command, SessionKey: sessionKey, AgentID: "main"},
		CreatedAtMs: createdAtMs,
		ExpiresAtMs: createdAtMs + 30*time.Minute.Milliseconds(),
	}
}

// gatewayStub is an ApprovalGateway whose pending set is whatever the test put
// there. That is the point of the new shape: the platform has no record to seed,
// so a test that wants a pending approval puts it where the real one lives.
type gatewayStub struct {
	mu         sync.Mutex
	byUser     map[string][]ws.ApprovalRequested
	calls      []string // "user|approvalID|decision"
	listErr    error
	resolveErr error
	// onResolve runs inside ResolveApproval before it records anything, which is
	// where a test stages what the gateway does while a decision is in flight.
	onResolve func(id string)
	// blockResolve, when non-nil, parks ResolveApproval until the channel is
	// closed (or receives), holding a decision in flight.
	blockResolve chan struct{}
}

func (g *gatewayStub) set(user string, list ...ws.ApprovalRequested) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byUser == nil {
		g.byUser = map[string][]ws.ApprovalRequested{}
	}
	g.byUser[user] = list
}

func (g *gatewayStub) ListApprovals(_ context.Context, user string) ([]ws.ApprovalRequested, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.listErr != nil {
		return nil, g.listErr
	}
	return g.byUser[user], nil
}

func (g *gatewayStub) ResolveApproval(ctx context.Context, user, approvalID, decision string) error {
	g.mu.Lock()
	hook, block := g.onResolve, g.blockResolve
	g.mu.Unlock()
	if hook != nil {
		hook(approvalID)
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.resolveErr != nil {
		return g.resolveErr
	}
	g.calls = append(g.calls, user+"|"+approvalID+"|"+decision)
	// A resolved approval is no longer pending: the real gateway drops it from
	// exec.approval.list, and a competing decision has to see that.
	if list := g.byUser[user]; list != nil {
		kept := list[:0]
		for _, ev := range list {
			if ev.ID != approvalID {
				kept = append(kept, ev)
			}
		}
		g.byUser[user] = kept
	}
	return nil
}

func (g *gatewayStub) decisions() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

// openStream opens a session's stream and returns the recorder its events land
// in, so a test can assert on what the browser was told.
func openStream(t *testing.T, hub *Hub, sessionKey string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	if _, err := hub.Open(sessionKey, rec, rec); err != nil {
		t.Fatalf("open stream %s: %v", sessionKey, err)
	}
	return rec
}

func serviceWithGateway(t *testing.T, gw ApprovalGateway, st *store.Store) (*Hub, *ApprovalService) {
	t.Helper()
	hub := NewHub()
	svc := NewApprovalService(hub, st, t.Logf)
	svc.SetGateway(gw)
	return hub, svc
}

// TestApprovalRelayPublishesPendingWithStamps pins what the browser gets when the
// gateway raises an approval: the card's whole content, plus the stamps that
// order it among a session's other cards. Nothing is recorded -- the relay is a
// projection of the gateway's record, and the record it came from is what every
// later read asks for.
func TestApprovalRelayPublishesPendingWithStamps(t *testing.T) {
	hub, svc := serviceWithGateway(t, &gatewayStub{}, nil)
	rec := openStream(t, hub, "agent:main:conv-1")

	svc.RelayRequested("alice", approvalRecord("appr-1", "agent:main:conv-1", "kubectl delete pod foo", 1000))

	body := rec.Body.String()
	for _, want := range []string{
		"event: approval_pending",
		`"callId":"appr-1"`,
		`"command":"kubectl delete pod foo"`,
		`"createdAtMs":1000`,
		`"expiresAtMs":1801000`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream is missing %s; got %q", want, body)
		}
	}
}

// TestApprovalPendingComesFromTheGateway pins the read that replaces the
// platform's registry. A service that has just been constructed -- the state
// after a restart -- reports the session's approvals, in the platform's own
// order, and only that session's.
func TestApprovalPendingComesFromTheGateway(t *testing.T) {
	gw := &gatewayStub{}
	gw.set("alice",
		approvalRecord("appr-2", "agent:main:conv-1", "second", 2000),
		approvalRecord("appr-1", "agent:main:conv-1", "first", 1000),
		approvalRecord("appr-9", "agent:main:conv-2", "another conversation", 1500),
	)
	_, svc := serviceWithGateway(t, gw, nil)

	list, err := svc.Pending(context.Background(), "alice", "agent:main:conv-1")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("Pending returned %d approvals, want the session's 2: %+v", len(list), list)
	}
	if list[0].ApprovalID != "appr-1" || list[1].ApprovalID != "appr-2" {
		t.Errorf("order = %s, %s; want oldest first", list[0].ApprovalID, list[1].ApprovalID)
	}
	if list[0].Tool != "exec" || list[0].Level != "write" || list[0].Command != "first" {
		t.Errorf("projection = %+v", list[0])
	}
	// The URL key is canonicalised before it is compared, so the short form a
	// client sends addresses the same conversation.
	if _, err := svc.Pending(context.Background(), "alice", "conv-1"); err != nil {
		t.Fatalf("Pending with the short key: %v", err)
	}
	// Bob's connection sees nothing, which is the whole of the owner check: the
	// gateway filters the list per connection.
	list, err = svc.Pending(context.Background(), "bob", "agent:main:conv-1")
	if err != nil {
		t.Fatalf("Pending(bob): %v", err)
	}
	if len(list) != 0 {
		t.Errorf("bob sees %+v, want nothing", list)
	}
}

// TestApprovalPendingWithoutAChannelIsNotAnEmptyList pins that "cannot ask" is
// never answered as "nothing pending": an empty list is a statement about the
// gateway's state, and this process is in no position to make one.
func TestApprovalPendingWithoutAChannelIsNotAnEmptyList(t *testing.T) {
	svc := NewApprovalService(NewHub(), nil, t.Logf) // no gateway wired
	_, err := svc.Pending(context.Background(), "alice", "conv-1")
	if !errors.Is(err, errNoApprovalChannel) {
		t.Fatalf("Pending without a channel = %v, want errNoApprovalChannel", err)
	}
}

// TestApprovalResolveSettlesTheClickedId is the bug from issue #226 in one test:
// one session, two pending approvals, and a decision that names the older one.
// The id the human clicked is the id the gateway is asked to settle, and the
// only card that drops is that one. Previously the decision was addressed by
// session and settled whichever approval the platform had last recorded -- which
// was never the older card -- while both cards reported themselves approved.
func TestApprovalResolveSettlesTheClickedId(t *testing.T) {
	for _, decision := range []string{"approve", "reject"} {
		t.Run(decision, func(t *testing.T) {
			gw := &gatewayStub{}
			gw.set("alice",
				approvalRecord("appr-old", "agent:main:conv-1", "kubectl delete pod old", 1000),
				approvalRecord("appr-new", "agent:main:conv-1", "kubectl delete pod new", 2000),
			)
			hub, svc := serviceWithGateway(t, gw, nil)
			rec := openStream(t, hub, "agent:main:conv-1")

			p, err := svc.Resolve(context.Background(), "alice", "agent:main:conv-1", "appr-old", decision)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if p.ApprovalID != "appr-old" || p.Command != "kubectl delete pod old" {
				t.Errorf("resolved = %+v, want the older approval", p)
			}
			want := "alice|appr-old|approve"
			if decision == "reject" {
				want = "alice|appr-old|reject"
			}
			if got := gw.decisions(); len(got) != 1 || got[0] != want {
				t.Fatalf("gateway decisions = %v, want [%s]", got, want)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"callId":"appr-old"`) {
				t.Errorf("resolved event does not name the settled approval: %q", body)
			}
			if strings.Contains(body, `"callId":"appr-new"`) {
				t.Errorf("the other approval's card was settled too: %q", body)
			}
			// The other approval is still the gateway's to answer, so it is still
			// the session's to show -- and the settled one is gone.
			list, err := svc.Pending(context.Background(), "alice", "agent:main:conv-1")
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 1 || list[0].ApprovalID != "appr-new" {
				t.Errorf("pending after the decision = %+v, want only appr-new", list)
			}
		})
	}
}

// TestApprovalResolveRefusesAnotherSessionsId pins the binding the id needs: an
// id that exists but belongs to another conversation is not settleable through
// this session's path, and nothing is sent to the gateway for it.
func TestApprovalResolveRefusesAnotherSessionsId(t *testing.T) {
	gw := &gatewayStub{}
	gw.set("alice", approvalRecord("appr-9", "agent:main:conv-2", "elsewhere", 1000))
	_, svc := serviceWithGateway(t, gw, nil)

	_, err := svc.Resolve(context.Background(), "alice", "agent:main:conv-1", "appr-9", "approve")
	if !errors.Is(err, errNoPending) {
		t.Fatalf("Resolve with another session's id = %v, want errNoPending", err)
	}
	if got := gw.decisions(); len(got) != 0 {
		t.Errorf("gateway decisions = %v, want none", got)
	}
}

// TestApprovalResolveRefusesAnUnknownId pins the simplest case: an id the
// gateway no longer lists -- expired, resolved elsewhere, or never existed -- is
// not a decision to send.
func TestApprovalResolveRefusesAnUnknownId(t *testing.T) {
	gw := &gatewayStub{}
	gw.set("alice", approvalRecord("appr-1", "agent:main:conv-1", "cmd", 1000))
	_, svc := serviceWithGateway(t, gw, nil)

	if _, err := svc.Resolve(context.Background(), "alice", "agent:main:conv-1", "appr-gone", "approve"); !errors.Is(err, errNoPending) {
		t.Fatalf("Resolve(unknown) = %v, want errNoPending", err)
	}
	if got := gw.decisions(); len(got) != 0 {
		t.Errorf("gateway decisions = %v, want none", got)
	}
}

// TestApprovalResolveSerializesConcurrentDecisions pins the one piece of local
// state left, and what a competing decision is told. Two tabs, or a double click,
// must not produce two resolves: the second waits for the first and then asks the
// gateway what is left. Here the first settled the approval, so the second finds
// nothing pending -- the truth -- rather than a conflict claiming an outcome
// before there was one.
func TestApprovalResolveSerializesConcurrentDecisions(t *testing.T) {
	gw := &gatewayStub{}
	gw.set("alice", approvalRecord("appr-1", "agent:main:conv-1", "cmd", 1000))
	release := make(chan struct{})
	gw.blockResolve = release
	_, svc := serviceWithGateway(t, gw, nil)

	first := make(chan error, 1)
	go func() {
		_, err := svc.Resolve(context.Background(), "alice", "agent:main:conv-1", "appr-1", "approve")
		first <- err
	}()
	waitFor(t, "the decision to reach the gateway", func() bool { return svc.deciding("appr-1") })

	second := make(chan error, 1)
	go func() {
		_, err := svc.Resolve(context.Background(), "alice", "agent:main:conv-1", "appr-1", "reject")
		second <- err
	}()
	// The second decision is parked on the reservation, not answered with a
	// conflict: nothing has been decided yet, so there is no outcome to report.
	select {
	case err := <-second:
		t.Fatalf("the competing decision was answered before the first finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first decision: %v", err)
	}
	if err := <-second; !errors.Is(err, errNoPending) {
		t.Fatalf("second decision = %v, want errNoPending (the approval is settled)", err)
	}
	if got := gw.decisions(); len(got) != 1 || got[0] != "alice|appr-1|approve" {
		t.Fatalf("gateway decisions = %v, want exactly the first one", got)
	}
	// The reservation is released on the way out, so the id is decidable again --
	// a failed or superseded decision must not wedge the approval.
	waitFor(t, "the reservation to be released", func() bool { return !svc.deciding("appr-1") })
}

// TestApprovalResolveRetriesACompetingFailure pins the other half of the same
// rule, and the reason a conflict is the wrong answer: when the decision ahead of
// it FAILED, the waiter's approval is still pending, so the waiter settles it
// instead of reporting someone else's failure. Refusing it would have left the
// card unanswered -- and a client that closes the card on a 409 has no way back.
func TestApprovalResolveRetriesACompetingFailure(t *testing.T) {
	gw := &gatewayStub{}
	gw.set("alice", approvalRecord("appr-1", "agent:main:conv-1", "cmd", 1000))
	release := make(chan struct{})
	gw.blockResolve = release
	_, svc := serviceWithGateway(t, gw, nil)

	first := make(chan error, 1)
	go func() {
		_, err := svc.Resolve(context.Background(), "alice", "agent:main:conv-1", "appr-1", "approve")
		first <- err
	}()
	waitFor(t, "the decision to reach the gateway", func() bool { return svc.deciding("appr-1") })

	second := make(chan error, 1)
	go func() {
		_, err := svc.Resolve(context.Background(), "alice", "agent:main:conv-1", "appr-1", "approve")
		second <- err
	}()
	select {
	case err := <-second:
		t.Fatalf("the competing decision was answered before the first finished: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	// The first decision fails on the gateway; the waiter then settles the
	// approval itself.
	gw.mu.Lock()
	gw.resolveErr = errors.New("ws write exec.approval.resolve: broken pipe")
	gw.mu.Unlock()
	close(release)
	if err := <-first; err == nil {
		t.Fatal("the first decision should have failed")
	}
	gw.mu.Lock()
	gw.resolveErr = nil
	gw.mu.Unlock()
	if err := <-second; err != nil {
		t.Fatalf("the waiter did not settle the still-pending approval: %v", err)
	}
	if got := gw.decisions(); len(got) != 1 || got[0] != "alice|appr-1|approve" {
		t.Fatalf("gateway decisions = %v, want the waiter's one", got)
	}
}

// TestApprovalResolveFailureIsRetryable pins that a decision the gateway refused
// leaves nothing behind: no card settled, and the id immediately decidable
// again.
func TestApprovalResolveFailureIsRetryable(t *testing.T) {
	gw := &gatewayStub{resolveErr: errors.New("gateway gone")}
	gw.set("alice", approvalRecord("appr-1", "agent:main:conv-1", "cmd", 1000))
	hub, svc := serviceWithGateway(t, gw, nil)
	rec := openStream(t, hub, "agent:main:conv-1")

	if _, err := svc.Resolve(context.Background(), "alice", "agent:main:conv-1", "appr-1", "approve"); err == nil {
		t.Fatal("expected the gateway failure to propagate")
	}
	if strings.Contains(rec.Body.String(), "approval_resolved") {
		t.Errorf("a failed decision settled the card: %q", rec.Body.String())
	}
	if svc.deciding("appr-1") {
		t.Fatal("the approval is still reserved after a failed decision: retrying it would be refused")
	}

	gw.mu.Lock()
	gw.resolveErr = nil
	gw.mu.Unlock()
	if _, err := svc.Resolve(context.Background(), "alice", "agent:main:conv-1", "appr-1", "approve"); err != nil {
		t.Fatalf("retry after a failed decision: %v", err)
	}
}

// TestApprovalResolveRecordsTheDecisionWithItsId pins the ledger's new key. A
// session can hold several approvals, so a decision row that does not name the
// one it settled cannot be reconciled against them after the fact.
func TestApprovalResolveRecordsTheDecisionWithItsId(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gw := &gatewayStub{}
	gw.set("alice", approvalRecord("appr-1", "agent:main:conv-1", "kubectl delete pod foo", 1000))
	_, svc := serviceWithGateway(t, gw, st)

	if _, err := svc.Resolve(context.Background(), "alice", "agent:main:conv-1", "appr-1", "approve"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	audit, err := st.ListAudit("alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 {
		t.Fatalf("audit = %+v, want one decision row", audit)
	}
	got := audit[0]
	if got.ApprovalID != "appr-1" || got.SessionID != "agent:main:conv-1" || got.Status != "approved" {
		t.Errorf("audit row = %+v, want appr-1 approved on the session", got)
	}
	if got.Command != "kubectl delete pod foo" {
		t.Errorf("audit command = %q, want the gateway's record of it", got.Command)
	}
}

// TestApprovalSettleSkipsAnInFlightDecision pins the interaction between Stop
// and a decision: an approval being decided right now is left to that decision,
// which is about to publish its own outcome, and the rest of the session's
// approvals are settled. The gateway resolve is held open so the interleaving is
// forced rather than raced for.
func TestApprovalSettleSkipsAnInFlightDecision(t *testing.T) {
	gw := &gatewayStub{}
	gw.set("alice",
		approvalRecord("appr-1", "agent:main:conv-1", "one", 1000),
		approvalRecord("appr-2", "agent:main:conv-1", "two", 2000),
	)
	release := make(chan struct{})
	gw.blockResolve = release
	hub, svc := serviceWithGateway(t, gw, nil)
	rec := openStream(t, hub, "agent:main:conv-1")
	srv := &Server{hub: hub, approvals: svc}

	deciding := make(chan error, 1)
	go func() {
		_, err := svc.Resolve(context.Background(), "alice", "agent:main:conv-1", "appr-1", "approve")
		deciding <- err
	}()
	waitFor(t, "the decision to reach the gateway", func() bool { return svc.deciding("appr-1") })

	captured, err := svc.Pending(context.Background(), "alice", "agent:main:conv-1")
	if err != nil {
		t.Fatal(err)
	}
	srv.settlePendingForSession(context.Background(), "alice", "agent:main:conv-1", captured)

	// The stop settles the approval nobody is deciding, and only that one: a
	// neutral "stopped" published under the in-flight decision would sit on its
	// card until the decision's own resolution landed.
	body := rec.Body.String()
	if !strings.Contains(body, `"callId":"appr-2"`) {
		t.Fatalf("the stopped turn did not settle the approval nobody is deciding: %q", body)
	}
	if strings.Contains(body, `"callId":"appr-1"`) {
		t.Fatalf("the stop published over a decision in flight: %q", body)
	}

	close(release)
	if err := <-deciding; err != nil {
		t.Fatalf("decision: %v", err)
	}
	body = rec.Body.String()
	if !strings.Contains(body, `"callId":"appr-1"`) || !strings.Contains(body, `"approved":true`) {
		t.Errorf("the decision's own resolution is missing: %q", body)
	}
}

// --- HTTP surface ------------------------------------------------------------

func newApprovalServer(t *testing.T, gw ApprovalGateway) (*Server, *gatewayStub) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stub, _ := gw.(*gatewayStub)
	srv := New(config.Config{DefaultUser: "alice"}, nil, st, nil, nil)
	srv.approvals.SetGateway(gw)
	return srv, stub
}

// TestHandleApprovalSettlesTheNamedId pins the request contract: the decision
// names its approval, and the answer says which one it settled -- the client
// holds that id and is entitled to check the platform's account of the decision
// against the card that was clicked.
func TestHandleApprovalSettlesTheNamedId(t *testing.T) {
	gw := &gatewayStub{}
	gw.set("alice", approvalRecord("appr-1", "agent:main:conv-1", "kubectl delete pod foo", 1000))
	srv, _ := newApprovalServer(t, gw)

	rec := doReq(t, srv.Handler(), http.MethodPost, "/api/v1/sessions/conv-1/approval", "alice",
		map[string]any{"approvalId": "appr-1", "decision": "approve"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Approved   bool   `json:"approved"`
		Decision   string `json:"decision"`
		ApprovalID string `json:"approvalId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Approved || body.Decision != "approve" || body.ApprovalID != "appr-1" {
		t.Errorf("body = %+v, want appr-1 approved", body)
	}
	if got := gw.decisions(); len(got) != 1 || got[0] != "alice|appr-1|approve" {
		t.Errorf("gateway decisions = %v", got)
	}
}

// TestHandleApprovalRequiresTheId pins that the id is not optional. A session
// can hold several approvals, so "the session's approval" is not a thing a
// request can mean -- refusing is the only answer that cannot settle the wrong
// one.
func TestHandleApprovalRequiresTheId(t *testing.T) {
	gw := &gatewayStub{}
	gw.set("alice", approvalRecord("appr-1", "agent:main:conv-1", "cmd", 1000))
	srv, _ := newApprovalServer(t, gw)

	rec := doReq(t, srv.Handler(), http.MethodPost, "/api/v1/sessions/conv-1/approval", "alice",
		map[string]any{"decision": "approve"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if got := gw.decisions(); len(got) != 0 {
		t.Errorf("gateway decisions = %v, want none", got)
	}
}

// TestHandleApprovalIsConnectionScoped pins the only owner check there is: the
// list and the decision run on the caller's own gateway connection, so another
// operator's approval is not visible -- and not settleable -- through that path.
func TestHandleApprovalIsConnectionScoped(t *testing.T) {
	gw := &gatewayStub{}
	gw.set("alice", approvalRecord("appr-1", "agent:main:conv-1", "cmd", 1000))
	srv, _ := newApprovalServer(t, gw)

	rec := doReq(t, srv.Handler(), http.MethodGet, "/api/v1/sessions/conv-1/approval/pending", "bob", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob pending GET status = %d, want 404", rec.Code)
	}
	rec = doReq(t, srv.Handler(), http.MethodPost, "/api/v1/sessions/conv-1/approval", "bob",
		map[string]any{"approvalId": "appr-1", "decision": "approve"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bob decision status = %d, want 404", rec.Code)
	}
	if got := gw.decisions(); len(got) != 0 {
		t.Fatalf("gateway decisions = %v, want none for a connection that cannot see the approval", got)
	}
}

// TestHandleApprovalMapsGatewayErrors pins the statuses a click needs to tell an
// outcome from a failure: an approval that expired or was settled elsewhere
// between the card being painted and the click is 404/409 and closes the card,
// while a transport failure is a 502 the client may retry.
func TestHandleApprovalMapsGatewayErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		want   int
	}{
		{"gone", "APPROVAL_NOT_FOUND", http.StatusNotFound},
		{"already resolved", "APPROVAL_ALREADY_RESOLVED", http.StatusConflict},
		{"transport", "", http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := &gatewayStub{}
			gw.set("alice", approvalRecord("appr-1", "agent:main:conv-1", "cmd", 1000))
			gw.resolveErr = &ws.RPCError{Code: "INVALID_REQUEST", Message: "denied", Reason: tc.reason}
			if tc.reason == "" {
				gw.resolveErr = errors.New("ws write exec.approval.resolve: broken pipe")
			}
			srv, _ := newApprovalServer(t, gw)

			rec := doReq(t, srv.Handler(), http.MethodPost, "/api/v1/sessions/conv-1/approval", "alice",
				map[string]any{"approvalId": "appr-1", "decision": "approve"})
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestHandleApprovalNoChannelIsUnavailable pins the fail-closed answer for a
// caller with no live gateway connection: the decision cannot be delivered, and
// saying so is what stops a client from treating it as settled.
func TestHandleApprovalNoChannelIsUnavailable(t *testing.T) {
	srv, _ := newApprovalServer(t, &gatewayStub{listErr: errNoApprovalChannel})

	rec := doReq(t, srv.Handler(), http.MethodPost, "/api/v1/sessions/conv-1/approval", "alice",
		map[string]any{"approvalId": "appr-1", "decision": "approve"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleApprovalDoubleClickSettlesOnce pins what the losing click of a double
// click gets. It waits for the winner and then finds the approval settled -- a 404
// the client reads as "someone settled this", which is true -- rather than a
// conflict asserting an outcome before the gateway had produced one.
func TestHandleApprovalDoubleClickSettlesOnce(t *testing.T) {
	gw := &gatewayStub{}
	gw.set("alice", approvalRecord("appr-1", "agent:main:conv-1", "cmd", 1000))
	release := make(chan struct{})
	gw.blockResolve = release
	srv, _ := newApprovalServer(t, gw)

	first := make(chan int, 1)
	go func() {
		rec := doReq(t, srv.Handler(), http.MethodPost, "/api/v1/sessions/conv-1/approval", "alice",
			map[string]any{"approvalId": "appr-1", "decision": "approve"})
		first <- rec.Code
	}()
	waitFor(t, "the decision to reach the gateway", func() bool { return srv.approvals.deciding("appr-1") })

	second := make(chan int, 1)
	go func() {
		rec := doReq(t, srv.Handler(), http.MethodPost, "/api/v1/sessions/conv-1/approval", "alice",
			map[string]any{"approvalId": "appr-1", "decision": "reject"})
		second <- rec.Code
	}()

	close(release)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first decision status = %d, want 200", code)
	}
	if code := <-second; code != http.StatusNotFound {
		t.Fatalf("second decision status = %d, want 404 (the approval is settled): %s", code, "")
	}
	if got := gw.decisions(); len(got) != 1 {
		t.Fatalf("gateway decisions = %v, want exactly one resolve", got)
	}
}

// TestHandlePendingApprovalListsTheSessionSet pins the recovery contract: every
// pending approval of the session comes back, oldest first, in the shape a card
// is drawn from. A single-approval envelope cannot express the state this
// endpoint exists to restore.
func TestHandlePendingApprovalListsTheSessionSet(t *testing.T) {
	gw := &gatewayStub{}
	gw.set("alice",
		approvalRecord("appr-2", "agent:main:conv-1", "second", 2000),
		approvalRecord("appr-1", "agent:main:conv-1", "first", 1000),
		approvalRecord("appr-9", "agent:main:conv-2", "elsewhere", 1500),
	)
	srv, _ := newApprovalServer(t, gw)

	rec := doReq(t, srv.Handler(), http.MethodGet, "/api/v1/sessions/conv-1/approval/pending", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Approvals []approvalEntry `json:"approvals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Approvals) != 2 {
		t.Fatalf("approvals = %+v, want the session's two", body.Approvals)
	}
	if body.Approvals[0].ApprovalID != "appr-1" || body.Approvals[1].ApprovalID != "appr-2" {
		t.Errorf("order = %s, %s; want oldest first", body.Approvals[0].ApprovalID, body.Approvals[1].ApprovalID)
	}
	if body.Approvals[0].CreatedAtMs != 1000 || body.Approvals[0].ExpiresAtMs == 0 {
		t.Errorf("stamps = %+v, want the gateway's", body.Approvals[0])
	}
	if body.Approvals[0].SessionID != "agent:main:conv-1" || body.Approvals[0].Level != "write" {
		t.Errorf("entry = %+v", body.Approvals[0])
	}
}

// TestHandlePendingApprovalEmptyOrUnavailable pins the two answers that are not
// a list. "Nothing pending" is a 404, the convention the sibling question
// endpoint uses; a gateway that could not answer is a 502, because a page that
// reloaded onto a parked write must not be told there is nothing to show.
func TestHandlePendingApprovalEmptyOrUnavailable(t *testing.T) {
	gw := &gatewayStub{}
	gw.set("alice", approvalRecord("appr-9", "agent:main:conv-2", "elsewhere", 1000))
	srv, _ := newApprovalServer(t, gw)

	rec := doReq(t, srv.Handler(), http.MethodGet, "/api/v1/sessions/conv-1/approval/pending", "alice", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("empty status = %d, want 404", rec.Code)
	}

	gw.mu.Lock()
	gw.listErr = errors.New("ws write exec.approval.list: broken pipe")
	gw.mu.Unlock()
	rec = doReq(t, srv.Handler(), http.MethodGet, "/api/v1/sessions/conv-1/approval/pending", "alice", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("unreadable status = %d, want 502: a failed read must not read as \"nothing pending\"", rec.Code)
	}
}

// TestHandleApprovalAcceptsAShortSessionKey pins the key agreement between the
// endpoints that address one conversation. The decision path canonicalises the
// key before matching it against the record's own, so the short form a client
// sends works here exactly as it does on /turn and /abort.
func TestHandleApprovalAcceptsAShortSessionKey(t *testing.T) {
	gw := &gatewayStub{}
	gw.set("alice", approvalRecord("appr-1", "agent:main:conv-1", "cmd", 1000))
	srv, _ := newApprovalServer(t, gw)

	rec := doReq(t, srv.Handler(), http.MethodPost, "/api/v1/sessions/conv-1/approval", "alice",
		map[string]any{"approvalId": "appr-1", "decision": "approve"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// TestRelayApprovalResolvedAddressesTheEvent pins the relay of the gateway's own
// resolution: the broadcast carries the approval's request, so the session comes
// from the wire and an approval this process never relayed still reaches its
// card.
func TestRelayApprovalResolvedAddressesTheEvent(t *testing.T) {
	hub := NewHub()
	srv := &Server{hub: hub, approvals: NewApprovalService(hub, nil, t.Logf)}
	rec := openStream(t, hub, "agent:main:conv-1")

	srv.relayApprovalResolved("alice", ws.ApprovalResolved{
		ID:         "appr-1",
		Decision:   "allow-once",
		ResolvedBy: "zhujian",
		Request:    ws.ExecApprovalRequest{Command: "cmd", SessionKey: "agent:main:conv-1"},
	})

	body := rec.Body.String()
	if !strings.Contains(body, "event: approval_resolved") || !strings.Contains(body, `"callId":"appr-1"`) {
		t.Fatalf("resolution not relayed: %q", body)
	}
	if !strings.Contains(body, `"approved":true`) {
		t.Errorf("a decision taken by a human must arrive as approved: %q", body)
	}

	// An event with no session behind it cannot be addressed: it is dropped
	// rather than published somewhere arbitrary.
	rec.Body.Reset()
	srv.relayApprovalResolved("alice", ws.ApprovalResolved{ID: "appr-2", Decision: "deny"})
	if strings.Contains(rec.Body.String(), "approval_resolved") {
		t.Errorf("an unaddressable resolution was published: %q", rec.Body.String())
	}
}

// TestRelayApprovalResolvedWithoutADecisionIsNeutral pins the expiry case: the
// gateway fills an absent decision with "deny", so Approved is driven by
// ResolvedBy instead. Painting a red "rejected" for an approval nobody answered
// would attribute a decision to the human that they never made.
func TestRelayApprovalResolvedWithoutADecisionIsNeutral(t *testing.T) {
	hub := NewHub()
	srv := &Server{hub: hub, approvals: NewApprovalService(hub, nil, t.Logf)}
	rec := openStream(t, hub, "agent:main:conv-1")

	srv.relayApprovalResolved("alice", ws.ApprovalResolved{
		ID: "appr-1", Decision: "deny", ResolvedBy: "",
		Request: ws.ExecApprovalRequest{SessionKey: "agent:main:conv-1"},
	})

	if !strings.Contains(rec.Body.String(), "approval_resolved") {
		t.Fatalf("expiry not relayed: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"approved"`) {
		t.Errorf("an unanswered approval carries a decision: %q", rec.Body.String())
	}
}
