// API types -- mirror the CubePilot REST/SSE contract
// (internal/server/handlers*.go).

export interface SessionInfo {
  sessionKey: string
  title?: string
}

// HistoryContentBlock is one content block of a history message. The gateway
// serves user-role messages as a plain string and assistant/toolResult messages
// as an array of these blocks (issue #104).
export interface HistoryContentBlock {
  type: 'text' | 'toolCall'
  text?: string
  name?: string
  id?: string
  arguments?: unknown
}

export interface HistoryMessage {
  role: 'user' | 'assistant' | 'toolResult'
  content: string | HistoryContentBlock[]
}

export interface Task {
  id: string
  name: string
  instruction: string
  cron: string
  templateRef?: string // bound TaskTemplate name; undefined/'' = free-form
  // state is the only enablement field: an "enabled" boolean alongside it would
  // be a second source of truth for the same fact.
  state: 'Enabled' | 'Paused'
  creator: string
  createdAt: string
  lastRunAt?: string
  lastStatus?: string
  // Most recent TaskRun name; absent for a task that has never run.
  lastRunId?: string
  nextRunAt?: string
}

// A TaskTemplate as served by GET /api/v1/tasktemplates (raw CR: metadata.name +
// spec). Mirrors the Go TaskTemplateSpec json tags.
export interface TaskParamSchema {
  name: string
  type?: string
  default?: string
  enum?: string[]
}

export interface TaskTemplate {
  kind?: string
  apiVersion?: string
  metadata: {
    name: string
    creationTimestamp?: string
  }
  spec?: {
    displayName?: string
    description?: string
    instruction?: string
    paramsSchema?: TaskParamSchema[]
    defaultCron?: string
    skills?: string[]
  }
}

export interface Report {
  id: string
  taskId: string
  taskName: string
  trigger: 'Cron' | 'Manual' | 'Inspect'
  // status also carries 'running' while the TaskRun is queued/executing
  // (issue #95); success/failed only once the scheduler finishes the run.
  status: 'success' | 'failed' | 'running'
  startedAt: string
  finishedAt: string
  content: string
}

export interface AuditEntry {
  id: string
  ts: string
  user: string
  sessionId: string
  tool: string
  command: string
  level: 'L0' | 'L1'
  status: 'executed' | 'failed'
  detail?: string
}

// Agent config served by GET /api/v1/agent/config: the caller's own selections,
// which live on the AgentInstance CR (design §3.2). The payload is flat -- it is
// two fields of the instance, not an object the platform calls "config"; the
// field names are the CRD's. exists = the instance is provisioned;
// selectedModel "" = "Runtime Default" (clear the override); userInstructions
// "" = template instructions only.
export interface AgentConfig {
  exists: boolean
  selectedModel: string
  userInstructions: string
}

export interface AgentStatus {
  exists: boolean
  id?: string
  phase?: string
  startedAt?: string
  uptimeSeconds?: number
  gatewayImage?: string
  gatewayPort?: number
  user: string
}

export interface PlatformObject {
  kind: string
  apiVersion: string
  metadata: {
    name: string
    uid?: string
    creationTimestamp?: string
    labels?: Record<string, string>
  }
  spec?: Record<string, unknown>
  status?: Record<string, unknown>
}

