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

{{- define "cacheppuccino.headlessServiceName" -}}
{{- printf "%s-headless" (include "cacheppuccino.fullname" .) | trunc 63 | trimSuffix "-" -}}
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

{{/*
envFrom sources for the container, in precedence order: a later entry wins on a
key conflict, so an externally managed object overrides the chart's own.

Nothing here rolls the pods when an external object changes — the deployment's
checksum/config annotation only covers the ConfigMap this chart renders. The
reloader annotation covers the rest where that controller is installed.
*/}}
{{- define "cacheppuccino.envFrom" -}}
- secretRef:
    name: {{ include "cacheppuccino.secretName" . }}
{{- with .Values.extraSecretsName }}
- secretRef:
    name: {{ . }}
{{- end }}
- configMapRef:
    name: {{ include "cacheppuccino.fullname" . }}
{{- with .Values.extraConfigMapName }}
- configMapRef:
    name: {{ . }}
{{- end }}
{{- end -}}

{{/*
Render an annotations map as key/value lines. A string value passes through
`tpl`, so it can reference release data; any other value renders as JSON.
Either way the value lands as the string Kubernetes requires. A null value
drops its key, so an overlay can unset one the chart or CI wrote.

Body-only: the caller emits the `annotations:` key and guards on the result.
Usage: include "cacheppuccino.renderAnnotations" (dict "Annotations" $map "Context" $)
*/}}
{{- define "cacheppuccino.renderAnnotations" -}}
{{- $out := dict -}}
{{- range $k, $v := .Annotations -}}
{{- if not (kindIs "invalid" $v) -}}
{{- $_ := set $out $k (kindIs "string" $v | ternary (tpl (toString $v) $.Context) (toJson $v)) -}}
{{- end -}}
{{- end -}}
{{- if $out -}}
{{- toYaml $out -}}
{{- end -}}
{{- end -}}
