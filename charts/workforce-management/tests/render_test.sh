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
tuned="$(render --set config.outboxRelayInterval=7s --show-only templates/configmap.yaml)"
grep -q 'OUTBOX_RELAY_INTERVAL: "7s"' <<<"$tuned" || fail "config.outboxRelayInterval is not rendered into the ConfigMap"

# The other workloads read their own explicit env and must NOT inherit the
# api's HTTP_ADDR (reports would bind :8080) or OTEL_SERVICE_NAME from it.
for template in reports-deployment.yaml projector-deployment.yaml mcp-deployment.yaml; do
  rendered="$(render --set analytics.enabled=true --set mcp.enabled=true --set analytics.database.projectorUrl=postgres://projector --show-only "templates/$template")"
  if grep -q 'configMapRef:' <<<"$rendered"; then
    fail "$template must not mount the api ConfigMap"
  fi
done

echo "PASS: workforce-management chart render assertions"
