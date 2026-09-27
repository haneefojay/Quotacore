# v0.3 — outbound notification

Quotacore · 2026-09-27

Three phases, `IP-21` to `IP-23`. The only egress the product will ever have, and the first release
in which the service opens an outbound connection. That is a change in kind, not in degree, and
these phases are written as though it is one.

Read [roadmap-index.md](roadmap-index.md) first.

## Phase map

| ID | Phase | Depends on | Status |
| --- | --- | --- | --- |
| `IP-21` | Webhook configuration and signing | `IP-16` | `BLOCKED` |
| `IP-22` | Delivery, retries and dead letters | `IP-21` | `BLOCKED` |
| `IP-23` | Egress protections and delivery metrics | `IP-22` | `BLOCKED` |

---

## The change in kind

NFR-S3 requires that the service makes **no** outbound connection in the MVP, and NFR-C5 rests on
data never leaving the host. v0.3 ends both properties for the operator who opts in. Three things
follow, and each is a phase rather than a task inside a phase:

1. **NFR-S3 must be re-scoped, not deleted.** It becomes "no outbound connection except the
   subscriptions a tenant has explicitly configured". Re-scoping a requirement is a specification
   change and needs an ADR, because it alters a stated security property.
2. **The data-residency claim must be re-worded honestly.** Data still does not leave the host as
   data; a signed notification carries an identifier and an amount. An operator reading NFR-C5 after
   v0.3 must not believe otherwise.
3. **The failure mode inverts.** Everywhere else in this product, an unavailable dependency makes
   Quotacore less available. Here, an unreachable customer endpoint must not. Every DoD item in
   `IP-22` and `IP-23` exists to keep that true.

---

## IP-21 — Webhook configuration and signing

**Status** `BLOCKED`, gated on `IP-16`, and on the ADR that re-scopes NFR-S3.

**Objective.** A per-tenant endpoint, a secret, and an event-type subscription set, with signatures a
customer can verify.

**Scope.**
- Per-tenant endpoint URL, secret, and subscribed event types.
- Threshold crossing configuration, as distinct from an absolute threshold.
- The signature scheme, its timestamp, and the anti-replay window.
- The admin routes, the CLI commands, and the audit records.

**Specification references.**
- [mvp-scope.md](../product/mvp-scope.md) §2, v0.3
- [ADR-0016](../decisions/0016-usage-event-ledger.md), whose event identity the payload reuses
- [DR-041](../product/domain-rules.md), [DR-043](../product/domain-rules.md)
- [error-catalog.md](../product/error-catalog.md)
- NFR-S3, which this phase re-scopes, and NFR-S1, which still applies to the secret
- [security-model.md](../architecture/security-model.md) §4

**Dependencies.** Blocked by `IP-16`. Blocks `IP-22`.

**Definition of Done.**

1. A tenant can configure an endpoint, a secret and a subscription set, and every change is audited
   with before and after values (DR-041).
2. The secret is stored so it is not recoverable in plaintext from any store, log or metric, and the
   plaintext-storage test from `IP-09` covers it (NFR-S1, DR-043).
3. Every payload carries a stable `event_id`, identical to the one in the ledger event and to the
   `Quotacore-Delivery` header, so a customer's handler can deduplicate (INV-W1, INV-U1).
4. A threshold crossing is distinguishable from an absolute threshold, and the subscription set can
   name either.
5. The signature covers the body, a timestamp and the `event_id`, and the timestamp's anti-replay
   window is stated in the document a customer reads.
6. A verification recipe exists as a runnable snippet in the documentation, and a test runs it
   against a real signature and asserts it passes — then tampers with one byte and asserts it fails.
