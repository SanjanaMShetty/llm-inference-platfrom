# Experiments

One directory per experiment: `experiments/<milestone>-<short-name>/`.

Each directory contains:

- `README.md`: the question, the exact commands, the results table, and what the
  results do **not** show. Start from `_template/README.md`.
- `environment.txt`: output of `hack/capture-env.sh`, captured when the run happened.
- `raw/`: unprocessed output (probe logs, metric dumps, CSVs). Commit these.
- Any script used to turn raw data into the numbers in the README.

## Rules

- Record the environment with every number. A number without hardware and version
  context is not evidence.
- Do a warm-up and say how many requests it was. Discard warm-up results.
- Repeat runs (at least 3, prefer 5) and report median and range.
- Percentiles need enough samples: p95 wants hundreds of requests, p99 thousands.
- Compare configurations with matched, alternating runs to reduce drift.
- Keep negative and null results.
- Label cost figures as estimates unless they come from billing data.
- Update `CLAIMS.md` when a result lands.
