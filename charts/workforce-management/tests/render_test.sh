#!/usr/bin/env bash
# Chart render assertions for charts/workforce-management (run in CI by the
# chart-tests job, next to test_service_selectors.py).
#
# The chart deploys the REAL runtime of this context (ADR-0038: Postgres and
# Kafka are the only runtime, no in-memory mode, no log sink, no file
# catalogue), so the mandatory configuration has no silent default, no
# permissive/off mode is ever rendered and the env the binaries read is the
# env the chart renders.
#
# Run: bash charts/workforce-management/tests/render_test.sh   (needs helm)
set -euo pipefail

chart_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
repo_dir="$(cd "$chart_dir/../.." && pwd)"
release=workforce-management
# Dummy values, no password: the chart refuses to render without a database
# source and without Kafka brokers; neither is ever dialled.
required=(--set "database.url=postgres://u@example.invalid:5432/db" --set "kafka.brokers=kafka.example.invalid:9092")
analytics=(--set analytics.enabled=true --set analytics.database.projectorUrl=postgres://projector@example.invalid:5432/a)

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

render() {
  helm template "$release" "$chart_dir" "${required[@]}" "$@"
}

env_value() { # env_value <rendered> <NAME>
  grep -A1 "name: $2\$" <<<"$1" | grep -m1 'value:' | sed -E 's/.*value: "?//; s/"$//'
}

api_deployment="$(render --show-only templates/deployment.yaml)"
configmap="$(render --show-only templates/configmap.yaml)"
configmap_name="$(awk '/^  name:/ {print $2; exit}' <<<"$configmap")"
[[ -n "$configmap_name" ]] || fail "could not read the ConfigMap name"

# D14 (2026-10-12): every config.* value is rendered into the chart ConfigMap,
# and the api Deployment -- the only workload whose binary (cmd/workforce)
# reads those keys -- must mount it with envFrom, or a value that is only in
# the ConfigMap silently does nothing.
grep -A3 'envFrom:' <<<"$api_deployment" | grep -q 'configMapRef:' \
  || fail "api Deployment does not mount the ConfigMap via envFrom"
grep -A3 'envFrom:' <<<"$api_deployment" | grep -q "name: $configmap_name" \
  || fail "api Deployment envFrom does not reference ConfigMap $configmap_name"

# --- ADR-0038: the mandatory configuration is rendered from values -------------
[[ "$(env_value "$api_deployment" KAFKA_BROKERS)" == "kafka.example.invalid:9092" ]] || fail "KAFKA_BROKERS is not rendered from kafka.brokers"
[[ "$(env_value "$api_deployment" EVENT_PUBLISHER)" == "kafka" ]] || fail "EVENT_PUBLISHER must default to kafka"
grep -q '^  EVENT_PUBLISHER: "kafka"' <<<"$configmap" || fail "the ConfigMap must carry EVENT_PUBLISHER=kafka"
grep -q '^  KAFKA_BROKERS: "kafka.example.invalid:9092"' <<<"$configmap" || fail "the ConfigMap must carry KAFKA_BROKERS"
grep -A4 'name: DATABASE_URL$' <<<"$api_deployment" | grep -q 'secretKeyRef:' || fail "DATABASE_URL must come from a Secret (secretKeyRef)"
if grep -qE '^  (MIGRATIONS_)?DATABASE_URL:' <<<"$configmap"; then fail "a DSN must never land in the ConfigMap"; fi
# warehouse-infra still passes the pre-ADR-0038 switch kafka.enabled=true: it must keep rendering.
render --set kafka.enabled=true --show-only templates/deployment.yaml >/dev/null || fail "kafka.enabled=true (set by warehouse-infra) must keep rendering"

# --- no in-memory / permissive / log / file-catalogue mode, ever ----------------
all_docs="$(render --set mcp.enabled=true --set frontend.enabled=true "${analytics[@]}")"
if grep -qiE 'value: "?(permissive|log)"?$' <<<"$all_docs"; then fail "the render carries permissive/log, which the binaries reject at boot"; fi
if grep -q 'PATH_CATALOGUE_FILE' <<<"$all_docs"; then fail "the render carries PATH_CATALOGUE_FILE: the file catalogue was removed"; fi
if grep -q 'DATABASE_URL: ""' <<<"$all_docs"; then fail "an empty DATABASE_URL must never be rendered"; fi
if grep -qi 'in-memory\|in memory' <<<"$all_docs"; then fail "the render mentions an in-memory mode"; fi
# The only *_MODE the chart renders is the REST sync edge (out of scope).
if grep -E '(name: |^  )[A-Z_]+_MODE' <<<"$all_docs" | grep -vE 'INSTALLED_CAPACITY_MODE' | grep -q .; then
  fail "the render carries a *_MODE variable other than INSTALLED_CAPACITY_MODE"
fi

# --- probes: /healthz liveness (and startup), /readyz readiness ------------------
probe_block() { # probe_block <probeName> <rendered deployment>
  awk -v p="$1:" '$1 == p {f=1; next} f && /^          [a-zA-Z]/ {exit} f {print}' <<<"$2"
}
grep -q 'path: /healthz' <<<"$(probe_block livenessProbe "$api_deployment")" || fail "livenessProbe must hit /healthz"
grep -q 'path: /healthz' <<<"$(probe_block startupProbe "$api_deployment")" || fail "startupProbe must hit /healthz"
grep -q 'path: /readyz' <<<"$(probe_block readinessProbe "$api_deployment")" || fail "readinessProbe must hit /readyz"

# --- graceful shutdown room -------------------------------------------------------
grace="$(awk '/terminationGracePeriodSeconds:/ {print $2; exit}' <<<"$api_deployment")"
(( grace >= 25 )) || fail "terminationGracePeriodSeconds is $grace; want at least 25 (drain delay + 2 x 10 s bounds)"

# A tuned config.* value reaches the ConfigMap the api mounts.
tuned="$(render --set config.outboxRelayInterval=7s --set config.outboxMaxAttempts=5 --show-only templates/configmap.yaml)"
grep -q 'OUTBOX_RELAY_INTERVAL: "7s"' <<<"$tuned" || fail "config.outboxRelayInterval is not rendered into the ConfigMap"
grep -q 'OUTBOX_MAX_ATTEMPTS: "5"' <<<"$tuned" || fail "config.outboxMaxAttempts is not rendered into the ConfigMap"
# D12 (2026-10-12): the outbox relay attempt cap defaults to the fleet's 20.
grep -q 'OUTBOX_MAX_ATTEMPTS: "20"' <<<"$configmap" || fail "OUTBOX_MAX_ATTEMPTS must default to 20 (ADR-0036)"

# D24 (2026-10-12): the chart wires the installed-capacity edge by default.
# The binary defaults INSTALLED_CAPACITY_MODE to "permissive", which fails
# EVERY shift-plan commit (ADR-0014); the chart defaults to "http" against
# fulfillment-execution's in-cluster Service (port 80) in the release
# namespace, and both stay overridable. A REST sync edge: out of ADR-0038's scope.
grep -q 'INSTALLED_CAPACITY_MODE: "http"' <<<"$configmap" \
  || fail "INSTALLED_CAPACITY_MODE must default to http in the chart"
ns_render="$(render --namespace warehouse-systems --show-only templates/configmap.yaml)"
grep -q 'FULFILLMENT_EXECUTION_BASE_URL: "http://fulfillment-execution.warehouse-systems.svc.cluster.local:80"' <<<"$ns_render" \
  || fail "FULFILLMENT_EXECUTION_BASE_URL must default to fulfillment-execution's in-cluster Service in the release namespace"
overridden="$(render --set config.installedCapacityMode=permissive --set config.fulfillmentExecutionBaseUrl=http://fe.example:8080 --show-only templates/configmap.yaml)"
grep -q 'INSTALLED_CAPACITY_MODE: "permissive"' <<<"$overridden" || fail "config.installedCapacityMode is not rendered"
grep -q 'FULFILLMENT_EXECUTION_BASE_URL: "http://fe.example:8080"' <<<"$overridden" || fail "config.fulfillmentExecutionBaseUrl is not rendered"
# They reach the api only through the ConfigMap: an explicit env entry would
# collide with warehouse-infra's extraEnv for the same names (a duplicate env
# entry breaks ArgoCD's diff), while envFrom lets extraEnv win.
if grep -qE 'name: (INSTALLED_CAPACITY_MODE|FULFILLMENT_EXECUTION_BASE_URL)$' <<<"$api_deployment"; then
  fail "INSTALLED_CAPACITY_MODE / FULFILLMENT_EXECUTION_BASE_URL must come from the ConfigMap, not an explicit env entry"
fi

# The other workloads read their own explicit env and must NOT inherit the
# api's HTTP_ADDR (reports would bind :8080) or OTEL_SERVICE_NAME from it.
for template in reports-deployment.yaml projector-deployment.yaml mcp-deployment.yaml; do
  rendered="$(render --set mcp.enabled=true "${analytics[@]}" --show-only "templates/$template")"
  if grep -q 'configMapRef:' <<<"$rendered"; then
    fail "$template must not mount the api ConfigMap"
  fi
done

# --- the mandatory env of every binary is rendered --------------------------------
mcp_dep="$(render --set mcp.enabled=true --show-only templates/mcp-deployment.yaml)"
grep -A4 'name: DATABASE_URL$' <<<"$mcp_dep" | grep -q 'secretKeyRef:' || fail "the mcp DATABASE_URL must come from a Secret"
[[ "$(env_value "$mcp_dep" KAFKA_BROKERS)" == "kafka.example.invalid:9092" ]] || fail "the mcp KAFKA_BROKERS is not rendered from kafka.brokers"
[[ "$(env_value "$mcp_dep" EVENT_PUBLISHER)" == "kafka" ]] || fail "the mcp EVENT_PUBLISHER must be kafka"
proj="$(render "${analytics[@]}" --show-only templates/projector-deployment.yaml)"
[[ "$(env_value "$proj" ANALYTICS_CONSUMER_GROUP)" == "workforce-analytics" ]] || fail "ANALYTICS_CONSUMER_GROUP must default to the projector's historical group workforce-analytics"
[[ "$(env_value "$proj" KAFKA_BROKERS)" == "kafka.example.invalid:9092" ]] || fail "the projector KAFKA_BROKERS is not rendered from kafka.brokers"
renamed="$(render "${analytics[@]}" --set analytics.projector.consumerGroup=g-analytics --show-only templates/projector-deployment.yaml)"
[[ "$(env_value "$renamed" ANALYTICS_CONSUMER_GROUP)" == "g-analytics" ]] || fail "analytics.projector.consumerGroup does not reach ANALYTICS_CONSUMER_GROUP"

# --- chart vs binary --------------------------------------------------------------
# Every mandatory variable a binary's boot config demands (ADR-0038) is
# rendered by its Deployment: a new mandatory variable fails until the chart
# carries it.
for var in DATABASE_URL KAFKA_BROKERS EVENT_PUBLISHER; do
  grep -q "name: $var\$" <<<"$api_deployment" || fail "the api Deployment does not render $var"
  grep -q "name: $var\$" <<<"$mcp_dep" || fail "the mcp Deployment does not render $var"
done
grep -q 'name: ANALYTICS_DATABASE_URL$' <<<"$proj" || fail "the projector Deployment does not render ANALYTICS_DATABASE_URL"
proj_vars="$(grep -ho 'errors.New("[A-Z_]* is required' "$repo_dir"/cmd/workforce-projector/main.go | sed -E 's/errors.New\("//; s/ is required//' | sort -u)"
[[ -n "$proj_vars" ]] || fail "found no mandatory variables in cmd/workforce-projector; the scan is looking in the wrong place"
for var in $proj_vars; do
  grep -q "name: $var\$" <<<"$proj" || fail "the projector Deployment does not render $var, which cmd/workforce-projector requires"
done

# --- refusals: the mandatory values name themselves --------------------------------
expect_refusal() { # expect_refusal <why> <needle> <helm args...>
  local why="$1" needle="$2" err; shift 2
  if err="$(helm template "$release" "$chart_dir" "$@" 2>&1)"; then
    fail "the chart must refuse to render: $why"
  fi
  grep -qF -- "$needle" <<<"$err" || fail "the refusal for '$why' does not name '$needle':
$err"
}
base=("${required[@]}")
expect_refusal "no database source and no brokers" "database.url or database.existingSecret"
expect_refusal "no database source" "database.url or database.existingSecret" --set kafka.brokers=k:9092
expect_refusal "no brokers" "kafka.brokers" --set database.url=postgres://u@h/db
expect_refusal "the log publisher" "config.eventPublisher" "${base[@]}" --set config.eventPublisher=log
expect_refusal "an unknown publisher" "the only publisher" "${base[@]}" --set config.eventPublisher=rabbit
expect_refusal "kafka switched off" "Kafka cannot be switched off" "${base[@]}" --set kafka.enabled=false
expect_refusal "the retired file catalogue" "pathCatalogue.enabled" "${base[@]}" --set pathCatalogue.enabled=true
expect_refusal "a short grace period" "terminationGracePeriodSeconds" "${base[@]}" --set terminationGracePeriodSeconds=10
expect_refusal "permissive labor-performance through extraEnv" "LABOR_PERFORMANCE_MODE" "${base[@]}" --set 'extraEnv[0].name=LABOR_PERFORMANCE_MODE' --set 'extraEnv[0].value=permissive'
expect_refusal "the file catalogue source through extraEnv" "PATH_CATALOGUE_SOURCE" "${base[@]}" --set 'extraEnv[0].name=PATH_CATALOGUE_SOURCE' --set 'extraEnv[0].value=file'
expect_refusal "PATH_CATALOGUE_FILE through extraEnv" "PATH_CATALOGUE_FILE" "${base[@]}" --set 'extraEnv[0].name=PATH_CATALOGUE_FILE' --set 'extraEnv[0].value=/x.yaml'
expect_refusal "the log publisher through extraEnv" "EVENT_PUBLISHER" "${base[@]}" --set 'extraEnv[0].name=EVENT_PUBLISHER' --set 'extraEnv[0].value=log'
expect_refusal "an unknown *_MODE through extraEnv" "*_MODE" "${base[@]}" --set 'extraEnv[0].name=LAYOUT_MODE' --set 'extraEnv[0].value=permissive'
expect_refusal "analytics without a DSN source" "analytics.database.projectorUrl" "${base[@]}" --set analytics.enabled=true
expect_refusal "analytics without a consumer group" "analytics.projector.consumerGroup" "${base[@]}" "${analytics[@]}" --set analytics.projector.consumerGroup=

# --- what warehouse-infra sets keeps working -----------------------------------------
# tests/warehouse-infra-values.yaml mirrors helm-values/workforce-management.yaml
# + _platform.yaml of warehouse-infra: kafka.enabled=true, eventPublisher=kafka,
# the extraEnv entries (PATH_CATALOGUE_SOURCE=kafka, the REST sync-edge
# INSTALLED_CAPACITY_MODE=http, LABOR_PERFORMANCE_MODE=kafka-cache), the retired
# pathCatalogue keys, the existing database secret and the platform brokers.
infra=(-f "$chart_dir/tests/warehouse-infra-values.yaml")
infra_api="$(helm template "$release" "$chart_dir" "${infra[@]}" --show-only templates/deployment.yaml)" \
  || fail "the warehouse-infra values no longer render"
[[ "$(env_value "$infra_api" KAFKA_BROKERS)" == "kafka.warehouse-systems.svc.cluster.local:9092" ]] || fail "infra values: KAFKA_BROKERS"
[[ "$(env_value "$infra_api" PATH_CATALOGUE_SOURCE)" == "kafka" ]] || fail "infra values: PATH_CATALOGUE_SOURCE=kafka must reach the api"
[[ "$(env_value "$infra_api" LABOR_PERFORMANCE_MODE)" == "kafka-cache" ]] || fail "infra values: LABOR_PERFORMANCE_MODE=kafka-cache must reach the api"
helm template "$release" "$chart_dir" "${infra[@]}" --show-only templates/mcp-deployment.yaml >/dev/null || fail "infra values: mcp"
helm template "$release" "$chart_dir" "${infra[@]}" --show-only templates/projector-deployment.yaml >/dev/null || fail "infra values: projector"

echo "PASS: workforce-management chart render assertions"
