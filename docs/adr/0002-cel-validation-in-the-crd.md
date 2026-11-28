# ADR 0002: Validate the InferenceService with CEL rules in the CRD, not a webhook

- Status: accepted
- Date: 2026-11-28

## Context

Some specs must never be admitted: Restricted data on a CPU-only backend, Restricted
data without a named device class, a device class together with a shared claim,
`maxReplicas < minReplicas`, and names that cannot also be a Service name. These rules
must hold even when the operator is down, being upgraded, or crash-looping.

## Decision

Express every rule as an `x-kubernetes-validations` (CEL) rule in the CRD schema, next
to ordinary OpenAPI constraints (enums, minimums, lengths). Five CEL rules exist today:
Restricted needs a GPU-capable backend, Restricted needs `accelerator.deviceClassName`,
device class and shared claim are exclusive, `maxReplicas >= minReplicas`, and the name
is a DNS label of at most 63 characters.

## Alternatives considered

- Validating admission webhook: can express cross-object rules (for example, total GPU
  memory share across services on one device) but needs a TLS certificate (cert-manager
  or hand-rolled), a Service, and availability. If the webhook is down, creates either fail
  or, with `failurePolicy: Ignore`, silently skip validation.
- Validate only inside the reconciler: cheap, but the bad object is already stored and the
  user finds out from status, not from `kubectl apply`.

## Consequences

- Rejection happens in the API server, with no operator involved. The envtest suite proves
  it by creating objects with no reconciler running, and `kubectl apply --dry-run=server`
  shows the messages.
- No certificates and no extra moving parts.
- CEL cannot look at other objects. If a rule ever needs to (shared-device capacity in the
  DRA milestone), add a webhook for that rule only and keep the rest in CEL.
- Rules reference defaulted fields (`classification`), which the API server applies before
  validation.
