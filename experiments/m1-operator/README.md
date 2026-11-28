# M1: core operator

## Question

Does the operator turn one InferenceService into a correct, self-reporting deployment, and
how much work does it do when nothing changes?

## Setup

- Commit: `<git sha>` (see `environment.txt`)
- Cluster for envtest numbers: envtest API server (no kubelet, no controllers)
- Cluster for kind numbers: `kind` 2 nodes, fake backend image, `weightLoadTimeSeconds: 30`
- Runs: 5 for the apply-to-Ready timing

## Commands

```bash
hack/capture-env.sh > experiments/m1-operator/environment.txt
mkdir -p experiments/m1-operator/raw

make cover                                   | tee experiments/m1-operator/raw/coverage.txt
make test-run RUN=TestAdmission              | tee experiments/m1-operator/raw/admission.txt
make test-run RUN=TestSteadyState            | tee experiments/m1-operator/raw/steady-state.txt
make test-run RUN=TestReconcileDurationReport | tee experiments/m1-operator/raw/reconcile-duration.txt

make cluster load-image install-crds
make measure-ready RUNS=5                    | tee experiments/m1-operator/raw/apply-to-ready.txt   # operator running
make e2e                                     | tee experiments/m1-operator/raw/e2e.txt
```

## Results

| Measurement | Value | Source |
|---|---|---|
| CEL rules in the CRD | 5 | `TestAdmission` log |
| CEL negative test cases | | `TestAdmission` log |
| Schema negative test cases | | `TestAdmission` log |
| Test functions, total | | `go test -v ./... \| grep -c -- '--- PASS'` |
| Statement coverage, `internal/controller` and `api` | | `make cover` |
| Steady-state API requests per reconcile | 2 apply requests, 0 status writes, 0 creates/updates/deletes | `TestSteadyState` log |
| resourceVersions changed in steady state | no | `TestSteadyState` |
| Reconcile duration, steady state (envtest, uncached) | p50 ___ p95 ___ | `TestReconcileDurationReport` |
| First reconcile duration (envtest) | ___ | `TestReconcileDurationReport` |
| `kubectl apply` to Ready on kind, median (min-max) of 5 | ___ (___-___) | `measure-ready.sh` |

## What this does not show

- Durations against envtest exclude scheduling, image pulls and probes; the kind number
  includes them.
- A real autoscaler (KEDA) has not been run yet; ownership is tested with another field
  manager and with `kubectl scale`.
- Nothing here was measured with a real GPU or a real vLLM image.
- Reconcile latency under many InferenceServices has not been tested.

## Raw data

- `raw/*.txt`
