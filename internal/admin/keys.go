package admin

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	contract "github.com/quotacore/quotacore/api"
	"github.com/quotacore/quotacore/internal/auth"
)

func (s *Service) listKeys(w http.ResponseWriter, r *http.Request) {
	var tenant string
	l, ok := s.startList(w, r, "api_keys", func(q *query) []string {
		tenant = q.str("tenant_external_id", externalIDShape)
		return []string{tenant}
	})
	if !ok {
		return
	}
	rows, err := s.store.ListKeys(r.Context(), l.page, tenant)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	n, cur, err := s.next(r, l, len(rows), func(i int) Position { return pos(rows[i].CreatedAt, rows[i].ID) })
	if err != nil {
		s.unavailable(w, r, err)
		return
	}
	respond(w, r, http.StatusOK, &contract.KeyList{Keys: rows[:n], NextCursor: cur})
}

// createKey mints the key here, stores only its prefix and digest, and
// returns the plaintext in this response and nowhere else (INV-K3, ADR-0018).
func (s *Service) createKey(w http.ResponseWriter, r *http.Request) {
	b, err := readBody(r, "name", "scope", "tenant_external_id", "expires_at")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	name, _ := b.str("name", true, nil, 1, nameMax)
	if name == "bootstrap" {
		// The audit actor of the bootstrap is reserved, so no request can be
		// mistaken for it (INV-K2).
		b.add("name", "is reserved")
	}
	scope, _ := b.str("scope", true, nil, 0, 0)
	if scope != "" && scope != string(auth.Runtime) && scope != string(auth.Admin) {
		b.add("scope", "must be runtime or admin")
	}
	// A binding is a runtime key's alone: an admin key that reached one
	// tenant would still reach the whole configuration (DR-053).
	tenant, _ := b.str("tenant_external_id", false, externalIDShape, 0, 0)
	if tenant != "" && scope == string(auth.Admin) {
		b.add("tenant_external_id", "is for a runtime key only")
	}
	expires, _ := b.timestamp("expires_at")
	if expires != nil && !expires.After(accepted(r)) {
		b.add("expires_at", "must be in the future")
	}
	if p := b.check(&contract.KeyCreate{}); p != nil {
		problem(w, r, p)
		return
	}
	var (
		key    contract.Key
		issued auth.Issued
	)
	// A prefix collision is 64 random bits meeting; it is retried, never
	// answered (db.ErrPrefixCollision).
	for attempt := 0; ; attempt++ {
		issued, err = auth.IssueToken(auth.Scope(scope))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		key, err = s.store.CreateKey(r.Context(), actor(r), NewKey{
			Name: name, Scope: auth.Scope(scope), ExpiresAt: expires, Prefix: issued.Prefix, Hash: issued.Hash,
			TenantExternalID: tenant,
		})
		if err == nil || attempt == 2 || !errors.Is(err, ErrPrefixTaken) {
			break
		}
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.afterKeyChange(r, issued.Prefix)
	respond(w, r, http.StatusCreated, &contract.KeyCreated{
		ID: key.ID, Name: key.Name, Prefix: key.Prefix, Scope: key.Scope, State: key.State,
		TenantExternalID: key.TenantExternalID,
		CreatedAt:        key.CreatedAt, ExpiresAt: key.ExpiresAt, Key: issued.Token,
	})
}

// ErrPrefixTaken is a new key's prefix colliding with a stored one. The
// store returns it so the route can mint another; it never reaches a caller.
var ErrPrefixTaken = errors.New("admin: the key prefix is taken")

// revokeKey is immediate on this instance, because the cache is reconciled
// before the response is written, and within NFR-D3 on every other, through
// the invalidation (K-2). An unknown id is 404 api_key_not_found, so a typo
// in an incident never reads as a revocation (D5).
func (s *Service) revokeKey(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !uuidShape.MatchString(id) {
		problem(w, r, notFound(contract.ErrorCodeAPIKeyNotFound))
		return
	}
	prefix, changed, err := s.store.RevokeKey(r.Context(), actor(r), id, accepted(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if changed {
		s.afterKeyChange(r, prefix)
	}
	noContent(w, r)
}

// afterKeyChange drops the key from this instance's cache before the
// response is written, so this instance's next request reads the committed
// row, then publishes so every other instance reconciles (S6). Dropping the
// entry cannot fail, which is what makes revocation hold here even when the
// publish does not.
func (s *Service) afterKeyChange(r *http.Request, prefix string) {
	if s.keys != nil {
		s.keys.Forget(prefix)
	}
	s.publish(r, nil)
}
