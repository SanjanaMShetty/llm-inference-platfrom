# ADR 0003: Server-side apply with two field managers; the autoscaler owns replicas

- Status: accepted
- Date: 2026-11-28

## Context

The operator builds a Deployment from the InferenceService. Later, an autoscaler (KEDA's
HPA, `kubectl scale`) will change `spec.replicas`. If the operator also writes
`spec.replicas`, the two fight: every reconcile resets the count the autoscaler chose.

Under server-side apply (SSA), a field that a manager owned and then stops mentioning is
removed. So the operator cannot "set replicas on create, then leave it out": leaving it
out later would delete it and reset it to the default of 1.

## Decision

- `buildDeployment` never sets `spec.replicas`. The main field manager
  (`llm-platform-operator`) applies everything else on every reconcile and so owns it.
- After the apply, if no field manager owns `spec.replicas` (read from `managedFields`),
  a second manager (`llm-platform-operator-bootstrap`) applies only
  `spec.replicas = autoscaling.minReplicas`. This happens once, right after creation, and
  is retried on the next reconcile if it fails.
- From then on an autoscaler owns the field. When another manager changes the value,
  SSA moves ownership to it, and neither operator manager owns it any more.
- The Deployment selector holds only `llm-platform.io/inference-service: <name>`. It is
  immutable, so nothing a user can edit may appear in it.
- The container name is constant (`model-server`) across backends, so switching backend
  rewrites one container in place. SSA prunes fields the operator stops applying (env vars,
  ports), which a test checks across vLLM, Ollama and fake.
- No finalizer: every owned object is in the cluster and carries an owner reference, so the
  garbage collector removes it. A finalizer is only justified once something outside the
  cluster needs cleanup.

## Alternatives considered

- Set replicas on every reconcile: simple and wrong; fights the autoscaler.
- `Create` once then never touch replicas: leaves the object's field manager as a generic
  "Update" owner of every field, so removed fields would never be pruned.
- Read the Deployment first and copy its replicas into the desired object: racy, and makes
  the operator own the field again.

## Consequences

- Changing `autoscaling.minReplicas` on a running service does not change the Deployment's
  replica count. In the autoscaling milestone it changes the autoscaler's bounds instead.
  Without an autoscaler, scale with `kubectl scale`.
- Apply is idempotent: when nothing changed, the API server treats it as a no-op and the
  resourceVersion does not move. The steady state is two apply requests and zero writes.
