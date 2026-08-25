// API types -- mirror the CubePilot REST/SSE contract
// (internal/server/handlers*.go).

export interface SessionInfo {
  sessionKey: string
  title?: string
}

export interface HistoryMessage {
  role: 'user' | 'assistant' | 'toolResult'
  content: Array<{
    type: 'text' | 'toolCall'
    text?: string
    name?: string
    id?: string
    arguments?: unknown
  }>
}

export interface Task {
  id: string
  name: string
  prompt: string
  schedule: string
  state: 'Enabled' | 'Paused'
  enabled: boolean
  creator: string
  createdAt: string
  lastRunAt?: string
  lastStatus?: string
  nextRunAt?: string
}

export interface Report {
  id: string
  taskId: string
  taskName: string
  trigger: 'Cron' | 'Manual' | 'Inspect'
  status: 'success' | 'failed'
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

export type SSEEvent =
  | SSEMessageStart
  | SSEAgentThinking
  | SSEToolCall
  | SSEToolResult
  | SSEMessageDelta
  | SSEMessageDone