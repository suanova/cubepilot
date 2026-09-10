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

// reason returns the structured detail.reason of a gateway error, or "" when
// the frame carries no usable details.
func (e *frameError) reason() string {
	if len(e.Details) == 0 {
		return ""
	}
	var d struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(e.Details, &d); err != nil {
		return ""
	}
	return d.Reason
}

// rpcError is a failed method call surfaced to callers. Reason carries the
// gateway's structured detail.reason when it sends one (e.g. QUESTION_NOT_FOUND
// / QUESTION_ALREADY_TERMINAL), which callers map onto HTTP statuses.
type rpcError struct {
	Code    string
	Message string
	Reason  string
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
		Command     string `json:"command"`
		SessionKey  string `json:"sessionKey"`
		AgentID     string `json:"agentId"`
		Security    string `json:"security"`
		Ask         string `json:"ask"`
		WarningText string `json:"warningText"`
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

// --- question.requested / resolved event payloads ---

// QuestionOption is one selectable answer of a question.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Question is one question of a record. The variants this platform does not
// project (isSecret / secretStore) are decoded rather than dropped so an
// unsupported record can be filtered explicitly instead of silently losing the
// fact that the question was not an ordinary choice prompt. isOther is decoded
// for completeness but is never a reason to drop: ask_user sets it on every
// question it emits, to declare that free text is offered alongside the options.
type Question struct {
	QuestionID  string           `json:"questionId"`
	Header      string           `json:"header"`
	Question    string           `json:"question"`
	Options     []QuestionOption `json:"options"`
	MultiSelect bool             `json:"multiSelect,omitempty"`
	IsOther     bool             `json:"isOther,omitempty"`
	IsSecret    bool             `json:"isSecret,omitempty"`
	SecretStore json.RawMessage  `json:"secretStore,omitempty"`
}

// QuestionRecord is the question.requested broadcast payload and the payload
// shape shared by question.get / question.list. Unlike the other consumers of
// an approval-style record this one is routed by SessionKey, which the gateway
// supplies here.
type QuestionRecord struct {
	ID          string     `json:"id"`
	Questions   []Question `json:"questions"`
	AgentID     string     `json:"agentId,omitempty"`
	SessionKey  string     `json:"sessionKey,omitempty"`
	RunID       string     `json:"runId,omitempty"`
	CreatedAtMs int64      `json:"createdAtMs"`
	ExpiresAtMs int64      `json:"expiresAtMs"`
	Status      string     `json:"status"`
}

// QuestionResolved is the question.resolved broadcast payload. It is NOT a
// QuestionRecord: the gateway's QuestionResolvedEventSchema is a closed union
// of answered / cancelled / expired and carries no sessionKey, so the resolve
// path routes through the platform's own id -> sessionKey table.
type QuestionResolved struct {
	ID     string `json:"id"`
	Status string `json:"status"` // answered | cancelled | expired
}

// QuestionListResult is the question.list response payload.
type QuestionListResult struct {
	Questions []QuestionRecord `json:"questions"`
}

// questionGetResult is the question.get response payload.
type questionGetResult struct {
	Question QuestionRecord `json:"question"`
}

// --- question.resolve params ---

// questionAnswers is the answer map keyed by questionId: {answers:{qid:[labels]}}.
type questionAnswers struct {
	Answers map[string][]string `json:"answers"`
}

// questionResolveParams answers a question. Cancel=true dismisses it instead;
// the two shapes are distinct variants of the gateway schema, so only one is
// ever populated.
type questionResolveParams struct {
	ID         string           `json:"id"`
	Answers    *questionAnswers `json:"answers,omitempty"`
	Cancel     bool             `json:"cancel,omitempty"`
	ResolvedBy string           `json:"resolvedBy,omitempty"`
}

// --- device.pair.list / approve payloads ---

// DevicePairingPending is one pending pairing request (device.pair.list).
type DevicePairingPending struct {
	RequestID   string   `json:"requestId"`
	DeviceID    string   `json:"deviceId"`
	PublicKey   string   `json:"publicKey"`
	Role        string   `json:"role"`
	Roles       []string `json:"roles"`
	Scopes      []string `json:"scopes"`
	DisplayName string   `json:"displayName"`
	Platform    string   `json:"platform"`
	TS          int64    `json:"ts"`
}

// PairedDevice is one paired device (device.pair.list; token fields redacted).
type PairedDevice struct {
	DeviceID  string `json:"deviceId"`
	PublicKey string `json:"publicKey"`
}

// DevicePairListResult is the device.pair.list response payload.
type DevicePairListResult struct {
	Pending []DevicePairingPending `json:"pending"`
	Paired  []PairedDevice         `json:"paired"`
}

type devicePairApproveParams struct {
	RequestID string `json:"requestId"`
}

// --- sessions.patch / sessions.create params ---

type sessionPatchParams struct {
	Key            string   `json:"key"`
	PermissionMode **string `json:"permissionMode,omitempty"`
	Model          **string `json:"model,omitempty"`
}

type sessionCreateParams struct {
	Key            string `json:"key"`
	PermissionMode string `json:"permissionMode"`
}

// SessionState is the mutable session state needed to reconcile a turn before
// it starts.
type SessionState struct {
	Model          string
	PermissionMode string
}

// OptionalString represents one optional sessions.patch field. Set=false
// omits the field; Set=true with an empty Value sends JSON null to clear it.
type OptionalString struct {
	Set   bool
	Value string
}

// SessionSettingsPatch carries the model and permission changes for one
// atomic sessions.patch call.
type SessionSettingsPatch struct {
	Model          OptionalString
	PermissionMode OptionalString
}
