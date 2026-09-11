package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	challengeEvent        = "connect.challenge"
	execApprovalRequested = "exec.approval.requested"
	execApprovalResolved  = "exec.approval.resolved"
	questionRequested     = "question.requested"
	questionResolved      = "question.resolved"

	defaultClientVersion  = "cubepilot/2026.9"
	defaultClientPlatform = "linux"
)

// Client is one gateway-protocol WebSocket connection to a gateway, acting as a
// paired operator device (issue #20 HITL approvals/control channel).
type Client struct {
	url   string
	token string
	dev   *Device

	connMu    sync.Mutex
	conn      *websocket.Conn
	deviceTok string // deviceToken from hello; used on reconnect to skip re-signing
	connected bool

	// writeToken serialises frames written to conn. It is a cap-1 channel
	// rather than a sync.Mutex because acquisition is context-aware: a writer
	// wedged inside conn.Write holds the token, and a second writer must be
	// able to give up at its own deadline instead of parking on the mutex until
	// the first one returns -- a mutex has no way to be interrupted, so the
	// caller's context could not bound the wait at all. Acquire with
	// acquireWrite, release with releaseWrite.
	writeToken chan struct{}

	nextID atomic.Int64

	pendingMu sync.Mutex
	pending   map[string]chan responseFrame

	onRequested func(ApprovalRequested)
	onResolved  func(ApprovalResolved)
	// onQuestionRequested / onQuestionResolved handle the ask_user question
	// broadcasts (issue #161). They are separate from onEvent so a question is
	// never mistaken for live turn content.
	onQuestionRequested func(QuestionRecord)
	onQuestionResolved  func(QuestionResolved)
	// onEvent receives every non-approval event frame pushed by the gateway
	// (agent / chat / session.message live-stream events, heartbeats, ...).
	// Registered once per connection so callers can subscribe to session
	// message streams (issue #130) without losing earlier event kinds.
	onEvent func(evName string, payload []byte)

	closeOnce sync.Once
	done      chan struct{} // closed when the read pump exits

	// callFn overrides Call in tests; nil means use the real transport.
	callFn func(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// NewClient returns a Client for url (ws://host/gateway), the shared gateway
// token, and the operator device identity. dev may be nil for a device-less
// connection -- only meaningful over the gateway's loopback, where OpenClaw
// admits a gateway-client/backend shared-token connection with its declared
// scopes intact (used by the supervisor to bootstrap device pairing).
func NewClient(url, token string, dev *Device) *Client {
	return &Client{
		url:        url,
		token:      token,
		dev:        dev,
		pending:    map[string]chan responseFrame{},
		done:       make(chan struct{}),
		writeToken: make(chan struct{}, 1),
	}
}

// acquireWrite takes the connection's write token, or returns ctx.Err() when
// ctx ends first. That ordering is the point: the frame is not written when the
// context wins, so a caller whose deadline expires learns nothing was sent and
// does not have to treat "the RPC failed" and "the request may have been
// delivered" as the same thing. Callers must release with releaseWrite.
//
// A zero-value Client has no token (the method-builder fixtures construct one
// directly); there is no transport to serialise there, and Call returns before
// it would write.
func (c *Client) acquireWrite(ctx context.Context) error {
	if c.writeToken == nil {
		return nil
	}
	select {
	case c.writeToken <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseWrite returns the token taken by acquireWrite.
func (c *Client) releaseWrite() {
	if c.writeToken != nil {
		<-c.writeToken
	}
}

// OnApprovalRequested registers the handler for exec.approval.requested.
func (c *Client) OnApprovalRequested(f func(ApprovalRequested)) {
	c.onRequested = f
}

// OnApprovalResolved registers the handler for exec.approval.resolved.
func (c *Client) OnApprovalResolved(f func(ApprovalResolved)) {
	c.onResolved = f
}

// OnQuestionRequested registers the handler for question.requested.
func (c *Client) OnQuestionRequested(f func(QuestionRecord)) {
	c.onQuestionRequested = f
}

// OnQuestionResolved registers the handler for question.resolved.
func (c *Client) OnQuestionResolved(f func(QuestionResolved)) {
	c.onQuestionResolved = f
}

// OnEvent registers a callback for every non-approval event frame received on
// this connection (agent / chat / session.message live-stream events and
// informational pushes). payload is the raw event payload JSON.
func (c *Client) OnEvent(f func(evName string, payload []byte)) {
	c.onEvent = f
}

// Connected reports whether the connection is established.
func (c *Client) Connected() bool {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.connected
}

// Connect dials and completes the connect.challenge handshake, then starts the
// read pump. deviceToken (when present) is preferred over a fresh signature; a
// nil Device connects device-less (loopback bootstrap).
func (c *Client) Connect(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, c.url, &websocket.DialOptions{
		HTTPClient: &http.Client{Timeout: 0},
	})
	if err != nil {
		return fmt.Errorf("ws dial %s: %w", c.url, err)
	}

	// Read the connect.challenge event for its nonce.
	challenge, err := c.readChallenge(ctx, conn)
	if err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "challenge failed")
		return err
	}

	params, err := c.buildConnectParams(challenge.Nonce)
	if err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "sign failed")
		return err
	}

	// Send connect and read its response synchronously.
	hello, err := c.doConnect(ctx, conn, params)
	if err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, err.Error())
		return err
	}
	c.connMu.Lock()
	c.conn = conn
	c.connected = true
	if hello.Auth.DeviceToken != "" {
		c.deviceTok = hello.Auth.DeviceToken
	}
	c.connMu.Unlock()

	go c.readPump(conn)
	return nil
}

