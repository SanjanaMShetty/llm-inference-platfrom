# LLM inference platform

A Kubernetes-native platform for serving LLMs: a Go operator, a multi-tenant Go
gateway, and the production AI stack (vLLM, KEDA, DRA, llm-d) wired together and
measured. See the project plan for the milestone breakdown.

**Status:** M0 (foundations) — repo scaffold, CI, local cluster, and a fake
OpenAI-compatible streaming backend used as the test double for everything later.

## Quick start (no GPU needed)

```bash
make check          # gofmt check, go vet, unit tests with -race
make build          # bin/fakebackend and bin/ttftprobe
make run-fake       # fake backend on :8000

# in another terminal
curl -N http://localhost:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"fake-llm","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}'

./bin/ttftprobe -url http://localhost:8000 -n 5 -expect-ttft 500ms -expect-rate 50
```

Kubernetes (kind):

```bash
make cluster        # create the kind cluster
make deploy         # build, load and deploy the fake backend
make smoke          # end-to-end check through a port-forward
make clean          # delete the cluster
```

Run `make help` for every target. Step-by-step verification of M0 is in
[docs/M0.md](docs/M0.md).

## Layout

```
cmd/fakebackend/        fake OpenAI-compatible server (SSE, delays, queue gauges)
cmd/ttftprobe/          measures time-to-first-token and decode rate
internal/fakebackend/   the server implementation and its tests
deploy/                 Kubernetes manifests
hack/                   kind config, smoke test, environment capture
docs/adr/               architecture decision records
experiments/            raw results, one directory per experiment
CLAIMS.md               every public claim mapped to the experiment that backs it
```

## Fake backend knobs

Flags or environment variables: `FAKE_FIRST_TOKEN_DELAY`, `FAKE_TOKENS_PER_SECOND`,
`FAKE_MAX_CONCURRENCY`, `FAKE_DEFAULT_MAX_TOKENS`, `FAKE_MODEL`, `FAKE_ADDR`.

Per-request overrides (headers): `X-Fake-First-Token-Delay` (e.g. `250ms`),
`X-Fake-Token-Rate` (tokens/s), `X-Fake-Fail-After-Tokens` (streaming only: drop
the connection after N tokens, no `[DONE]`).

The `vllm:*` metric names mimic vLLM's gauges. They are not guaranteed to match
the vLLM version you pin later; reconcile them in M4.
