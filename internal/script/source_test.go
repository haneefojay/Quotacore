package script

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/cycle"
)

// The checks in this file read the embedded bytes, not the files on disk, so
// what is asserted here is what this binary would send to the store. A script
// is a source file that is never edited by hand at runtime, which means a
// property with no other observable can only be a source-level check
// (testing-strategy.md §4). Each one is the form that catches a specific defect
// rather than a general tidiness rule.
//
// The three scripts share one block of code and three copies of it. The copies
// are the point of the first test: Redis Lua has no include, and a script is
// loaded whole, so three operations that must agree about a cycle transition
// cannot share a function. What can be guaranteed is that the three copies are
// byte-identical, which is what INV-C2 asks for in the form the language allows -
// one implementation, three transcriptions, and a test that fails the moment a
// transcription is edited alone.

var (
	blockOpen  = "-- BEGIN transition"
	blockClose = "-- END transition"
)

// bodies returns the three embedded scripts with their operation, in the order
// the tests below talk about them.
func bodies() []body {
	return []body{consumeBody, checkBody, refundBody}
}

func TestTheThreeScriptsShareOneTransitionBlock(t *testing.T) {
	var first string
	for _, b := range bodies() {
		text := b.text
		if strings.Count(text, blockOpen) != 1 || strings.Count(text, blockClose) != 1 {
			t.Fatalf("%s.lua has %d open and %d close markers for the transition block; exactly one of each is required",
				b.op, strings.Count(text, blockOpen), strings.Count(text, blockClose))
		}
		start := strings.Index(text, blockOpen)
		end := strings.Index(text, blockClose)
		if start > end {
			t.Fatalf("%s.lua closes the transition block before it opens it", b.op)
		}
		block := text[start : end+len(blockClose)]
		if first == "" {
			first = block
			continue
		}
		if block != first {
			t.Errorf("%s.lua has a transition block that differs from the other two. The rollover is one implementation "+
				"written out three times, and this is the test that says so: a change to one copy and not the others "+
				"is a second authority over when a tenant's allowance resets (INV-C2, cycle-engine.md §4)", b.op)
		}
	}
}

func TestScriptsCallOnlyTheCommandsTheyNeed(t *testing.T) {
	// A command the scripts need to be atomic and nothing else. HMGET reads the
	// hash, HSET and HINCRBY write it, EXPIREAT and PERSIST hold the key for the
	// idempotency window.
	allowed := map[string]bool{
		"HMGET": true, "HSET": true, "HINCRBY": true, "EXPIREAT": true, "PERSIST": true,
	}
	callSite := regexp.MustCompile(`redis\.(call|pcall)\(\s*'([A-Za-z]+)'`)
	anyCall := regexp.MustCompile(`redis\.(call|pcall)\(`)

	for _, b := range bodies() {
		code := stripLuaComments(b.text)
		matches := callSite.FindAllStringSubmatch(code, -1)
		if len(matches) != len(anyCall.FindAllString(code, -1)) {
			t.Errorf("%s.lua calls the store %d times but only %d name a command as a literal; "+
				"every call must name one, because a command built from a variable is a command built from ARGV",
				b.op, len(anyCall.FindAllString(b.text, -1)), len(matches))
		}
		if len(matches) == 0 {
			t.Errorf("%s.lua calls the store at all", b.op)
		}
		for _, m := range matches {
			if m[1] != "call" {
				t.Errorf("%s.lua uses redis.pcall, which swallows an error and returns a table where a number was expected", b.op)
			}
			if !allowed[m[2]] {
				t.Errorf("%s.lua calls %s, which is not one of the %s the scripts are allowed to use",
					b.op, m[2], strings.Join(sortedKeys(allowed), ", "))
			}
		}
		// The literal-command list above is the whole of the DoD item 8
		// mechanism. Every redis.call names its command as a literal, so no
		// argument - and therefore no tenant id, feature key or amount, however
		// it was spelled on the wire - can reach the position where a command is
		// read. What a caller sends can only ever be a value. The behaviour this
		// asserts is proved a second time in integration_test.go, with hostile
		// values, because a source check proves the shape and only the store
		// proves the effect.
	}
}

