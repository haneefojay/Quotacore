package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

type RouterOptions struct {
	Ready Readiness
}

// NewRouter builds the HTTP surface the current phase owns.
//
// It serves the two probes and the two documentation routes, and nothing else. Every product
// route in the contract is deliberately absent, so a request to one is a 404 from chi rather
// than a 501 dressed up as an implementation. That is the honest answer: the behaviour does not
// exist yet, and a page that says `not_implemented` would be the service admitting a code it
// should never raise. The routes arrive with the phases named in roadmap-index.md.
func NewRouter(opts RouterOptions) http.Handler {
	liveness, readiness := Health(opts.Ready)
	r := chi.NewRouter()
	r.Get("/healthz", liveness)
	r.Get("/readyz", readiness)
	r.Get("/openapi.json", serveOpenAPI)
	r.Get("/docs", serveDocs)
	return r
}
