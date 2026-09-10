package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
