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
NAMESPACE="${AWS_NAMESPACE:-AWS/ECS}"
METRIC="${AWS_METRIC:-CPUUtilization}"

enc() { jq -rn --arg v "$1" '$v|@uri'; }

fail() {
  echo "aws federation: FAIL - $1" >&2
  exit 1
}

echo "aws federation: base_url=${BASE_URL} tenant=${TENANT} namespace=${NAMESPACE} metric=${METRIC}"

sources_url="${BASE_URL}/api/v1/label/source/values?tenant=$(enc "${TENANT}")"
sources_json="$(curl -fsS "${sources_url}")"
if ! jq -e '.data | index("aws")' <<<"${sources_json}" >/dev/null; then
  fail "label source does not include \"aws\"; is DOCTOR_AWS_ENABLED set and data flowing?"
fi
echo "source label values: $(jq -c '.data' <<<"${sources_json}")"

selector="{source=\"aws\", aws_namespace=\"${NAMESPACE}\"}"
series_url="${BASE_URL}/api/v1/series?tenant=$(enc "${TENANT}")&match[]=$(enc "${selector}")"
series_json="$(curl -fsS "${series_url}")"
series_count="$(jq '.data | length' <<<"${series_json}")"
echo "series with selector ${selector}: ${series_count}"

metric_url="${BASE_URL}/api/v1/query?tenant=$(enc "${TENANT}")&query=$(enc "${METRIC}{source=\"aws\", aws_namespace=\"${NAMESPACE}\"}")"
metric_json="$(curl -fsS "${metric_url}")"
if ! jq -e '.status == "success"' <<<"${metric_json}" >/dev/null; then
  fail "metric query for ${METRIC} did not succeed: $(jq -c '.' <<<"${metric_json}")"
fi
echo "metric result count: $(jq '.data.result | length' <<<"${metric_json}")"

services_url="${BASE_URL}/api/search/tag/service.name/values?tenant=$(enc "${TENANT}")"
services_json="$(curl -fsS "${services_url}")"
if ! jq -e '.tagValues | length > 0' <<<"${services_json}" >/dev/null; then
  fail "no Tempo trace services returned (expected native and/or X-Ray services)"
fi
echo "tempo trace services: $(jq -c '.tagValues' <<<"${services_json}")"

if [[ "${series_count}" -le 0 ]]; then
  echo "aws federation: WARN - no AWS metric series matched; check the namespace/metric and IAM permissions"
fi

echo "aws federation: PASS - AWS read surface is reachable through Doctor"
