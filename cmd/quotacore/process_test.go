package main

import (
	"context"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The two inherited process tests run the built binary, because a SIGKILL is
// the thing under test and an in-process server cannot be killed.

var (
	binaryOnce sync.Once
	binaryPath string
	binaryErr  error
)

func binary(t *testing.T) string {
	t.Helper()
	binaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "quotacore-bin")
		if err != nil {
			binaryErr = err
			return
		}
		binaryPath = filepath.Join(dir, "quotacore")
		out, err := exec.Command("go", "build", "-o", binaryPath, ".").CombinedOutput()
		if err != nil {
			binaryErr = &buildError{err, string(out)}
		}
	})
	if binaryErr != nil {
		t.Fatal(binaryErr)
	}
	return binaryPath
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + ": " + e.out }

// process is one running instance of the binary against the stack's stores.
type process struct {
	cmd  *exec.Cmd
	base string
}

func (s *stack) start() *process {
	s.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, redisURL := integrationURLs(s.t)
	cmd := exec.Command(binary(s.t))
	cmd.Env = append(os.Environ(),
		"QUOTACORE_DATABASE_URL="+s.dbURL,
		"QUOTACORE_REDIS_URL="+redisURL,
		"QUOTACORE_LISTEN_ADDR="+addr,
		"QUOTACORE_CONFIG_REFRESH_INTERVAL=1s",
		"QUOTACORE_LOG_LEVEL=error",
	)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	p := &process{cmd: cmd, base: "http://" + addr}
	s.t.Cleanup(p.kill)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if resp, err := http.Get(p.base + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return p
			}
		}
		if time.Now().After(deadline) {
			s.t.Fatal("the binary did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (p *process) kill() {
	if p.cmd.ProcessState == nil {
		_ = p.cmd.Process.Signal(syscall.SIGKILL)
		_ = p.cmd.Wait()
	}
}

// consume sends one consume to the process; err is set when the process died
// before it answered.
func (p *process) consume(token, feature string, amount int64, external, key string) (int, error) {
	req, err := http.NewRequest(http.MethodPost, p.base+"/v1/consume", strings.NewReader(metered(feature, amount, external)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// TestT09KillDuringConsume: a process killed at a random point of a consume
// of 30 from 100 leaves the balance at 100 or 70, never anything else, and a
// retry with the same key on the next process settles it at exactly 70.
// Two hundred kills in CI, twenty elsewhere.
func TestT09KillDuringConsume(t *testing.T) {
	runs := 20
	if os.Getenv("CI") != "" {
		runs = 200
	}
	s := newStack(t)
	tn := s.tenant(externalID("t09"), "UTC", monthlyAnchor, ent{"t09.f", 100, "monthly", false})
	p := s.start()
	for i := 0; i < runs; i++ {
		s.seedCurrent(tn, "t09.f", 100)
		key := newKey()
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = p.consume(s.runtime, "t09.f", 30, tn.External, key)
		}()
		time.Sleep(time.Duration(rand.IntN(3000)) * time.Microsecond)
		p.kill()
		<-done
		if b := s.balance(tn, "t09.f"); b != 100 && b != 70 {
			t.Fatalf("run %d: the balance after a kill is %d, want 100 or 70", i, b)
		}
		p = s.start()
		if status, err := p.consume(s.runtime, "t09.f", 30, tn.External, key); err != nil || status != 200 {
			t.Fatalf("run %d: the retry = %d %v", i, status, err)
		}
		if b := s.balance(tn, "t09.f"); b != 70 {
			t.Fatalf("run %d: after the retry the balance is %d, want exactly one deduction, 70", i, b)
		}
	}
}

// TestT10RestartChangesNothing: kill the process under traffic, then start
// it again; every balance hash, its expiry and every idempotency record is
// byte-identical before and after the restart, including a hash whose window
// has closed and that nothing has touched since.
func TestT10RestartChangesNothing(t *testing.T) {
	s := newStack(t)
	busy := s.tenant(externalID("t10"), "UTC", monthlyAnchor,
		ent{"t10.a", 1000, "monthly", false}, ent{"t10.b", 1000, "daily", false})
	s.seedCurrent(busy, "t10.a", 1000)
	s.seedCurrent(busy, "t10.b", 1000)
	idle := s.tenant(externalID("t10i"), "UTC", monthlyAnchor, ent{"t10.c", 50, "daily", false})
	now := s.currentWindow(idle, "t10.c", time.Now())
	prev := s.currentWindow(idle, "t10.c", now.Start.Add(-time.Millisecond))
	s.seed(idle, "t10.c", stored{Balance: 12, Limit: 50, Index: prev.Index, Start: prev.Start, End: prev.End})

	p := s.start()
	ctx, stop := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			f := []string{"t10.a", "t10.b"}[w%2]
			for ctx.Err() == nil {
				if _, err := p.consume(s.runtime, f, 1, busy.External, newKey()); err != nil {
					return
				}
			}
		}(w)
	}
	time.Sleep(300 * time.Millisecond)
	p.kill()
	stop()
	wg.Wait()

	snap := func() map[string]state {
		return map[string]state{
			"a": s.state(busy, "t10.a"),
			"b": s.state(busy, "t10.b"),
			"c": s.state(idle, "t10.c"),
		}
	}
	before := snap()
	if before["a"].Hash["balance"] == "1000" && before["b"].Hash["balance"] == "1000" {
		t.Fatal("no consume landed before the kill, so the test proves nothing")
	}
	s.start()
	time.Sleep(2500 * time.Millisecond) // past two configuration refreshes
	if after := snap(); !reflect.DeepEqual(before, after) {
		t.Errorf("the restart changed stored state:\nbefore %+v\nafter  %+v", before, after)
	}
}
