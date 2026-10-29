package main

import (
	"slices"
	"sync"
)

// readinessState is the set of conditions /readyz reports on, named as
// observability.md section 4 names them. A term is either set, by the code
// that learns it, or watched, read from its source on every probe. A term
// nothing has set is unsatisfied rather than assumed.
type readinessState struct {
	mu      sync.Mutex
	terms   map[string]bool
	watched map[string]func() bool
	order   []string
}

func newReadinessState(terms ...string) *readinessState {
	s := &readinessState{terms: map[string]bool{}, watched: map[string]func() bool{}}
	for _, t := range terms {
		s.order = append(s.order, t)
		s.terms[t] = false
	}
	return s
}

func (s *readinessState) Set(term string, satisfied bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, known := s.terms[term]; !known {
		s.order = append(s.order, term)
	}
	s.terms[term] = satisfied
}

// Watch makes a term read its source on every probe.
func (s *readinessState) Watch(term string, satisfied func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, known := s.terms[term]; !known {
		s.order = append(s.order, term)
	}
	s.terms[term] = false
	s.watched[term] = satisfied
}

func (s *readinessState) Unsatisfied() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, t := range s.order {
		ok := s.terms[t]
		if w := s.watched[t]; w != nil {
			ok = w()
		}
		if !ok {
			out = append(out, t)
		}
	}
	return out
}

func (s *readinessState) Ready() bool { return len(s.Unsatisfied()) == 0 }

func (s *readinessState) ReadyFunc() func() bool { return s.Ready }

// ReadyFuncLogging reports readiness and logs it only when the set of unsatisfied
// terms changes, so a probe polled every second does not fill the log. The
// closure is called from the HTTP handler, so it is called concurrently: the
// remembered set and the first-observation flag are guarded, and the first
// observation always logs, because the transition an operator needs is the one
// from "unknown" to whatever the service is.
func (s *readinessState) ReadyFuncLogging(log func(msg string, terms []string)) func() bool {
	var mu sync.Mutex
	var seen bool
	var last []string
	return func() bool {
		now := s.Unsatisfied()
		ready := len(now) == 0
		mu.Lock()
		defer mu.Unlock()
		if !seen || !slices.Equal(now, last) {
			seen = true
			last = now
			if ready {
				log("ready", now)
			} else {
				log("not ready", now)
			}
		}
		return ready
	}
}
