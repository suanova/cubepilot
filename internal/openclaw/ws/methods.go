package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// GetApprovalsPolicy reads the per-agent exec-approvals policy
// (exec.approvals.get). Returns its snapshot including the base hash needed for
// a subsequent set.
func (c *Client) GetApprovalsPolicy(ctx context.Context) (*ApprovalsSnapshot, error) {
	raw, err := c.Call(ctx, "exec.approvals.get", struct{}{})
	if err != nil {
		return nil, err
	}
	var snap ApprovalsSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("decode exec.approvals.get: %w", err)
	}
	return &snap, nil
}

// SetApprovalsPolicy writes the per-agent exec-approvals policy
// (exec.approvals.set). baseHash must be the hash from a prior get (CAS); pass
// "" when the file does not exist yet.
func (c *Client) SetApprovalsPolicy(ctx context.Context, file ApprovalsFile, baseHash string) (*ApprovalsSnapshot, error) {
	params := map[string]any{"file": file}
	if baseHash != "" {
		params["baseHash"] = baseHash
	}
	raw, err := c.Call(ctx, "exec.approvals.set", params)
	if err != nil {
		return nil, err
	}
	var snap ApprovalsSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("decode exec.approvals.set: %w", err)
	}
	return &snap, nil
}

// ResolveApproval resolves a pending exec approval (exec.approval.resolve).
// decision is allow-once or deny.
func (c *Client) ResolveApproval(ctx context.Context, id, decision string) error {
	_, err := c.Call(ctx, "exec.approval.resolve", approvalResolveParams{ID: id, Decision: decision})
	return err
}

// ResolveQuestion answers a pending question (question.resolve). answers maps
// each question's id to the selected option labels; resolvedBy is audit
// metadata shown by the gateway, not an authorization input.
func (c *Client) ResolveQuestion(ctx context.Context, id string, answers map[string][]string, resolvedBy string) error {
	if len(answers) == 0 {
		return fmt.Errorf("question.resolve %q: empty answer set", id)
	}
	params := questionResolveParams{ID: id, Answers: &questionAnswers{Answers: answers}, ResolvedBy: resolvedBy}
	if _, err := c.Call(ctx, "question.resolve", params); err != nil {
		return fmt.Errorf("question.resolve %q: %w", id, err)
	}
	return nil
}

// CancelQuestion dismisses a pending question (question.resolve with
// cancel:true), letting the agent continue instead of waiting out its timeout.
func (c *Client) CancelQuestion(ctx context.Context, id, resolvedBy string) error {
	params := questionResolveParams{ID: id, Cancel: true, ResolvedBy: resolvedBy}
	if _, err := c.Call(ctx, "question.resolve", params); err != nil {
		return fmt.Errorf("question.resolve cancel %q: %w", id, err)
	}
	return nil
}

// chatAbortParams is the chat.abort request body. runId is omitted entirely when
// empty: the gateway's schema rejects an empty string, and omitting it is what
// selects the session-scoped abort.
type chatAbortParams struct {
	SessionKey string `json:"sessionKey"`
	RunID      string `json:"runId,omitempty"`
}

// chatAbortResult is chat.abort's success payload. `aborted` is the field that
// says whether the RPC actually stopped anything, and it must not be discarded:
// the gateway answers ok=true with aborted=false and an empty runIds when the
// run id matched no abortable run, which is a real answer, not a stop. A run
// whose gateway snapshot carries sessionAbortable:true is cancellable only
// through the session-owned path, and a run promoted between the caller's
// in-flight read and this RPC yields the same payload. Reading the ok as "the
// run stopped" is how a caller ends up settling a session whose run is still
// going.
type chatAbortResult struct {
	Aborted bool `json:"aborted"`
}

// AbortChat cancels a run (chat.abort). runID scopes the abort to that run and
// is what every caller passes; an empty runID aborts the session's active run
// instead, which is the session-scoped form. That form is kept here as a
// capability of the protocol but /abort never uses it: a Stop that cannot name
// a run is answered rather than sent, because the session-scoped abort
// terminates whatever the session is running at that moment and the gateway's
// aborted:true carries nothing that distinguishes it from the run the user
// meant to stop (see server.handleAbort).
//
// It reports whether the gateway actually aborted a run. A nil error with
// aborted=false is a successful RPC that stopped nothing: the run id matched no
// abortable run. The caller must reconcile that against the session's liveness
// before treating the stop as done. An undecodable payload is reported as an
// error rather than as aborted=false, because "the gateway said something this
// client cannot read" is not the gateway saying it stopped nothing -- the caller
// treats a returned error conservatively either way.
func (c *Client) AbortChat(ctx context.Context, sessionKey, runID string) (bool, error) {
	if sessionKey == "" {
		return false, fmt.Errorf("chat.abort: empty session key")
	}
	raw, err := c.Call(ctx, "chat.abort", chatAbortParams{SessionKey: sessionKey, RunID: runID})
	if err != nil {
		return false, fmt.Errorf("chat.abort %q: %w", sessionKey, err)
	}
	var out chatAbortResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, fmt.Errorf("decode chat.abort %q: %w", sessionKey, err)
	}
	return out.Aborted, nil
}

