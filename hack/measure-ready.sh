#!/usr/bin/env bash
# Measures the time from `kubectl apply` of an InferenceService to status.phase=Ready
# on the kind cluster, over several runs.
#
# Usage: hack/measure-ready.sh [runs]     (the operator must be running)
set -euo pipefail

RUNS="${1:-5}"
CLUSTER_NAME="${CLUSTER_NAME:-llm-platform}"
CTX="kind-${CLUSTER_NAME}"
NS="${E2E_NAMESPACE:-m1-measure}"
RES="inferenceservices.inference.llmplatform.dev"
K=(kubectl --context "$CTX")

now() { python3 -c 'import time; print(time.time())' 2>/dev/null || date +%s.%N; }

"${K[@]}" get crd "$RES" >/dev/null 2>&1 || { echo "the CRD is not installed: run 'make install-crds'" >&2; exit 1; }
"${K[@]}" create namespace "$NS" --dry-run=client -o yaml | "${K[@]}" apply -f - >/dev/null

times=()
for i in $(seq 1 "$RUNS"); do
  name="measure-$i"
  start="$(now)"
  cat <<YAML | "${K[@]}" -n "$NS" apply -f - >/dev/null
apiVersion: inference.llmplatform.dev/v1alpha1
kind: InferenceService
metadata:
  name: $name
spec:
  model: fake-llm
  backend: fake
  image: fakebackend:dev
  weightLoadTimeSeconds: 30
YAML
  "${K[@]}" -n "$NS" wait --for=jsonpath='{.status.phase}'=Ready "$RES/$name" --timeout=180s >/dev/null
  end="$(now)"
  elapsed="$(awk -v s="$start" -v e="$end" 'BEGIN { printf "%.2f", e - s }')"
  echo "run $i: apply -> Ready in ${elapsed}s"
  times+=("$elapsed")
  "${K[@]}" -n "$NS" delete "$RES" "$name" --wait >/dev/null
  "${K[@]}" -n "$NS" wait --for=delete "deployment/$name" --timeout=90s >/dev/null 2>&1 || true
done

printf '%s\n' "${times[@]}" | sort -n | awk '
  { a[NR] = $1 }
  END {
    median = (NR % 2) ? a[(NR + 1) / 2] : (a[NR / 2] + a[NR / 2 + 1]) / 2
    printf "\napply -> Ready over %d runs: median %.2fs, min %.2fs, max %.2fs\n", NR, median, a[1], a[NR]
  }'
"${K[@]}" delete namespace "$NS" --wait=false >/dev/null 2>&1 || true
