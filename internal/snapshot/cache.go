package snapshot

import (
	"container/list"
)

// tenantCache is the bounded recency cache that is the snapshot proper. It is
// an LRU over tenants together with a byte budget, so both limits of the
// phase's Definition of Done are enforced (NFR-T4 entries, NFR-T7 bytes). Plans
// are interned separately and counted once: ten thousand tenants on three plans
// store three plan objects, not thirty thousand (request-lifecycle.md section
// 3), and the 50k-x-100 memory claim is only provable if the shared plan is not
// recounted per tenant.
//
// The byte budget counts the per-tenant payload and one copy of each interned
// plan. It is approximate by design — it is the cheap bound the eviction loop
// enforces, not the proof; the proof of NFR-T7 is the memory test, which
// measures HeapAlloc directly.
type tenantCache struct {
	ll        *list.List
	byID      map[string]*list.Element
	plans     map[string]*Plan // id → interned plan, refcounted
	tenantMem int64
	planMem   int64
	limit     int
	byteCap   int64
	onEvict   func(tenantID string)
	onPlanGC  func(planID string)
}

type cacheEntry struct {
	id     string
	tenant *Tenant
	bytes  int64
}

func newTenantCache(limit int, byteCap int64) *tenantCache {
	return &tenantCache{
		ll:      list.New(),
		byID:    make(map[string]*list.Element),
		plans:   make(map[string]*Plan),
		limit:   limit,
		byteCap: byteCap,
	}
}

// get returns the cached tenant and touches recency. A cached tenant being
// present is what makes the resolve path I/O-free; a nil return is the miss the
// limiter guards.
func (c *tenantCache) get(id string) *Tenant {
	e, ok := c.byID[id]
	if !ok {
		return nil
	}
	c.ll.MoveToFront(e)
	return e.Value.(*cacheEntry).tenant
}

// add inserts or refreshes a tenant, then evicts to the bounds. The tenant's
// plan is interned; a refresh that changes the plan releases the old reference
// and retires the old plan if it is now unused.
func (c *tenantCache) add(t *Tenant) {
	id := t.ID
	t.Plan = c.intern(t.Plan)
	entry := &cacheEntry{id: id, tenant: t, bytes: tenantBytes(t)}
	if existing, ok := c.byID[id]; ok {
		old := existing.Value.(*cacheEntry)
		if old.tenant.Plan != t.Plan {
			c.release(old.tenant.Plan)
		}
		c.tenantMem += entry.bytes - old.bytes
		existing.Value = entry
		c.ll.MoveToFront(existing)
	} else {
		c.byID[id] = c.ll.PushFront(entry)
		c.tenantMem += entry.bytes
	}
	for !c.withinBounds() {
		if !c.evictOldest() {
			break
		}
	}
}

// withinBounds reports whether both the entry and the byte budget hold. The
// single-resident rule means a tenant alone in the cache is never evicted,
// even past the byte cap; the cap is a sizing backstop, not a paging policy
// (deployment.md section 5).
func (c *tenantCache) withinBounds() bool {
	return c.ll.Len() <= c.limit && (c.ll.Len() <= 1 || c.mem() <= c.byteCap)
}

// mem is the accounted memory: per-tenant payload plus one copy per interned
// plan.
func (c *tenantCache) mem() int64 { return c.tenantMem + c.planMem }

// evictOldest removes the least-recently-used tenant and releases its plan
// reference. It reports whether anything was evicted.
func (c *tenantCache) evictOldest() bool {
	back := c.ll.Back()
	if back == nil {
		return false
	}
	entry := back.Value.(*cacheEntry)
	c.ll.Remove(back)
	delete(c.byID, entry.id)
	c.tenantMem -= entry.bytes
	c.release(entry.tenant.Plan)
	if c.onEvict != nil {
		c.onEvict(entry.id)
	}
	return true
}

// prune removes tenants whose effective version no longer matches the control
// plane's: a tenant that disappeared, or one whose per-tenant version moved
// away from the version the snapshot loaded. This is the incremental
// invalidation of data-model.md section 3.4; tenants that did not change are
// touched only to restore recency order.
func (c *tenantCache) prune(latest map[string]int64) (evicted int) {
	var stale []string
	for id, e := range c.byID {
		cur, present := latest[id]
		if !present || cur != e.Value.(*cacheEntry).tenant.Version {
			stale = append(stale, id)
		}
	}
	for _, id := range stale {
		e := c.byID[id]
		entry := e.Value.(*cacheEntry)
		c.ll.Remove(e)
		delete(c.byID, id)
		c.tenantMem -= entry.bytes
		c.release(entry.tenant.Plan)
		if c.onEvict != nil {
			c.onEvict(id)
		}
		evicted++
	}
	for id := range latest {
		if e, ok := c.byID[id]; ok {
			c.ll.MoveToFront(e)
		}
	}
	return evicted
}

func (c *tenantCache) len() int { return c.ll.Len() }

// intern finds or creates the shared plan a tenant points at, and records one
// reference. Plans are identified by their UUID; a new id interns a fresh
// object and charges its footprint once.
func (c *tenantCache) intern(p *Plan) *Plan {
	if p == nil {
		return nil
	}
	if interned, ok := c.plans[p.ID]; ok {
		interned.refs++
		return interned
	}
	cp := &Plan{ID: p.ID, Key: p.Key, Entitlements: p.Entitlements, refs: 1}
	c.plans[p.ID] = cp
	c.planMem += planBytes(cp)
	return cp
}

// release drops one reference and retires an unused plan, refunding its
// footprint.
func (c *tenantCache) release(p *Plan) {
	if p == nil {
		return
	}
	p.refs--
	if p.refs <= 0 {
		delete(c.plans, p.ID)
		c.planMem -= planBytes(p)
		if c.onPlanGC != nil {
			c.onPlanGC(p.ID)
		}
	}
}

// tenantBytes is the per-tenant payload: the strings a tenant row carries plus
// a little for the numbers. It deliberately does not include the plan's
// entitlements, which are shared and counted once by planBytes.
func tenantBytes(t *Tenant) int64 {
	n := int64(len(t.ID)+len(t.ExternalID)+len(t.State)+len(t.Timezone)) + 24
	for k := range t.Overrides {
		n += int64(len(k)) + 24
	}
	return n
}

// planBytes is the footprint of one interned plan.
func planBytes(p *Plan) int64 {
	n := int64(len(p.ID)+len(p.Key)) + 16
	for feature := range p.Entitlements {
		n += int64(len(feature)) + 16
	}
	return n
}

// Plan is an interned plan: the entitlements are the shared object the tenant
// rows point at. refs is owned by the cache and never read elsewhere.
type Plan struct {
	ID           string
	Key          string
	Entitlements map[string]Entitlement
	refs         int
}
