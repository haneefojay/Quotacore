// Package api holds the OpenAPI contract: the source document, the types ogen generates
// from it, and the JSON codecs and request validators that come with those types.
//
// It is deliberately the only package at the module root, and it holds no behaviour. The
// types know what a valid request looks like; nothing here decides whether a request is
// allowed, computes a balance, or opens a connection. The routes that would do those things
// belong to IP-07 and IP-09, and `api/.ogen/ogen.yml` disables ogen's router, client and
// unimplemented-handler features so that a future regeneration cannot quietly add a 501
// handler that answers with the catalogue's own not_implemented code.
package api

import _ "embed"

// Document is the contract this package was generated from, byte for byte.
//
// It is embedded rather than read from disk for two reasons. A binary that can serve a document
// it does not carry serves a 404 on the one route a client needs to debug a 400, and
// regenerating from the embedded copy rather than from the working tree means the served
// document and the generated code cannot disagree within one build.
//
// GET /openapi.json serves a JSON rendering of these bytes rather than the bytes themselves,
// because a client fetching a .json route expects to parse JSON. DocumentJSON does the
// conversion and the tests assert it is faithful; see openapi.go.
//
//go:embed openapi.yaml
var Document []byte
