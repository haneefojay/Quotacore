package harness

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Metrics is one scrape of the service's /metrics, keyed by series as it is
// printed: the name, with its labels if it has any.
type Metrics map[string]float64

// Scrape reads the metrics listener once.
func Scrape() (Metrics, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, MetricsURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return parseMetrics(resp.Body), nil
}

func parseMetrics(r io.Reader) Metrics {
	m := Metrics{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			continue
		}
		if v, err := strconv.ParseFloat(line[i+1:], 64); err == nil {
			m[line[:i]] = v
		}
	}
	return m
}

// Sum adds every series of a name, whatever its labels, with each label
// filter ("key=\"value\"") present.
func (m Metrics) Sum(name string, labels ...string) float64 {
	var total float64
	for series, v := range m {
		base, rest, _ := strings.Cut(series, "{")
		if base != name {
			continue
		}
		match := true
		for _, l := range labels {
			if !strings.Contains(rest, l) {
				match = false
				break
			}
		}
		if match {
			total += v
		}
	}
	return total
}

// Point is one sample of the figures a soak watches.
type Point struct {
	At         time.Duration `json:"at"`
	Goroutines float64       `json:"goroutines"`
	Memory     float64       `json:"memory_bytes"`
	FDs        float64       `json:"open_fds"`
	StoreBytes float64       `json:"store_used_memory"`
	Skew       float64       `json:"clock_skew_seconds"`
}

// Sampler takes a Point every interval until stopped.
type Sampler struct {
	mu     sync.Mutex
	points []Point
	stop   chan struct{}
	done   chan struct{}
}

// Sample starts a sampler. fds and storeBytes are read by the caller's
// functions, since one needs the container's pid and the other the store.
func Sample(every time.Duration, fds, storeBytes func() float64) *Sampler {
	s := &Sampler{stop: make(chan struct{}), done: make(chan struct{})}
	start := time.Now()
	go func() {
		defer close(s.done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			if m, err := Scrape(); err == nil {
				p := Point{At: time.Since(start), Goroutines: m.Sum("quotacore_goroutines"),
					Memory: m.Sum("quotacore_memory_bytes"), Skew: m.Sum("quotacore_clock_skew_seconds")}
				if fds != nil {
					p.FDs = fds()
				}
				if storeBytes != nil {
					p.StoreBytes = storeBytes()
				}
				s.mu.Lock()
				s.points = append(s.points, p)
				s.mu.Unlock()
			}
			select {
			case <-s.stop:
				return
			case <-t.C:
			}
		}
	}()
	return s
}

// Stop ends the sampling and returns every point.
func (s *Sampler) Stop() []Point {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Point(nil), s.points...)
}

// Slope is the least-squares slope of y over x, per unit of x.
func Slope(x, y []float64) float64 {
	n := float64(len(x))
	if n < 2 {
		return 0
	}
	var sx, sy, sxx, sxy float64
	for i := range x {
		sx += x[i]
		sy += y[i]
		sxx += x[i] * x[i]
		sxy += x[i] * y[i]
	}
	d := n*sxx - sx*sx
	if d == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / d
}
