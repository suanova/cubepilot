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
  addLLM: (body: { name: string; endpoint: string; apiKey?: string }) =>
    apiFetch<{ model: PlatformObject }>('/api/llms', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }).then((d) => d.model),
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