// API service layer -- one function per backend endpoint.
import { apiFetch } from './client'
import type {
  AgentConfig,
  AgentConfirmView,
  AgentStatus,
  AllowlistRule,
  AuditEntry,
  HistoryMessage,
  PendingConfirm,
  PendingQuestion,
  PlatformObject,
  Report,
  SessionInfo,
  Task,
  TaskTemplate,
} from './types'

export const api = {
  // Chat / sessions
  listSessions: () =>
    apiFetch<{ sessions: SessionInfo[] }>('/api/sessions').then((d) => d.sessions),
  sessionHistory: (sessionKey: string) =>
    apiFetch<{ items: HistoryMessage[] }>(
      `/api/sessions/${encodeURIComponent(sessionKey)}/messages`,
    ).then((d) => d.items),

  // Stops the session's running turn. The server does not answer until the turn
  // has settled, so a send issued after this resolves cannot be rejected as a
  // concurrent turn.
  abortSession: (sessionKey: string) =>
    apiFetch<{ ok: boolean }>(
      `/api/sessions/${encodeURIComponent(sessionKey)}/abort`,
      { method: 'POST' },
    ),

  // Whether the session still has a turn in flight -- used after a reload, when
  // this tab has no stream to tell it.
  sessionTurn: (sessionKey: string) =>
    apiFetch<{ active: boolean }>(
      `/api/sessions/${encodeURIComponent(sessionKey)}/turn`,
    ),

  // HITL write confirmations (issue #20 / #116)
  postConfirm: (sessionKey: string, decision: 'approve' | 'reject' | 'allow-always') =>
    apiFetch<{ approved: boolean; decision: string; approval_id?: string; allowlisted?: boolean }>(
      `/api/sessions/${encodeURIComponent(sessionKey)}/confirm`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ decision }),
      },
    ),
  pendingConfirm: (sessionKey: string) =>
    apiFetch<PendingConfirm>(
      `/api/sessions/${encodeURIComponent(sessionKey)}/confirm/pending`,
    ),

  // Ask-user questions (issue #161): answer or dismiss a question the agent is
  // blocked on, and restore the card after a reload.
  postQuestion: (sessionKey: string, id: string, answers: Record<string, string[]>) =>
    apiFetch<{ question_id: string; cancelled: boolean }>(
      `/api/sessions/${encodeURIComponent(sessionKey)}/question`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id, answers }),
      },
    ),
  postQuestionCancel: (sessionKey: string, id: string) =>
    apiFetch<{ question_id: string; cancelled: boolean }>(
      `/api/sessions/${encodeURIComponent(sessionKey)}/question`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id, cancel: true }),
      },
    ),
  pendingQuestions: (sessionKey: string) =>
    apiFetch<{ questions: PendingQuestion[] }>(
      `/api/sessions/${encodeURIComponent(sessionKey)}/question/pending`,
    ).then((d) => d.questions ?? []),

  // Tasks (FR-M4)
  listTasks: () => apiFetch<{ tasks: Task[] }>('/api/tasks').then((d) => d.tasks),
  listTaskTemplates: () =>
    apiFetch<{ taskTemplates: TaskTemplate[] }>('/api/tasktemplates').then((d) => d.taskTemplates),
  createTask: (body: {
    name: string
    prompt?: string
    schedule: string
    templateRef?: string
    params?: Record<string, string>
    state?: 'Enabled' | 'Paused'
  }) =>
    apiFetch<{ task: Task }>('/api/tasks', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }).then((d) => d.task),
  deleteTask: (id: string) => apiFetch<{ deleted: string }>(`/api/tasks/${id}`, { method: 'DELETE' }),
  runTask: (id: string) =>
    apiFetch<{ started: boolean }>(`/api/tasks/${id}/run`, { method: 'POST' }),
  toggleTask: (id: string) =>
    apiFetch<{ task: Task }>(`/api/tasks/${id}/toggle`, { method: 'POST' }).then((d) => d.task),
  taskReports: (id: string) =>
    apiFetch<{ reports: Report[] }>(`/api/tasks/${id}/reports`).then((d) => d.reports),

  // Audit (M5)
  listAudit: (limit = 400) =>
    apiFetch<{ entries: AuditEntry[] | null }>(`/api/audit?limit=${limit}`).then((d) => d.entries ?? []),

  // Agent config (per-user instance CR: selectedModel + userInstructions)
  agentConfig: () => apiFetch<{ config: AgentConfig }>('/api/agent/config').then((d) => d.config),
  saveAgentConfig: (config: { model?: string; systemPrompt?: string }) =>
    apiFetch<{ config: AgentConfig }>('/api/agent/config', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ config }),
    }).then((d) => d.config),
  agentStatus: () => apiFetch<AgentStatus>('/api/agent/status'),

  // Confirmations (issue #116): effective + owned confirm policy / allowlist.
  agentConfirm: () => apiFetch<AgentConfirmView>('/api/agent/confirm'),
  saveAgentConfirm: (body: { confirmPolicy?: string; allowlist?: AllowlistRule[] }) =>
    apiFetch<AgentConfirmView>('/api/agent/confirm', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }),

  // Platform objects (read-only CRD views)
  listAgentTemplates: () => apiFetch<{ agentTemplates: PlatformObject[] }>('/api/agenttemplates').then((d) => d.agentTemplates),
  // The catalog: add a model, or edit/remove one that already exists. The name
  // is the model's identity everywhere downstream (provider key, model id,
  // credential Secret), so it is immutable and there is no rename -- a rename
  // is a delete plus an add. A model with no credential must say so explicitly
  // (public), or the server rejects it: a keyless provider fails every turn.
  addLLM: (body: { name: string; endpoint: string; apiKey?: string; public?: boolean }) =>
    apiFetch<{ model: PlatformObject }>('/api/llms', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }).then((d) => d.model),
  updateLLM: (name: string, body: { endpoint: string; apiKey?: string; public?: boolean }) =>
    apiFetch<{ model: PlatformObject; warning?: string }>(`/api/llms/${encodeURIComponent(name)}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }),
  deleteLLM: (name: string) =>
    apiFetch<{ removed: string; warning?: string }>(`/api/llms/${encodeURIComponent(name)}`, {
      method: 'DELETE',
    }),
  listInstances: () =>
    apiFetch<{ instances: PlatformObject[] }>('/api/instances').then((d) => d.instances),
  createInstance: (body: { templateRef?: string; selectedModel?: string; enabledSkills?: string[]; userInstructions?: string }) =>
    apiFetch<{ instance: PlatformObject }>('/api/instances', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }).then((d) => d.instance),
  listSkills: () =>
    apiFetch<{ skills: PlatformObject[] }>('/api/skills').then((d) => d.skills),
  listTaskRuns: () =>
    apiFetch<{ taskruns: PlatformObject[] }>('/api/taskruns').then((d) => d.taskruns),
  publishSkill: (name: string, opts: { displayName: string; description?: string }, tar: Uint8Array<ArrayBuffer>) =>
    apiFetch<PlatformObject>(
      `/api/skills/${encodeURIComponent(name)}/publish?displayName=${encodeURIComponent(opts.displayName)}${opts.description ? `&description=${encodeURIComponent(opts.description)}` : ''}`,
      { method: 'POST', headers: { 'Content-Type': 'application/gzip' }, body: tar },
    ),
  installSkill: (name: string) =>
    apiFetch<{ enabledSkills: string[] }>(`/api/skills/${encodeURIComponent(name)}/install`, { method: 'POST' })
      .then((d) => d.enabledSkills),
  uninstallSkill: (name: string) =>
    apiFetch<{ enabledSkills: string[] }>(`/api/skills/${encodeURIComponent(name)}/uninstall`, { method: 'POST' })
      .then((d) => d.enabledSkills),
}