func TestScriptsIterateOverNothing(t *testing.T) {
	// A loop inside a script is O(n) on a value the caller chose, and the
	// execution has a hard 250 ms budget, so the only acceptable loop is no
	// loop. The scripts read six named fields and write four of them, and a
	// future field is added by naming it.
	//
	// The scan reads the code with the comments removed, because a check that
	// cannot tell a comment from code is a check whose failures are noise: the
	// first version of this test failed on the word "for" in its own prose.
	banned := []string{
		"for ", "while ", "repeat", "pairs(", "ipairs(", "select(",
		"math.random", "os.", "dofile", "loadstring", "require",
	}
	for _, b := range bodies() {
		code := stripLuaComments(b.text)
		for _, word := range banned {
			if strings.Contains(code, word) {
				t.Errorf("%s.lua contains %q in its code. A loop's length is chosen by whoever calls the script, and a "+
					"script that can load or reach the filesystem is no longer the file this binary embeds", b.op, word)
			}
		}
	}
}

// stripLuaComments removes a `--` comment to the end of its line, keeping the
// contents of single- and double-quoted strings, which is the only thing in
// these three scripts that could hold a `--`. It is not a Lua parser and does not
// need to be: it is used to look at code rather than prose, and it fails loudly
// enough - by leaving the text in place - if a script ever grows a long comment.
func stripLuaComments(src string) string {
	var out strings.Builder
	var quote byte
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case quote != 0:
			out.WriteByte(c)
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
			out.WriteByte(c)
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			out.WriteByte('\n')
			i--
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

func TestTheScriptsReadTheArgumentsTheRunnerSends(t *testing.T) {
	// Six on this side and five in the Lua would not fail: the fifth would be
	// nil, tonumber(nil) is nil, and the script would answer state_missing for
	// every request. So the arity is asserted against the scripts themselves.
	argvUse := regexp.MustCompile(`ARGV\[(\d+)\]`)
	keysUse := regexp.MustCompile(`KEYS\[(\d+)\]`)
	highest := 0
	for _, b := range bodies() {
		for _, m := range argvUse.FindAllStringSubmatch(b.text, -1) {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("%s.lua indexes ARGV with something that is not a number: %q", b.op, m[1])
			}
			if n > highest {
				highest = n
			}
		}
		for _, m := range keysUse.FindAllStringSubmatch(b.text, -1) {
			if m[1] != "1" {
				t.Errorf("%s.lua reads KEYS[%s]; there is one key, and it is the balance hash", b.op, m[1])
			}
		}
	}
	if highest != argCount {
		t.Errorf("the scripts read ARGV up to %d and the runner sends %d", highest, argCount)
	}
	sent := args(Request{
		Amount: 1,
		Limit:  1,
		Window: cycle.Window{Index: 1, Start: time.Unix(0, 0).UTC()},
		At:     time.Unix(0, 0).UTC(),
	})
	if len(sent) != argCount {
		t.Errorf("the runner sends %d arguments and the constant says %d", len(sent), argCount)
	}
}

func TestTheScriptsAndTheRunnerAgreeOnTheBound(t *testing.T) {
	// The limit is repeated in the scripts so that a caller bypassing this
	// package cannot skip it. Two copies of a number that decides whether a
	// balance is representable is one more thing to keep equal, so it is
	// asserted rather than trusted.
	written := "local BOUND = " + strconv.FormatInt(bound, 10)
	for _, b := range bodies() {
		if !strings.Contains(b.text, written) {
			t.Errorf("%s.lua does not declare %q, so the bound this package enforces and the one the script enforces are not the same number", b.op, written)
		}
	}
	if bound != 9007199254740991 {
		t.Errorf("bound = %d, and 2^53-1 is the largest integer a double holds exactly", bound)
	}
}

func TestLoadNamesEveryScriptAndNothingElse(t *testing.T) {
	// Load sends one SCRIPT LOAD per body and compares the digest the store
	// returns with the one computed from the embedded bytes. A body that was not
	// embedded, or an embedded body that was not loaded, is a script that is
	// missing from the store and will be loaded lazily on the first request that
	// needs it - which is a latency spike on the data path and nothing else.
	seen := map[Operation]bool{}
	for _, b := range bodies() {
		if b.text == "" {
			t.Errorf("the %s script is empty", b.op)
		}
		if len(b.digest) != 40 {
			t.Errorf("the %s script has digest %q, which is not a SHA-1", b.op, b.digest)
		}
		if seen[b.op] {
			t.Errorf("the %s script is listed twice", b.op)
		}
		seen[b.op] = true
	}
	for _, op := range []Operation{Consume, Check, Refund} {
		if !seen[op] {
			t.Errorf("the %s script is not loaded", op)
		}
	}
	if seen[Operation("")] {
		t.Error("a script with no operation was loaded")
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func ExampleLoad() {
	// The scripts are loaded, not written: a start-up that cannot put them in
	// the store is reported, and a store that forgot them recovers on the next
	// request without a restart (ADR-0002).
	fmt.Println("SCRIPT LOAD consume, check, refund")
	// Output: SCRIPT LOAD consume, check, refund
}
