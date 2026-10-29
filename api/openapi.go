package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"go.yaml.in/yaml/v3"
)

// DocumentJSON is the contract rendered as JSON, which is what `GET /openapi.json` serves.
//
// The route is named `.json` because a client that fetches a contract expects to parse it as
// JSON, and handing it YAML under a JSON content type would be a lie the client only discovers
// at its first `JSON.parse`. The conversion is therefore done here rather than by serving the
// embedded bytes with a mismatched header.
//
// The document is the same document. Two things make that a claim rather than a hope, and both
// are asserted by tests: the conversion preserves mapping order, so the output reads in the
// order the source was written rather than in whatever order a Go map iterates, and
// TestServedContractMatchesTheEmbeddedDocument compares the two by value rather than by bytes.
//
// It is computed once and cached. A 180 KB parse on every request to a debugging route would be
// the kind of cost that makes a route slow in production for a benefit that only accrues to
// developers, and the document cannot change within a process.
var DocumentJSON = sync.OnceValues(func() ([]byte, error) {
	return RenderJSON(Document)
})

// RenderJSON converts an OpenAPI document from YAML to JSON.
//
// It takes the document as an argument for the same reason RenderDocs does: the embedded copy is
// what the service serves, but a converter that can only be run against the embedded copy cannot
// be tested against a document it is supposed to reject.
func RenderJSON(document []byte) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(document, &root); err != nil {
		return nil, fmt.Errorf("parse the contract: %w", err)
	}
	if len(root.Content) == 0 {
		return nil, fmt.Errorf("the contract is empty, so there is nothing to serve")
	}
	// An OpenAPI document is a JSON object. A scalar or a sequence at the root parses, and would
	// render as valid JSON, so without this check a file that is not a contract at all would be
	// served as one with a 200 and a correct content type.
	if root.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("the contract is a YAML %s at the root, not a mapping, so it is not an OpenAPI document",
			kindName(root.Content[0].Kind))
	}
	var buf bytes.Buffer
	if err := writeJSONNode(&buf, root.Content[0]); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// kindName names a YAML node kind for an error message. The yaml package exposes the kinds as
// numbers, and an error that says "kind 2" helps nobody.
func kindName(kind yaml.Kind) string {
	switch kind {
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	default:
		return "unknown"
	}
}

// writeJSONNode renders one YAML node as JSON.
//
// The switch is on the node kind rather than on how the value was quoted, because a YAML scalar
// and a JSON value are the same thing once the tag says which it is. Values that claim a numeric
// or boolean tag but do not parse as one are written as JSON strings rather than emitted raw: an
// invalid bare token would produce a document no client can parse, and a contract that cannot be
// parsed is worse than a contract that quotes one value oddly.
func writeJSONNode(buf *bytes.Buffer, n *yaml.Node) error {
	// An alias is a pointer to an already-parsed node, not a distinct value. Resolving it is
	// what a YAML consumer does, and the document has no merge keys, so a dereference is
	// equivalent to the merge the author asked for.
	if n.Kind == yaml.AliasNode {
		if n.Alias == nil {
			return fmt.Errorf("a YAML alias at line %d has no target", n.Line)
		}
		return writeJSONNode(buf, n.Alias)
	}

	switch n.Kind {
	case yaml.MappingNode:
		buf.WriteByte('{')
		for i := 0; i+1 < len(n.Content); i += 2 {
			if i > 0 {
				buf.WriteByte(',')
			}
			key, err := json.Marshal(n.Content[i].Value)
			if err != nil {
				return fmt.Errorf("marshal the key at line %d: %w", n.Content[i].Line, err)
			}
			buf.Write(key)
			buf.WriteByte(':')
			if err := writeJSONNode(buf, n.Content[i+1]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil

	case yaml.SequenceNode:
		buf.WriteByte('[')
		for i, item := range n.Content {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeJSONNode(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil

	case yaml.ScalarNode:
		return writeJSONScalar(buf, n)

	default:
		return fmt.Errorf("a YAML node of kind %d at line %d has no JSON equivalent", n.Kind, n.Line)
	}
}

func writeJSONScalar(buf *bytes.Buffer, n *yaml.Node) error {
	// Numeric and boolean scalars are marshalled from the parsed value rather than copied from
	// the source text, because the source text is not always valid JSON: YAML accepts `0o17`
	// and a leading `+`, and copying either would produce a document no client can parse. JSON has
	// one number type, so canonicalising `017` to `17` loses nothing a reader could have seen.
	var parsed any
	switch n.ShortTag() {
	case "!!null":
		buf.WriteString("null")
		return nil
	case "!!bool":
		b, err := strconv.ParseBool(n.Value)
		if err != nil {
			break
		}
		parsed = b
	case "!!int":
		i, err := strconv.ParseInt(n.Value, 0, 64)
		if err != nil {
			break
		}
		parsed = i
	case "!!float":
		f, err := strconv.ParseFloat(n.Value, 64)
		if err != nil {
			break
		}
		parsed = f
	}
	if parsed != nil {
		encoded, err := json.Marshal(parsed)
		if err != nil {
			return fmt.Errorf("marshal the scalar at line %d: %w", n.Line, err)
		}
		buf.Write(encoded)
		return nil
	}

	// Everything else is a string, including a timestamp: JSON has no date type, and
	// api-conventions.md requires the offset to be written out, which a string preserves.
	encoded, err := json.Marshal(n.Value)
	if err != nil {
		return fmt.Errorf("marshal the scalar at line %d: %w", n.Line, err)
	}
	buf.Write(encoded)
	return nil
}
