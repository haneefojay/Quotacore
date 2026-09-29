// Package script is the data plane's enforcement: the three Lua scripts that
// decide and apply a balance change as one indivisible step, and the Go code
// that loads them and runs them.
//
// The three scripts are embedded at build time rather than read from disk or
// fetched at runtime (NFR-S8, ADR-0002). Embedding is what makes "one round
// trip" and "no external dependency" true at once: the bytes the store executes
// are the bytes this binary was built from, so a checkout, a container and a
// laptop all enforce identically, and there is no path by which a script can
// be replaced at runtime.
//
// The division of labour is deliberate and is the whole design. Cycle
// arithmetic belongs to internal/cycle and to nothing else: the caller computes
// the window and passes the numbers in ARGV, and this package never performs a
// boundary calculation. What the scripts add is the part no amount of care in
// the caller can provide: the comparison and the write in the same execution,
// so no two requests can both pass a check that only one of them can pass
// (DR-017, INV-C2).
//
// Every value crosses the boundary as a separate argument, never as text
// assembled into the script. That is what makes it impossible for a tenant id,
// a feature key or an amount to be parsed as Lua, and it is asserted in two
// places: a source test over the embedded bytes, and an integration test that
// sends hostile values and asserts the store's contents are what a benign
// value would have produced.
package script

import (
	"context"
	"crypto/sha1"
	_ "embed"
	"encoding/hex"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// The three bodies. They are package-level constants, not files opened at
// runtime, and a test asserts the store's copy of each is byte-identical to the
// one named here.
//
//go:embed consume.lua
var consumeSource string

//go:embed check.lua
var checkSource string

//go:embed refund.lua
var refundSource string

// Operation names one of the three scripts. It is a closed set with no fourth
// member, because one script per endpoint is what performance.md §5 requires and
// a mode flag inside one script would put both decisions in the body a reviewer
// has to read twice.
type Operation string

const (
	Consume Operation = "consume"
	Check   Operation = "check"
	Refund  Operation = "refund"
)

// body is one embedded script together with the digest the store knows it by.
// The digest is computed here, in Go, from the same bytes that get loaded, so
// the two can be compared rather than trusted.
type body struct {
	op     Operation
	text   string
	digest string
}

var (
	consumeBody = newBody(Consume, consumeSource)
	checkBody   = newBody(Check, checkSource)
	refundBody  = newBody(Refund, refundSource)
)

func newBody(op Operation, text string) body {
	sum := sha1.Sum([]byte(text))
	return body{op: op, text: text, digest: hex.EncodeToString(sum[:])}
}

// Load puts the three scripts in the store's script cache. It is called once at
// start-up and again by the runner whenever the store answers NOSCRIPT, so a
// store that was flushed or restarted recovers without a restart of this
// process (ADR-0002).
//
// Each returned digest is compared with the one this binary computed, so a store
// holding different bytes under the same digest - or a proxy that rewrote the
// body - is a start-up error rather than a silent difference between what was
// tested and what runs.
func Load(ctx context.Context, client redis.Scripter) error {
	for _, b := range []body{consumeBody, checkBody, refundBody} {
		loaded, err := client.ScriptLoad(ctx, b.text).Result()
		if err != nil {
			return fmt.Errorf("load the %s script: %w", b.op, err)
		}
		if loaded != b.digest {
			return fmt.Errorf("load the %s script: the store holds %s but this binary embeds %s", b.op, loaded, b.digest)
		}
	}
	return nil
}
