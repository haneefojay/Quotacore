# Webhooks

What a Quotacore webhook delivery is, how it is signed, and how a receiver proves a delivery is
genuine. This is the document a customer's engineer reads before writing the receiver. Added
2026-10-31 (`IP-21`); the phase is
[IP-21 in the v0.3 roadmap](../roadmaps/v0-3-webhooks.md#ip-21--webhook-configuration-and-signing),
and the scope it serves is [mvp-scope.md](../product/mvp-scope.md) §2's v0.3 rows. The rule it
implements is [DR-055](../product/domain-rules.md), and the egress it opens is the one
[ADR-0019](../decisions/0019-webhook-egress.md) permits.

`IP-21` builds the configuration, the payload and the signature. The delivery worker, its retries
and its dead letters are `IP-22`, and the address policy is `IP-23`; until they ship no delivery is
sent, and this document describes what each one will carry.

## 1. Configuration

An operator configures one webhook per tenant with `PUT /v1/admin/tenants/{id}/webhook`, or
`quotacore tenant webhook set` ([cli.md](cli.md)): an `https` endpoint and a subscription set.

| Subscription | Fields | Told about |
| --- | --- | --- |
| `threshold_crossing` | `feature_key`, `value` 1 to 100 | The balance falling to `value` percent of the limit used or beyond |
| `absolute_threshold` | `feature_key`, `value` 0 to 2^53 − 1 | The balance falling to `value` left or below |
| `plan_change` | none | A ledger `plan_changed` event |
| `cycle_rollover` | none | A ledger `cycle_rolled_over` or `force_rolled_over` event |

A threshold crossing and an absolute threshold are different subscriptions, so 80% used and 100
left can be named on the same feature and are reported apart. Only a fall crosses a threshold; a
refund or a grant re-arms it by moving the balance back above (DR-055).

Webhooks are off on an instance whose operator has not set `QUOTACORE_WEBHOOK_SIGNING_KEY`
([deployment.md](deployment.md#4-configuration) §4); every webhook route then answers
`501 not_implemented` ([Q-42](../product/assumptions-and-open-questions.md#2-open-questions)).

## 2. A delivery

A delivery is an HTTP `POST` of one JSON object to the endpoint, with these headers.

| Header | Value |
| --- | --- |
| `Content-Type` | `application/json` |
| `Quotacore-Delivery` | The `event_id`: the ledger event's own identifier, identical to the payload's `event_id` |
| `Quotacore-Timestamp` | When this attempt was signed, in whole seconds since the Unix epoch |
| `Quotacore-Signature` | `v1=` and the lower-case hex HMAC-SHA256 of the signed string, under the secret |

There is **one delivery per ledger event**. An event that crosses two subscribed thresholds is one
delivery listing both, never two deliveries with one `event_id`, so deduplicating on `event_id`
never discards a different notification ([INV-W1](../product/state-machines.md#10-webhook-delivery-v03)).

### 2.1 The payload

`WebhookPayload` in [the contract](../../api/openapi.yaml) is the schema; every field is always
present.

| Field | Contains |
| --- | --- |
| `event_id` | The ledger event's identifier, a UUID. Deduplicate on it |
| `event_type` | The ledger event type: `consumed`, `refunded`, `granted`, `set`, `plan_changed`, `cycle_rolled_over` or `force_rolled_over` |
| `occurred_at` | When the event happened, RFC 3339 in UTC |
| `tenant_external_id` | The tenant's `external_id`, the operator's own identifier |
| `feature_key` | The feature the event moved |
| `delta` | The signed change to the balance |
| `balance` | The balance after the event |
| `crossings` | Each subscribed threshold the event crossed, as `{type, value}`; empty for a plan change or a rollover that crossed none |

That is all a payload carries: identifiers and amounts. No tenant name, no metadata, no request
body.

## 3. The signature

The signed string is the timestamp, a full stop, the `event_id`, a full stop, and the raw request
body, byte for byte:

```text
<Quotacore-Timestamp>.<Quotacore-Delivery>.<body>
```

The signature covers the body, the timestamp and the `event_id` together, so none can be changed,
and no signature can be moved to another delivery, without the check failing. The key is the
webhook secret exactly as it was shown, `qcwh_` and 43 more characters, used as bytes and never
decoded.

The secret is shown once, when the webhook is created and each time it is rotated with
`POST /v1/admin/tenants/{id}/webhook/rotate-secret`. It is derived from the operator's signing key
and stored nowhere, so it cannot be read back; a lost secret is rotated
([Q-40](../product/assumptions-and-open-questions.md#2-open-questions)). A rotation ends the old
secret at once.

### 3.1 The anti-replay window

A receiver refuses a delivery whose `Quotacore-Timestamp` is more than **300 seconds** from its own
clock, in either direction
([Q-41](../product/assumptions-and-open-questions.md#2-open-questions)). Inside the window, it
refuses an `event_id` it has already processed. The two together are the replay guard: the window
bounds how long a captured delivery stays valid, and the record of `event_id`s covers the window
itself. The `event_id` is the scheme's JTI ([glossary.md](../product/glossary.md)).

Delivery is at least once. A retry carries the same `event_id` and a fresh timestamp and
signature, so a duplicate is expected and the receiver's deduplication is what makes processing
exactly once ([INV-W1](../product/state-machines.md#10-webhook-delivery-v03)).

## 4. Verifying a delivery

This program is the whole check, with nothing but the Go standard library. It reads the body on
standard input and the secret and the three headers from the environment, and exits `0` for a
genuine delivery and `1` for anything else. A test runs it against a real signature, then against
the same delivery with one byte changed, and then with a timestamp outside the window
(T-20, [testing-strategy.md](testing-strategy.md)).

<!-- verify-recipe:start -->
```go
// verify.go: go run verify.go < body.json
//
// QUOTACORE_WEBHOOK_SECRET is the webhook's secret; QUOTACORE_DELIVERY,
// QUOTACORE_TIMESTAMP and QUOTACORE_SIGNATURE are the three headers.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const window = 300 // seconds either side of this clock

func verify(secret string, body []byte, delivery, timestamp, signature string, now time.Time) bool {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if d := now.Unix() - ts; d > window || d < -window {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "." + delivery + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, part := range strings.Split(signature, ",") {
		value, ok := strings.CutPrefix(strings.TrimSpace(part), "v1=")
		if !ok {
			continue
		}
		got, err := hex.DecodeString(value)
		if err == nil && hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

func main() {
	body, err := io.ReadAll(os.Stdin)
	if err != nil || !verify(os.Getenv("QUOTACORE_WEBHOOK_SECRET"), body,
		os.Getenv("QUOTACORE_DELIVERY"), os.Getenv("QUOTACORE_TIMESTAMP"),
		os.Getenv("QUOTACORE_SIGNATURE"), time.Now()) {
		fmt.Println("rejected")
		os.Exit(1)
	}
	fmt.Println("verified")
}
```
<!-- verify-recipe:end -->

Two details are easy to get wrong in another language. Compare in constant time, as
`hmac.Equal` does, so the comparison does not leak how much of a guess was right. And verify the
body as received, before any JSON parsing, because a parser that reorders keys or reformats a
number changes the bytes the signature covers.

After it verifies, the receiver looks the `event_id` up in its record of processed events and
stops if it is there, then processes the event and records the `event_id` in the same
transaction as its own effect.
