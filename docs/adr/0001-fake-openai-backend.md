# ADR 0001: Use a fake OpenAI-compatible backend for development and CI

- Status: accepted
- Date: 2026-11-27

## Context

Almost everything in this platform (operator, gateway, quotas, tenant isolation,
SSE relay) is logic that does not depend on what a model says. A real model server
needs a GPU or is slow on CPU, makes tests flaky, and cannot be told to fail on cue.

## Decision

Write a small stdlib-only Go server that speaks the OpenAI chat-completions API,
streams server-sent events, and lets us configure first-token delay, decode rate,
concurrency, and mid-stream failure. It exposes `vllm:*`-style gauges so the
autoscaler and scheduler can be exercised without a GPU.

## Alternatives considered

- Ollama on CPU: real text, but slow, large, and not controllable enough for tests.
  It stays as an optional demo backend.
- A simulator from a serving project: possibly higher fidelity for scheduler
  telemetry. To be evaluated before M7; this server remains the default for logic tests.
- Mocking at the HTTP-client level: cannot exercise streaming, flushing,
  disconnects or backpressure, which are the hard parts of the gateway.

## Consequences

- Fast, deterministic, GPU-free tests and CI.
- Performance and cache-isolation claims cannot come from this backend. They need
  real vLLM and are produced in the later evidence milestone.
- The fake's own timing must be validated, or it is a test double nobody should
  trust. See `experiments/m0-fakebackend-validation`.
