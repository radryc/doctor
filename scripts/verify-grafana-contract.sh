#!/usr/bin/env bash
# Verifies the Grafana datasource contract for doctor-query: every endpoint and
# query shape Grafana 12's Prometheus, Loki, and Tempo datasources (including
# the Traces/Logs/Metrics Drilldown apps) send must return a decodable JSON
# response - never the SPA shell, never a 404.
#
# Usage:
#   DOCTOR_QUERY_URL=http://localhost:18080 ./scripts/verify-grafana-contract.sh
set -euo pipefail

if ! command -v curl >/dev/null 2>&1; then
  echo "error: curl is required" >&2
  exit 2
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "error: jq is required" >&2
  exit 2
fi

BASE_URL="${DOCTOR_QUERY_URL:-http://localhost:18080}"
TENANT="${DOCTOR_TENANT:-default}"
NOW="$(date +%s)"
FROM="$((NOW - 3600))"
STEP="60"
PASS=0
FAIL=0

enc() {
  jq -rn --arg v "$1" '$v|@uri'
}

# check_json NAME URL [EXTRA_HEADER]
# Requires: HTTP 200, JSON content type, parseable body (never HTML).
check_json() {
  local name="$1" url="$2" extra_header="${3:-}"
  local body status content_type
  if [[ -n "${extra_header}" ]]; then
    body="$(curl -sS -m 30 -H "${extra_header}" -H "X-Doctor-Tenant: ${TENANT}" -w $'\n%{http_code} %{content_type}' "${url}" 2>&1)" || true
  else
    body="$(curl -sS -m 30 -H "X-Doctor-Tenant: ${TENANT}" -w $'\n%{http_code} %{content_type}' "${url}" 2>&1)" || true
  fi
  status="$(tail -1 <<<"${body}" | awk '{print $1}')"
  content_type="$(tail -1 <<<"${body}" | awk '{print $2}')"
  local payload
  payload="$(sed '$d' <<<"${body}")"

  if [[ "${status}" != "200" ]]; then
    echo "FAIL ${name}: HTTP ${status} (${url})"
    echo "     body: $(head -c 200 <<<"${payload}")"
    FAIL=$((FAIL + 1))
    return 1
  fi
  if [[ "${content_type}" == text/html* ]]; then
    echo "FAIL ${name}: served HTML instead of JSON (${url})"
    FAIL=$((FAIL + 1))
    return 1
  fi
  if ! jq -e . >/dev/null 2>&1 <<<"${payload}"; then
    echo "FAIL ${name}: body is not JSON (${url})"
    echo "     body: $(head -c 200 <<<"${payload}")"
    FAIL=$((FAIL + 1))
    return 1
  fi
  echo "ok   ${name}"
  PASS=$((PASS + 1))
  return 0
}

echo "grafana-contract: base_url=${BASE_URL} tenant=${TENANT}"

# --- datasource health checks ------------------------------------------------
check_json "prometheus buildinfo" "${BASE_URL}/api/v1/status/buildinfo"
check_json "loki query (health)" "${BASE_URL}/loki/api/v1/query?query=$(enc 'vector(1)+vector(1)')"
check_json "tempo echo" "${BASE_URL}/api/echo"

# --- Tempo search (Traces Drilldown) -----------------------------------------
search_url() {
  local query="$1" extra="${2:-}"
  echo "${BASE_URL}/api/search?q=$(enc "${query}")&start=${FROM}&end=${NOW}&limit=20${extra}"
}
check_json "tempo search {}" "$(search_url '{}')"
check_json "tempo search root spans" "$(search_url '{nestedSetParent<0 && true}')"
check_json "tempo search service present" "$(search_url '{resource.service.name != nil}')"
check_json "tempo search span sets" "$(search_url '{nestedSetParent<0 && true}' '&spss=10')"

