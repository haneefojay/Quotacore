package main

import (
	"context"
	"testing"
)

// IP-15 D19: deployment.md section 7 sets the search_path and a statement
// timeout on every connection of the service's pool. The harness opens its
// pool with the constructor run uses, so this reads back what ships.
func TestThePoolSetsSearchPathAndTimeout(t *testing.T) {
	s := newStack(t, func(o *stackOptions) { o.env["QUOTACORE_CONTROLPLANE_TIMEOUT"] = "1500ms" })
	ctx := context.Background()
	// More than one connection, so the setting is the pool's and not a
	// SET that happened to land on the first one.
	s.sql.SetMaxIdleConns(4)
	for range 4 {
		conn, err := s.sql.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var path, timeout string
		if err := conn.QueryRowContext(ctx, "SHOW search_path").Scan(&path); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, "SHOW statement_timeout").Scan(&timeout); err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if path != "public" {
			t.Errorf("search_path = %q, want public", path)
		}
		if timeout != "1500ms" {
			t.Errorf("statement_timeout = %q, want 1500ms (QUOTACORE_CONTROLPLANE_TIMEOUT)", timeout)
		}
	}
}
