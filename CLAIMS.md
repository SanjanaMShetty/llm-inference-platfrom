# Claims ledger

Every claim made in the README, a blog post, or an interview must appear here with
the experiment that backs it. Delete claims you cannot back. Status is one of:
`planned`, `measured`, `dropped`.

| ID | Claim | Experiment | Status | Raw data |
|----|-------|------------|--------|----------|
| C-001 | The fake backend's median TTFT and decode rate match its configuration within 25% | `experiments/m0-fakebackend-validation` | measured | |
| C-002 | A client disconnect releases the backend slot and is counted | `internal/fakebackend` tests + `experiments/m0-fakebackend-validation` | measured | |
| C-003 | The waiting gauge rises when concurrency exceeds the configured limit | `experiments/m0-fakebackend-validation` | measured | |

## Rules

1. Write the claim first, then design the experiment.
2. Link raw data and the exact command that produced it.
3. State the environment (`hack/capture-env.sh`) next to every number.
4. Report ranges and run counts, not a single best run.
5. A null result ("no measurable difference") is a valid, publishable finding.
