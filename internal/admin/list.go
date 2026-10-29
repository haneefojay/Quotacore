package admin

import (
	"errors"
	"net/http"
	"time"

	uuid "github.com/google/uuid"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/envelope"
)

// listing is one list request in progress: its parameters, the filter digest
// its cursor is bound to, and the page the store is asked for.
type listing struct {
	q      *query
	filter string
	page   Page
}

// startList reads the shared list parameters. extra adds the route's own
// filters to q before the cursor is checked against them; it returns the
// filter values the cursor is bound to.
func (s *Service) startList(w http.ResponseWriter, r *http.Request, route string, extra func(q *query) []string) (*listing, bool) {
	q := readQuery(r)
	var filters []string
	if extra != nil {
		filters = extra(q)
	}
	if len(q.issues) > 0 {
		problem(w, r, reject(q.issues))
		return nil, false
	}
	l := &listing{q: q, filter: filterDigest(route, filters...)}
	after, err := s.decodeCursor(r.Context(), q.cursor, l.filter)
	if errors.Is(err, errBadCursor) {
		problem(w, r, reject([]issue{{Field: "cursor", Issue: "is not a cursor this service issued for this list"}}))
		return nil, false
	}
	if err != nil {
		s.unavailable(w, r, err)
		return nil, false
	}
	l.page = Page{Limit: q.limit, After: after}
	return l, true
}

// next trims the extra row and signs the cursor for the page after it.
func (s *Service) next(r *http.Request, l *listing, n int, last func(i int) Position) (int, contract.OptNilCursor, error) {
	if n <= l.page.Limit {
		return n, contract.OptNilCursor{Set: true, Null: true}, nil
	}
	c, err := s.encodeCursor(r.Context(), last(l.page.Limit-1), l.filter)
	if err != nil {
		return 0, contract.OptNilCursor{}, err
	}
	return l.page.Limit, contract.NewOptNilCursor(contract.Cursor(c)), nil
}

// unavailable is the fast store failing to answer a read the route needed:
// here, the cursor key.
func (s *Service) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Warn("the data store did not answer an admin request",
		"request_id", envelope.From(r.Context()).RequestID, "error", err)
	envelope.Write(w, r, envelope.Error(contract.ErrorCodeServiceUnavailable, message(contract.ErrorCodeServiceUnavailable)).Retry(1000))
}

func at(t contract.Timestamp) time.Time { return time.Time(t) }

func pos(t contract.Timestamp, id uuid.UUID) Position { return Position{At: at(t), Tie: id.String()} }
