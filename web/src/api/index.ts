// API service layer -- one function per backend endpoint.
import { apiFetch } from './client'
import type {
  AgentConfig,
  AgentStatus,
  AuditEntry,
  HistoryMessage,
  PlatformObject,
  Report,
  SessionInfo,
  Task,
} from './types'

export const api = {
  // Chat / sessions
  listSessions: () =>
    apiFetch<{ sessions: SessionInfo[] }>('/api/sessions').then((d) => d.sessions),
  sessionHistory: (sessionKey: string) =>
    apiFetch<{ items: HistoryMessage[] }>(
      `/api/sessions/${encodeURIComponent(sessionKey)}/messages`,
    ).then((d) => d.items),
  ledger: (sessionKey: string) =>
    apiFetch<{ rows: unknown[] }>(
      `/api/sessions/${encodeURIComponent(sessionKey)}/ledger`,
    ).then((d) => d.rows),

  // Tasks (FR-M4)
  listTasks: () => apiFetch<{ tasks: Task[] }>('/api/tasks').then((d) => d.tasks),
  createTask: (body: { name: string; prompt: string; schedule: string; state?: 'Enabled' | 'Paused' }) =>
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

  // Agent config (FR-M2-005)
  agentConfig: () => apiFetch<{ config: AgentConfig }>('/api/agent/config').then((d) => d.config),
  saveAgentConfig: (config: AgentConfig) =>
    apiFetch<{ config: AgentConfig }>('/api/agent/config', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ config }),
    }).then((d) => d.config),
  agentStatus: () => apiFetch<AgentStatus>('/api/agent/status'),

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
}