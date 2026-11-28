#!/usr/bin/env bash
# End-to-end check of the operator on the kind cluster.
#
# Prerequisites:
#   make cluster load-image install-crds
#   the operator running: `make run-operator` in another terminal, or `make operator-deploy`
#
# Requires: kubectl, curl, go (for ttftprobe).
set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-llm-platform}"
CTX="kind-${CLUSTER_NAME}"
NS="${E2E_NAMESPACE:-m1-e2e}"
LOCAL_PORT="${LOCAL_PORT:-18001}"
RES="inferenceservices.inference.llmplatform.dev"
K=(kubectl --context "$CTX")
PF_PID=""

pass() { echo "ok   $*"; }
fail() { echo "FAIL $*" >&2; exit 1; }
cleanup() {
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
  if [ -z "${E2E_KEEP:-}" ]; then
    "${K[@]}" delete namespace "$NS" --wait=false >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

phase_of() { "${K[@]}" -n "$NS" get "$RES" "$1" -o jsonpath='{.status.phase}' 2>/dev/null || true; }
ready_msg() { "${K[@]}" -n "$NS" get "$RES" "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}: {.status.conditions[?(@.type=="Ready")].message}' 2>/dev/null || true; }

wait_phase() { # name phase [timeout_seconds]
  local name=$1 want=$2 timeout=${3:-120} elapsed=0
  while [ "$elapsed" -lt "$timeout" ]; do
    if [ "$(phase_of "$name")" = "$want" ]; then return 0; fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
  fail "$name did not reach phase $want within ${timeout}s (phase is '$(phase_of "$name")': $(ready_msg "$name"))"
}

apply_isvc() { # name image [extra spec yaml, indented two spaces]
  cat <<YAML | "${K[@]}" -n "$NS" apply -f - >/dev/null
apiVersion: inference.llmplatform.dev/v1alpha1
kind: InferenceService
metadata:
  name: $1
spec:
  model: fake-llm
  backend: fake
  image: $2
  weightLoadTimeSeconds: 30
${3:-}
YAML
}

# ---- preflight -------------------------------------------------------------
"${K[@]}" get crd "$RES" >/dev/null 2>&1 || fail "the CRD is not installed: run 'make install-crds'"
if command -v docker >/dev/null 2>&1; then
  docker exec "${CLUSTER_NAME}-control-plane" crictl images 2>/dev/null | grep -q fakebackend \
    || echo "warn the fakebackend image may not be loaded into kind: run 'make load-image'"
fi
"${K[@]}" create namespace "$NS" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null

# ---- 1. admission: the API server rejects bad specs on its own -------------
echo "== admission (server-side dry run; the operator is not involved)"
for f in config/samples/invalid/*.yaml; do
  if out="$("${K[@]}" apply --dry-run=server -f "$f" 2>&1)"; then
    fail "$f was accepted but must be rejected"
  fi
  pass "rejected $(basename "$f"): $(echo "$out" | head -n 1 | cut -c1-170)"
done
for f in config/samples/inferenceservice_fake.yaml config/samples/inferenceservice_vllm_restricted.yaml; do
  "${K[@]}" apply --dry-run=server -f "$f" >/dev/null || fail "$f should be accepted"
  pass "accepted $(basename "$f")"
done

# ---- 2. healthy service: children, ownership, probes, traffic --------------
echo "== healthy service"
apply_isvc healthy fakebackend:dev
wait_phase healthy Ready 180
pass "healthy reached Ready: $(ready_msg healthy)"

owner="$("${K[@]}" -n "$NS" get deploy healthy -o jsonpath='{.metadata.ownerReferences[0].kind}')"
[ "$owner" = "InferenceService" ] || fail "Deployment owner is '$owner'"
owner="$("${K[@]}" -n "$NS" get svc healthy -o jsonpath='{.metadata.ownerReferences[0].kind}')"
[ "$owner" = "InferenceService" ] || fail "Service owner is '$owner'"
pass "Deployment and Service are owned by the InferenceService"

selector="$("${K[@]}" -n "$NS" get deploy healthy -o jsonpath='{.spec.selector.matchLabels}')"
[ "$selector" = '{"llm-platform.io/inference-service":"healthy"}' ] || fail "selector is $selector"
pass "selector holds only the service name"

containers="$("${K[@]}" -n "$NS" get deploy healthy -o jsonpath='{.spec.template.spec.containers[*].name}')"
[ "$containers" = "model-server" ] || fail "containers: $containers"
threshold="$("${K[@]}" -n "$NS" get deploy healthy -o jsonpath='{.spec.template.spec.containers[0].startupProbe.failureThreshold}')"
[ "$threshold" = "6" ] || fail "startup probe failureThreshold is $threshold, want 6 (30s load time / 5s period)"
pass "one container; startup probe sized from weightLoadTimeSeconds (6 x 5s)"

"${K[@]}" -n "$NS" port-forward svc/healthy "${LOCAL_PORT}:80" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:${LOCAL_PORT}/healthz" >/dev/null 2>&1; then break; fi
  sleep 1
done
go run ./cmd/ttftprobe -url "http://127.0.0.1:${LOCAL_PORT}" -n 3 -expect-ttft 500ms -expect-rate 50 >/dev/null \
  || fail "traffic through the operator-created Service did not behave like the fake backend"
pass "streaming requests work through the operator-created Service"
kill "$PF_PID" 2>/dev/null || true
PF_PID=""

# ---- 3. a bad rollout while a replica still serves: Degraded ---------------
echo "== degraded (bad image while the old replica still serves)"
"${K[@]}" -n "$NS" patch "$RES" healthy --type merge -p '{"spec":{"image":"fakebackend:does-not-exist"}}' >/dev/null
wait_phase healthy Degraded 180
pass "phase Degraded: $(ready_msg healthy)"
"${K[@]}" -n "$NS" patch "$RES" healthy --type merge -p '{"spec":{"image":"fakebackend:dev"}}' >/dev/null
wait_phase healthy Ready 180
pass "recovered to Ready after restoring the image"

# ---- 4. the operator does not fight an autoscaler --------------------------
echo "== autoscaler ownership of replicas"
"${K[@]}" -n "$NS" scale deployment/healthy --replicas=2 >/dev/null
"${K[@]}" -n "$NS" patch "$RES" healthy --type merge -p '{"spec":{"extraArgs":["-first-token-delay=200ms"]}}' >/dev/null
for _ in $(seq 1 30); do
  args="$("${K[@]}" -n "$NS" get deploy healthy -o jsonpath='{.spec.template.spec.containers[0].args[0]}')"
  if [ "$args" = "-first-token-delay=200ms" ]; then break; fi
  sleep 2
done
[ "$args" = "-first-token-delay=200ms" ] || fail "the operator did not apply the spec change (args: '$args')"
sleep 5
replicas="$("${K[@]}" -n "$NS" get deploy healthy -o jsonpath='{.spec.replicas}')"
[ "$replicas" = "2" ] || fail "replicas is $replicas: the operator reset what the autoscaler set"
pass "spec change applied and replicas stayed at 2 (operator did not fight the scale)"
"${K[@]}" -n "$NS" scale deployment/healthy --replicas=1 >/dev/null

# ---- 5. unschedulable: the scheduler's reason reaches status ---------------
echo "== unschedulable (asks for 1000 CPUs)"
apply_isvc unsched fakebackend:dev $'  resources:\n    cpu: "1000"'
wait_phase unsched Unschedulable 120
msg="$(ready_msg unsched)"
echo "$msg" | grep -q "Insufficient cpu" || fail "Ready message does not carry the scheduler's reason: $msg"
pass "phase Unschedulable: $msg"

# ---- 6. failing: image cannot be pulled ------------------------------------
echo "== failing (image that does not exist)"
apply_isvc badimage fakebackend:does-not-exist
wait_phase badimage Failing 180
pass "phase Failing: $(ready_msg badimage)"

# ---- 7. deletion is garbage collected --------------------------------------
echo "== deletion"
"${K[@]}" -n "$NS" delete "$RES" healthy >/dev/null
"${K[@]}" -n "$NS" wait --for=delete deployment/healthy --timeout=90s >/dev/null || fail "Deployment was not garbage collected"
"${K[@]}" -n "$NS" wait --for=delete service/healthy --timeout=90s >/dev/null || fail "Service was not garbage collected"
pass "deleting the InferenceService removed its Deployment and Service"

echo "E2E OK"
