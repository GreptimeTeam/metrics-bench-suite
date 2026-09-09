{{/* Expand the chart name. */}}
{{- define "metrics-loader.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Use a revisioned Job name because Job pod templates are immutable. */}}
{{- define "metrics-loader.jobName" -}}
{{- printf "%s-%d" (include "metrics-loader.fullname" . | trunc 52 | trimSuffix "-") .Release.Revision | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Create a fully qualified application name. */}}
{{- define "metrics-loader.fullname" -}}
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

{{/* Create the chart label. */}}
{{- define "metrics-loader.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Common labels. */}}
{{- define "metrics-loader.labels" -}}
helm.sh/chart: {{ include "metrics-loader.chart" . }}
{{ include "metrics-loader.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/* Selector labels. */}}
{{- define "metrics-loader.selectorLabels" -}}
app.kubernetes.io/name: {{ include "metrics-loader.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/* Resolve the service account name. */}}
{{- define "metrics-loader.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "metrics-loader.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
