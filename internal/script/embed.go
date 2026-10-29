// Package script is the data plane's enforcement: the three Lua scripts that
// decide and apply a balance change as one indivisible step, and the Go code
// that loads them and runs them. It also holds the five control-plane scripts
// the admin routes run (IP-10): grant, set, relimit, reanchor and rollover,
// and the create-only rebuild script `quotacore rebuild` runs (IP-11 D7).
// None of them is on the request path, and three of them open with the same
// transition block as the data plane's, so there is still one rollover.
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

// The nine bodies. They are package-level constants, not files opened at
// runtime, and a test asserts the store's copy of each is byte-identical to the
// one named here.
//
//go:embed consume.lua
var consumeSource string

//go:embed check.lua
var checkSource string

//go:embed refund.lua
var refundSource string

//go:embed grant.lua
var grantSource string

//go:embed set.lua
var setSource string

//go:embed relimit.lua
var relimitSource string

//go:embed reanchor.lua
var reanchorSource string

//go:embed rollover.lua
var rolloverSource string

//go:embed rebuild.lua
var rebuildSource string

// Operation names one of the scripts. It is a closed set: three data-plane
// operations, five control-plane ones and the rebuild, and no mode flag, because one script
// per operation is what performance.md §5 requires and a mode flag inside one
// script would put both decisions in the body a reviewer has to read twice.
type Operation string

const (
	Consume Operation = "consume"
	Check   Operation = "check"
	Refund  Operation = "refund"

	Grant    Operation = "grant"
	Set      Operation = "set"
	Relimit  Operation = "relimit"
	Reanchor Operation = "reanchor"
	Rollover Operation = "rollover"

	Rebuild Operation = "rebuild"
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

	grantBody    = newBody(Grant, grantSource)
	setBody      = newBody(Set, setSource)
	relimitBody  = newBody(Relimit, relimitSource)
	reanchorBody = newBody(Reanchor, reanchorSource)
	rolloverBody = newBody(Rollover, rolloverSource)

	rebuildBody = newBody(Rebuild, rebuildSource)
)

// allBodies is every script Load puts in the store: the data plane's three
// first, then the control plane's five, then the rebuild's.
func allBodies() []body {
	return []body{consumeBody, checkBody, refundBody, grantBody, setBody, relimitBody, reanchorBody, rolloverBody,
		rebuildBody}
}

func newBody(op Operation, text string) body {
	sum := sha1.Sum([]byte(text))
	return body{op: op, text: text, digest: hex.EncodeToString(sum[:])}
}

// Load puts the nine scripts in the store's script cache. It is called once at
// start-up and again by the runner whenever the store answers NOSCRIPT, so a
// store that was flushed or restarted recovers without a restart of this
// process (ADR-0002).
//
// Each returned digest is compared with the one this binary computed, so a store
// holding different bytes under the same digest - or a proxy that rewrote the
// body - is a start-up error rather than a silent difference between what was
// tested and what runs.
func Load(ctx context.Context, client redis.Scripter) error {
	for _, b := range allBodies() {
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