type chatHistoryParams struct {
	SessionKey string `json:"sessionKey"`
}

// chatHistoryResult is the subset of chat.history's delta result this client
// reads. The delta payload also carries messages and a deltaCursor that a future
// stream re-attach would use; nothing here consumes them, and the gateway's
// "reset" shape simply has no inFlightRun.
type chatHistoryResult struct {
	InFlightRun json.RawMessage `json:"inFlightRun"`
}

// sessionHistory reads the session's history projection (chat.history).
func (c *Client) sessionHistory(ctx context.Context, sessionKey string) (chatHistoryResult, error) {
	raw, err := c.Call(ctx, "chat.history", chatHistoryParams{SessionKey: sessionKey})
	if err != nil {
		return chatHistoryResult{}, fmt.Errorf("chat.history %q: %w", sessionKey, err)
	}
	var out chatHistoryResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return chatHistoryResult{}, fmt.Errorf("decode chat.history: %w", err)
	}
	return out, nil
}

// hasInFlightRun reports whether chat.history described a run at all: the field
// is absent (or explicitly null) when the session is idle.
func hasInFlightRun(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null"
}

// SessionBusy reports whether the gateway has an in-flight run for the session.
// It is the authoritative "is this session busy" signal: the SSE hub only knows
// whether a browser is attached, which is false after a reload while the run is
// still going.
func (c *Client) SessionBusy(ctx context.Context, sessionKey string) (bool, error) {
	out, err := c.sessionHistory(ctx, sessionKey)
	if err != nil {
		return false, err
	}
	return hasInFlightRun(out.InFlightRun), nil
}

// SessionInFlightRun returns the gateway's in-flight run for the session: its
// run id when the snapshot carries one, and whether a run is in flight at all.
//
// The two are separate answers on purpose, and the caller must not collapse
// them into a bare id. The id is what scopes an abort (chat.abort runId) so it
// cannot terminate a run promoted after the one the caller meant; the browser
// never learns it, but on the reload-takeover path the server has lost its own
// copy too -- releaseLive dropped the local turn when the request driving it
// ended, while the run it started can still be executing -- and chat.history is
// then the only place the id exists. active reports whether chat.history
// described a run at all: the inFlightRun payload is the gateway's own run
// descriptor and so opaque to this schema, and a descriptor that carries no
// runId yields active=true with an empty id. Reading that as "nothing is
// running" is wrong -- it means a run exists that cannot be named, which a
// caller about to issue a session-scoped abort in its place needs to know.
func (c *Client) SessionInFlightRun(ctx context.Context, sessionKey string) (string, bool, error) {
	out, err := c.sessionHistory(ctx, sessionKey)
	if err != nil {
		return "", false, err
	}
	if !hasInFlightRun(out.InFlightRun) {
		return "", false, nil
	}
	var run struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(out.InFlightRun, &run); err != nil {
		return "", false, fmt.Errorf("decode chat.history inFlightRun: %w", err)
	}
	return run.RunID, true, nil
}

// GetQuestion reads one question record (question.get). The gateway keeps a
// terminal record for a short grace period, so this still answers shortly
// after the question resolves.
func (c *Client) GetQuestion(ctx context.Context, id string) (*QuestionRecord, error) {
	raw, err := c.Call(ctx, "question.get", map[string]any{"id": id})
	if err != nil {
		return nil, fmt.Errorf("question.get %q: %w", id, err)
	}
	var out questionGetResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode question.get: %w", err)
	}
	return &out.Question, nil
}

// ListQuestions returns the gateway's pending questions (question.list).
func (c *Client) ListQuestions(ctx context.Context) ([]QuestionRecord, error) {
	raw, err := c.Call(ctx, "question.list", struct{}{})
	if err != nil {
		return nil, fmt.Errorf("question.list: %w", err)
	}
	var out QuestionListResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode question.list: %w", err)
	}
	return out.Questions, nil
}