// SSE events from /api/v1/messages
export interface SSEMessageStart {
  type: 'message_start'
  sessionId: string
}
export interface SSEAgentThinking {
  type: 'agent_thinking'
  sessionId: string
}
export interface SSEToolCall {
  type: 'tool_call'
  sessionId: string
  name: string
  callId?: string
  arguments: string
}
export interface SSEToolResult {
  type: 'tool_result'
  sessionId: string
  callId?: string
  name?: string
  output: string
}
export interface SSEMessageDelta {
  type: 'message_delta'
  sessionId: string
  delta: string
}
// The gateway rewrote the assistant text (replace:true) -- replace the bubble
// text with delta instead of appending.
export interface SSETextReplace {
  type: 'text_replace'
  sessionId: string
  delta: string
}
export interface SSEMessageDone {
  type: 'message_done'
  sessionId: string
  error?: string
  // The user stopped this turn (chat.abort or /stop). Mutually exclusive with
  // error: a stopped turn is neither a failure nor a normal completion, and its
  // partial text must not be presented as a finished answer.
  stopped?: boolean
  // This terminal was NOT emitted by the server: sse.ts synthesized it because
  // the stream ended -- or never opened -- without one (transport failure,
  // truncated response). It reports that observation was lost, not that the
  // turn ended: the gateway run may still be executing, with its approval or
  // question still live and answerable. Absence always means a real server
  // terminal, so an older server that never sets the marker keeps reading as
  // one.
  synthetic?: boolean
}
// HITL (issue #20): a matched write paused for the human. callId is the gateway
// approval id; name/command/level/message describe the gated operation.
export interface SSEApprovalPending {
  type: 'approval_pending'
  sessionId: string
  callId?: string
  name?: string
  command?: string
  level?: 'read' | 'write'
  message?: string
}
export interface SSEApprovalResolved {
  type: 'approval_resolved'
  sessionId: string
  callId?: string
  approved?: boolean
}

// Ask-user (issue #161): the agent's ask_user tool is blocked on a human
// answer. callId is the gateway question id the answer must be submitted with.
export interface QuestionOption {
  label: string
  description?: string
}

export interface QuestionItem {
  questionId: string
  header: string
  question: string
  options: QuestionOption[]
  multiSelect?: boolean
}

export interface QuestionPrompt {
  questions: QuestionItem[]
  // Remaining time when the event was produced (not an absolute deadline), so
  // the countdown does not depend on this machine's clock matching the API's.
  timeoutSeconds?: number
}

export interface SSEQuestionPending {
  type: 'question_pending'
  sessionId: string
  callId?: string
  question?: QuestionPrompt
}

export interface SSEQuestionResolved {
  type: 'question_resolved'
  sessionId: string
  callId?: string
  // Terminal status: answered | cancelled | expired.
  message?: string
}

export type SSEEvent =
  | SSEMessageStart
  | SSEAgentThinking
  | SSEToolCall
  | SSEToolResult
  | SSEMessageDelta
  | SSETextReplace
  | SSEMessageDone
  | SSEApprovalPending
  | SSEApprovalResolved
  | SSEQuestionPending
  | SSEQuestionResolved

// A question awaiting an answer, served by GET
// /api/v1/sessions/{key}/question/pending (used to restore a question card
// after a reload). Mirrors the question_pending event payload.
export interface PendingQuestion {
  id: string
  questions: QuestionItem[]
  timeoutSeconds?: number
}

// A write awaiting a decision, served by GET
// /api/v1/sessions/{key}/approval/pending (used to restore an approval card
// after a reload mid-approval).
export interface PendingApproval {
  sessionId: string
  approvalId: string
  tool: string
  command: string
  level: 'read' | 'write'
  message?: string
}

// Approvals (issue #116)
export interface AllowlistRule {
  pattern: string
  argPattern?: string
  // Server-provided human meaning; set ONLY for platform builtin read-only
  // rules. Absent for user/template rules (which may allow writes) so the UI
  // never presents them as read-only.
  label?: string
  // Where the rule came from (issue #185). 'learned' rules are recorded from an
  // allow-always in chat; the rest are declarative.
  source?: 'builtin' | 'template' | 'user' | 'learned'
  // The invocation the user approved. Set for learned rules only; the server
  // omits it otherwise.
  command?: string
}
export interface AgentApprovalView {
  exists: boolean
  // Effective values (what the runtime enforces).
  approvalPolicy: string
  allowlist: AllowlistRule[]
  // Instance's own state ('' / [] = inheriting the template default live).
  override: string
  allowlistOwned: AllowlistRule[]
  // Learned grants are absent when empty -- the server tags the field
  // `omitempty` -- so optional, like the value the normalizer already guards.
  allowlistLearned?: AllowlistRule[]
  templatePolicy: string
  // Approval-channel state (issue #127): "up" | "pairing" | "down" |
  // "unconfigured". A gated policy (Allowlist / AlwaysAsk) is only enforced
  // while the channel is up; "down"/"unconfigured" means gated turns fail
  // closed until the channel recovers.
  channel: string
}
