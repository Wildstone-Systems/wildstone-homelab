{{- define "wildstone-management.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "wildstone-management.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "wildstone-management.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "wildstone-management.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | quote }}
app.kubernetes.io/name: {{ include "wildstone-management.name" . | quote }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
{{- end }}

{{- define "wildstone-management.selectorLabels" -}}
app.kubernetes.io/name: {{ include "wildstone-management.name" . | quote }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end }}

{{- define "wildstone-management.serviceAccountName" -}}
{{- default (include "wildstone-management.fullname" .) .Values.serviceAccount.name }}
{{- end }}

{{- define "wildstone-management.image" -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- end }}

