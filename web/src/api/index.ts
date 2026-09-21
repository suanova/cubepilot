// API service layer -- one function per backend endpoint.
import { apiFetch } from './client'
import type {
  AgentConfig,
  AgentApprovalView,
  AgentStatus,
  AllowlistRule,
  AuditEntry,
  HistoryMessage,
  PendingApproval,
  PendingQuestion,
  PlatformObject,
  Report,
  SessionInfo,
  Task,
  TaskTemplate,
} from './types'

// wireRule reduces a rule to the two fields the server knows before it goes on
// the wire. The server decodes request bodies strictly -- an unknown field is a
// 400 for the whole request -- and its rule type is only { pattern, argPattern },
// while a rule read from the approval view also carries label / source /
// command. (issue #185)
const wireRule = (r: AllowlistRule): AllowlistRule => ({ pattern: r.pattern, argPattern: r.argPattern })

// sessionPath builds a per-session subresource URL. Every session route goes
// through it so the key is encoded exactly once and exactly the same way: the
// key contains colons and may contain slashes, and a route that encoded it
// differently from its siblings would address a different conversation.
export const sessionPath = (sessionKey: string, sub: string): string =>
  `/api/v1/sessions/${encodeURIComponent(sessionKey)}${sub}`

export const api = {
  // Chat / sessions
  listSessions: () =>
    apiFetch<{ sessions: SessionInfo[] }>('/api/v1/sessions').then((d) => d.sessions),
  sessionHistory: (sessionKey: string) =>
    apiFetch<{ items: HistoryMessage[] }>(sessionPath(sessionKey, '/messages')).then((d) => d.items),

  // Stops the session's running turn. The server does not answer until the turn
  // has settled, so a send issued after this resolves cannot be rejected as a
  // concurrent turn.
  abortSession: (sessionKey: string) =>
    apiFetch<{ ok: boolean }>(sessionPath(sessionKey, '/abort'), { method: 'POST' }),

  // Whether the session still has a turn in flight -- used after a reload, when
  // this tab has no stream to tell it.
  sessionTurn: (sessionKey: string) => apiFetch<{ active: boolean }>(sessionPath(sessionKey, '/turn')),

  // HITL write approvals (issue #20 / #116 / #226). A decision names the
  // approval it settles: a session can hold several pending approvals at once,
  // so the id is what makes the answer land on the card the user clicked. It is
  // the gateway's approval id, and the endpoint keeps checking it against the
  // caller, the canonical session key and the still-pending state together.
  postApproval: (
    sessionKey: string,
    approvalId: string,
    decision: 'approve' | 'reject' | 'allow-always',
  ) =>
    apiFetch<{ approved: boolean; decision: string; approvalId?: string; allowlisted?: boolean }>(
      sessionPath(sessionKey, '/approvals/decision'),
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ approvalId, decision }),
      },
    ),
  // The session's whole pending set, oldest first, as a collection read: a
  // session with nothing pending is an empty array, not a 404. A failure here
  // is the gateway being unreachable, which is a different answer and must not
  // be read as "nothing is pending".
  pendingApprovals: (sessionKey: string) =>
    apiFetch<{ approvals: PendingApproval[] }>(sessionPath(sessionKey, '/approvals')).then(
      (d) => d.approvals ?? [],
    ),

  // Ask-user questions (issue #161): answer or dismiss a question the agent is
  // blocked on, and restore the card after a reload.
  //
  // `id` is the QUESTION RECORD id -- the one the pending list hands out and the
  // one the SSE event carries as callId. It is not the per-question
  // `questionId` inside that record, which is only ever the key of an entry in
  // `answers`. Sending the inner one is a 404 for a question that is on screen.
  postQuestion: (sessionKey: string, id: string, answers: Record<string, string[]>) =>
    apiFetch<{ questionId: string; cancelled: boolean }>(sessionPath(sessionKey, '/questions/answer'), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id, answers }),
    }),
  postQuestionCancel: (sessionKey: string, id: string) =>
    apiFetch<{ questionId: string; cancelled: boolean }>(sessionPath(sessionKey, '/questions/cancel'), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id }),
    }),
  pendingQuestions: (sessionKey: string) =>
    apiFetch<{ questions: PendingQuestion[] }>(sessionPath(sessionKey, '/questions')).then(
      (d) => d.questions ?? [],
    ),

  // Tasks (FR-M4)
  listTasks: () => apiFetch<{ tasks: Task[] }>('/api/v1/tasks').then((d) => d.tasks),
  listTaskTemplates: () =>
    apiFetch<{ taskTemplates: TaskTemplate[] }>('/api/v1/tasktemplates').then((d) => d.taskTemplates),
  createTask: (body: {
    name: string
    instruction?: string
    cron: string
    templateRef?: string
    params?: Record<string, string>
    state?: 'Enabled' | 'Paused'
  }) =>
    apiFetch<{ task: Task }>('/api/v1/tasks', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }).then((d) => d.task),
  deleteTask: (id: string) => apiFetch<{ deleted: string }>(`/api/v1/tasks/${id}`, { method: 'DELETE' }),
  runTask: (id: string) =>
    apiFetch<{ started: boolean }>(`/api/v1/tasks/${id}/run`, { method: 'POST' }),
  toggleTask: (id: string) =>
    apiFetch<{ task: Task }>(`/api/v1/tasks/${id}/toggle`, { method: 'POST' }).then((d) => d.task),
  taskReports: (id: string) =>
    apiFetch<{ reports: Report[] }>(`/api/v1/tasks/${id}/reports`).then((d) => d.reports),

  // Audit (M5)
  listAudit: (limit = 400) =>
    apiFetch<{ entries: AuditEntry[] | null }>(`/api/v1/audit?limit=${limit}`).then((d) => d.entries ?? []),

  // Agent config (per-user instance CR: selectedModel + userInstructions). The
  // payload is flat on both the response and the PUT body.
  agentConfig: () => apiFetch<AgentConfig>('/api/v1/agent/config'),
  saveAgentConfig: (config: { selectedModel?: string; userInstructions?: string }) =>
    apiFetch<AgentConfig>('/api/v1/agent/config', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(config),
    }),
  agentStatus: () => apiFetch<AgentStatus>('/api/v1/agent/status'),

  // Approvals (issue #116): effective + owned approval policy / allowlist.
  agentApproval: () => apiFetch<AgentApprovalView>('/api/v1/agent/approval'),
  saveAgentApproval: (body: { approvalPolicy?: string; allowlist?: AllowlistRule[]; revokeGrants?: AllowlistRule[] }) =>
    apiFetch<AgentApprovalView>('/api/v1/agent/approval', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        ...body,
        allowlist: (body.allowlist ?? []).map(wireRule),
        revokeGrants: body.revokeGrants?.map(wireRule),
      }),
    }),

  // Platform objects (read-only CRD views)
  listAgentTemplates: () => apiFetch<{ agentTemplates: PlatformObject[] }>('/api/v1/agenttemplates').then((d) => d.agentTemplates),
  // The catalog: add a provider, or edit/remove one that already exists. The
  // name is the provider's identity everywhere downstream (the gateway provider
  // key, the prefix of every model ref, the credential Secret), so it is
  // immutable and there is no rename -- a rename is a delete plus an add. models
  // is required and non-empty: it is the list of backend model ids the endpoint
  // serves, and a provider that lists none renders nothing and cannot be
  // selected. A provider with no credential must say so explicitly (public), or
  // the server rejects it: a keyless provider fails every turn.
  addLLM: (body: { name: string; endpoint: string; apiKey?: string; public?: boolean; models: string[] }) =>
    apiFetch<{ provider: PlatformObject }>('/api/v1/llms', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }).then((d) => d.provider),
  // A PUT carries the full model list -- it replaces the stored one, so adding
  // and removing an id are this same request. An empty apiKey keeps the stored
  // credential.
  updateLLM: (name: string, body: { endpoint: string; apiKey?: string; public?: boolean; models: string[] }) =>
    apiFetch<{ provider: PlatformObject; warning?: string }>(`/api/v1/llms/${encodeURIComponent(name)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }),
  deleteLLM: (name: string) =>
    apiFetch<{ removed: string; warning?: string }>(`/api/v1/llms/${encodeURIComponent(name)}`, {
      method: 'DELETE',
    }),
  listInstances: () =>
    apiFetch<{ instances: PlatformObject[] }>('/api/v1/instances').then((d) => d.instances),
  createInstance: (body: { templateRef?: string; selectedModel?: string; enabledSkills?: string[]; userInstructions?: string }) =>
    apiFetch<{ instance: PlatformObject }>('/api/v1/instances', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }).then((d) => d.instance),
  listSkills: () =>
    apiFetch<{ skills: PlatformObject[] }>('/api/v1/skills').then((d) => d.skills),
  listTaskRuns: () =>
    apiFetch<{ taskruns: PlatformObject[] }>('/api/v1/taskruns').then((d) => d.taskruns),
  publishSkill: (name: string, opts: { displayName: string; description?: string }, tar: Uint8Array<ArrayBuffer>) =>
    apiFetch<{ skill: PlatformObject }>(
      `/api/v1/skills/${encodeURIComponent(name)}/publish?displayName=${encodeURIComponent(opts.displayName)}${opts.description ? `&description=${encodeURIComponent(opts.description)}` : ''}`,
      { method: 'POST', headers: { 'Content-Type': 'application/gzip' }, body: tar },
    ).then((d) => d.skill),
  installSkill: (name: string) =>
    apiFetch<{ enabledSkills: string[] }>(`/api/v1/skills/${encodeURIComponent(name)}/install`, { method: 'PUT' })
      .then((d) => d.enabledSkills),
  uninstallSkill: (name: string) =>
    apiFetch<{ enabledSkills: string[] }>(`/api/v1/skills/${encodeURIComponent(name)}/uninstall`, { method: 'PUT' })
      .then((d) => d.enabledSkills),
}