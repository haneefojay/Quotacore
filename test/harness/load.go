package harness

import (
	"context"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Load is one open-loop run (IP-16 D2): requests are sent at a constant
// arrival rate whatever the service does, and each latency is measured from
// the instant the request was released to the workers, not from when a worker
// got to it, so a stall shows as the latency every waiting request saw rather
// than as a pause in sending. That is coordinated omission avoided, which a
// closed loop would hide (performance.md section 6). The release is the due
// instant or, when the generator's own timer woke late, the instant it woke:
// a sleep that overshoots is the generator's delay, not the service's.
type Load struct {
	URL string
	// Keys are runtime tokens, used in turn; the per-key rate limit is
	// 2,000 a second (deployment.md section 4), so a run above it needs more
	// than one key.
	Keys []string
	// Tenants are external ids and Features feature keys; request i uses
	// tenant i mod len and feature (i / len(Tenants)) mod len, so every pair
	// is reached.
	Tenants  []string
	Features []string
	// Op is "consume" or "check".
	Op     string
	Amount int64
	Rate   float64
	// Duration is how long requests are sent for.
	Duration time.Duration
	// Concurrency bounds the requests in flight; one due while all are busy
	// waits, and its wait is part of its latency.
	Concurrency int
	Timeout     time.Duration
	// RunID makes every Idempotency-Key unique across runs.
	RunID string
}

// Result is a run's figures. Latencies are in microseconds.
type Result struct {
	Op        string             `json:"op"`
	Rate      float64            `json:"rate_per_s"`
	Duration  string             `json:"duration"`
	Sent      int64              `json:"sent"`
	OK        int64              `json:"ok"`
	Outcomes  map[string]int64   `json:"outcomes"`
	Achieved  float64            `json:"achieved_rate_per_s"`
	Latency   Percentiles        `json:"latency_us"`
	PerMinute []MinutePercentile `json:"per_minute"`
}

// Percentiles of one set of latencies, in microseconds.
type Percentiles struct {
	Count int64 `json:"count"`
	P50   int64 `json:"p50"`
	P99   int64 `json:"p99"`
	P999  int64 `json:"p999"`
	Max   int64 `json:"max"`
}

// MinutePercentile is one minute of the run.
type MinutePercentile struct {
	Minute int `json:"minute"`
	Percentiles
}

type sample struct {
	due     time.Duration // since start
	latency time.Duration
	outcome string
}

// Run sends the load and returns its figures. It stops early only if ctx ends.
func Run(ctx context.Context, l Load) Result {
	if l.Concurrency <= 0 {
		l.Concurrency = 512
	}
	if l.Timeout <= 0 {
		l.Timeout = 5 * time.Second
	}
	if l.Op == "" {
		l.Op = "consume"
	}
	if l.Amount == 0 && l.Op == "consume" {
		l.Amount = 1
	}
	transport := &http.Transport{
		MaxIdleConns:        l.Concurrency,
		MaxIdleConnsPerHost: l.Concurrency,
		MaxConnsPerHost:     l.Concurrency,
		IdleConnTimeout:     time.Minute,
		DisableCompression:  true,
		DialContext:         (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}
	client := &http.Client{Transport: transport, Timeout: l.Timeout}
	defer transport.CloseIdleConnections()

	type job struct {
		i        int64
		due      time.Time
		released time.Time
	}
	total := int64(l.Rate * l.Duration.Seconds())
	jobs := make(chan job, min(total, 1<<20)+1)
	results := make(chan sample, 4096)
	start := time.Now().Add(50 * time.Millisecond)
	path := "/v1/" + l.Op

	var wg sync.WaitGroup
	for range l.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				tenant := l.Tenants[j.i%int64(len(l.Tenants))]
				feature := l.Features[(j.i/int64(len(l.Tenants)))%int64(len(l.Features))]
				body := `{"feature_key":"` + feature + `","amount":` + strconv.FormatInt(l.Amount, 10) +
					`,"tenant_external_id":"` + tenant + `"}`
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, l.URL+path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+l.Keys[j.i%int64(len(l.Keys))])
				req.Header.Set("Content-Type", "application/json")
				if l.Op == "consume" {
					req.Header.Set("Idempotency-Key", "lg-"+l.RunID+"-"+strconv.FormatInt(j.i, 36))
				}
				outcome := "transport"
				resp, err := client.Do(req)
				if err == nil {
					raw, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					outcome = strconv.Itoa(resp.StatusCode)
					if resp.StatusCode >= 300 {
						outcome += " " + errorCode(raw)
					}
				}
				results <- sample{due: j.due.Sub(start), latency: time.Since(j.released), outcome: outcome}
			}
		}()
	}

	var collected []sample
	done := make(chan struct{})
	go func() {
		for r := range results {
			collected = append(collected, r)
		}
		close(done)
	}()

	// The dispatcher releases every request whose instant has come, then
	// sleeps to the next one; at 10,000 a second that is a batch per wake.
	period := time.Duration(float64(time.Second) / l.Rate)
	var sent atomic.Int64
	time.Sleep(time.Until(start))
	for i := int64(0); i < total && ctx.Err() == nil; {
		now := time.Now()
		for ; i < total; i++ {
			due := start.Add(time.Duration(i) * period)
			if due.After(now) {
				break
			}
			jobs <- job{i: i, due: due, released: now}
			sent.Add(1)
		}
		if i < total {
			time.Sleep(time.Until(start.Add(time.Duration(i) * period)))
		}
	}
	close(jobs)
	wg.Wait()
	close(results)
	<-done
	elapsed := time.Since(start)

	res := Result{Op: l.Op, Rate: l.Rate, Duration: l.Duration.String(), Sent: sent.Load(), Outcomes: map[string]int64{}}
	var all []time.Duration
	minutes := map[int][]time.Duration{}
	for _, s := range collected {
		res.Outcomes[s.outcome]++
		if strings.HasPrefix(s.outcome, "2") {
			res.OK++
		}
		all = append(all, s.latency)
		m := int(s.due / time.Minute)
		minutes[m] = append(minutes[m], s.latency)
	}
	res.Achieved = float64(len(collected)) / elapsed.Seconds()
	res.Latency = percentiles(all)
	for m := 0; m <= int(l.Duration/time.Minute); m++ {
		if v := minutes[m]; len(v) > 0 {
			res.PerMinute = append(res.PerMinute, MinutePercentile{Minute: m, Percentiles: percentiles(v)})
		}
	}
	return res
}

// errorCode is the envelope's code, read without decoding the body.
func errorCode(body []byte) string {
	s := string(body)
	i := strings.Index(s, `"code":"`)
	if i < 0 {
		return ""
	}
	s = s[i+8:]
	if j := strings.IndexByte(s, '"'); j >= 0 {
		return s[:j]
	}
	return ""
}

func percentiles(v []time.Duration) Percentiles {
	if len(v) == 0 {
		return Percentiles{}
	}
	slices.Sort(v)
	at := func(q float64) int64 {
		i := int(q*float64(len(v))+0.5) - 1
		i = max(0, min(i, len(v)-1))
		return v[i].Microseconds()
	}
	return Percentiles{Count: int64(len(v)), P50: at(0.50), P99: at(0.99), P999: at(0.999), Max: v[len(v)-1].Microseconds()}
}

// Errors is every outcome that is not a 2xx.
func (r Result) Errors() map[string]int64 {
	out := map[string]int64{}
	for k, n := range r.Outcomes {
		if !strings.HasPrefix(k, "2") {
			out[k] = n
		}
	}
	return out
}
