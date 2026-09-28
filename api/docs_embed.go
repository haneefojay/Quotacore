package api

import _ "embed"

// DocsPage is the generated HTML reference served at /docs.
//
// It is embedded, not rendered per request, for the same reason the contract is embedded: the
// service must serve its own reference on a host with no egress, and a page assembled from a
// template at request time is a page that can fail to assemble. The page is also the one
// artefact whose staleness is checked, by TestDocsPageIsUpToDate, because a reference that has
// drifted from the contract is worse than no reference: it answers confidently and wrongly.
//
// It carries no script tag and loads nothing from a network. That is a requirement of the page,
// not a preference, and TestDocsPageIsSelfContained fails the build if a future edit adds one.
//
// Regenerate it with `make docs` after any change to openapi.yaml.
//
//go:embed docs.html
var DocsPage []byte
