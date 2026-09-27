# Glossary

One definition per term, used identically in every document. The **Not** column exists because
most of the confusion in this product comes from words that sound right and mean something else.

## A

| Term | Definition | Not |
| --- | --- | --- |
| **Allocation cap** | A metered feature counting entities rather than events, e.g. "3 projects". Implemented as `consume 1` on create and `refund 1` on delete | A limit on in-flight concurrency, which the MVP does not model |
| **Anchor** (`anchor_at`) | The tenant-specific instant from which all cycle boundaries are computed. Pure-function input, per DR-002 | A stored "current cycle start". There is no such field, precisely so the two cannot disagree |
| **Apply-now** | An audited admin operation applying a plan's current entitlements to every assigned tenant immediately, re-anchoring their cycles | A migration, a deploy, or a way to grant more allowance. It discards unused balance |
| **Atomic** | Performed indivisibly with respect to concurrent callers, inside one datastore script execution | Eventually consistent, or protected by a lock taken later |
| **Audit log** (`audit_log`) | Append-only record of every administrative mutation: actor, action, subject, before, after, request ID | The usage ledger. They answer different questions and are never merged |

## B

| Term | Definition | Not |
| --- | --- | --- |
| **Balance** | Remaining allowance in the current cycle: `limit + bonus − Σ(consumed) + Σ(refunded)` | The customer's total historical usage, or their spend |
| **Boolean entitlement** | A feature that is included or excluded, with no counter. v0.2 | An entitlement with a limit of infinity. Inclusion and exhaustion are distinct states (DR-012) |
| **Bonus** | Allowance granted outside the plan for the current cycle, from an admin grant. Resets to zero at every boundary | A permanent credit, a negative balance, or a discount |

## C

| Term | Definition | Not |
| --- | --- | --- |
| **Cadence** | The reset model: anchored calendar cycle, or rolling window (v0.2) | The reset *interval*, which is one of six values, or the period string a caller passes |
| **Ceiling** | `limit + bonus`. The invariant `balance ≤ ceiling` is enforced inside the atomic script (DR-019) | A rate limit, or a hard cap on lifetime usage |
| **Control plane** | The Postgres-backed administrative surface: features, plans, tenants, keys, audit | The enforcement path. It is never read during enforcement (DR-039) |
| **Cycle** | One period of an anchored cadence, identified by an integer `cycle_index` | A rolling window, which is a lookback and not a period |
| **Cycle index** | The integer `n` such that `boundary(n) ≤ now < boundary(n+1)`. Monotonically non-decreasing | A wall-clock timestamp, or a count of resets performed |
| **Cycle end** (`cycle_end`) | The exclusive end of the current cycle, returned as RFC 3339 **with offset**, in the tenant's time zone | The reset time in UTC only. The offset is present so the customer can display local time without reimplementing zone maths |
| **Cycle start** (`cycle_start`) | The inclusive start of the current cycle, same format as `cycle_end` | The tenant creation time. It is derived from the anchor and the index |
| **Cycle window** | The half-open interval `[cycle_start, cycle_end)` | An inclusive range. A request at exactly `cycle_end` belongs to the next cycle |

## D–F

| Term | Definition | Not |
| --- | --- | --- |
| **Data plane** | The Redis-compatible, Lua-enforced runtime surface: `consume`, `refund`, `check`, `balance` | A cache. It is authoritative at runtime, and losing it is a real event, not a performance event |
| **Dangling key** | A key past its `cycle_end + 24h` TTL that no longer has a live tenant bound to it | A correctness risk. Expiry is garbage collection only, never the reset trigger (ADR-0003) |
| **Denial** | A `consume` that is refused. Records no event, consumes no idempotency key, changes no balance (DR-025) | A failure. A denial is a correct, successful outcome of the business rule |
| **DR-nnn** | A domain rule with a stable identifier in [domain-rules.md](domain-rules.md) | An ADR. Rules say what must be true; ADRs say why a choice was made |
| **Effective entitlement** | The result of resolving the plan entitlement against the tenant override, using the fixed order in [state-machines.md](state-machines.md) section 5 | A third stored entity. It is computed, never persisted |
| **Expiry** | The Redis TTL on a key, set to `cycle_end + 24h` so a live key cannot disappear | The reset mechanism. Expiry never resets a balance |
| **External ID** (`external_id`) | The customer's own stable identifier for a tenant, matching `[A-Za-z0-9._:-]{1,128}` | A username, an email address, or an internal database identifier. It is opaque to Quotacore |

## G–L

