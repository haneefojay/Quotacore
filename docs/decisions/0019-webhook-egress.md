# ADR-0019 — Webhook egress: NFR-S3 re-scoped, NFR-C5 re-worded, and a secret derived rather than stored

- **Status:** Accepted
- **Date:** 2026-10-31
- **Deciders:** project owner
- **Affects:** [non-functional-requirements](../architecture/non-functional-requirements.md), [security-model](../architecture/security-model.md), [data-model](../architecture/data-model.md), [deployment](../architecture/deployment.md), [webhooks](../architecture/webhooks.md), [SECURITY.md](../../SECURITY.md), `api/openapi.yaml`

## Context

v0.3 is the first release in which the service opens an outbound connection
([v0-3-webhooks.md](../roadmaps/v0-3-webhooks.md#the-change-in-kind)). Two stated properties say it
never does: NFR-S3, "the service makes no outbound connection in the MVP", and NFR-C5, "no data
leaves the host in the MVP". Both were written for v0.1 and both are true of every build to date.
A delivery to a customer's endpoint makes the first false and the second at best misleading, and
the phase that builds delivery (`IP-22`) cannot ship honestly while either stands unchanged.
`IP-21` is gated on this record.

Signing raises a second question that the security model had not met. Every secret the product
held until now is verified, not used: an API key is stored as a SHA-256 digest
([ADR-0018](0018-api-key-format-and-verifier.md)) and the service never needs the plaintext again.
A webhook secret is the key of an HMAC the service computes on every delivery, so a digest is not
enough, and NFR-S1's "not recoverable in plaintext from any store" still has to hold
([DR-043](../product/domain-rules.md)).

## Options considered

**For the two properties.**

**A — Re-scope NFR-S3 and re-word NFR-C5, with egress off unless the operator opts in (chosen).**

**B — Delete both.** Rejected: an operator who never configures a webhook still gets exactly the
v0.1 property, and deleting the requirement would stop the specification promising it.

**C — Keep both and treat webhooks as an exception noted elsewhere.** Rejected: a stated security
property with an unstated exception is the kind of claim this specification exists to avoid.

**For the secret.**

**D — Derive it from an operator signing key, and store nothing (chosen).**

**E — Encrypt a stored secret with an operator key.** Rejected: it needs the same operator key as
D and adds ciphertext to every backup, so it protects nothing D does not and has more to protect.

**F — Accept a customer-supplied secret.** Rejected: it must then be stored recoverably, which is
E's cost, and a human-chosen secret is weaker than a derived one.

## Decision

- **NFR-S3** becomes: the service makes no outbound connection except a webhook delivery to an
  endpoint an operator has configured for a tenant. No telemetry, no update check, no phone-home,
  ever.
- **NFR-C5** becomes: data residency is the customer's, because the data is on the customer's host.
  It does not leave the host as data; a webhook delivery carries identifiers and amounts to an
  endpoint the operator chose, and [webhooks.md](../architecture/webhooks.md) §2.1 lists every field.
- **Webhooks are off by default.** They are enabled by setting `QUOTACORE_WEBHOOK_SIGNING_KEY`
  ([deployment.md](../architecture/deployment.md#4-configuration)). Without it every webhook route
  answers `501 not_implemented` (Q-42), so an instance that has not opted in keeps both v0.1
  properties exactly, including a release built from a branch that contains the routes.
- **The secret is derived.** A webhook's secret is `qcwh_` and the base64url HMAC-SHA256, under the
  signing key, of a fixed label, the webhook's id and its `secret_generation`. Postgres holds the
  generation, never the secret; the secret is returned on the create and on each rotation, and
  nowhere else (Q-40).
- **The signature** is HMAC-SHA256 under the secret over the timestamp, the `event_id` and the
  body, with a 300-second replay window and the `event_id` as the JTI (Q-41,
  [webhooks.md](../architecture/webhooks.md) §3).

## Rationale

Re-scoping keeps the property for everyone who does not ask for egress and states precisely what
those who do get. Off by default means the default deployment's answer to "does it phone out?" is
still no, without a footnote.

A derived secret satisfies NFR-S1 structurally: no table, backup, log or audit row contains it,
and a stolen database yields nothing that signs. The signing key is a deployment secret of the
same kind as the database URL, held in the environment and never in the database.

## Consequences

**Positive**
- The two properties remain true statements, for every deployment, before and after `IP-22`.
- Nothing about a webhook secret is in a backup, so restoring one cannot leak it.

**Negative**
- Rotating the signing key rotates every tenant's secret at once. That is the operator's lever for
  a leaked key, and it is also a hazard; [deployment.md](../architecture/deployment.md) §4 says so.
- A lost secret cannot be read back, only rotated, which is the same trade as an API key's.

## Revisit when

- A customer needs a secret they choose, or a second secret valid during a rotation.
- A per-tenant signing algorithm is required (deferred by `IP-21`).
