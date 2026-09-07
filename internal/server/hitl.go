package server

import (
	"context"
	"crypto/ed25519"
	"crypto/sha512"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/suanova/cubepilot/internal/api/v1alpha1"
	"github.com/suanova/cubepilot/internal/instances"
	"github.com/suanova/cubepilot/internal/openclaw/ws"
)

// hitlPairRetryDelay is the pause between NOT_PAIRED connect retries while the
// in-pod supervisor approves the device pairing (overridable in tests).
var hitlPairRetryDelay = 1500 * time.Millisecond

// hitlGateway is the subset of the gateway-protocol WS client the HITL glue
// depends on, so tests can substitute a fake.
type hitlGateway interface {
	Connected() bool
	Connect(ctx context.Context) error
	OnApprovalRequested(f func(ws.ApprovalRequested))
	GetApprovalsPolicy(ctx context.Context) (*ws.ApprovalsSnapshot, error)
	SetApprovalsPolicy(ctx context.Context, file ws.ApprovalsFile, baseHash string) (*ws.ApprovalsSnapshot, error)
	EnsureSessionGuarded(ctx context.Context, key string) error
	ResolveApproval(ctx context.Context, id, decision string) error
	Close()
}

// hitlManager owns the per-user approval connections (issue #20). It is inert
// until the API is configured with a device master key (see ConfiguredHITL);
// with no device, confirmPolicy stays declarative and chat is unchanged.
type hitlManager struct {
	mgr       *instances.Manager
	token     string
	masterKey []byte
	newClient func(url string, dev *ws.Device) hitlGateway
	logf      func(format string, args ...any)

	// bridge is set by the server so a gateway approval can reach the
	// ApprovalService (which resolves Portal decisions and injects SSE).
	bridge func(user string, ev ws.ApprovalRequested)

	// resolved returns the user's confirm policy, effective allowlist and
	// config revision. Overridable in tests; the default reads the resolved
	// config via the instance manager.
	resolved func(ctx context.Context, user string) (v1alpha1.ConfirmPolicy, []v1alpha1.AllowlistRule, string, error)

	// wsURLOf returns the gateway WS endpoint for a user. Overridable in tests.
	wsURLOf func(user string) string

	mu         sync.Mutex
	conns      map[string]*userHitlConn
	connecting map[string]*sync.Mutex // serializes first connect per user
	revPol     map[string]string      // user -> applied policy revision
}

type userHitlConn struct {
	user string
	gw   hitlGateway
}

// ConfiguredHITL builds the manager, or returns nil (HITL disabled) when no
// master key is provided.
func ConfiguredHITL(mgr *instances.Manager, token string, masterKey []byte, logf func(string, ...any)) *hitlManager {
	if mgr == nil || len(masterKey) == 0 || token == "" {
		return nil
	}
	m := &hitlManager{
		mgr:        mgr,
		token:      token,
		masterKey:  masterKey,
		logf:       logf,
		conns:      map[string]*userHitlConn{},
		connecting: map[string]*sync.Mutex{},
		revPol:     map[string]string{},
	}
	m.newClient = func(url string, dev *ws.Device) hitlGateway {
		return ws.NewClient(url, token, dev)
	}
	m.resolved = func(ctx context.Context, user string) (v1alpha1.ConfirmPolicy, []v1alpha1.AllowlistRule, string, error) {
		cfg, err := m.mgr.ResolvedConfigForUser(ctx, user)
		if err != nil {
			return "", nil, "", err
		}
		if cfg == nil || cfg.Empty() {
			return "", nil, cfg.Revision, nil
		}
		return cfg.ConfirmPolicy, cfg.Allowlist, cfg.Revision, nil
	}
	m.wsURLOf = func(user string) string {
		return wsURL(m.mgr.BaseURL(user))
	}
	return m
}

// SetConnFactory overrides connection construction (tests).
func (m *hitlManager) SetConnFactory(f func(url string, dev *ws.Device) hitlGateway) {
	m.newClient = f
}

func (m *hitlManager) sayf(format string, args ...any) {
	if m.logf != nil {
		m.logf(format, args...)
	}
}

// deviceFor derives the deterministic per-user device identity from the master
// key (sha512(masterKey | user) as the Ed25519 seed).
func (m *hitlManager) deviceFor(user string) *ws.Device {
	sum := sha512.Sum512(append(append([]byte{}, m.masterKey...), []byte("|"+user)...))
	seed := sum[:ed25519.SeedSize]
	return mustDevice(ed25519.NewKeyFromSeed(seed))
}