// ReasonOf returns the gateway's structured error reason (error.details.reason)
// when err wraps a failed RPC that carried one, or "" otherwise. Callers use it
// to map a protocol-level reason such as QUESTION_ALREADY_TERMINAL onto their
// own status without parsing message text.
func ReasonOf(err error) string {
	var re *rpcError
	if errors.As(err, &re) {
		return re.Reason
	}
	return ""
}

// PatchSessionGuarded sets a session's permissionMode to guarded
// (sessions.patch). Requires the session to exist.
func (c *Client) PatchSessionGuarded(ctx context.Context, key string) error {
	guarded := "guarded"
	_, err := c.Call(ctx, "sessions.patch", sessionPatchParams{Key: key, PermissionMode: stringField(guarded)})
	return err
}

// PatchSessionSettings atomically applies the selected model and permission
// mode changes before a turn. Empty values are encoded as JSON null so an old
// override is explicitly cleared.
func (c *Client) PatchSessionSettings(ctx context.Context, key string, patch SessionSettingsPatch) error {
	params := sessionPatchParams{Key: key}
	if patch.Model.Set {
		params.Model = stringField(patch.Model.Value)
	}
	if patch.PermissionMode.Set {
		params.PermissionMode = stringField(patch.PermissionMode.Value)
	}
	if _, err := c.Call(ctx, "sessions.patch", params); err != nil {
		return fmt.Errorf("sessions.patch settings for %q: %w", key, err)
	}
	return nil
}

// stringField returns a pointer-to-pointer representation that distinguishes
// an omitted field from an explicit JSON null.
func stringField(value string) **string {
	if value == "" {
		var cleared *string
		return &cleared
	}
	set := value
	ptr := &set
	return &ptr
}

// CreateSessionGuarded creates a session with permissionMode guarded
// (sessions.create). Used when patch reports the session does not exist.
func (c *Client) CreateSessionGuarded(ctx context.Context, key string) error {
	_, err := c.Call(ctx, "sessions.create", sessionCreateParams{Key: key, PermissionMode: "guarded"})
	return err
}

// DevicePairList lists pending and paired device requests (device.pair.list).
func (c *Client) DevicePairList(ctx context.Context) (*DevicePairListResult, error) {
	raw, err := c.Call(ctx, "device.pair.list", struct{}{})
	if err != nil {
		return nil, err
	}
	var out DevicePairListResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode device.pair.list: %w", err)
	}
	return &out, nil
}

// DevicePairApprove approves a pending pairing request (device.pair.approve).
// Approving grants exactly the role/scopes the request asked for.
func (c *Client) DevicePairApprove(ctx context.Context, requestID string) error {
	_, err := c.Call(ctx, "device.pair.approve", devicePairApproveParams{RequestID: requestID})
	return err
}

