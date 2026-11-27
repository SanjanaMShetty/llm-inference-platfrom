# M0 Fake Backend Validation

**Result: PASS**  
**Run date:** 2026-11-27  
**Purpose:** Validate the fake OpenAI-compatible backend locally and in kind, including streaming, configured first-token delay, token rate, and queue-like metrics.

## Environment

| Item | Recorded value |
|---|---|
| Runtime | Linux `amd64` in WSL |
| Go | `go1.27.4 linux/amd64` |
| kind node image | `kindest/node:v1.37.0` |
| kind cluster | `llm-platform`, control-plane and worker |
| Docker, kind CLI, kubectl versions | Not captured |
| Host OS release, CPU, memory | Not captured |
| Model backend | Fake backend, model name `fake-llm` |
| Configured first-token delay | 500 ms |
| Configured generation rate | 50 tokens/s |

## Checks run

| Check | Result | Evidence |
|---|---|---|
| `make check` | PASS | Formatting check, `go vet ./...`, and race-enabled Go tests completed successfully. `internal/fakebackend` passed in 2.179s. |
| `make build` | PASS | Built `bin/fakebackend` and `bin/ttftprobe`. |
| Local streamed chat completion | PASS | Returned 8 content chunks and ended with `data: [DONE]`. |
| Local `ttftprobe` | PASS | 5 sequential probes at concurrency 1; median TTFT 502.4 ms and median rate 49.9 tokens/s. Both were within the probe’s configured acceptance range. |
| `make image` | PASS | Built `fakebackend:dev`. The first build took 140.8 seconds while pulling/building image layers; this is not treated as a benchmark result. |
| `make cluster` | PASS | Created the `llm-platform` kind cluster using `kindest/node:v1.37.0`; cluster reported Ready after 10 seconds. |
| `make deploy` | PASS | Loaded the image, applied the namespace, Deployment and Service, and completed the Deployment rollout. |
| `make smoke` | PASS | Health, model listing, streaming completion, queue-like metrics, TTFT, and generation-rate checks passed. |

## Timing measurements

The probe accepts a ±25% range around the configured values.

### Local backend

- Samples: 5; concurrency: 1
- TTFT samples: 515.5, 517.2, 502.3, 502.4, 501.4 ms
- Median TTFT: **502.4 ms**
- Generation-rate samples: 50.8, 50.0, 49.9, 49.9, 49.9 tokens/s
- Median generation rate: **49.9 tokens/s**
- Result: **PASS**

### Backend in kind

- Samples: 5; concurrency: 1
- TTFT samples: 508.5, 504.3, 504.6, 503.3, 502.7 ms
- Median TTFT: **504.3 ms**
- Generation-rate samples: 50.0, 49.9, 50.0, 49.9, 49.9 tokens/s
- Median generation rate: **49.9 tokens/s**
- Result: **PASS**

## Conclusion

The fake backend built and ran locally and in kind. The smoke test confirmed the expected health and model endpoints, SSE completion behavior, and queue-like metrics. Across five sequential probes at concurrency 1, the measured median first-token delay and generation rate were within the configured acceptance ranges both locally and in kind.

These results validate the fake backend as an M0 test double under this setup. They do not measure real-model quality or performance, higher-concurrency behavior, or production capacity.

## Evidence and reproducibility

Commands run:

```bash
make check
make build
make run-fake
curl -N http://localhost:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"fake-llm","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}'
./bin/ttftprobe -url http://localhost:8000 -n 5 \
  -expect-ttft 500ms -expect-rate 50
make image
make cluster
make deploy
make smoke