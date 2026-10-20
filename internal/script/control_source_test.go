package script

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The control-plane scripts (IP-10, D8) are held to the data plane's source
// rules and to two of their own: the three that adjust a balance open with the
// data plane's transition block byte for byte, so a grant to an idle tenant
// lands in the current cycle; and the two that overwrite read exactly the
// arguments their runner sends.

func controlBodies() []body {
	return []body{grantBody, setBody, relimitBody, reanchorBody, rolloverBody}
}

func transitionBlock(t *testing.T, b body) string {
	t.Helper()
	if strings.Count(b.text, blockOpen) != 1 || strings.Count(b.text, blockClose) != 1 {
		t.Fatalf("%s.lua has %d open and %d close markers for the transition block; exactly one of each is required",
			b.op, strings.Count(b.text, blockOpen), strings.Count(b.text, blockClose))
	}
	start, end := strings.Index(b.text, blockOpen), strings.Index(b.text, blockClose)
	if start > end {
		t.Fatalf("%s.lua closes the transition block before it opens it", b.op)
	}
	return b.text[start : end+len(blockClose)]
}

// TestTheSixScriptsShareOneTransitionBlock extends INV-C2 to grant, set and
// relimit: one transition, six transcriptions.
func TestTheSixScriptsShareOneTransitionBlock(t *testing.T) {
	want := transitionBlock(t, consumeBody)
	for _, b := range []body{grantBody, setBody, relimitBody} {
		if transitionBlock(t, b) != want {
			t.Errorf("%s.lua has a transition block that differs from consume.lua's; a control-plane write would then "+
				"decide differently from a request when a tenant's allowance resets (INV-C2)", b.op)
		}
	}
	for _, b := range []body{reanchorBody, rolloverBody} {
		if strings.Contains(b.text, blockOpen) {
			t.Errorf("%s.lua carries the transition block, and it overwrites by design rather than rolling", b.op)
		}
	}
}

// TestTheControlScriptsKeepTheSourceRules: literal commands from the data
// plane's list and nothing else, no loop, no pcall, and the same bound.
func TestTheControlScriptsKeepTheSourceRules(t *testing.T) {
	allowed := map[string]bool{"HMGET": true, "HSET": true, "HINCRBY": true, "PERSIST": true}
	callSite := regexp.MustCompile(`redis\.(call|pcall)\(\s*'([A-Za-z]+)'`)
	anyCall := regexp.MustCompile(`redis\.(call|pcall)\(`)
	banned := []string{
		"for ", "while ", "repeat", "pairs(", "ipairs(", "select(",
		"math.random", "os.", "dofile", "loadstring", "require",
	}
	bound := "local BOUND = " + strconv.FormatInt(bound, 10)
	for _, b := range controlBodies() {
		code := stripLuaComments(b.text)
		matches := callSite.FindAllStringSubmatch(code, -1)
		if len(matches) == 0 || len(matches) != len(anyCall.FindAllString(code, -1)) {
			t.Errorf("%s.lua makes %d store calls of which %d name a literal command", b.op,
				len(anyCall.FindAllString(code, -1)), len(matches))
		}
		for _, m := range matches {
			if m[1] != "call" || !allowed[m[2]] {
				t.Errorf("%s.lua uses redis.%s('%s'), outside %s", b.op, m[1], m[2], strings.Join(sortedKeys(allowed), ", "))
			}
		}
		for _, word := range banned {
			if strings.Contains(code, word) {
				t.Errorf("%s.lua contains %q in its code", b.op, word)
			}
		}
		if !strings.Contains(b.text, bound) {
			t.Errorf("%s.lua does not declare %q", b.op, bound)
		}
		if strings.Contains(b.text, "KEYS[2]") {
			t.Errorf("%s.lua reads a second key; a control-plane script writes no idempotency record", b.op)
		}
	}
}

// TestTheControlScriptsReadTheArgumentsTheRunnerSends: the three adjusting
// scripts read the data plane's first six, reanchor reads six of its own and
// rollover four. A script reading fewer than it is sent fails open.
func TestTheControlScriptsReadTheArgumentsTheRunnerSends(t *testing.T) {
	want := map[Operation]int{Grant: 6, Set: 6, Relimit: 6, Reanchor: 6, Rollover: 4}
	argvUse := regexp.MustCompile(`ARGV\[(\d+)\]`)
	for _, b := range controlBodies() {
		highest := 0
		for _, m := range argvUse.FindAllStringSubmatch(stripLuaComments(b.text), -1) {
			n, _ := strconv.Atoi(m[1])
			if n > highest {
				highest = n
			}
		}
		if highest != want[b.op] {
			t.Errorf("%s.lua reads ARGV up to %d; its runner sends %d", b.op, highest, want[b.op])
		}
	}
}

// TestLoadNamesAllEightScripts: the load loop puts every embedded body in the
// store, the control plane's included, once each.
func TestLoadNamesAllEightScripts(t *testing.T) {
	seen := map[Operation]bool{}
	for _, b := range allBodies() {
		if b.text == "" || len(b.digest) != 40 || seen[b.op] {
			t.Errorf("the %s script is empty, has digest %q, or is listed twice", b.op, b.digest)
		}
		seen[b.op] = true
	}
	for _, op := range []Operation{Consume, Check, Refund, Grant, Set, Relimit, Reanchor, Rollover} {
		if !seen[op] {
			t.Errorf("the %s script is not loaded", op)
		}
	}
}
