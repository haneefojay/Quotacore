package envelope

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	contract "github.com/quotacore/quotacore/api"
)

var catalogueRow = regexp.MustCompile("(?m)^\\| `([a-z][a-z0-9_]*)` \\| (\\d{3}) \\|")

// TestStatusesAreTheCatalogues: the status read from the contract for every
// code is the status error-catalog.md section 2 gives it, so the one table the
// service reads and the one a reader reads cannot disagree.
func TestStatusesAreTheCatalogues(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "product", "error-catalog.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	section := text[strings.Index(text, "\n## 2. "):strings.Index(text, "\n## 3. ")]
	rows := catalogueRow.FindAllStringSubmatch(section, -1)
	if len(rows) != len(contract.ErrorCode("").AllValues()) {
		t.Fatalf("%d catalogue rows, %d enum values", len(rows), len(contract.ErrorCode("").AllValues()))
	}
	for _, m := range rows {
		want, _ := strconv.Atoi(m[2])
		if got := Status(contract.ErrorCode(m[1])); got != want {
			t.Errorf("Status(%s) = %d, want the catalogue's %d", m[1], got, want)
		}
	}
	if Status("no_such_code") != 500 {
		t.Error("an unknown code must be a loud 500")
	}
}

// TestTheEnvelopeIsTheContractsShape: what Write produces decodes and
// validates as the generated ErrorResponse, carries the request id, writes
// Retry-After only when asked, and counts itself once.
func TestTheEnvelopeIsTheContractsShape(t *testing.T) {
	var counted []string
	ex := NewExchange("01J8Z6M2QK9V3X7B4C0N5TDEFA", time.Now(), func(c contract.ErrorCode, endpoint string) {
		counted = append(counted, string(c)+"|"+endpoint)
	})
	ex.Endpoint = "POST /v1/consume"
	for _, p := range []Problem{
		Error(contract.ErrorCodeTenantNotFound, "no such tenant"),
		Error(contract.ErrorCodeServiceUnavailable, "later").Retry(100),
		func() Problem {
			p := Error(contract.ErrorCodeRateLimited, "slow down").Retry(250).With("limit_per_second", 2000)
			p.RetryAfter = 1
			return p
		}(),
		Error(contract.ErrorCodeValidationFailed, "bad").With("errors", []map[string]string{{"field": "amount", "issue": "x"}}),
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/consume", nil)
		Write(rec, req.WithContext(With(req.Context(), ex)), p)
		var got contract.ErrorResponse
		if err := got.UnmarshalJSON(rec.Body.Bytes()); err != nil {
			t.Fatalf("%s: %v: %s", p.Code, err, rec.Body)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("%s: %v", p.Code, err)
		}
		if got.Error.Code != p.Code || string(got.Error.RequestID) != ex.RequestID || rec.Code != Status(p.Code) {
			t.Errorf("%s: %+v at %d", p.Code, got.Error, rec.Code)
		}
		if (rec.Header().Get("Retry-After") != "") != (p.RetryAfter > 0) {
			t.Errorf("%s: Retry-After = %q", p.Code, rec.Header().Get("Retry-After"))
		}
		if got.Error.RetryAfterMs.Set != (p.RetryAfterMs != nil) {
			t.Errorf("%s: retry_after_ms present = %v", p.Code, got.Error.RetryAfterMs.Set)
		}
		if strings.Contains(rec.Body.String(), "null") {
			t.Errorf("%s: an absent field was written as null: %s", p.Code, rec.Body)
		}
	}
	if len(counted) != 4 || counted[0] != "tenant_not_found|POST /v1/consume" {
		t.Errorf("counted = %v", counted)
	}
}

// TestOperationsAreTheContracts: the parsed operations carry their codes and
// scopes, and the four data-plane routes are runtime.
func TestOperationsAreTheContracts(t *testing.T) {
	ops := MustOperations()
	byEndpoint := map[string]Operation{}
	for _, o := range ops {
		byEndpoint[o.Endpoint()] = o
	}
	for _, e := range []string{"POST /v1/consume", "POST /v1/refund", "POST /v1/check", "GET /v1/balance"} {
		if byEndpoint[e].RequiredScope != "runtime" {
			t.Errorf("%s scope = %q, want runtime", e, byEndpoint[e].RequiredScope)
		}
	}
	if n := len(byEndpoint["POST /v1/consume"].Codes); n != 18 {
		t.Errorf("consume lists %d codes, want the matrix row's 18", n)
	}
	if !byEndpoint["POST /v1/check"].HasBody || byEndpoint["GET /v1/balance"].HasBody {
		t.Error("HasBody is wrong for check or balance")
	}
}