// CreateSession ensures a conversation session exists and returns the mutable
// state needed to avoid redundant settings patches. On OpenClaw v2026.8.2,
// sessions.create adopts an existing key instead of returning an exists error.
func (c *Client) CreateSession(ctx context.Context, sessionKey string) (SessionState, error) {
	raw, err := c.Call(ctx, "sessions.create", map[string]any{"key": sessionKey})
	if err != nil {
		return SessionState{}, fmt.Errorf("sessions.create %q: %w", sessionKey, err)
	}
	var out struct {
		Entry struct {
			ProviderOverride string `json:"providerOverride"`
			ModelOverride    string `json:"modelOverride"`
			PermissionMode   string `json:"permissionMode"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return SessionState{}, fmt.Errorf("decode sessions.create %q: %w", sessionKey, err)
	}
	model := out.Entry.ModelOverride
	if out.Entry.ProviderOverride != "" {
		model = out.Entry.ProviderOverride + "/" + model
	}
	return SessionState{Model: model, PermissionMode: out.Entry.PermissionMode}, nil
}

// EnsureSessionGuarded makes the session guarded, creating it if absent. A
// patch failure is only retried as a create when it looks like the session is
// missing; other errors propagate.
func (c *Client) EnsureSessionGuarded(ctx context.Context, key string) error {
	err := c.PatchSessionGuarded(ctx, key)
	if err == nil {
		return nil
	}
	if re, ok := err.(*rpcError); ok && re.Code == "FORBIDDEN" {
		return err // permission problem -- do not paper over it with a create
	}
	// INVALID_REQUEST (or an unavailable/missing session) -- try create once.
	if cerr := c.CreateSessionGuarded(ctx, key); cerr != nil {
		return fmt.Errorf("patch guarded %q: %v; create guarded: %w", key, err, cerr)
	}
	return nil
}

// SubscribeSessionMessages opts this connection into the live message stream
// of one session (sessions.messages.subscribe). While subscribed, the gateway
// pushes agent / chat / session.message events for that session onto this
// connection (issue #130: live tool activity for the chat SSE).
func (c *Client) SubscribeSessionMessages(ctx context.Context, sessionKey string) error {
	_, err := c.Call(ctx, "sessions.messages.subscribe", map[string]any{"key": sessionKey})
	if err != nil {
		return fmt.Errorf("sessions.messages.subscribe %q: %w", sessionKey, err)
	}
	return nil
}

// UnsubscribeSessionMessages stops the live message stream for one session
// (sessions.messages.unsubscribe).
func (c *Client) UnsubscribeSessionMessages(ctx context.Context, sessionKey string) error {
	_, err := c.Call(ctx, "sessions.messages.unsubscribe", map[string]any{"key": sessionKey})
	if err != nil {
		return fmt.Errorf("sessions.messages.unsubscribe %q: %w", sessionKey, err)
	}
	return nil
}

// SendSessionMessage sends a user message into a session (sessions.send) and
// returns once the gateway ACKs the run start. The ACK carries the run id
// (chat.send responds {status:"started", runId} before the run finishes); the
// run's live events arrive on the subscribed session-message stream while it
// runs, and AgentWait blocks until the run is terminal. runID is empty when the
// gateway did not echo one (older/failure path).
func (c *Client) SendSessionMessage(ctx context.Context, sessionKey, message, idempotencyKey string) (runID string, err error) {
	params := map[string]any{"key": sessionKey, "message": message}
	if idempotencyKey != "" {
		params["idempotencyKey"] = idempotencyKey
	}
	raw, err := c.Call(ctx, "sessions.send", params)
	if err != nil {
		return "", fmt.Errorf("sessions.send %q: %w", sessionKey, err)
	}
	var ack struct {
		Status string `json:"status"`
		RunID  string `json:"runId"`
	}
	// The ACK payload may be absent on some error shapes; ignore decode failure
	// and keep going (the caller can still complete via terminal events).
	_ = json.Unmarshal(raw, &ack)
	return ack.RunID, nil
}

// agentWaitTimeoutMs bounds each agent.wait RPC. The gateway's wait has its own
// 30s default that returns a "timeout" status while the run is still going;
// we loop until a terminal status instead of trusting a single RPC.
const agentWaitTimeoutMs = 25000

// agentWaitPollInterval is the backoff between non-terminal agent.wait retries
// (a run still in flight, or a turn queued behind another).
const agentWaitPollInterval = 400 * time.Millisecond

// AgentWait blocks on the same connection until the run identified by runID is
// terminal, then returns (agent.wait is the authoritative run completion RPC;
// chat.send only ACKs start). agent.wait may return an RPC-ok payload whose
// status is still "timeout" while the run continues (its own wait default is
// 30s); this decodes the payload and keeps waiting until "ok" / "error" or the
// context is done, so a slow first token or a long tool chain does not make the
// caller conclude the turn is over and unsubscribe early.
func (c *Client) AgentWait(ctx context.Context, runID string) error {
	if runID == "" {
		return fmt.Errorf("agent.wait: empty run id")
	}
	for {
		raw, err := c.Call(ctx, "agent.wait", map[string]any{"runId": runID, "timeoutMs": agentWaitTimeoutMs})
		if err != nil {
			return fmt.Errorf("agent.wait %q: %w", runID, err)
		}
		var res struct {
			Status       string `json:"status"`
			Error        string `json:"error"`
			TimeoutPhase string `json:"timeoutPhase"`
		}
		if err := json.Unmarshal(raw, &res); err != nil || res.Status == "" {
			// Fail closed on a malformed/unknown payload: never treat an
			// ambiguous wait result as a finished run.
			return fmt.Errorf("agent.wait %q: malformed response (status empty)", runID)
		}
		switch res.Status {
		case "ok":
			return nil
		case "error":
			msg := res.Error
			if msg == "" {
				msg = "agent run failed"
			}
			return fmt.Errorf("%s", msg)
		case "timeout":
			// OpenClaw uses status "timeout" for a bounded wait deadline (run
			// still in flight) and for a genuinely terminal timeout. The two
			// cannot be told apart by payload metadata on the real gateway (a
			// wait deadline also stamps endedAt/timeoutPhase), so a timeout only
			// fails the turn when it carries an explicit error; otherwise it is a
			// deadline and we keep waiting with backoff.
			if res.Error != "" {
				return fmt.Errorf("%s", res.Error)
			}
		case "pending":
			// queued, not yet started -- wait briefly
		default:
			return fmt.Errorf("agent.wait %q: unexpected status %q", runID, res.Status)
		}
		// Non-terminal: bounded backoff, still cancellable.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(agentWaitPollInterval):
		}
	}
}
