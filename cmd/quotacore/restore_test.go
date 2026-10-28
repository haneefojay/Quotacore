package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestConfigBumpReloadsEverySnapshot is consistency-and-recovery.md section
// 4.2, step 4 (Q-28): a configuration that changed under the service, as a
// Postgres restore changes it, is not seen by a snapshot that holds the
// tenant until something announces it; `config bump` does, and the change is
// enforced at once.
func TestConfigBumpReloadsEverySnapshot(t *testing.T) {
	s := newStack(t)
	f := uniq("bump")
	s.createFeature(f)
	plan := uniq("bumpp")
	s.createPlan(plan, 100, "monthly", f)
	tn := asTenant(t, s.createTenant(externalID("bump"), plan, "UTC"), ent{f, 100, "monthly", false})
	mustStatus(t, s.check(f, tn.External), http.StatusOK)

	// The restored database has the tenant suspended; nothing announced it.
	if _, err := s.seedDB.ExecContext(context.Background(), `UPDATE tenants SET state = 'suspended' WHERE id = $1`, tn.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	mustStatus(t, s.check(f, tn.External), http.StatusOK)

	c, out := s.commands()
	code, err := c.run(context.Background(), []string{"config bump"})
	if err != nil || code != exitClean {
		t.Fatalf("config bump: exit %d, %v", code, err)
	}
	var rep struct {
		Version int64 `json:"config_version"`
		Tenants int   `json:"tenants"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil || rep.Version == 0 || rep.Tenants < 1 {
		t.Fatalf("config bump printed %q: %v", out.String(), err)
	}
	until(t, "tenant_suspended", func() response { return s.check(f, tn.External) })
}

// TestAuditNoteRecordsTheTruncation is section 4.2, step 6 (Q-28): the note
// is an audit row about the installation, with the operator's words, that the
// audit API returns like any other.
func TestAuditNoteRecordsTheTruncation(t *testing.T) {
	s := newStack(t)
	c, out := s.commands()
	if code, _ := c.run(context.Background(), []string{"audit note"}); code != exitUsage {
		t.Errorf("a note with no reason: exit %d, want a usage error", code)
	}
	reason := "restored from the 02:00 backup; history from 02:00 to 09:14 is lost"
	code, err := c.run(context.Background(), []string{"audit note", "--reason", reason})
	if err != nil || code != exitClean {
		t.Fatalf("audit note: exit %d, %v", code, err)
	}
	if !strings.Contains(out.String(), `"system.note"`) {
		t.Fatalf("audit note printed %q", out.String())
	}
	r := mustStatus(t, s.adminDo(http.MethodGet, "/v1/admin/audit", ""), http.StatusOK)
	raw, _ := json.Marshal(r)
	if !strings.Contains(string(raw), reason) || !strings.Contains(string(raw), `"subject_type":"system"`) {
		t.Fatalf("the audit log does not return the note: %s", raw)
	}
}
