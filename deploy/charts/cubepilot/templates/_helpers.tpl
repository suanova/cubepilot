{{/* Common labels for all resources. */}}
{{- define "cubepilot.labels" -}}
app.kubernetes.io/name: cubepilot
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Sanitize a user identity to a DNS-1123 name segment, mirroring k8s.Sanitize
(internal/k8s/client.go): lowercase, collapse runs of non [a-z0-9-] to a single
"-", trim "-", empty becomes "user".
*/}}
{{- define "cubepilot.sanitize" -}}
{{- $s := regexReplaceAll "[^a-z0-9-]+" (lower .) "-" -}}
{{- $s = trimAll "-" $s -}}
{{- if eq $s "" -}}user{{- else -}}{{$s}}{{- end -}}
{{- end -}}

{{/*
Shared agent-runtime env block. Both the operator and the api process consume
config.Load() (internal/config), so they receive the same environment.
*/}}
{{- define "cubepilot.agentEnv" -}}
- name: CUBEPILOT_NAMESPACE
  value: {{ .Release.Namespace | quote }}
- name: CUBEPILOT_AGENT_IMAGE
  value: {{ .Values.agents.image | quote }}
- name: CUBEPILOT_RECLAIM
  value: {{ .Values.agents.reclaim | quote }}
- name: CUBEPILOT_IDLE_TTL
  value: {{ .Values.agents.idleTTL | quote }}
- name: CUBEPILOT_GC_WINDOW
  value: {{ .Values.agents.gcWindow | quote }}
- name: CUBEPILOT_GC_WATERMARK
  value: {{ .Values.agents.gcWatermark | quote }}
- name: CUBEPILOT_USERS
  value: {{ .Values.agents.users | quote }}
- name: CUBEPILOT_REPLICAS
  value: {{ .Values.operator.replicas | quote }}
{{- end -}}
