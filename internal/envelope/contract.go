// Package envelope is the one place a response leaves the service: the error
// envelope every non-2xx answer uses (error-catalog.md section 1), the JSON
// writer for the 2xx bodies, and the request-scoped record that carries the
// request id, the endpoint and the outcome to the access log and the metrics.
//
// The status of each error code and the codes each operation may return are
// read from the embedded contract, not written down a second time, so a code
// and its status cannot drift apart: the contract is the only table.
package envelope

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"

	contract "github.com/quotacore/quotacore/api"
)

// Operation is one route of the contract, as the router and the metrics need
// it.
type Operation struct {
	Method        string // GET, POST, PATCH, DELETE
	Path          string // the contract's template, such as /v1/admin/plans/{id}
	ID            string // operationId
	RequiredScope string // runtime, admin, or empty for the operational routes
	HasBody       bool
	Codes         []contract.ErrorCode
}

// Endpoint is the label value observability.md section 2 uses: "POST /v1/consume".
func (o Operation) Endpoint() string { return o.Method + " " + o.Path }

type catalogue struct {
	status     map[contract.ErrorCode]int
	operations []Operation
}

var loadCatalogue = sync.OnceValues(func() (catalogue, error) { return parseContract(contract.Document) })

// Status is the HTTP status the contract gives a code. An unknown code is a
// programming error, answered as 500 so it is at least loud.
func Status(code contract.ErrorCode) int {
	c, err := loadCatalogue()
	if err != nil {
		return 500
	}
	if s, ok := c.status[code]; ok {
		return s
	}
	return 500
}

// Operations lists every operation in the contract, sorted by endpoint. The
// error is a build defect: the contract is compiled into the binary.
func Operations() ([]Operation, error) {
	c, err := loadCatalogue()
	if err != nil {
		return nil, err
	}
	return append([]Operation(nil), c.operations...), nil
}

// MustOperations is Operations for the wiring that cannot proceed without them.
func MustOperations() []Operation {
	ops, err := Operations()
	if err != nil {
		panic(err)
	}
	return ops
}

func parseContract(document []byte) (catalogue, error) {
	var doc struct {
		Paths      map[string]map[string]yaml.Node `yaml:"paths"`
		Components struct {
			Responses map[string]struct {
				Status int      `yaml:"x-status"`
				Codes  []string `yaml:"x-codes"`
			} `yaml:"responses"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(document, &doc); err != nil {
		return catalogue{}, fmt.Errorf("envelope: parse the contract: %w", err)
	}
	c := catalogue{status: map[contract.ErrorCode]int{}}
	for name, r := range doc.Components.Responses {
		for _, raw := range r.Codes {
			code := contract.ErrorCode(raw)
			if prev, seen := c.status[code]; seen && prev != r.Status {
				return catalogue{}, fmt.Errorf("envelope: %s is %d in one response and %d in %s", raw, prev, r.Status, name)
			}
			c.status[code] = r.Status
		}
	}
	for path, item := range doc.Paths {
		for method, node := range item {
			upper := strings.ToUpper(method)
			switch upper {
			case "GET", "POST", "PUT", "PATCH", "DELETE":
			default:
				continue // parameters and the like
			}
			var op struct {
				ID          string                    `yaml:"operationId"`
				Scope       string                    `yaml:"x-required-scope"`
				RequestBody any                       `yaml:"requestBody"`
				Responses   map[string]map[string]any `yaml:"responses"`
			}
			if err := node.Decode(&op); err != nil {
				return catalogue{}, fmt.Errorf("envelope: decode %s %s: %w", upper, path, err)
			}
			o := Operation{Method: upper, Path: path, ID: op.ID, RequiredScope: op.Scope, HasBody: op.RequestBody != nil}
			seen := map[contract.ErrorCode]bool{}
			for _, resp := range op.Responses {
				var codes []string
				if ref, ok := resp["$ref"].(string); ok {
					codes = doc.Components.Responses[strings.TrimPrefix(ref, "#/components/responses/")].Codes
				} else if inline, ok := resp["x-codes"].([]any); ok {
					for _, c := range inline {
						if s, ok := c.(string); ok {
							codes = append(codes, s)
						}
					}
				}
				for _, raw := range codes {
					if code := contract.ErrorCode(raw); !seen[code] {
						seen[code] = true
						o.Codes = append(o.Codes, code)
					}
				}
			}
			sort.Slice(o.Codes, func(i, j int) bool { return o.Codes[i] < o.Codes[j] })
			c.operations = append(c.operations, o)
		}
	}
	sort.Slice(c.operations, func(i, j int) bool { return c.operations[i].Endpoint() < c.operations[j].Endpoint() })
	return c, nil
}
