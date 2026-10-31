package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/envelope"
)

// The contract's patterns, repeated so one pass can name every offending field
// (S7). The generated decoder and validator run after the pass on the same
// body, so a pattern that drifted from the contract is caught there.
var (
	featureKeyShape = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)
	planKeyShape    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	unitShape       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	timezoneShape   = regexp.MustCompile(`^(?:UTC|[A-Za-z][A-Za-z0-9_+-]*(?:/[A-Za-z0-9_+-]+)+)$`)
	clientKeyShape  = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,255}$`)
	uuidShape       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// externalIDShape is DR-034's class. It is the same expression as
// db.ValidExternalID, which this package cannot import; the integration test
// over IP-08's corpus holds the boundary and the constraint to one answer.
var externalIDShape = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

const (
	limitCeiling  = 1<<53 - 1
	nameMax       = 200
	textMax       = 2000
	entitlementsN = 100
)

// issue is one offending field, as it appears in details.errors. It never
// carries the value that was sent (DR-043, S7).
type issue struct {
	Field string `json:"field"`
	Issue string `json:"issue"`
}

// body is one decoded request object and the problems found in it so far.
type body struct {
	raw    []byte
	fields map[string]json.RawMessage
	issues []issue
}

// errTooLarge is a body or a field above its cap, answered 413.
type errTooLarge struct {
	field string
	limit int64
}

func (e errTooLarge) Error() string { return e.field + " is larger than the limit" }

// readBody reads one JSON object. A body that is not one, or a field that is
// not declared, is collected as an issue rather than answered at once, so one
// response names every problem.
func readBody(r *http.Request, allowed ...string) (*body, error) {
	b := &body{}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		b.add("Content-Type", "must be application/json")
	}
	if r.Header.Get("Content-Encoding") != "" {
		b.add("Content-Encoding", "a request body is never compressed")
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, errTooLarge{field: "body", limit: tooBig.Limit}
		}
		b.add("body", "could not be read")
		return b, nil
	}
	b.raw = raw
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&b.fields); err != nil || b.fields == nil || dec.More() {
		b.fields = nil
		b.add("body", "must be one JSON object")
		return b, nil
	}
	known := map[string]bool{}
	for _, a := range allowed {
		known[a] = true
	}
	var unknown []string
	for name := range b.fields {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	for _, name := range unknown {
		b.add(name, "is not a field of this request")
	}
	return b, nil
}

func (b *body) add(field, problem string) {
	b.issues = append(b.issues, issue{Field: field, Issue: problem})
}

func (b *body) has(name string) bool {
	_, ok := b.fields[name]
	return ok
}

// str reads a string field. required decides whether absence is an issue;
// shape, when set, is the pattern; max is a length in characters.
func (b *body) str(name string, required bool, shape *regexp.Regexp, minLen, maxLen int) (string, bool) {
	raw, ok := b.fields[name]
	if !ok {
		if required {
			b.add(name, "is required")
		}
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		b.add(name, "must be a string")
		return "", false
	}
	n := utf8.RuneCountInString(s)
	switch {
	case n < minLen:
		b.add(name, fmt.Sprintf("must be at least %d characters", minLen))
		return "", false
	case maxLen > 0 && n > maxLen:
		b.add(name, fmt.Sprintf("must be at most %d characters", maxLen))
		return "", false
	case shape != nil && !shape.MatchString(s):
		b.add(name, "does not match "+shape.String())
		return "", false
	}
	return s, true
}

// flag reads an optional JSON boolean, false when absent. Anything else is
// refused naming the field.
func (b *body) flag(name string) bool {
	raw, ok := b.fields[name]
	if !ok {
		return false
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		b.add(name, "must be a boolean")
		return false
	}
	return v
}

// integer accepts a JSON integer and nothing else: 3.0 and 3e0 are refused.
func (b *body) integer(name string, required bool, lo, hi int64) (int64, bool) {
	raw, ok := b.fields[name]
	if !ok {
		if required {
			b.add(name, "is required")
		}
		return 0, false
	}
	n, err := parseInteger(raw)
	if err != nil {
		b.add(name, "must be an integer")
		return 0, false
	}
	if n < lo || n > hi {
		b.add(name, fmt.Sprintf("must be between %d and %d", lo, hi))
		return 0, false
	}
	return n, true
}

func parseInteger(raw json.RawMessage) (int64, error) {
	text := string(bytes.TrimSpace(raw))
	if text == "" || bytes.ContainsAny(raw, ".eE\"") {
		return 0, errors.New("not an integer")
	}
	return strconv.ParseInt(text, 10, 64)
}

// timestamp reads an RFC 3339 instant with an offset.
func (b *body) timestamp(name string) (*time.Time, bool) {
	raw, ok := b.fields[name]
	if !ok {
		return nil, false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		b.add(name, "must be an RFC 3339 timestamp with an offset")
		return nil, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		b.add(name, "must be an RFC 3339 timestamp with an offset")
		return nil, false
	}
	return &t, true
}

// object reads a JSON object field, refused above max bytes with a 413.
func (b *body) object(name string, max int) ([]byte, bool, error) {
	raw, ok := b.fields[name]
	if !ok {
		return nil, false, nil
	}
	if len(raw) > max {
		return nil, false, errTooLarge{field: name, limit: int64(max)}
	}
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '{' {
		b.add(name, "must be an object")
		return nil, false, nil
	}
	return t, true, nil
}

// version reads the optimistic-concurrency token, required on every
// versioned write (S4).
func (b *body) version() int64 {
	v, _ := b.integer("version", true, 1, 1<<62)
	return v
}

// reason reads the audited reason archive, delete, suspend and resume take.
func (b *body) reason() string {
	s, _ := b.str("reason", true, nil, 1, textMax)
	return s
}

// check runs the generated decoder and validator on the same bytes once the
// pass has found nothing, so the contract has the last word. A failure there
// is drift between this pass and the contract, and is still a 400.
func (b *body) check(generated interface {
	UnmarshalJSON([]byte) error
	Validate() error
}) *envelope.Problem {
	if len(b.issues) > 0 {
		return reject(b.issues)
	}
	if err := generated.UnmarshalJSON(b.raw); err != nil {
		return reject([]issue{{Field: "body", Issue: "does not match the contract: " + err.Error()}})
	}
	if err := generated.Validate(); err != nil {
		return reject([]issue{{Field: "body", Issue: "does not match the contract: " + err.Error()}})
	}
	return nil
}

// reject is one validation_failed naming every problem.
func reject(issues []issue) *envelope.Problem {
	msg := fmt.Sprintf("%d problems with the request", len(issues))
	if len(issues) == 1 {
		msg = issues[0].Field + " " + issues[0].Issue
	}
	p := envelope.Error(contract.ErrorCodeValidationFailed, msg).With("errors", issues)
	return &p
}

// query reads the list parameters every admin list shares, and collects the
// problems with the rest for the caller to add to.
type query struct {
	limit  int
	cursor string
	issues []issue
	vals   map[string][]string
}

func readQuery(r *http.Request) *query {
	q := &query{limit: 50, vals: r.URL.Query()}
	if raw, ok := q.vals["limit"]; ok {
		n, err := strconv.Atoi(raw[0])
		if err != nil || n < 1 || n > 200 || len(raw) > 1 {
			q.add("limit", "must be an integer from 1 to 200")
		} else {
			q.limit = n
		}
	}
	if raw, ok := q.vals["cursor"]; ok {
		if len(raw[0]) < 1 || len(raw[0]) > 512 || len(raw) > 1 {
			q.add("cursor", "is not a cursor this service issued")
		} else {
			q.cursor = raw[0]
		}
	}
	return q
}

func (q *query) add(field, problem string) {
	q.issues = append(q.issues, issue{Field: field, Issue: problem})
}

// str reads an optional parameter against a pattern or an enum.
func (q *query) str(name string, shape *regexp.Regexp, enum ...string) string {
	raw, ok := q.vals[name]
	if !ok {
		return ""
	}
	v := raw[0]
	if len(raw) > 1 {
		q.add(name, "must be given once")
		return ""
	}
	if shape != nil && !shape.MatchString(v) {
		q.add(name, "does not match "+shape.String())
		return ""
	}
	if len(enum) > 0 {
		for _, e := range enum {
			if v == e {
				return v
			}
		}
		q.add(name, "must be one of "+strings.Join(enum, ", "))
		return ""
	}
	return v
}

func (q *query) instant(name string) *time.Time {
	raw, ok := q.vals[name]
	if !ok {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw[0])
	if err != nil || len(raw) > 1 {
		q.add(name, "must be an RFC 3339 timestamp with an offset")
		return nil
	}
	return &t
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// externalIDValue reads external_id without letting its value reach any
// message: the caller names the field only (DR-034, NFR-S2).
func externalIDValue(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || !externalIDShape.MatchString(s) {
		return "", false
	}
	return s, true
}
