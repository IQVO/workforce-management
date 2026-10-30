{{- define "workforce-management.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "workforce-management.fullname" -}}
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

{{- define "workforce-management.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "workforce-management.labels" -}}
helm.sh/chart: {{ include "workforce-management.chart" . }}
{{ include "workforce-management.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "workforce-management.selectorLabels" -}}
app.kubernetes.io/name: {{ include "workforce-management.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "workforce-management.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "workforce-management.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "workforce-management.databaseSecretName" -}}
{{- if .Values.database.existingSecret }}
{{- .Values.database.existingSecret }}
{{- else }}
{{- include "workforce-management.fullname" . }}-database
{{- end }}
{{- end }}

{{/*
MANDATORY (ADR-0038: Postgres is the only runtime). cmd/workforce, cmd/mcp and
cmd/workforce-projector exit at boot without their database URL; there is no
in-memory mode and no file/log fallback.
*/}}
{{- define "workforce-management.requireDatabase" -}}
{{- if not (or .Values.database.url .Values.database.existingSecret) -}}
{{- fail "workforce-management requires database.url or database.existingSecret to be set (DATABASE_URL): the service runs on real Postgres only and exits at boot without it (ADR-0038)." -}}
{{- end -}}
{{- end -}}

{{/*
MANDATORY (ADR-0038: Kafka is the only runtime). cmd/workforce and cmd/mcp exit
at boot without KAFKA_BROKERS and without the kafka event publisher; there is
no default broker, no log sink and no way to switch Kafka off. kafka.enabled is
kept only so values written before ADR-0038 still render (warehouse-infra sets
it to true): it may be true, never false.
*/}}
{{- define "workforce-management.requireKafka" -}}
{{- $_ := required "workforce-management requires kafka.brokers to be set (KAFKA_BROKERS, comma-separated): the service runs on real Kafka only and exits at boot without it (ADR-0038)." .Values.kafka.brokers -}}
{{- if and (hasKey .Values.kafka "enabled") (not .Values.kafka.enabled) -}}
{{- fail "kafka.enabled is false — Kafka cannot be switched off: the service runs on real Kafka only (ADR-0038). Drop the key or set it to true and set kafka.brokers." -}}
{{- end -}}
{{- if ne (toString .Values.config.eventPublisher) "kafka" -}}
{{- fail (printf "config.eventPublisher is %q — the only publisher is \"kafka\": the log sink was removed (ADR-0038); the binary rejects EVENT_PUBLISHER=log at boot." (toString .Values.config.eventPublisher)) -}}
{{- end -}}
{{- if .Values.pathCatalogue.enabled -}}
{{- fail "pathCatalogue.enabled is true — the file catalogue (PATH_CATALOGUE_FILE) was removed (ADR-0038): process paths come from process-path-management's Kafka events. Drop pathCatalogue or set enabled=false." -}}
{{- end -}}
{{- end -}}

{{/*
Shutdown is sequential: SHUTDOWN_DRAIN_DELAY (10s default) + HTTP drain 10s +
outbox relay 10s = 30s of budget before the 5s telemetry flush; the default
grace period is 30s and a shorter one gets the pod SIGKILLed mid-drain.
*/}}
{{- define "workforce-management.requireGracePeriod" -}}
{{- if lt (int .Values.terminationGracePeriodSeconds) 25 -}}
{{- fail (printf "terminationGracePeriodSeconds is %v — want at least 25: SHUTDOWN_DRAIN_DELAY + 10s HTTP drain + 10s outbox relay." .Values.terminationGracePeriodSeconds) -}}
{{- end -}}
{{- end -}}

{{/*
The Kafka-consumer *_MODE switches have no permissive or off position any more
(ADR-0038): extraEnv may carry PATH_CATALOGUE_SOURCE=kafka and
LABOR_PERFORMANCE_MODE=kafka-cache (warehouse-infra does), the REST sync-edge
INSTALLED_CAPACITY_MODE (ADR-0014, out of scope) and the labor-performance
REST rollback LABOR_PERFORMANCE_MODE=http (R10), and nothing else of that
kind: not EVENT_PUBLISHER=log, not the retired PATH_CATALOGUE_FILE, not any
other *_MODE.
*/}}
{{- define "workforce-management.requireNoInMemoryEnv" -}}
{{- range .Values.extraEnv -}}
{{- $name := toString .name -}}
{{- $value := toString (default "" .value) -}}
{{- if and (eq $name "EVENT_PUBLISHER") (ne $value "kafka") -}}
{{- fail (printf "extraEnv sets EVENT_PUBLISHER=%s — the only publisher is \"kafka\" (ADR-0038)." $value) -}}
{{- end -}}
{{- if eq $name "PATH_CATALOGUE_FILE" -}}
{{- fail "extraEnv sets PATH_CATALOGUE_FILE — the file catalogue was removed (ADR-0038); process paths come from Kafka." -}}
{{- end -}}
{{- if and (eq $name "PATH_CATALOGUE_SOURCE") (ne $value "kafka") -}}
{{- fail (printf "extraEnv sets PATH_CATALOGUE_SOURCE=%s — only \"kafka\" is supported (ADR-0038)." $value) -}}
{{- end -}}
{{- if and (eq $name "LABOR_PERFORMANCE_MODE") (not (has $value (list "kafka-cache" "http"))) -}}
{{- fail (printf "extraEnv sets LABOR_PERFORMANCE_MODE=%s — only \"kafka-cache\" (or the \"http\" REST rollback) is supported; permissive is gone (ADR-0038)." $value) -}}
{{- end -}}
{{- if and (hasSuffix "_MODE" $name) (not (has $name (list "LABOR_PERFORMANCE_MODE" "INSTALLED_CAPACITY_MODE"))) -}}
{{- fail (printf "extraEnv sets %s — *_MODE variables are not configurable: workforce-management runs on real Postgres and real Kafka only, with no permissive or off mode (ADR-0038)." $name) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
analytics.enabled needs an analytical DSN source (both binaries refuse to boot
without ANALYTICS_DATABASE_URL) and a stable consumer group (the projector
refuses to boot without ANALYTICS_CONSUMER_GROUP). Its broker is the one
kafka.brokers that requireKafka already makes mandatory. Fail at render
instead of crash-looping.
*/}}
{{- define "workforce-management.requireAnalyticsConfig" -}}
{{- if not (or .Values.analytics.database.projectorUrl .Values.analytics.database.existingSecret) -}}
{{- fail "analytics.enabled is true but neither analytics.database.projectorUrl nor analytics.database.existingSecret is set — the projector and reports binaries refuse to boot without ANALYTICS_DATABASE_URL." -}}
{{- end -}}
{{- $_ := required "analytics.enabled is true but analytics.projector.consumerGroup is empty: the projector exits at boot without ANALYTICS_CONSUMER_GROUP, a stable Kafka group id (ADR-0038)." .Values.analytics.projector.consumerGroup -}}
{{- end -}}

{{/*
Every render-time requirement of the real runtime, in one include. The api
Deployment (always rendered) includes it, so a mis-set release fails
`helm template` / `helm install` whichever component is enabled.
*/}}
{{- define "workforce-management.requireRealRuntime" -}}
{{- include "workforce-management.requireDatabase" . -}}
{{- include "workforce-management.requireKafka" . -}}
{{- include "workforce-management.requireGracePeriod" . -}}
{{- include "workforce-management.requireNoInMemoryEnv" . -}}
{{- if .Values.analytics.enabled -}}
{{- include "workforce-management.requireAnalyticsConfig" . -}}
{{- end -}}
{{- end -}}

{{/*
Fully qualified name of the analytics projector deployment (ADR-0010).
*/}}
{{- define "workforce-management.projectorFullname" -}}
{{- include "workforce-management.fullname" . }}-projector
{{- end }}

{{/*
Fully qualified name of the analytics reports deployment/service (ADR-0010).
*/}}
{{- define "workforce-management.reportsFullname" -}}
{{- include "workforce-management.fullname" . }}-reports
{{- end }}

{{/*
Name of the Secret holding the analytics DSNs, when the chart creates its own.
*/}}
{{- define "workforce-management.analyticsSecretName" -}}
{{- if .Values.analytics.database.existingSecret }}
{{- .Values.analytics.database.existingSecret }}
{{- else }}
{{- include "workforce-management.fullname" . }}-analytics
{{- end }}
{{- end }}

{{/*
Fully qualified name of the MCP server deployment/service (ADR-0008).
*/}}
{{- define "workforce-management.mcpFullname" -}}
{{- include "workforce-management.fullname" . }}-mcp
{{- end }}

{{/*
Fully qualified name of the A2A agent Deployment/Service (<release>-a2a, ADR-0039).
*/}}
{{- define "workforce-management.a2aFullname" -}}
{{- include "workforce-management.fullname" . }}-a2a
{{- end }}


{{/*
Fully qualified name of the frontend Module Federation remote deployment/service.

The remote is served by its own nginx pod and reached through warehouse-infra's
Nginx web gateway at /mfes/workforce-management/. It is deliberately a separate
workload from the API: Kong never routes to it, and the OLTP Service must never
select it.
*/}}
{{- define "workforce-management.frontendFullname" -}}
{{- include "workforce-management.fullname" . }}-frontend
{{- end }}


{{/*
Comma-joined names of this chart's enabled KongPlugins, for the
konghq.com/plugins annotation on every HTTPRoute this chart renders.
Empty string (annotation omitted) when none are enabled — standalone
installs without Kong are unaffected.
*/}}
{{- define "workforce-management.kongPluginNames" -}}
{{- $names := list -}}
{{- range $name, $cfg := .Values.kongPlugins -}}
{{- if $cfg.enabled -}}
{{- $names = append $names (printf "%s-%s" (include "workforce-management.fullname" $) $name) -}}
{{- end -}}
{{- end -}}
{{- join "," $names -}}
{{- end -}}