func (c *Client) readChallenge(ctx context.Context, conn *websocket.Conn) (connectChallenge, error) {
	var ch connectChallenge
	if err := c.readFrame(ctx, conn, func(ev eventFrame) (bool, error) {
		if ev.Event != challengeEvent {
			return false, fmt.Errorf("ws: expected connect.challenge, got event %q", ev.Event)
		}
		if err := json.Unmarshal(ev.Payload, &ch); err != nil {
			return false, fmt.Errorf("ws: decode connect.challenge: %w", err)
		}
		if ch.Nonce == "" {
			return false, fmt.Errorf("ws: connect.challenge carried no nonce")
		}
		return true, nil
	}); err != nil {
		return ch, err
	}
	return ch, nil
}

// readFrame reads websocket messages, skipping control/ignored frames, until the
// predicate returns true.
func (c *Client) readFrame(ctx context.Context, conn *websocket.Conn, accept func(eventFrame) (bool, error)) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("ws read: %w", err)
		}
		var ev eventFrame
		if err := json.Unmarshal(data, &ev); err != nil {
			return fmt.Errorf("ws decode frame: %w", err)
		}
		if ev.Type != "event" {
			// During the handshake nothing else is expected.
			continue
		}
		done, err := accept(ev)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// buildConnectParams assembles the connect request, signing the challenge
// nonce when no deviceToken is available yet.
func (c *Client) buildConnectParams(nonce string) (connectParams, error) {
	c.connMu.Lock()
	deviceTok := c.deviceTok
	c.connMu.Unlock()

	p := connectParams{
		MinProtocol: protocolVersion,
		MaxProtocol: protocolVersion,
		Client: clientInfo{
			ID:       "gateway-client",
			Mode:     "backend",
			Version:  defaultClientVersion,
			Platform: defaultClientPlatform,
		},
		Caps:   defaultCaps,
		Role:   "operator",
		Scopes: defaultScopes,
	}
	if deviceTok != "" {
		p.Auth = &connectAuth{Token: deviceTok}
		return p, nil
	}
	// Device-less connection (loopback bootstrap): shared token only, no proof.
	// Over the network the gateway clears the scopes of a device-less token
	// connection; over loopback it preserves them.
	if c.dev == nil {
		p.Auth = &connectAuth{Token: c.token}
		return p, nil
	}
	now := time.Now().UnixMilli()
	sig, err := c.dev.SignProof(signInput{
		DeviceID:     c.dev.ID,
		ClientID:     "gateway-client",
		ClientMode:   "backend",
		Role:         "operator",
		Scopes:       defaultScopes,
		SignedAtMs:   now,
		Token:        c.token,
		Nonce:        nonce,
		Platform:     defaultClientPlatform,
		DeviceFamily: "",
	})
	if err != nil {
		return p, err
	}
	p.Device = &deviceProof{
		ID:        c.dev.ID,
		PublicKey: c.dev.PublicKey,
		Signature: sig,
		SignedAt:  now,
		Nonce:     nonce,
	}
	p.Auth = &connectAuth{Token: c.token}
	return p, nil
}

func (c *Client) doConnect(ctx context.Context, conn *websocket.Conn, params connectParams) (helloOk, error) {
	var hello helloOk
	raw, _ := json.Marshal(params)
	if err := c.writeReq(ctx, conn, "connect", raw); err != nil {
		return hello, err
	}
	for {
		mt, data, err := conn.Read(ctx)
		if err != nil {
			return hello, fmt.Errorf("ws connect read: %w", err)
		}
		if mt != websocket.MessageText {
			continue
		}
		var res responseFrame
		if err := json.Unmarshal(data, &res); err != nil {
			return hello, fmt.Errorf("ws decode connect response: %w", err)
		}
		if res.Type != "res" {
			continue
		}
		if !res.OK {
			return hello, frameErrorOf(res)
		}
		if err := json.Unmarshal(res.Payload, &hello); err != nil {
			return hello, fmt.Errorf("ws decode hello-ok: %w", err)
		}
		return hello, nil
	}
}

func frameErrorOf(res responseFrame) error {
	if res.Error == nil {
		return fmt.Errorf("ws rpc failed")
	}
	return &rpcError{Code: res.Error.Code, Message: res.Error.Message, Reason: res.Error.reason()}
}

