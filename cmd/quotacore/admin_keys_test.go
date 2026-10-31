package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quotacore/quotacore/internal/auth"
	"github.com/quotacore/quotacore/internal/db"
)

// issueKey creates a key over the API and returns the response.
func (s *stack) issueKey(name, scope string) map[string]any {
	s.t.Helper()
	return mustStatus(s.t, s.adminDo(http.MethodPost, "/v1/admin/keys",
		`{"name":"`+name+`","scope":"`+scope+`"}`), http.StatusCreated)
}

// secretOf is the raw secret a token carries.
func secretOf(t *testing.T, token string) []byte {
	t.Helper()
	_, enc, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatalf("not a token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestNoKeyIsRecoverableAfterIssuance is IP-09 DoD 1 and IP-20 DoD 4 (NFR-S1): once a key is
// issued, neither the token, its encoded secret nor the secret's raw bytes is
// in any column of any table, in the log, or in the metrics. The columns are
// read from pg_catalog, so a column added later is covered without editing
// this test.
func TestNoKeyIsRecoverableAfterIssuance(t *testing.T) {
	s := newStack(t)
	var tokens []string
	for _, scope := range []string{"runtime", "admin"} {
		k := s.issueKey(uniq("rec-"+scope), scope)
		tokens = append(tokens, str(k, "key"))
		// A key in use is the realistic case: last_used_at, the cache and
		// the access log have all seen it.
		s.do(http.MethodGet, "/v1/admin/keys", str(k, "key"), "")
	}
	// A tenant-scoped key is covered too (IP-20 DoD 4, DR-053), in use on
	// its own tenant.
	feature, plan := uniq("rec-f"), uniq("rec-p")
	s.createFeature(feature)
	s.createPlan(plan, 100, "daily", feature)
	owner := str(s.createTenant(externalID("rec"), plan, "UTC"), "external_id")
	bound := s.issueBound(uniq("rec-bound"), owner)
	tokens = append(tokens, str(bound, "key"))
	if r := s.consume(str(bound, "key"), feature, 1, owner, newKey()); r.Status != http.StatusOK {
		t.Fatalf("the bound key in use: %d %s", r.Status, r.Body)
	}
	s.app.Keys.Flush(context.Background())

	rows, err := s.seedDB.Query(`
SELECT c.relname, a.attname, t.typname
  FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace JOIN pg_type t ON t.oid = a.atttypid
 WHERE n.nspname = 'public' AND c.relkind = 'r' AND a.attnum > 0 AND NOT a.attisdropped`)
	if err != nil {
		t.Fatal(err)
	}
	type col struct{ table, column, typ string }
	var cols []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.table, &c.column, &c.typ); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	rows.Close()
	if len(cols) < 40 {
		t.Fatalf("read %d columns from the catalogue", len(cols))
	}
	for _, tok := range tokens {
		secret := secretOf(t, tok)
		_, encoded, _ := strings.Cut(tok, ".")
		for _, c := range cols {
			var hits int
			id := `"` + c.column + `"`
			q := `SELECT count(*) FROM ` + c.table + ` WHERE position($1 in ` + id + `::text) > 0 OR position($2 in ` + id + `::text) > 0`
			args := []any{tok, encoded}
			if c.typ == "bytea" {
				q = `SELECT count(*) FROM ` + c.table + ` WHERE position($1::bytea in ` + id + `) > 0`
				args = []any{secret}
			}
			if err := s.seedDB.QueryRow(q, args...).Scan(&hits); err != nil {
				t.Fatalf("%s.%s: %v", c.table, c.column, err)
			}
			if hits > 0 {
				t.Errorf("%s.%s holds the issued secret", c.table, c.column)
			}
		}
		logs := s.logs.String()
		if strings.Contains(logs, tok) || strings.Contains(logs, encoded) {
			t.Error("the log holds an issued secret")
		}
		families, err := s.reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range families {
			if strings.Contains(f.String(), encoded) {
				t.Errorf("the metric %s holds an issued secret", f.GetName())
			}
		}
	}
}

// TestTheSecretIsReturnedOnce is IP-09 DoD 2 (INV-K3): the create carries the
// key, and nothing after it does, the list included.
func TestTheSecretIsReturnedOnce(t *testing.T) {
	s := newStack(t)
	k := s.issueKey(uniq("once"), "runtime")
	tok := str(k, "key")
	if !strings.HasPrefix(tok, "qc_runtime_"+str(k, "prefix")+".") {
		t.Fatalf("the create returned %q for prefix %s", tok, str(k, "prefix"))
	}
	_, encoded, _ := strings.Cut(tok, ".")
	for _, r := range []response{
		s.adminDo(http.MethodGet, "/v1/admin/keys", ""),
		s.adminDo(http.MethodGet, "/v1/admin/keys?limit=1", ""),
		s.adminDo(http.MethodGet, "/v1/admin/audit", ""),
		s.adminDo(http.MethodDelete, "/v1/admin/keys/"+str(k, "id"), ""),
		s.adminDo(http.MethodGet, "/v1/admin/audit", ""),
	} {
		if bytes.Contains(r.Body, []byte(encoded)) {
			t.Errorf("a response after the create carries the secret: %s", r.Body)
		}
		if bytes.Contains(r.Body, []byte(`"key":`)) || bytes.Contains(r.Body, []byte(`"key_hash"`)) {
			t.Errorf("a response after the create carries a key or its hash: %s", r.Body)
		}
	}
}

// TestRotationIsAnExplicitPair is IP-09 DoD 3 (INV-K1, K-4), and the revoke
// half closes IP-07's deferred end-to-end revocation test: issuing a second
// key changes nothing about the first, both work until the first is revoked,
// and the serving instance refuses the first on its very next request.
func TestRotationIsAnExplicitPair(t *testing.T) {
	s := newStack(t)
	first := s.issueKey(uniq("rot-a"), "admin")
	use := func(tok string) response { return s.do(http.MethodGet, "/v1/admin/keys", tok, "") }
	mustStatus(t, use(str(first, "key")), http.StatusOK)
	row := func(id string) string {
		var name, prefix, state string
		var hash []byte
		if err := s.seedDB.QueryRow(`SELECT name, prefix, state, key_hash FROM api_keys WHERE id = $1`, id).
			Scan(&name, &prefix, &state, &hash); err != nil {
			t.Fatal(err)
		}
		return name + prefix + state + string(hash)
	}
	before := row(str(first, "id"))
	second := s.issueKey(uniq("rot-b"), "admin")
	if after := row(str(first, "id")); after != before {
		t.Error("issuing a second key changed the first")
	}
	mustStatus(t, use(str(first, "key")), http.StatusOK)
	mustStatus(t, use(str(second, "key")), http.StatusOK)

	r := s.do(http.MethodDelete, "/v1/admin/keys/"+str(first, "id"), str(second, "key"), "")
	mustStatus(t, r, http.StatusNoContent)
	if got := use(str(first, "key")); got.code(t) != "api_key_invalid" {
		t.Fatalf("the revoked key on the next request: %d %s", got.Status, got.Body)
	}
	mustStatus(t, use(str(second, "key")), http.StatusOK)

	// Revoking again is 204 with no new audit row; an id that names no key
	// is 404 api_key_not_found (D5).
	var n int
	count := func() int {
		if err := s.seedDB.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'api_key.revoke'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	revokes := count()
	mustStatus(t, s.do(http.MethodDelete, "/v1/admin/keys/"+str(first, "id"), str(second, "key"), ""), http.StatusNoContent)
	if count() != revokes {
		t.Error("revoking a revoked key wrote an audit row")
	}
	if r := s.do(http.MethodDelete, "/v1/admin/keys/00000000-0000-4000-8000-000000000000", str(second, "key"), ""); r.code(t) != "api_key_not_found" {
		t.Errorf("revoking an unknown id: %d %s", r.Status, r.Body)
	}
}

// TestRevokeReachesASecondInstanceWithinTheBound: two instances on one
// Postgres and one data store; the second has the key cached, and refuses it
// within NFR-D3's second once the first revokes it.
func TestRevokeReachesASecondInstanceWithinTheBound(t *testing.T) {
	s := newStack(t)
	other := newStack(t, func(o *stackOptions) { o.dbURL = s.dbURL })
	k := s.issueKey(uniq("two"), "admin")
	mustStatus(t, other.do(http.MethodGet, "/v1/admin/status", str(k, "key"), ""), http.StatusOK)
	mustStatus(t, s.adminDo(http.MethodDelete, "/v1/admin/keys/"+str(k, "id"), ""), http.StatusNoContent)
	revoked := time.Now()
	for {
		r := other.do(http.MethodGet, "/v1/admin/keys", str(k, "key"), "")
		if r.Status == http.StatusUnauthorized {
			break
		}
		if time.Since(revoked) > time.Second {
			t.Fatalf("the second instance still accepts the revoked key after %v: %d", time.Since(revoked), r.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

const bootstrapToken = "qc_admin_0123456789abcdef.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func bootstrapConfig(key string) Config {
	return Config{BootstrapAdminKey: key, BootstrapKeyIsAcked: true}
}

func emptyDatabase(t *testing.T) *sql.DB {
	t.Helper()
	base, _ := integrationURLs(t)
	conn, err := db.Open(context.Background(), freshDatabaseURL(t, base))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// TestBootstrapKeyIsIssuedOnceOnAnEmptyTable is IP-09 DoD 7: on an empty
// table the supplied key is installed with its bootstrap audit row, logged by
// prefix only, and works.
func TestBootstrapKeyIsIssuedOnceOnAnEmptyTable(t *testing.T) {
	conn := emptyDatabase(t)
	logs := &syncBuffer{}
	if err := bootstrap(context.Background(), bootstrapConfig(bootstrapToken), newTestLogger(logs), conn); err != nil {
		t.Fatal(err)
	}
	var n, audits int
	var scope string
	if err := conn.QueryRow(`SELECT count(*), min(scope::text) FROM api_keys`).Scan(&n, &scope); err != nil || n != 1 || scope != "admin" {
		t.Fatalf("api_keys holds %d rows of scope %s (%v)", n, scope, err)
	}
	if err := conn.QueryRow(`SELECT count(*) FROM audit_log WHERE actor = 'bootstrap' AND action = 'api_key.bootstrap' AND actor_key_id IS NULL`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("bootstrap audit rows = %d (%v)", audits, err)
	}
	_, secret, _ := strings.Cut(bootstrapToken, ".")
	if !strings.Contains(logs.String(), "0123456789abcdef") || strings.Contains(logs.String(), secret) {
		t.Errorf("the bootstrap log line must carry the prefix and never the secret: %s", logs.String())
	}
	tok, _ := auth.Parse(bootstrapToken)
	var hash []byte
	if err := conn.QueryRow(`SELECT key_hash FROM api_keys WHERE prefix = $1`, tok.Prefix).Scan(&hash); err != nil || !bytes.Equal(hash, tok.Hash()) {
		t.Fatalf("the stored digest is not the supplied key's (%v)", err)
	}
}

// TestBootstrapKeyIsIgnoredOnceKeysExist: a second start, with the same key
// or another, installs nothing and says so.
func TestBootstrapKeyIsIgnoredOnceKeysExist(t *testing.T) {
	conn := emptyDatabase(t)
	ctx := context.Background()
	if err := bootstrap(ctx, bootstrapConfig(bootstrapToken), newTestLogger(&syncBuffer{}), conn); err != nil {
		t.Fatal(err)
	}
	other := "qc_admin_fedcba9876543210.BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	logs := &syncBuffer{}
	for _, key := range []string{bootstrapToken, other} {
		if err := bootstrap(ctx, bootstrapConfig(key), newTestLogger(logs), conn); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM api_keys`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("api_keys holds %d rows after two more starts (%v)", n, err)
	}
	if !strings.Contains(logs.String(), "ignoring QUOTACORE_BOOTSTRAP_ADMIN_KEY") {
		t.Errorf("an ignored key is not reported: %s", logs.String())
	}
}

// TestConcurrentStartsBootstrapOnce: replicas starting together, each with its
// own key, install exactly one.
func TestConcurrentStartsBootstrapOnce(t *testing.T) {
	conn := emptyDatabase(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			is, err := auth.IssueToken(auth.Admin)
			if err != nil {
				errs <- err
				return
			}
			tok, _ := auth.Parse(is.Token)
			_, err = db.Bootstrap(context.Background(), conn, tok.Prefix, tok.Hash())
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var keys, audits int
	_ = conn.QueryRow(`SELECT count(*) FROM api_keys`).Scan(&keys)
	_ = conn.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'api_key.bootstrap'`).Scan(&audits)
	if keys != 1 || audits != 1 {
		t.Fatalf("eight concurrent starts installed %d keys and %d audit rows, want 1 and 1", keys, audits)
	}
}

// TestNoRouteCreatesTheBootstrapKey: no request can write an audit row whose
// actor is the bootstrap's, because the name is refused on the key route
// (INV-K2).
func TestNoRouteCreatesTheBootstrapKey(t *testing.T) {
	s := newStack(t)
	if r := s.adminDo(http.MethodPost, "/v1/admin/keys", `{"name":"bootstrap","scope":"admin"}`); r.code(t) != "validation_failed" {
		t.Fatalf("a key named bootstrap: %d %s", r.Status, r.Body)
	}
	s.issueKey(uniq("any"), "admin")
	s.createFeature(uniq("nb"))
	var n int
	if err := s.seedDB.QueryRow(`SELECT count(*) FROM audit_log WHERE actor = 'bootstrap'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("API requests wrote %d audit rows as the bootstrap (%v)", n, err)
	}
}
