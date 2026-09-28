package api

import "net/http"

type Readiness func() bool

func Health(ready Readiness) (liveness http.HandlerFunc, readiness http.HandlerFunc) {
	liveness = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	readiness = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ready == nil || !ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return liveness, readiness
}
