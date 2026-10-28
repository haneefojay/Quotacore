package harness

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// Seeded is what Seed made.
type Seeded struct {
	Plan     string
	Features []string
	Tenants  []string
	Keys     []string
}

// Seed creates features, one monthly plan carrying every one of them at
// limit, n tenants on it in the zones given in turn, and runtime keys, all
// through the admin API as an operator would. Tenants are created with the
// concurrency given; a 50,000-tenant seed is the tenant-scale row's.
func (s *Stack) Seed(prefix string, features, tenants, keys int, limit int64, concurrency int, zones ...string) Seeded {
	s.T.Helper()
	if len(zones) == 0 {
		zones = []string{"UTC"}
	}
	out := Seeded{Plan: prefix + "-plan"}
	var ents []string
	for i := range features {
		f := fmt.Sprintf("%s.f%03d", prefix, i)
		s.Must(http.StatusCreated, http.MethodPost, "/v1/admin/features", `{"feature_key":"`+f+`","name":"F","unit":"unit"}`)
		out.Features = append(out.Features, f)
		ents = append(ents, fmt.Sprintf(`{"feature_key":%q,"limit":%d,"reset_interval":"monthly"}`, f, limit))
	}
	s.Must(http.StatusCreated, http.MethodPost, "/v1/admin/plans",
		`{"plan_key":"`+out.Plan+`","name":"P","entitlements":[`+strings.Join(ents, ",")+`]}`)
	for i := range keys {
		k := s.Must(http.StatusCreated, http.MethodPost, "/v1/admin/keys", fmt.Sprintf(`{"name":"%s-k%d","scope":"runtime"}`, prefix, i))
		out.Keys = append(out.Keys, k["key"].(string))
	}

	out.Tenants = make([]string, tenants)
	var next atomic.Int64
	var failed atomic.Value
	var wg sync.WaitGroup
	for range max(1, concurrency) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= tenants || failed.Load() != nil {
					return
				}
				ext := fmt.Sprintf("%s-t%06d", prefix, i)
				body := `{"external_id":"` + ext + `","display_name":"T","plan_key":"` + out.Plan + `","timezone":"` + zones[i%len(zones)] + `"}`
				status, raw := s.DoRaw(http.MethodPost, "/v1/admin/tenants", s.Admin, body)
				if status != http.StatusCreated {
					failed.Store(fmt.Sprintf("create tenant %s: %d %s", ext, status, raw))
					return
				}
				out.Tenants[i] = ext
			}
		}()
	}
	wg.Wait()
	if msg := failed.Load(); msg != nil {
		s.T.Fatal(msg)
	}
	return out
}
