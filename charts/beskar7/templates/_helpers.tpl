{{/*
Expand the name of the chart.
*/}}
{{- define "beskar7.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "beskar7.fullname" -}}
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

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "beskar7.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "beskar7.labels" -}}
helm.sh/chart: {{ include "beskar7.chart" . }}
{{ include "beskar7.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
cluster.x-k8s.io/provider: beskar7
{{- if .Values.labels }}
{{ toYaml .Values.labels }}
{{- end }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "beskar7.selectorLabels" -}}
app.kubernetes.io/name: {{ include "beskar7.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "beskar7.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "beskar7.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Create the namespace name
*/}}
{{- define "beskar7.namespace" -}}
{{- if .Values.namespace.create }}
{{- .Values.namespace.name | default "beskar7-system" }}
{{- else }}
{{- .Release.Namespace }}
{{- end }}
{{- end }}

{{/*
Create the controller manager image
*/}}
{{- define "beskar7.controllerImage" -}}
{{- $repository := .Values.controllerManager.image.repository | default .Values.image.repository }}
{{- $tag := .Values.controllerManager.image.tag }}
{{- if not $tag }}
  {{- $tag = .Values.image.tag | default .Chart.AppVersion }}
{{- end }}
{{- printf "%s:%s" $repository $tag }}
{{- end }}

