# ADR 0004: Status reports what the cluster is doing, not what the operator did

- Status: accepted
- Date: 2026-11-28

## Context

"Objects exist" is not "the model is serving". A status that says Ready because the
Deployment was created lies, and the gateway and autoscaler will later depend on it.

## Decision

Status is computed from the Deployment's status and the pods behind it, by a pure function
that is unit tested without a cluster.

| Phase | Meaning |
|---|---|
| Pending | the Deployment has not been created yet |
| Loading | nothing is ready and nothing is wrong yet (weights loading) |
| Ready | at least one replica is ready and no pod reports a problem |
| Degraded | at least one replica is ready, but some pod reports a problem |
| Failing | nothing is ready and pods report a failure (crash loop, image pull, OOM, replica failure) |
| Unschedulable | nothing is ready and the scheduler cannot place a pod |

- The scheduler's or kubelet's own words are copied into the `Ready` condition's reason and
  message (for example `Unschedulable: 0/2 nodes are available: 2 Insufficient cpu.`).
- `Ready` is True only in phase Ready. `Available` is True when at least one replica is
  ready. `Reconciled` says whether the last apply succeeded. `AcceleratorApplied` is False
  with reason `NotImplemented` while an accelerator is configured, until the DRA milestone.
- Scaling up while a replica serves stays Ready; capacity is in `readyReplicas`.
- Terminating and finished pods are ignored. A failed apply keeps the last known phase
  instead of flipping to Pending, and does not advance `observedGeneration`.
- Pods are watched by label because a ReplicaSet, not the InferenceService, owns them. The
  InferenceService itself is watched with a generation predicate so the operator does not
  react to its own status writes.
- Status is written only when it differs, and condition transition times change only when a
  condition's status changes, so a quiet cluster produces no writes.

## Alternatives considered

- Copy `Deployment.status.conditions` only: misses unschedulable pods and crash loops, which
  appear on pods, not on the Deployment, until a progress deadline passes.
- A single Ready boolean: hides why. The reason string is the point.

## Consequences

- Some reads depend on kubelet strings (`CrashLoopBackOff`, `ErrImagePull`). They are
  stable, and the set is one table in `status.go`.
- Rolling updates do not get their own phase; the old replicas keep the service Ready.
