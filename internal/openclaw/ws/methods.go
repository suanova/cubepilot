package ws

import (
	"context"
	"encoding/json"
	"fmt"
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

// PatchSessionGuarded sets a session's permissionMode to guarded
// (sessions.patch). Requires the session to exist.
func (c *Client) PatchSessionGuarded(ctx context.Context, key string) error {
	_, err := c.Call(ctx, "sessions.patch", sessionPatchParams{Key: key, PermissionMode: "guarded"})
	return err
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

// CreateSession creates a session with the default (unguarded) permissions
// (sessions.create). Used to ensure a fresh conversation session exists before
// sending to it; guarded sessions are only applied by the HITL policy path
// (EnsureSessionGuarded), never for ordinary chat.
func (c *Client) CreateSession(ctx context.Context, sessionKey string) error {
	_, err := c.Call(ctx, "sessions.create", map[string]any{"key": sessionKey})
	if err != nil {
		return fmt.Errorf("sessions.create %q: %w", sessionKey, err)
	}
	return nil
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
func (c *Client) SendSessionMessage(ctx context.Context, sessionKey, message string) (runID string, err error) {
	raw, err := c.Call(ctx, "sessions.send", map[string]any{"key": sessionKey, "message": message})
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
const agentWaitTimeoutMs = 10000

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
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		// A successful RPC always carries a status; tolerate decode/older
		// gateways by treating an absent one as terminal-ok.
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil
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
		case "timeout", "pending", "":
			// run still in flight -- keep waiting on this runId
		default:
			// Unknown terminal-looking status; treat as done.
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}