| Term | Definition | Not |
| --- | --- | --- |
| **Hard reset** | Discarding the current balance and starting a new cycle at the moment of a plan assignment change (DR-013) | A revocation, a suspension, or a migration. The previous balance is discarded, not carried |
| **Idempotency key** | A client-supplied key identifying one logical `consume` or `refund`, reused across every retry of that operation | A request ID, a deduplication of requests, or an optional convenience. It is required, and its absence is `400 missing_idempotency_key` |
| **Idempotency window** | 24 hours from application. A key first seen after that is treated as new (DR-029) | A retention period for events, or a guarantee for keys used after 24 h |
| **JTI** | The JSON Web Token identifier, used as the webhooks signing scheme's replay guard (v0.3) | The idempotency key. Different mechanism, different purpose |
| **JTBD-n** | A job to be done, in [foundation-and-personas.md](foundation-and-personas.md) | A feature request |
| **Ledger** (`usage_events`) | Append-only signed-delta record of every applied balance mutation, in Postgres | The balance. The balance is authoritative at runtime; the ledger is history and reconstruction |
| **Limit** | The plan entitlement's allowance for a cycle, an `int64` in the feature's unit | The balance. A tenant's balance is at or below its limit, and below it once they consume |
| **Liveness** | The process is running and can answer probes. `/healthz` | Readiness, which also requires that enforcement can actually serve |
| **Loopback** | A deployment where a customer runs the service on the same host as their application, accepting the network hop for operational simplicity | Local development only. It is a supported production topology |

## M–P

| Term | Definition | Not |
| --- | --- | --- |
| **Metered** | Counted in a unit, e.g. tokens or requests, with an allowance per cycle | A boolean inclusion, or a rate limit |
| **Monotonic transition** | A cycle advance that refuses to move to a lower cycle index, so duplicated or out-of-order writers are harmless (ADR-0003) | A last-write-wins field update, which is how a stale worker grants a second allowance |
| **Never** | The `reset_interval` for a lifetime allowance. No boundary, no expiry, no rollover | An infinite limit with a rolling window, or an unlimited free tier. It is a real, finite, permanent number |
| **Override** | A per-tenant modification of one feature's limit or interval, taking precedence over the plan (DR-011, DR-013 note) | A second plan, a discount, or a promotion. It cannot add a feature the plan excludes |
| **P-n** | A persona in [foundation-and-personas.md](foundation-and-personas.md) | A privilege or a plan |
| **Plan** | A named, versioned set of entitlements, assignable to tenants. Exactly one per tenant (DR-011) | A subscription, a price, or an invoice. There is no money in this product |
| **Plan key** | The stable, customer-chosen string identifying a plan | A database key or a Redis key |
| **Proration** | Crediting the unused remainder of a cycle when a plan changes mid-cycle. Not implemented (DR-013) | Rounding. Nothing here rounds |
| **P-1..P-4** | Personas | Priorities |
| **Quota key** | The Redis key holding one tenant's balance for one feature: `qc:{t:<tenant_id>}:bal:<feature_key>` | A feature key, or an idempotency key |
| **Quota exhausted** | `balance < amount` for a `consume`. Rendered as `429 quota_exceeded` | Rate limited. A different code with a different remedy (DR-040) |
| **Rate limited** | The service's own protective limit on request rate per API key, `429 rate_limited` with `Retry-After` | An exhausted business allowance. Deliberately distinguishable |

## R–Z

