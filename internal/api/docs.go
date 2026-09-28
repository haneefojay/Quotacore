package api

import (
	"net/http"

	contract "github.com/quotacore/quotacore/api"
)

// serveOpenAPI writes the contract as JSON.
//
// The conversion is done once, at the first request, and the error is reported rather than
// swallowed. A contract that cannot be rendered is a build problem, and answering `200` with an
// empty body here would hide it behind a route that looks like it is working.
func serveOpenAPI(w http.ResponseWriter, _ *http.Request) {
	document, err := contract.DocumentJSON()
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		// The parse error can name a line of a document that is compiled into the binary, so it
		// is a build artefact rather than anything about the request, and there is nothing the
		// caller can do with it. It is written plainly rather than through the error envelope
		// because the envelope is itself part of the contract this route is failing to serve.
		_, _ = w.Write([]byte("the embedded contract could not be rendered as JSON\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(document)
}

// serveDocs writes the generated reference page.
//
// The bytes are the committed artefact, so the handler cannot disagree with the checked page.
// A `Cache-Control` of no-store is deliberate: the page is a rendering of a contract that changes
// with the binary, and a cached reference is a reference to a version of the product that is no
// longer running.
func serveDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(contract.DocsPage)
}
