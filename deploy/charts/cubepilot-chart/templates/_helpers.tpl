{{/* Common labels for all resources. */}}
{{- define "cubepilot.labels" -}}
app.kubernetes.io/name: cubepilot
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Resolve a full image reference from a repository + tag pair. An empty tag falls
back to the chart's appVersion, which is what makes a published release chart
pin the images it was released with: the release workflow packages a tag build
with `--app-version X.Y.Z`, so every image defaults to :X.Y.Z. The rolling chart
built from main is packaged with `--app-version latest` and so keeps resolving
to :latest.

Call as:
  {{ include "cubepilot.image" (dict "image" .Values.operator.image "root" .) }}
*/}}
{{- define "cubepilot.image" -}}
{{- printf "%s:%s" .image.repository (.image.tag | default .root.Chart.AppVersion) -}}
{{- end -}}

{{/*
Shared agent-runtime env block. Both the operator and the api process consume
config.Load() (internal/config), so they receive the same environment.
*/}}
{{- define "cubepilot.agentEnv" -}}
- name: CUBEPILOT_NAMESPACE
  value: {{ .Release.Namespace | quote }}
- name: CUBEPILOT_AGENT_IMAGE
  value: {{ include "cubepilot.image" (dict "image" .Values.agents.image "root" .) | quote }}
- name: CUBEPILOT_AGENT_IMAGE_PULL_POLICY
  value: {{ .Values.imagePullPolicy | quote }}
- name: CUBEPILOT_GC_WINDOW
  value: {{ .Values.agents.gcWindow | quote }}
- name: CUBEPILOT_GC_WATERMARK
  value: {{ .Values.agents.gcWatermark | quote }}
- name: CUBEPILOT_USERS
  value: {{ .Values.agents.users | quote }}
- name: CUBEPILOT_REPLICAS
  value: {{ .Values.operator.replicas | quote }}
{{- end -}}
