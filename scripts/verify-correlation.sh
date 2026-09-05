#!/usr/bin/env bash
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
SERVICE="${DOCTOR_SERVICE:-}"
LIMIT="${VERIFY_LIMIT:-200}"
FROM="${VERIFY_FROM:-$(date -u -d '-1 hour' +%Y-%m-%dT%H:%M:%SZ)}"
TO="${VERIFY_TO:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
TRACE_ID="${TRACE_ID:-${1:-}}"

enc() {
  jq -rn --arg v "$1" '$v|@uri'
}

fail() {
  echo "verification: FAIL - $1" >&2
  exit 1
}

echo "verification: base_url=${BASE_URL} tenant=${TENANT} from=${FROM} to=${TO}"

if [[ -z "${TRACE_ID}" ]]; then
  recent_url="${BASE_URL}/v1/traces/recent?tenant=$(enc "${TENANT}")&from=$(enc "${FROM}")&to=$(enc "${TO}")&limit=50"
  if [[ -n "${SERVICE}" ]]; then
    recent_url+="&service=$(enc "${SERVICE}")"
  fi
  recent_json="$(curl -fsS "${recent_url}")"
  TRACE_ID="$(jq -r '.traces[0].trace_id // ""' <<<"${recent_json}")"
fi

[[ -n "${TRACE_ID}" ]] || fail "no trace_id found (provide TRACE_ID or ensure recent traces exist)"

trace_url="${BASE_URL}/v1/traces/${TRACE_ID}?tenant=$(enc "${TENANT}")"
trace_json="$(curl -fsS "${trace_url}")"
trace_span_count="$(jq '[.spans[]?] | length' <<<"${trace_json}")"
service_detected="$(jq -r '.spans[0].service // ""' <<<"${trace_json}")"
span_id="$(jq -r '.spans[]? | .span_id // empty' <<<"${trace_json}" | head -n1)"

[[ "${trace_span_count}" -gt 0 ]] || fail "trace ${TRACE_ID} returned 0 spans"
if [[ -z "${SERVICE}" && -n "${service_detected}" ]]; then
  SERVICE="${service_detected}"
fi

logs_url="${BASE_URL}/v1/logs/search?tenant=$(enc "${TENANT}")&from=$(enc "${FROM}")&to=$(enc "${TO}")&limit=$(enc "${LIMIT}")&q=$(enc "${TRACE_ID}")"
if [[ -n "${SERVICE}" ]]; then
  logs_url+="&service=$(enc "${SERVICE}")"
fi
logs_json="$(curl -fsS "${logs_url}")"
log_returned="$(jq '.returned // 0' <<<"${logs_json}")"
log_trace_exact="$(jq --arg t "${TRACE_ID}" '[.results[]? | select(.trace_id == $t)] | length' <<<"${logs_json}")"
log_span_exact="0"
if [[ -n "${span_id}" ]]; then
  log_span_exact="$(jq --arg s "${span_id}" '[.results[]? | select(.span_id == $s)] | length' <<<"${logs_json}")"
fi

selector_trace="{trace_id=\"${TRACE_ID}\"}"
series_trace_url="${BASE_URL}/api/v1/series?tenant=$(enc "${TENANT}")&start=$(enc "${FROM}")&end=$(enc "${TO}")&match[]=$(enc "${selector_trace}")"
series_trace_json="$(curl -fsS "${series_trace_url}")"
metric_trace_series="$(jq '.data | length' <<<"${series_trace_json}")"
metric_trace_names="$(jq -r '.data[]?.__name__ // empty' <<<"${series_trace_json}" | sort -u | tr '\n' ',' | sed 's/,$//')"

metric_span_series="0"
if [[ -n "${span_id}" ]]; then
  selector_span="{span_id=\"${span_id}\"}"
  series_span_url="${BASE_URL}/api/v1/series?tenant=$(enc "${TENANT}")&start=$(enc "${FROM}")&end=$(enc "${TO}")&match[]=$(enc "${selector_span}")"
  series_span_json="$(curl -fsS "${series_span_url}")"
  metric_span_series="$(jq '.data | length' <<<"${series_span_json}")"
fi

echo "trace_id: ${TRACE_ID}"
echo "service: ${SERVICE:-unknown}"
echo "spans_in_trace: ${trace_span_count}"
echo "logs_returned: ${log_returned}"
echo "logs_with_exact_trace_id: ${log_trace_exact}"
if [[ -n "${span_id}" ]]; then
  echo "sample_span_id: ${span_id}"
  echo "logs_with_sample_span_id: ${log_span_exact}"
fi
echo "metric_series_with_trace_id: ${metric_trace_series}"
if [[ -n "${span_id}" ]]; then
  echo "metric_series_with_sample_span_id: ${metric_span_series}"
fi
echo "metric_names_with_trace_id: ${metric_trace_names:-none}"

if [[ "${log_trace_exact}" -le 0 ]]; then
  fail "no logs matched trace_id=${TRACE_ID}"
fi
if [[ "${metric_trace_series}" -le 0 ]]; then
  fail "no metric series carried trace_id=${TRACE_ID} (ensure exemplars are enabled/exported)"
fi

echo "verification: PASS - trace/log/metric correlation found for trace_id=${TRACE_ID}"