7. The route returns `501 not_implemented` on a v0.1 or v0.2 build (NFR-S1 for the version check is
   NFR-S9's sibling, the version route).

**Exit criteria.** A customer can configure a subscription and verify that a delivery is genuinely
from Quotacore.

**Deferred.** Multiple endpoints per tenant, and a per-tenant signing algorithm choice.

---

## IP-22 — Delivery, retries and dead letters

**Status** `BLOCKED`, gated on `IP-21`.

**Objective.** At-least-once delivery with bounded attempts, a dead-letter table, and a guarantee
that a customer's outage changes nothing about enforcement.

**Scope.**
- The delivery worker, exponential backoff with jitter, and the attempt bound.
- The dead-letter table, and replay of a dead letter.
- The event types in [mvp-scope.md](../product/mvp-scope.md) §2: threshold crossings, plan changes
  and cycle rollover.
- Disabling a subscription that is failing repeatedly.

**Specification references.**
- [mvp-scope.md](../product/mvp-scope.md) §2, v0.3
- [INV-W1](../product/state-machines.md) to [INV-W3](../product/state-machines.md),
  [INV-WK1](../product/state-machines.md), [INV-WK2](../product/state-machines.md)
- [consistency-and-recovery.md](../architecture/consistency-and-recovery.md) §4.5
- NFR-D5, ledger durability, and the [ADR-0016](../decisions/0016-usage-event-ledger.md) contract
  this extends

**Dependencies.** Blocked by `IP-21`. Blocks `IP-23`.

**Definition of Done.**

1. A delivery is retried with exponential backoff and jitter, up to a bounded number of attempts,
   and then dead-lettered. The bound is a number in a document, and the test asserts the attempt
   count is exactly that.
2. A dead letter is visible to the operator and replayable, and a replay is itself audited
   (INV-W3).
3. A duplicate `event_id` reaches the customer at least once and possibly twice, and the customer's
   deduplication is what makes it exactly once (INV-W1). The contract states this plainly; the
   product does not claim otherwise.
4. **Enforcement latency is unchanged while deliveries are failing.** The ledger-saturation
   property from `IP-12` extends to webhooks: a customer whose endpoint hangs for 30 seconds every
   time does not slow down anyone else's `consume`. A test asserts the negative, and the negative is
   the point.
5. The delivery worker holds a runtime-scoped credential and no direct database access
   (INV-WK2), and deleting or duplicating it changes no enforcement outcome (INV-WK1).
6. A subscription failing repeatedly is disabled, and the disabling is audited and visible, so one
   tenant's broken endpoint cannot fill the queue indefinitely.
7. Every delivery is reconcilable against the ledger: an event with a subscription set and no
   delivery record, and no dead letter, is detectable.

**Exit criteria.** A customer's webhook receiver being broken is a customer problem, visible and
replayable, and never an enforcement problem.

**Deferred.** Ordering guarantees beyond per-event, and a customer-visible delivery log beyond the
dead-letter table.

---

## IP-23 — Egress protections and delivery metrics

**Status** `BLOCKED`, gated on `IP-22`.

**Objective.** Make the egress safe to point at a customer's infrastructure, and visible when it
fails.

**Scope.**
- Server-side request forgery protections: scheme, resolved address, redirect behaviour, and
  private address ranges.
- The egress policy as configuration, with a default that is safe.
- Delivery metrics, and alerts.
- The documentation of what leaves the host, written for the operator who has to answer that
  question.

**Specification references.**
- [security-model.md](../architecture/security-model.md), the egress and webhook entries
- [observability.md](../architecture/observability.md) §1.5, extended for deliveries
- [SECURITY.md](../../SECURITY.md) §6, known gaps
- NFR-S3 as re-scoped, NFR-C5 as re-worded, NFR-O7, alertable
- [deployment.md](../architecture/deployment.md) §4, the configuration keys

**Dependencies.** Blocked by `IP-22`. Blocks nothing.

**Definition of Done.**

1. A subscription pointing at a loopback, link-local, private or cloud-metadata address is refused
   at configuration time, and the refusal names the reason. Tests cover each range.
2. Redirects are not followed, or are followed under the same policy, and a test proves a delivery
   cannot be redirected to a forbidden address.
3. DNS is resolved once per attempt and the resolved address is checked, so a name that resolves to
   a private address is refused after resolution too.
4. The egress allowlist is configuration with a safe default, and the default is what a customer gets
   without asking.
5. Delivery metrics exist for attempts, successes, failures, retries and dead letters, with bounded
   labels — the tenant is a label here because delivery is per tenant, and the cardinality bound is
   documented (NFR-O2).
6. An alert fires on a sustained delivery failure rate, and names the runbook that answers it.
7. The documentation states exactly what data a payload contains, in a table, so the operator can
   answer a data-residency question without reading the code.

**Exit criteria.** A tenant can point the product at their own infrastructure without being able to
use it to reach anything else, and an operator can see every delivery attempt.

**Deferred.** A proxy configuration for customers who require one, and mTLS to the receiver.