| Term | Definition | Not |
| --- | --- | --- |
| **Replayed** | An idempotency-key repeat whose fingerprint matches: the stored response is returned with `replayed: true` and nothing changed | A retry that re-applied the mutation. A replay never mutates |
| **Reset interval** | One of `hourly`, `daily`, `weekly`, `monthly`, `yearly`, `never` | A duration in seconds, or a cron expression. A `monthly` tenant is not "every 30 days" |
| **Rolling window** | A lookback of a fixed duration over recent events, requested per call, in the current cycle. v0.2 | A sliding counter with exact boundary semantics, or a moving average. The MVP does not do this (ADR-0008) |
| **Runtime scope** | The `runtime` API-key scope, sufficient for the data plane and not for `/v1/admin` | Read-only. It can deduct and refund, which is why refund rights carry the ceiling invariant (DR-019) |
| **Runtime-reconstructed balance** | A balance rebuilt from the ledger when the data store is lost entirely, valid for a full-allowance-recovery case and flagged approximate (ADR-0016) | A general recovery mechanism. It is not equivalent to the live balance in every case |
| **Service instance** | One running Quotacore process, owning one Postgres schema and one Redis key space | A multi-tenant cloud service. One customer per instance (ADR-0013) |
| **Sink drop** | A usage event that could not be written to the ledger and was discarded, counted by `quotacore_event_sink_dropped_total`, visible as a gap in history | A lost enforcement. The deduction already happened atomically and is not affected (FS-10) |
| **Snapshot** | The bounded in-process cache of tenant and plan configuration used by the data plane, invalidated by pub/sub and refreshed periodically | A cache of balances. Balances are always read from the data store, never from a snapshot |
| **Soft delete** | Marking a tenant deleted: no further consumption, history retained, no physical removal | A suspension, which is reversible and preserves state |
| **Source** | The attribution on an event: `api`, `admin`, or `system`. It answers "who or what moved this balance" | An audit actor, which is a key identifier |
| **T-n, P-n, C-n, K-n, I-n, U-n, W-n** | Transition identifiers in [state-machines.md](state-machines.md) | Sequence numbers |
| **Tenant** | A billable entity in the customer's own domain, identified by an opaque `external_id` | A user, an organisation, or an account in Quotacore's own sense |
| **Threshold crossing** | The moment a balance crosses a configured fraction of its allowance, reported to webhooks in v0.3 | A rate limit, or an alert on a metric |
| **TTL** | The Redis key expiry, `cycle_end + 24h`, a garbage-collection bound | The reset schedule, or a data retention policy |
| **UC-nn** | A use case in [use-cases.md](use-cases.md) | A user story. Use cases here are normative and testable |
| **Usage event** (`usage_events`) | One appended record of one applied balance mutation, with a signed delta and a `request_id` | An audit record, or a metric |
| **Window** | In the MVP, the current cycle. In v0.2, an explicit `window` parameter selecting a rolling lookback | A configurable duration in the MVP. Omitting it means the anchored cycle |
| **Zone data version** | The `tzdata` version the binary was built with, reported by `GET /v1/admin/status` so a boundary discrepancy is diagnosable (DR-001) | A timezone itself |

---

## Naming conventions used in identifiers

| Element | Convention | Example |
| --- | --- | --- |
| API key scopes | `runtime`, `admin`, lowercase | `runtime` |
| Reset intervals | lower snake case, from a fixed set | `monthly` |
| Event types | past-tense verb, `snake_case` | `consumed`, `cycle_rolled_over`, `plan_changed` |
| Event sources | `api`, `admin`, `system` | `admin` |
| Error codes | lower snake case, stable, permanent | `idempotency_key_reuse` |
| Feature keys | reverse-DNS style, immutable | `llm.tokens.output` |
| Plan keys | lower snake case | `pro`, `enterprise` |
| Environment variables | `QUOTACORE_` prefix, upper snake case | `QUOTACORE_REDIS_URL` |
| Metrics | `quotacore_` prefix, `_total` for counters | `quotacore_consume_denied_total` |
| Redis keys | `qc:{t:<tenant>}:<kind>:<name>` with a per-tenant hash tag | `qc:{t:01JCQ…}:bal:llm.tokens.output` |
| ADR | four digits, one decision, dated | `0003-lazy-monotonic-cycle-rollover` |
| Domain rule | `DR-nnn`, stable, never renumbered | `DR-019` |
| Use case | `UC-nn` | `UC-08` |
| State transition | `T-`, `P-`, `F-`, `E-`, `K-`, `C-`, `I-`, `U-`, `W-` prefixed | `C-2` |
| Assumption | `A-nn` | `A-08` |
| Open question | `Q-nn` | `Q-14` |
| Scenario | `FS-nn` | `FS-05` |
| Journey | `J-n` | `J-4` |
| Persona | `P-1`..`P-4` (prefix `P-`, disambiguated from plan transitions by the numeric form) | `P-2` |

## Words deliberately not used

| Word | Why not |
| --- | --- |
| "Limit" as a verb | Ambiguous with the numeric `limit`. The verbs are "cap", "ceiling", "restrict" |
| "Credit" | Implies money. This product moves allowance, not currency (ADR-0014, foundation section 3) |
| "Billing", "invoice", "subscription", "charge" | Money and invoicing are out of scope, permanently. A "charge" in this codebase is always an event, never a payment |
| "Seats" | A customer's domain concept, not ours. We have allocation caps and a unit, and they have seats |
| "Fail open" / "fail closed" as a product feature | The customer chooses; we always fail closed and expose a fast error. See DR-037 |
| "Cache" for the data plane | It is authoritative, not a cache. Calling it a cache invites someone to treat its loss as a performance event |
| "Eventually consistent" | Nothing here is eventually consistent, and implying otherwise invites an incorrect design. Balances are strongly consistent; the ledger is best-effort by design and says so |
| "Soft quota", "hard quota" | Not part of the model. Allowance is exact, and a denial is exact |