// DevicePublicKeyFor returns the derived operator device public key for a user
// (served to the supervisor so it can approve this device's gateway pairing).
func (m *hitlManager) DevicePublicKeyFor(user string) string {
	return m.deviceFor(user).PublicKey
}

func mustDevice(priv ed25519.PrivateKey) *ws.Device {
	dev, err := ws.NewDevice(priv.Public().(ed25519.PublicKey), priv)
	if err != nil {
		panic(fmt.Sprintf("hitl device: %v", err)) // unreachable for a valid key
	}
	return dev
}

// wsURL derives the gateway-protocol endpoint from the instance's HTTP base
// URL (http://host:port -> ws://host:port/gateway).
func wsURL(baseURL string) string {
	return "ws" + strings.TrimPrefix(baseURL, "http") + "/gateway"
}

// conn returns (connecting on first use or after a drop) the user's gateway
// connection. First-connection per user is serialized so two concurrent turns
// cannot leave two live approval clients (duplicate event callbacks). A connect
// failure returns the stored client so later calls fail fast and can retry.
func (m *hitlManager) conn(ctx context.Context, user string) (hitlGateway, error) {
	m.mu.Lock()
	if c, ok := m.conns[user]; ok && c.gw.Connected() {
		gw := c.gw
		m.mu.Unlock()
		return gw, nil
	}
	if m.connecting == nil {
		m.connecting = map[string]*sync.Mutex{}
	}
	lm, ok := m.connecting[user]
	if !ok {
		lm = &sync.Mutex{}
		m.connecting[user] = lm
	}
	m.mu.Unlock()

	lm.Lock()
	defer lm.Unlock()

	// Re-check under the per-user lock: the first caller may have connected.
	m.mu.Lock()
	if c, ok := m.conns[user]; ok && c.gw.Connected() {
		gw := c.gw
		m.mu.Unlock()
		return gw, nil
	}
	m.mu.Unlock()

	dev := m.deviceFor(user)
	gw := m.newClient(m.wsURLOf(user), dev)
	gw.OnApprovalRequested(func(ev ws.ApprovalRequested) {
		if ev.Request.SessionKey == "" || ev.Request.Command == "" {
			return // cannot attribute to a conversation; leave to the gateway timeout
		}
		if m.bridge != nil {
			m.bridge(user, ev)
		}
	})

	m.mu.Lock()
	m.conns[user] = &userHitlConn{user: user, gw: gw}
	m.mu.Unlock()

	// First connect may be rejected NOT_PAIRED while the in-pod supervisor
	// approves this device (device.pair.approve on its next poll); retry briefly
	// so the first Allowlist turn can connect rather than fail.
	const maxPairAttempts = 4
	var connectErr error
	for attempt := 1; attempt <= maxPairAttempts; attempt++ {
		aCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := gw.Connect(aCtx)
		cancel()
		if err == nil {
			return gw, nil
		}
		connectErr = err
		if !strings.Contains(err.Error(), "NOT_PAIRED") || attempt == maxPairAttempts {
			break
		}
		select {
		case <-time.After(hitlPairRetryDelay):
		case <-ctx.Done():
			return gw, ctx.Err()
		}
	}
	return gw, fmt.Errorf("hitl connect %s: %w", user, connectErr)
}

