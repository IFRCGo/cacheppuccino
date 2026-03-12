{{- define "cacheppuccino.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "cacheppuccino.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := include "cacheppuccino.name" . -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "cacheppuccino.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "cacheppuccino.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "cacheppuccino.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cacheppuccino.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "cacheppuccino.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "cacheppuccino.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "cacheppuccino.secretProviderName" -}}
{{- if .Values.secretsStoreCsiDriver.secretProviderClassName -}}
{{- .Values.secretsStoreCsiDriver.secretProviderClassName -}}
{{- else -}}
{{- printf "%s-secret-provider" (include "cacheppuccino.fullname" .) -}}
{{- end -}}
{{- end -}}

{{- define "cacheppuccino.secretName" -}}
{{- if .Values.secretsStoreCsiDriver.secretName -}}
{{- .Values.secretsStoreCsiDriver.secretName -}}
{{- else -}}
{{- printf "%s-vault-secret" (include "cacheppuccino.fullname" .) -}}
{{- end -}}
{{- end -}}
