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
