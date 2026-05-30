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
{{- $repository := .Values.controllerManager.image.repository }}
{{- $tag := .Values.controllerManager.image.tag | default .Chart.AppVersion }}
{{- printf "%s:%s" $repository $tag }}
{{- end }}

{{/*
Self-signed webhook serving certificate (used when certManager.enabled=false).

Populates a memoized dict at $._beskar7WebhookCerts with base64-encoded
{crt, key, ca} so the Secret template (selfsigned-cert.yaml) and the webhook
configuration template (webhook-configuration.yaml) reference the SAME cert +
CA within a single render. Generating per-file would produce mismatched CAs
and the apiserver would reject the webhook.

Resolution order:
  1. Reuse an existing Secret of the same name (via lookup) so the cert is
     stable across `helm upgrade` and so an operator-provisioned cert is
     honored. caBundle uses ca.crt when present, else falls back to tls.crt
     (a self-signed leaf verifies itself).
  2. Otherwise generate a fresh self-signed CA + leaf covering the webhook
     Service DNS name, valid 10 years.

During `helm template` / `--dry-run` (no cluster) the lookup returns empty
and a fresh cert is generated each render — fine for inspection; the real
install/upgrade path hits the cluster and is stable.
*/}}
{{- define "beskar7.webhookCerts" -}}
{{- if not (hasKey . "_beskar7WebhookCerts") -}}
  {{- $ns := include "beskar7.namespace" . -}}
  {{- $secretName := .Values.certManager.certificate.secretName -}}
  {{- $svc := .Values.webhook.service.name -}}
  {{- $cn := printf "%s.%s.svc" $svc $ns -}}
  {{- $existing := (lookup "v1" "Secret" $ns $secretName) -}}
  {{- $data := dict -}}
  {{- if $existing -}}
    {{- $data = ($existing.data | default dict) -}}
  {{- end -}}
  {{- $certs := dict -}}
  {{- if and (hasKey $data "tls.crt") (hasKey $data "tls.key") -}}
    {{- $ca := index $data "tls.crt" -}}
    {{- if hasKey $data "ca.crt" -}}{{- $ca = index $data "ca.crt" -}}{{- end -}}
    {{- $certs = dict "crt" (index $data "tls.crt") "key" (index $data "tls.key") "ca" $ca -}}
  {{- else -}}
    {{- $caCert := genCA (printf "%s-ca" $svc) 3650 -}}
    {{- /*
      SAN list: the in-cluster .svc name (webhook server) plus any external
      callback names/IPs so the SAME cert validates for bare-metal hosts hitting
      :8082. genSignedCert signature is (CN, ipAddresses, dnsNames, days, ca).
    */ -}}
    {{- $dnsNames := concat (list $cn) (.Values.callback.externalNames | default (list)) -}}
    {{- $ipAddrs := .Values.callback.externalIPs | default (list) -}}
    {{- $cert := genSignedCert $cn $ipAddrs $dnsNames 3650 $caCert -}}
    {{- $certs = dict "crt" (b64enc $cert.Cert) "key" (b64enc $cert.Key) "ca" (b64enc $caCert.Cert) -}}
  {{- end -}}
  {{- $_ := set . "_beskar7WebhookCerts" $certs -}}
{{- end -}}
{{- end -}}

