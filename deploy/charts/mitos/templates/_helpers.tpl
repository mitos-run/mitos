{{/*
Chart name, optionally overridden by .Values.nameOverride.
*/}}
{{- define "mitos.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name. The base name is "mitos" so resources keep their
familiar mitos-* names regardless of the release name.
*/}}
{{- define "mitos.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- include "mitos.name" . | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
Target namespace for all namespaced resources.
*/}}
{{- define "mitos.namespace" -}}
{{- default .Release.Namespace .Values.namespace.name -}}
{{- end -}}

{{/*
Chart label, name plus version.
*/}}
{{- define "mitos.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels merged onto every resource. Matches the existing scheme:
app.kubernetes.io/name: mitos. commonLabels are merged in.
*/}}
{{- define "mitos.labels" -}}
app.kubernetes.io/name: {{ include "mitos.name" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ include "mitos.chart" . }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
Selector labels for a component. Call with a dict carrying "root" and
"component", for example:
  include "mitos.selectorLabels" (dict "root" . "component" "controller")
*/}}
{{- define "mitos.selectorLabels" -}}
app.kubernetes.io/name: {{ include "mitos.name" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/*
Compose a fully qualified image reference from the registry, a component image
block, and the global tag override. Call with a dict carrying "root" and
"image":
  include "mitos.image" (dict "root" . "image" .Values.controller.image)
*/}}
{{- define "mitos.image" -}}
{{- $registry := .root.Values.image.registry -}}
{{- $repo := .image.repository -}}
{{- $tag := .image.tag -}}
{{- if .root.Values.global.imageTag -}}
{{- $tag = .root.Values.global.imageTag -}}
{{- end -}}
{{- printf "%s/%s:%s" $registry $repo $tag -}}
{{- end -}}

{{/*
imagePullSecrets block shared by every workload pod spec.
*/}}
{{- define "mitos.imagePullSecrets" -}}
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
{{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{/*
True (non-empty) when the controller serves ANY webhook: the principal admission
webhook or the SandboxPool conversion webhook. The controller exposes a single
webhook server on .Values.admissionWebhook.port, so the serving port, the cert
volume, and the cert mount on the controller Deployment are rendered once when
either feature is on.
*/}}
{{- define "mitos.controllerWebhookEnabled" -}}
{{- if or .Values.admissionWebhook.enabled .Values.apiV2.poolConversionWebhook.enabled -}}
true
{{- end -}}
{{- end -}}
