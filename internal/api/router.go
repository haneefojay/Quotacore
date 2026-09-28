package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

type RouterOptions struct {
	Ready Readiness
}

func NewRouter(opts RouterOptions) http.Handler {
	liveness, readiness := Health(opts.Ready)
	r := chi.NewRouter()
	r.Get("/healthz", liveness)
	r.Get("/readyz", readiness)
	return r
}