// PreTurn is called at the start of an interactive turn. For Allowlist and
// AlwaysAsk it ensures the approval connection, applies the effective exec
// policy once per config revision, and guards the session.
//
// Allowlist keeps the issue #20 best-effort posture: on failure the turn
// proceeds ungated and audited (only a successfully guarded session pauses
// writes). AlwaysAsk is the strict posture and must NOT fail open -- if its
// policy or guard cannot be applied, PreTurn returns an error and the caller
// must not start the turn.
func (m *hitlManager) PreTurn(ctx context.Context, user, sessionKey string) error {
	pol, allow, rev, err := m.resolved(ctx, user)
	if err != nil {
		return nil
	}
	switch pol {
	case v1alpha1.ConfirmPolicyAllowlist, v1alpha1.ConfirmPolicyAlwaysAsk:
	default: // None / empty -> pass-through
		return nil
	}
	gw, err := m.conn(ctx, user)
	if err != nil {
		m.sayf("hitl %s: turn gating skipped (channel down): %v", user, err)
		if pol == v1alpha1.ConfirmPolicyAlwaysAsk {
			return fmt.Errorf("hitl %s: cannot gate AlwaysAsk turn: %w", user, err)
		}
		return nil
	}
	// Apply the effective exec policy when the resolved-config revision changed;
	// only a successful apply advances revPol so a transient failure is retried
	// next turn.
	m.mu.Lock()
	appliedRev := m.revPol[user]
	m.mu.Unlock()
	if rev != "" && rev != appliedRev {
		if err := m.applyPolicy(ctx, user, gw, pol, allow); err != nil {
			if pol == v1alpha1.ConfirmPolicyAlwaysAsk {
				return fmt.Errorf("hitl %s: cannot apply AlwaysAsk policy: %w", user, err)
			}
			// Allowlist: best-effort, leave the watermark so we retry next turn.
		} else {
			m.mu.Lock()
			m.revPol[user] = rev
			m.mu.Unlock()
		}
	}
	if err := gw.EnsureSessionGuarded(ctx, sessionKey); err != nil {
		m.sayf("hitl %s: guard session %s: %v", user, sessionKey, err)
		if pol == v1alpha1.ConfirmPolicyAlwaysAsk {
			return fmt.Errorf("hitl %s: cannot guard AlwaysAsk session %s: %w", user, sessionKey, err)
		}
	}
	return nil
}

// applyPolicy writes the effective exec-approvals policy into agents."main" of
// the gateway (get -> set, CAS). The allowlist is rewritten wholesale from the
// resolved config (issue #116): the platform bookkeeping is the instance
// allowlist, so a removed entry really disappears. AlwaysAsk runs a guarded,
// on-miss session with an empty allowlist -- every command misses and therefore
// asks -- which is the strictest posture and needs no unverified ask:always
// semantics. It reports failure so the caller can defer advancing the
// applied-revision watermark.
func (m *hitlManager) applyPolicy(ctx context.Context, user string, gw hitlGateway, pol v1alpha1.ConfirmPolicy, allow []v1alpha1.AllowlistRule) error {
	snap, err := gw.GetApprovalsPolicy(ctx)
	if err != nil {
		m.sayf("hitl %s: exec.approvals.get: %v", user, err)
		return err
	}
	file := snap.File
	if file.Agents == nil {
		file.Agents = map[string]ws.ApprovalAgentPolicy{}
	}
	agent := file.Agents["main"]
	switch pol {
	case v1alpha1.ConfirmPolicyAlwaysAsk:
		// Clear any allowlist a prior Allowlist mode wrote: on-miss with an
		// empty allowlist asks on everything.
		agent.Allowlist = nil
	default: // ConfirmPolicyAllowlist
		agent.Allowlist = toWSEntries(allow)
	}
	file.Agents["main"] = agent
	base := ""
	if snap.Exists {
		base = snap.Hash
	}
	if _, err := gw.SetApprovalsPolicy(ctx, file, base); err != nil {
		m.sayf("hitl %s: exec.approvals.set: %v", user, err)
		return err
	}
	return nil
}

// toWSEntries converts the resolved (public-API) allowlist rules into the
// gateway's exec-approvals entry shape.
func toWSEntries(rules []v1alpha1.AllowlistRule) []ws.AllowlistEntry {
	out := make([]ws.AllowlistEntry, 0, len(rules))
	for _, r := range rules {
		if r.Pattern == "" {
			continue
		}
		out = append(out, ws.AllowlistEntry{Pattern: r.Pattern, ArgPattern: r.ArgPattern})
	}
	return out
}

// ResolveApproval implements ApprovalResolver: the Portal decision is applied
// to the user's gateway connection.
func (m *hitlManager) ResolveApproval(ctx context.Context, user, approvalID, decision string) error {
	m.mu.Lock()
	c, ok := m.conns[user]
	m.mu.Unlock()
	if !ok || c == nil || c.gw == nil {
		return fmt.Errorf("no approval connection for %s", user)
	}
	gwDecision := "deny"
	if decision == "approve" {
		gwDecision = "allow-once"
	}
	return c.gw.ResolveApproval(ctx, approvalID, gwDecision)
}
