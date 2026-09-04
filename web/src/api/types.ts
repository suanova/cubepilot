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
  prompt: string
  schedule: string
  templateRef?: string // bound TaskTemplate name; undefined/'' = free-form
  state: 'Enabled' | 'Paused'
  enabled: boolean
  creator: string
  createdAt: string
  lastRunAt?: string
  lastStatus?: string
  nextRunAt?: string
}

// A TaskTemplate as served by GET /api/tasktemplates (raw CR: metadata.name +
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
  p0: number
  p1: number
  p2: number
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

export interface AgentConfig {
  model?: string
  systemPrompt?: string
  skills?: Array<{ name: string; enabled: boolean }>
}

export interface AgentStatus {
  exists: boolean
  id?: string
  phase?: string
  startedAt?: string
  uptimeSeconds?: number
  gatewayImage?: string
  gatewayPort?: number
  idleTTLMinutes?: number
  idleTTLSeconds?: number
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

// SSE events from /api/messages
export interface SSEMessageStart {
  type: 'message_start'
  session_id: string
}
export interface SSEAgentThinking {
  type: 'agent_thinking'
  session_id: string
}
export interface SSEToolCall {
  type: 'tool_call'
  session_id: string
  name: string
  call_id?: string
  arguments: string
}
export interface SSEToolResult {
  type: 'tool_result'
  session_id: string
  call_id?: string
  name?: string
  output: string
}
export interface SSEMessageDelta {
  type: 'message_delta'
  session_id: string
  delta: string
}
export interface SSEMessageDone {
  type: 'message_done'
  session_id: string
  error?: string
}
// HITL (issue #20): a matched write paused for the human. call_id is the gateway
// approval id; name/command/level/message describe the gated operation.
export interface SSEConfirmPending {
  type: 'confirm_pending'
  session_id: string
  call_id?: string
  name?: string
  command?: string
  level?: 'read' | 'write'
  message?: string
}
export interface SSEConfirmResolved {
  type: 'confirm_resolved'
  session_id: string
  call_id?: string
  approved?: boolean
}

export type SSEEvent =
  | SSEMessageStart
  | SSEAgentThinking
  | SSEToolCall
  | SSEToolResult
  | SSEMessageDelta
  | SSEMessageDone
  | SSEConfirmPending
  | SSEConfirmResolved

// A write awaiting a decision, served by GET /api/sessions/{key}/confirm/pending
// (used to restore a confirmation card after a reload mid-approval).
export interface PendingConfirm {
  session_id: string
  approval_id: string
  tool: string
  command: string
  level: 'read' | 'write'
  message?: string
}