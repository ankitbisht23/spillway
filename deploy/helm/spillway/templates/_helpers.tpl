{{/* Labels every object carries. */}}
{{- define "spillway.labels" -}}
app.kubernetes.io/part-of: spillway
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end }}

{{/* image: repository:tag, the tag defaulting to v<appVersion>, the release
   workflow's image tag (ADR-0076). */}}
{{- define "spillway.image" -}}
{{- printf "%s:%s" .image.repository (.image.tag | default (printf "v%s" .root.Chart.AppVersion)) }}
{{- end }}

{{/*
Pod and container security (SEC-10, ADR-0075 §D11): non-root, read-only
root filesystem, no privilege escalation, every capability dropped, the
runtime's default seccomp profile. Not configurable.
*/}}
{{- define "spillway.podSecurityContext" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
fsGroup: 65532
seccompProfile:
  type: RuntimeDefault
{{- end }}

{{- define "spillway.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
privileged: false
capabilities:
  drop: [ALL]
{{- end }}

{{/* A Secret volume with one key mounted as the file "value". */}}
{{- define "spillway.secretFile" -}}
- name: {{ .name }}
  secret:
    secretName: {{ .secretName }}
    defaultMode: 0440
    items:
      - { key: {{ .key }}, path: value }
{{- end }}

{{/* The Prometheus peer of the NetworkPolicies (GW-25). */}}
{{- define "spillway.prometheusPeer" -}}
- namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: {{ .Values.networkPolicy.prometheus.namespace }}
  podSelector:
    matchLabels:
      {{- toYaml .Values.networkPolicy.prometheus.podLabels | nindent 6 }}
{{- end }}

{{/* The kubelet's peers: the node network (GW-25). Required. */}}
{{- define "spillway.kubeletPeers" -}}
{{- if not .Values.networkPolicy.kubeletCIDRs }}
{{- fail "networkPolicy.kubeletCIDRs: the node network the kubelet's probes come from (required)" }}
{{- end }}
{{- range .Values.networkPolicy.kubeletCIDRs }}
- ipBlock:
    cidr: {{ . }}
{{- end }}
{{- end }}