# --- Tempo tag discovery (v1 + v2) -------------------------------------------
check_json "tempo tags v1" "${BASE_URL}/api/search/tags"
check_json "tempo tags v2" "${BASE_URL}/api/v2/search/tags"
check_json "tempo tag values v2" "${BASE_URL}/api/v2/search/tag/service.name/values"

# --- Tempo metrics (breakdown charts, node graph) ----------------------------
metrics_url() {
  local query="$1"
  echo "${BASE_URL}/api/metrics/query_range?q=$(enc "${query}")&start=${FROM}&end=${NOW}&step=${STEP}"
}
check_json "tempo metrics rate by service" \
  "$(metrics_url '{nestedSetParent<0 && true && resource.service.name != nil} | rate() by(resource.service.name)')"
check_json "tempo metrics quantile" \
  "$(metrics_url '{} | quantile_over_time(duration, 0.9)')"
check_json "tempo metrics instant" \
  "${BASE_URL}/api/metrics/query?q=$(enc '{} | rate()')&start=${FROM}&end=${NOW}"

# --- Loki (Explore + Logs Drilldown) -----------------------------------------
loki_range="${BASE_URL}/loki/api/v1/query_range?query=$(enc '{service=~".+"}')&start=${FROM}&end=${NOW}&limit=10"
check_json "loki query_range" "${loki_range}"
volume_range="${BASE_URL}/loki/api/v1/index/volume_range?query=$(enc '{service=~".+"}')&start=${FROM}&end=${NOW}&step=${STEP}&targetLabels=level"
check_json "loki volume_range" "${volume_range}"
check_json "loki index stats" \
  "${BASE_URL}/loki/api/v1/index/stats?query=$(enc '{service=~".+"}')&start=${FROM}&end=${NOW}"
check_json "loki patterns" "${BASE_URL}/loki/api/v1/patterns?query=$(enc '{service=~".+"}')&start=${FROM}&end=${NOW}"
check_json "loki detected_fields" "${BASE_URL}/loki/api/v1/detected_fields?query=$(enc '{service=~".+"}')"
check_json "loki detected_labels" "${BASE_URL}/loki/api/v1/detected_labels"
check_json "loki buildinfo" "${BASE_URL}/loki/api/v1/status/buildinfo"

# --- Prometheus ---------------------------------------------------------------
check_json "prometheus query" "${BASE_URL}/api/v1/query?query=$(enc 'up')&time=${NOW}"
check_json "prometheus query_range" \
  "${BASE_URL}/api/v1/query_range?query=$(enc 'up')&start=${FROM}&end=${NOW}&step=${STEP}"
check_json "prometheus labels" "${BASE_URL}/api/v1/labels"
check_json "prometheus label values" "${BASE_URL}/api/v1/label/__name__/values"
check_json "prometheus series" "${BASE_URL}/api/v1/series?match%5B%5D=$(enc '{__name__=~".+"}')&start=${FROM}&end=${NOW}"
check_json "prometheus metadata" "${BASE_URL}/api/v1/metadata"
check_json "prometheus exemplars" "${BASE_URL}/api/v1/query_exemplars?query=$(enc 'up')&start=${FROM}&end=${NOW}"
check_json "prometheus rules" "${BASE_URL}/api/v1/rules"

# --- unregistered API paths must 404, never serve HTML -----------------------
for path in "/loki/api/v1/not-a-route" "/api/v2/not-a-route"; do
  status="$(curl -sS -m 10 -o /dev/null -w '%{http_code}' -H 'Accept: application/json' "${BASE_URL}${path}")"
  if [[ "${status}" != "404" ]]; then
    echo "FAIL unregistered ${path}: HTTP ${status}, want 404"
    FAIL=$((FAIL + 1))
  else
    echo "ok   unregistered ${path} -> 404"
    PASS=$((PASS + 1))
  fi
done

echo ""
echo "grafana-contract: ${PASS} passed, ${FAIL} failed"
if [[ "${FAIL}" -gt 0 ]]; then
  exit 1
fi
