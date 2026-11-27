#!/usr/bin/env bash
# Smoke test for the fake backend deployed in the kind cluster.
# Requires: kubectl, curl, go (for ttftprobe).
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-llm-platform}"
NAMESPACE="${NAMESPACE:-llm-platform}"
LOCAL_PORT="${LOCAL_PORT:-18000}"
CTX="kind-${CLUSTER_NAME}"
BASE="http://127.0.0.1:${LOCAL_PORT}"

# Must match deploy/fakebackend.yaml.
EXPECT_TTFT="${EXPECT_TTFT:-500ms}"
EXPECT_RATE="${EXPECT_RATE:-50}"

fail() { echo "SMOKE FAILED: $*" >&2; exit 1; }

kubectl --context "$CTX" -n "$NAMESPACE" port-forward svc/fakebackend "${LOCAL_PORT}:80" >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "$PF_PID" 2>/dev/null || true' EXIT

for _ in $(seq 1 30); do
  if curl -fsS "${BASE}/healthz" >/dev/null 2>&1; then break; fi
  sleep 1
done
curl -fsS "${BASE}/healthz" >/dev/null || fail "healthz did not become ready"
echo "ok   healthz"

curl -fsS "${BASE}/v1/models" | grep -q '"fake-llm"' || fail "/v1/models did not list fake-llm"
echo "ok   /v1/models"

# -N disables curl's own buffering so events print as they arrive.
STREAM="$(curl -fsSN -H 'Content-Type: application/json' \
  -d '{"model":"fake-llm","stream":true,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}' \
  "${BASE}/v1/chat/completions")" || fail "streaming request failed"
grep -q '^data: \[DONE\]' <<<"$STREAM" || fail "stream did not end with [DONE]"
[ "$(grep -c '"content":"tok' <<<"$STREAM")" -eq 5 ] || fail "expected 5 content chunks"
echo "ok   streaming chat completion (5 tokens, ends with [DONE])"

METRICS="$(curl -fsS "${BASE}/metrics")"
for name in vllm:num_requests_running vllm:num_requests_waiting vllm:gpu_cache_usage_perc fakebackend_requests_total; do
  grep -q "^${name} " <<<"$METRICS" || fail "metric ${name} missing"
done
echo "ok   /metrics exposes queue gauges"

# Validate the test double: measured TTFT and rate must match the configuration.
go run ./cmd/ttftprobe -url "$BASE" -n 5 -max-tokens 32 \
  -expect-ttft "$EXPECT_TTFT" -expect-rate "$EXPECT_RATE" || fail "ttftprobe expectations not met"

echo "SMOKE OK"
