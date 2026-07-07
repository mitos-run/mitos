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
{{- if not $tag -}}
{{- $tag = .root.Chart.AppVersion -}}
{{- end -}}
{{- printf "%s/%s:%s" $registry $repo $tag -}}
{{- end -}}

{{/*
Comma-separated list of the secret-store providers the console advertises at
GET /console/capabilities. kube is appended when enabled; openbao when
configured. Order is stable (kube first).
*/}}
{{- define "mitos.console.secretProviders" -}}
{{- $providers := list -}}
{{- if .Values.console.secrets.kube.enabled -}}
{{- $providers = append $providers "kube" -}}
{{- end -}}
{{- if .Values.console.secrets.openbao.enabled -}}
{{- $providers = append $providers "openbao" -}}
{{- end -}}
{{- join "," $providers -}}
{{- end -}}

{{/*
Database DSN env entry shared by the gateway and console pods. When
database.dsnSecretRef.name is set, it renders a single env var
MITOS_DATABASE_DSN sourced from that Secret via secretKeyRef, so the DSN (which
carries the database password) is NEVER placed in the rendered manifests as a
plaintext value. When unset, it renders nothing, and the binary falls back to
in-memory persistence (dev only). The rendered fragment is a list entry intended
to be nindent'd into a container's env list.
*/}}
{{/*
The internal usage API port, derived from controller.usage.apiAddress (":<port>"
form, the same value passed to --usage-api-address). Used for the controller
container port and the usage Service.
*/}}
{{- define "mitos.usageAPIPort" -}}
{{- splitList ":" .Values.controller.usage.apiAddress | last -}}
{{- end -}}

{{/*
The usage API bearer-token env entry (MITOS_USAGE_API_TOKEN), shared by the
controller (which gates the internal usage API on it) and the console (which
presents it). The token is a SECRET: it is sourced via secretKeyRef ONLY and
never appears as plaintext in the rendered manifests. When
controller.usage.tokenSecret.name is unset, it renders nothing and the usage
API fails closed (refuses every request).
*/}}
{{- define "mitos.usageToken.env" -}}
{{- if .Values.controller.usage.tokenSecret.name }}
- name: MITOS_USAGE_API_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ .Values.controller.usage.tokenSecret.name | quote }}
      key: {{ .Values.controller.usage.tokenSecret.key | default "usage-api-token" | quote }}
{{- end }}
{{- end -}}

{{- define "mitos.database.env" -}}
{{- if .Values.database.dsnSecretRef.name }}
- name: MITOS_DATABASE_DSN
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.dsnSecretRef.name | quote }}
      key: {{ .Values.database.dsnSecretRef.key | default "dsn" | quote }}
{{- end }}
{{- end -}}

{{/*
API key hash pepper env entry, shared by the gateway and console pods (issue
#733). OPT-IN and OFF by default: renders nothing unless
saas.apiKeyPepperSecret.name is set. The pepper is a SECRET, sourced via
secretKeyRef ONLY, and the SAME Secret MUST back both pods so a key the console
mints verifies at the gateway. Introducing a pepper invalidates any pre-existing
keys (they must be reissued), so it is deliberately not enabled by default.
*/}}
{{- define "mitos.apiKeyPepper.env" -}}
{{- if .Values.saas.apiKeyPepperSecret.name }}
- name: MITOS_API_KEY_PEPPER
  valueFrom:
    secretKeyRef:
      name: {{ .Values.saas.apiKeyPepperSecret.name | quote }}
      key: {{ .Values.saas.apiKeyPepperSecret.key | default "pepper" | quote }}
{{- end }}
{{- end -}}

{{/*
Product-telemetry env entries shared by the gateway and console pods. Telemetry
is OPT-IN and OFF by default: this renders nothing unless telemetry.enabled is
true AND telemetry.endpoint is set, so the binaries fail closed to a no-op
emitter. The salt and the optional collector token are SECRETS: they are sourced
via secretKeyRef ONLY and never appear as plaintext in the rendered manifests.
With no salt Secret the org id is dropped from every event (fail closed). The
rendered fragment is a set of env list entries intended to be nindent'd into a
container's env list. DO_NOT_TRACK is honored by the binary at runtime; an
operator can also force-disable via telemetry.optOut here.
*/}}
{{- define "mitos.telemetry.env" -}}
{{- if and .Values.telemetry.enabled .Values.telemetry.endpoint }}
- name: MITOS_TELEMETRY_ENABLED
  value: "true"
- name: MITOS_TELEMETRY_ENDPOINT
  value: {{ .Values.telemetry.endpoint | quote }}
{{- if .Values.telemetry.optOut }}
- name: MITOS_TELEMETRY_OPTOUT
  value: "true"
{{- end }}
{{- if .Values.telemetry.saltSecretRef.name }}
- name: MITOS_TELEMETRY_SALT
  valueFrom:
    secretKeyRef:
      name: {{ .Values.telemetry.saltSecretRef.name | quote }}
      key: {{ .Values.telemetry.saltSecretRef.key | default "salt" | quote }}
{{- end }}
{{- if .Values.telemetry.tokenSecretRef.name }}
- name: MITOS_TELEMETRY_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ .Values.telemetry.tokenSecretRef.name | quote }}
      key: {{ .Values.telemetry.tokenSecretRef.key | default "token" | quote }}
{{- end }}
{{- end }}
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
