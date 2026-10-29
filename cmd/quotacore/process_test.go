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
	return s.startWith("", io.Discard, "QUOTACORE_METRICS_ADDR=127.0.0.1:0", "QUOTACORE_LOG_LEVEL=error")
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// startWith starts the binary in dir (the test's own when empty), writing to
// out, with env after the stack's own.
func (s *stack) startWith(dir string, out io.Writer, env ...string) *process {
	s.t.Helper()
	return s.startArgs(nil, dir, out, env...)
}

// startArgs is startWith with the binary's arguments, such as serve.
func (s *stack) startArgs(args []string, dir string, out io.Writer, env ...string) *process {
	s.t.Helper()
	addr := freeAddr(s.t)
	_, redisURL := integrationURLs(s.t)
	cmd := exec.Command(binary(s.t), args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(),
		"QUOTACORE_DATABASE_URL="+s.dbURL,
		"QUOTACORE_REDIS_URL="+redisURL,
		"QUOTACORE_LISTEN_ADDR="+addr,
		"QUOTACORE_CONFIG_REFRESH_INTERVAL=1s",
	), env...)
	cmd.Stdout, cmd.Stderr = out, out
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

// TestTheBinaryRunsOnAReadOnlyFilesystem (IP-13 DoD 8, D13): the built
// binary, run from a read-only working directory with read-only HOME and
// TMPDIR, becomes ready, serves a consume and its metrics, drains on
// SIGTERM, and leaves every one of those directories as it found them. It
// warns when it runs as root.
func TestTheBinaryRunsOnAReadOnlyFilesystem(t *testing.T) {
	s := newStack(t)
	f, _, tn := s.provisioned(100)
	work, home, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	for _, d := range []string{work, home, tmp} {
		if err := os.Chmod(d, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(d, 0o755) })
	}
	metricsAddr := freeAddr(t)
	var out syncBuffer
	p := s.startWith(work, &out, "HOME="+home, "TMPDIR="+tmp, "QUOTACORE_METRICS_ADDR="+metricsAddr)
	waitFor(t, 10*time.Second, "the binary to be ready", func() bool {
		resp, err := http.Get(p.base + "/readyz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	if status, err := p.consume(s.runtime, f, 1, tn.External, newKey()); err != nil || status != http.StatusOK {
		t.Errorf("a consume on a read-only filesystem = %d %v", status, err)
	}
	resp, err := http.Get("http://" + metricsAddr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "quotacore_build_info") {
		t.Errorf("the metrics listener = %d", resp.StatusCode)
	}
	if resp, err := http.Get(p.base + "/metrics"); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("the API listener answers /metrics %d, want 404", resp.StatusCode)
		}
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Wait(); err != nil {
		t.Errorf("the binary did not exit cleanly on SIGTERM: %v\n%s", err, out.String())
	}
	for _, d := range []string{work, home, tmp} {
		if entries, err := os.ReadDir(d); err != nil || len(entries) != 0 {
			t.Errorf("%s holds %v after the run (%v); the server wrote a file", d, entries, err)
		}
	}
	root := strings.Contains(out.String(), "running as root")
	if root != (os.Geteuid() == 0) {
		t.Errorf("running with euid %d, the root warning was logged: %v", os.Geteuid(), root)
	}
	for _, want := range []string{`"job":"startup"`, `"job":"shutdown"`, `"msg":"stopped"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the binary's output has no %s:\n%s", want, out.String())
		}
	}
}
