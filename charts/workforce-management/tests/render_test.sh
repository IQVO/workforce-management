#!/usr/bin/env bash
# Chart render assertions for charts/workforce-management (run in CI by the
# chart-tests job, next to test_service_selectors.py).
#
# Run: bash charts/workforce-management/tests/render_test.sh
set -euo pipefail

chart_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
release=workforce-management
# The chart refuses to render without a database URL; it is never dialled.
required=(--set "database.url=postgres://u:p@example.invalid:5432/db")

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

render() {
  helm template "$release" "$chart_dir" "${required[@]}" "$@"
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
# namespace, and both stay overridable.
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
  rendered="$(render --set analytics.enabled=true --set mcp.enabled=true --set analytics.database.projectorUrl=postgres://projector --show-only "templates/$template")"
  if grep -q 'configMapRef:' <<<"$rendered"; then
    fail "$template must not mount the api ConfigMap"
  fi
done

echo "PASS: workforce-management chart render assertions"
