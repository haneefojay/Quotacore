package api

import (
	"net/http"

	contract "github.com/quotacore/quotacore/api"
)

type Readiness func() bool

// Health is the two probes as the contract defines them (IP-13 D7). Liveness
// checks nothing and answers 200 {"status":"ok"} from start to exit, a drain
// included. Readiness answers 200 {"status":"ready"} when ready, and a bare
// 503 otherwise, because an orchestrator reads the status and nothing else.
func Health(ready Readiness) (liveness http.HandlerFunc, readiness http.HandlerFunc) {
	liveness = func(w http.ResponseWriter, _ *http.Request) {
		probeBody(w, contract.HealthResponseStatusOk)
	}
	readiness = func(w http.ResponseWriter, _ *http.Request) {
		if ready == nil || !ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		probeBody(w, contract.HealthResponseStatusReady)
	}
	return liveness, readiness
}

func probeBody(w http.ResponseWriter, status contract.HealthResponseStatus) {
	body, _ := (&contract.HealthResponse{Status: status}).MarshalJSON()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
