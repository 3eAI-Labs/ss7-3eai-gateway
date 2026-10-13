{{/*
Expand the name of the chart.
*/}}
{{- define "ss7-3eai-gateway.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name (bounded to 63 chars for the StatefulSet/Service
names, whose pod hostnames the sticky-CgPA ordinal logic parses).
*/}}
{{- define "ss7-3eai-gateway.fullname" -}}
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

{{- define "ss7-3eai-gateway.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "ss7-3eai-gateway.labels" -}}
helm.sh/chart: {{ include "ss7-3eai-gateway.chart" . }}
{{ include "ss7-3eai-gateway.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: ss7-3eai-gateway
{{- end }}

{{- define "ss7-3eai-gateway.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ss7-3eai-gateway.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Container image reference. Tag falls back to the chart appVersion.
*/}}
{{- define "ss7-3eai-gateway.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "ss7-3eai-gateway.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "ss7-3eai-gateway.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the Secret holding jwt/hmac/admin/AWS credentials. Either an operator-
provisioned existingSecret (production, ADR-036 secretRef) or a chart-created
one (smoke/dev only).
*/}}
{{- define "ss7-3eai-gateway.secretName" -}}
{{- if .Values.secrets.existingSecret }}
{{- .Values.secrets.existingSecret }}
{{- else }}
{{- printf "%s-secrets" (include "ss7-3eai-gateway.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Literals published as defaults by chart <= 0.2.0. They are working credentials
anyone can read from the registry, so they are never accepted again: rejected
when passed explicitly, rotated when found in a live Secret on upgrade.
*/}}
{{- define "ss7-3eai-gateway.publishedSecretLiterals" -}}
{{- list "smoke-jwt-override-me-0123456789abcdefghij" "smoke-hmac-override-me-0123456789abcdefgh" "smoke-admin-override-me" | toJson }}
{{- end }}

{{/*
Resolve one value of the chart-created Secret (ADR-036 secretValue, empty in
chart defaults). Precedence: explicit value > value already in the live Secret
(so upgrades never rotate it) > freshly generated random value.
lookup returns nothing under `helm template` / --dry-run, so those renders show
a new random value each time; a real install/upgrade does not.
Args: dict "ctx" $ "key" <Secret data key> "value" <explicit value> "length" <int>
*/}}
{{- define "ss7-3eai-gateway.secretValue" -}}
{{- $published := include "ss7-3eai-gateway.publishedSecretLiterals" .ctx | fromJsonArray }}
{{- if .value }}
{{- if has .value $published }}
{{- fail (printf "secrets: the value given for %s is a credential published in chart <= 0.2.0 defaults; supply a new one or leave it empty to generate one" .key) }}
{{- end }}
{{- .value }}
{{- else }}
{{- $live := "" }}
{{- $existing := lookup "v1" "Secret" .ctx.Release.Namespace (include "ss7-3eai-gateway.secretName" .ctx) }}
{{- if $existing }}
{{- $live = index ($existing.data | default dict) .key | default "" | b64dec }}
{{- end }}
{{- if and $live (not (has $live $published)) }}
{{- $live }}
{{- else }}
{{- randAlphaNum (int .length) }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Headless service name — StatefulSet serviceName; also the stable network domain
for M3UA/SCTP peer identity.
*/}}
{{- define "ss7-3eai-gateway.headlessServiceName" -}}
{{- printf "%s-headless" (include "ss7-3eai-gateway.fullname" .) }}
{{- end }}

{{/*
Resolved NATS URL. When nats.deploy=true, wire the bundled single-replica
JetStream service; otherwise require an operator-supplied external URL.
*/}}
{{- define "ss7-3eai-gateway.natsURL" -}}
{{- if .Values.nats.deploy }}
{{- printf "nats://%s-nats:4222" (include "ss7-3eai-gateway.fullname" .) }}
{{- else }}
{{- required "nats.url is required when nats.deploy=false (point at platform NATS)" .Values.nats.url }}
{{- end }}
{{- end }}
