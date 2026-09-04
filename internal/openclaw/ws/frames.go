package ws

import (
	"encoding/json"
)

// --- wire frames (frames.ts) ---

type requestFrame struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type responseFrame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload"`
	Error   *frameError     `json:"error"`
}

type frameError struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
}

type eventFrame struct {
	Type    string          `json:"type"`
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
	Seq     int             `json:"seq"`
}

// rpcError is a failed method call surfaced to callers.
type rpcError struct {
	Code    string
	Message string
}

func (e *rpcError) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

// --- connect (frames.ts ConnectParamsSchema / connect-hello.ts) ---

type clientInfo struct {
	ID          string `json:"id"`
	Mode        string `json:"mode"`
	DisplayName string `json:"displayName,omitempty"`
	Version     string `json:"version"`
	Platform    string `json:"platform"`
	InstanceID  string `json:"instanceId,omitempty"`
}

type deviceProof struct {
	ID        string `json:"id"`
	PublicKey string `json:"publicKey"`
	Signature string `json:"signature"`
	SignedAt  int64  `json:"signedAt"`
	Nonce     string `json:"nonce"`
}

type connectAuth struct {
	Token string `json:"token,omitempty"`
}

type connectParams struct {
	MinProtocol int          `json:"minProtocol"`
	MaxProtocol int          `json:"maxProtocol"`
	Client      clientInfo   `json:"client"`
	Caps        []string     `json:"caps"`
	Role        string       `json:"role"`
	Scopes      []string     `json:"scopes"`
	Device      *deviceProof `json:"device,omitempty"`
	Auth        *connectAuth `json:"auth,omitempty"`
}

// connectChallenge is the server's handshake event (the only event carrying a
// nonce).
type connectChallenge struct {
	Nonce string `json:"nonce"`
	TS    int64  `json:"ts"`
}

// helloOk is the connect response payload.
type helloOk struct {
	Type     string `json:"type"`
	Protocol int    `json:"protocol"`
	Auth     struct {
		Role        string   `json:"role"`
		Scopes      []string `json:"scopes"`
		DeviceToken string   `json:"deviceToken"`
	} `json:"auth"`
}

// --- exec.approvals.get/set payloads ---

// AllowlistEntry mirrors the persisted exec-approvals allowlist entry.
type AllowlistEntry struct {
	ID         string `json:"id,omitempty"`
	Pattern    string `json:"pattern"`
	ArgPattern string `json:"argPattern,omitempty"`
	Source     string `json:"source,omitempty"`
}

// approvalDefaults carries the security/ask fields shared by the file's
// defaults block and each agent entry.
type approvalDefaults struct {
	Security        string `json:"security,omitempty"`
	Ask             string `json:"ask,omitempty"`
	AskFallback     string `json:"askFallback,omitempty"`
	AutoAllowSkills *bool  `json:"autoAllowSkills,omitempty"`
}

// ApprovalAgentPolicy is one agent's block in the exec-approvals file.
type ApprovalAgentPolicy struct {
	approvalDefaults
	Allowlist []AllowlistEntry `json:"allowlist,omitempty"`
}

type approvalsSocket struct {
	Path  string `json:"path,omitempty"`
	Token string `json:"token,omitempty"`
}

// ApprovalsFile is the persisted exec-approvals policy file
// ({version:1, defaults?, agents:{...}}). Keep the full shape so a
// get→merge→set round trip preserves defaults, socket and other agents.
type ApprovalsFile struct {
	Version  int                            `json:"version"`
	Socket   *approvalsSocket               `json:"socket,omitempty"`
	Defaults *approvalDefaults              `json:"defaults,omitempty"`
	Agents   map[string]ApprovalAgentPolicy `json:"agents,omitempty"`
}

// ApprovalsSnapshot is the exec.approvals.get/set response payload.
type ApprovalsSnapshot struct {
	Exists bool          `json:"exists"`
	Hash   string        `json:"hash"`
	File   ApprovalsFile `json:"file"`
}

// --- exec.approval.requested / resolved event payloads ---

// ApprovalRequested is the exec.approval.requested broadcast payload.
type ApprovalRequested struct {
	Kind    string `json:"approvalKind"`
	ID      string `json:"id"`
	Request struct {
		Command    string `json:"command"`
		SessionKey string `json:"sessionKey"`
		AgentID    string `json:"agentId"`
		Security   string `json:"security"`
		Ask        string `json:"ask"`
	} `json:"request"`
	CreatedAtMs int64 `json:"createdAtMs"`
	ExpiresAtMs int64 `json:"expiresAtMs"`
}

// ApprovalResolved is the exec.approval.resolved broadcast payload.
type ApprovalResolved struct {
	ID         string `json:"id"`
	Decision   string `json:"decision"` // allow-once | allow-always | deny
	ResolvedBy string `json:"resolvedBy"`
	TS         int64  `json:"ts"`
}

// --- exec.approval.resolve params ---

type approvalResolveParams struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
}

// --- sessions.patch / sessions.create params ---

type sessionPatchParams struct {
	Key            string `json:"key"`
	PermissionMode string `json:"permissionMode"`
}

type sessionCreateParams struct {
	Key            string `json:"key"`
	PermissionMode string `json:"permissionMode"`
}