// writeReq sends one request frame over the given connection.
func (c *Client) writeReq(ctx context.Context, conn *websocket.Conn, method string, params json.RawMessage) error {
	id := "r" + strconv.FormatInt(c.nextID.Add(1), 10)
	frame := requestFrame{Type: "req", ID: id, Method: method}
	if len(params) > 0 && string(params) != "null" {
		frame.Params = params
	} else {
		frame.Params = json.RawMessage("{}")
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if err := c.acquireWrite(ctx); err != nil {
		return err
	}
	defer c.releaseWrite()
	return conn.Write(ctx, websocket.MessageText, raw)
}

// readPump dispatches responses and events until the connection dies.
func (c *Client) readPump(conn *websocket.Conn) {
	defer close(c.done)
	defer func() {
		c.connMu.Lock()
		c.connected = false
		c.connMu.Unlock()
		_ = conn.Close(websocket.StatusNormalClosure, "pump exit")
		c.failAllPending(fmt.Errorf("ws connection closed"))
	}()
	for {
		_, data, err := conn.Read(context.Background())
		if err != nil {
			return
		}
		var frame struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}
		switch frame.Type {
		case "res":
			var res responseFrame
			if err := json.Unmarshal(data, &res); err != nil {
				continue
			}
			c.deliver(res)
		case "event":
			var ev eventFrame
			if err := json.Unmarshal(data, &ev); err != nil {
				continue
			}
			c.dispatchEvent(ev)
		default:
			// unknown frame kind -- ignore
		}
	}
}

func (c *Client) deliver(res responseFrame) {
	c.pendingMu.Lock()
	ch, ok := c.pending[res.ID]
	if ok {
		delete(c.pending, res.ID)
	}
	c.pendingMu.Unlock()
	if ok {
		ch <- res
	}
}

func (c *Client) dispatchEvent(ev eventFrame) {
	switch ev.Event {
	case execApprovalRequested:
		if c.onRequested == nil {
			return
		}
		var req ApprovalRequested
		if err := json.Unmarshal(ev.Payload, &req); err == nil && req.ID != "" {
			c.onRequested(req)
		}
	case execApprovalResolved:
		if c.onResolved == nil {
			return
		}
		var res ApprovalResolved
		if err := json.Unmarshal(ev.Payload, &res); err == nil && res.ID != "" {
			c.onResolved(res)
		}
	case questionRequested:
		if c.onQuestionRequested == nil {
			return
		}
		var rec QuestionRecord
		if err := json.Unmarshal(ev.Payload, &rec); err == nil && rec.ID != "" {
			c.onQuestionRequested(rec)
		}
	case questionResolved:
		if c.onQuestionResolved == nil {
			return
		}
		var res QuestionResolved
		if err := json.Unmarshal(ev.Payload, &res); err == nil && res.ID != "" {
			c.onQuestionResolved(res)
		}
	default:
		// Live-stream pushes (agent / chat / session.message), heartbeats and
		// other informational events are surfaced to the registered observer so
		// the server can map live tool activity into the chat SSE (issue #130).
		if c.onEvent != nil {
			c.onEvent(ev.Event, ev.Payload)
		}
	}
}

// Call invokes a gateway method and returns its response payload.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.callFn != nil {
		return c.callFn(ctx, method, params)
	}
	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn == nil {
		return nil, fmt.Errorf("ws: not connected")
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	id := "m" + strconv.FormatInt(c.nextID.Add(1), 10)
	ch := make(chan responseFrame, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	frame := requestFrame{Type: "req", ID: id, Method: method, Params: raw}
	out, _ := json.Marshal(frame)
	// The token is taken under the caller's context, before the frame is
	// written: a writer wedged on the connection cannot hold this call past its
	// deadline, and when ctx wins nothing was sent (see acquireWrite).
	if werr := c.acquireWrite(ctx); werr != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("ws write %s: %w", method, werr)
	}
	err = conn.Write(ctx, websocket.MessageText, out)
	c.releaseWrite()
	if err != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, fmt.Errorf("ws write %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	case res := <-ch:
		if !res.OK {
			return nil, frameErrorOf(res)
		}
		return res.Payload, nil
	case <-c.done:
		return nil, fmt.Errorf("ws: connection closed")
	}
}

func (c *Client) failAllPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for id, ch := range c.pending {
		delete(c.pending, id)
		select {
		case ch <- responseFrame{Type: "res", ID: id, OK: false, Error: &frameError{Code: "UNAVAILABLE", Message: err.Error()}}:
		default:
		}
	}
}

// Done returns a channel closed when the connection is lost.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close terminates the connection.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.connMu.Lock()
		conn := c.conn
		c.connected = false
		c.connMu.Unlock()
		if conn != nil {
			_ = conn.Close(websocket.StatusNormalClosure, "client close")
		}
	})
}
