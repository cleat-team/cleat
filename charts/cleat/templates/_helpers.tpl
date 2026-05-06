{{- define "cleat.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "cleat.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "cleat.labels" -}}
helm.sh/chart: {{ include "cleat.name" . }}-{{ .Chart.Version | replace "+" "_" }}
{{ include "cleat.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "cleat.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cleat.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "cleat.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "cleat.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "cleat.databaseURL" -}}
{{- $host := .Values.postgres.host }}
{{- $port := .Values.postgres.port }}
{{- $database := .Values.postgres.database }}
{{- $username := .Values.postgres.username }}
{{- $password := .Values.postgres.password }}
{{- $sslmode := .Values.postgres.sslmode }}
postgres://{{ $username }}:{{ $password }}@{{ $host }}:{{ $port }}/{{ $database }}?sslmode={{ $sslmode }}
{{- end }}
