package snapshot

import (
	"container/list"
	"sort"
	"strconv"
	"strings"
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
	ll       *list.List
	byID     map[string]*list.Element
	byExtern map[string]string // external_id → tenant id, for cached tenants only
	plans    map[string]*Plan  // id → interned plan, refcounted
	// pinSets interns tenants' pins by content. Tenants anchored on the same
	// plan version share one set, so pinning a cadence per tenant costs one
	// map per distinct set rather than one per tenant (IP-10, NFR-T7).
	pinSets   map[string]*pinSet
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
		ll:       list.New(),
		byID:     make(map[string]*list.Element),
		byExtern: make(map[string]string),
		plans:    make(map[string]*Plan),
		pinSets:  make(map[string]*pinSet),
		limit:    limit,
		byteCap:  byteCap,
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
	t.Pins, t.pinKey = c.internPins(t.Pins)
	entry := &cacheEntry{id: id, tenant: t, bytes: tenantBytes(t)}
	if existing, ok := c.byID[id]; ok {
		old := existing.Value.(*cacheEntry)
		// intern took a reference for the new entry whether or not the plan
		// changed, so the old entry's reference is always released; skipping it
		// when the plan was the same kept that plan alive forever.
		c.release(old.tenant.Plan)
		c.releasePins(old.tenant.pinKey)
		if old.tenant.ExternalID != t.ExternalID && c.byExtern[old.tenant.ExternalID] == id {
			delete(c.byExtern, old.tenant.ExternalID)
		}
		c.tenantMem += entry.bytes - old.bytes
		existing.Value = entry
		c.ll.MoveToFront(existing)
	} else {
		c.byID[id] = c.ll.PushFront(entry)
		c.tenantMem += entry.bytes
	}
	c.byExtern[t.ExternalID] = id
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
	c.forgetExtern(entry)
	c.tenantMem -= entry.bytes
	c.release(entry.tenant.Plan)
	c.releasePins(entry.tenant.pinKey)
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
	evicted = c.forget(stale)
	for id := range latest {
		if e, ok := c.byID[id]; ok {
			c.ll.MoveToFront(e)
		}
	}
	return evicted
}

// forget removes the cached tenants among ids and reports how many it removed.
func (c *tenantCache) forget(ids []string) (evicted int) {
	for _, id := range ids {
		e, ok := c.byID[id]
		if !ok {
			continue
		}
		entry := e.Value.(*cacheEntry)
		c.ll.Remove(e)
		delete(c.byID, id)
		c.forgetExtern(entry)
		c.tenantMem -= entry.bytes
		c.release(entry.tenant.Plan)
		c.releasePins(entry.tenant.pinKey)
		if c.onEvict != nil {
			c.onEvict(id)
		}
		evicted++
	}
	return evicted
}

func (c *tenantCache) len() int { return c.ll.Len() }

// all is every cached tenant, without touching recency.
func (c *tenantCache) all() []*Tenant {
	out := make([]*Tenant, 0, c.ll.Len())
	for e := c.ll.Front(); e != nil; e = e.Next() {
		out = append(out, e.Value.(*cacheEntry).tenant)
	}
	return out
}

// byExternal returns the cached tenant an external_id names and touches
// recency, or nil. The mapping lives and dies with its tenant: an external_id
// is immutable (TenantUpdate has no such field) and never reused (the column
// is unique across soft-deleted rows), so a cached pair is never wrong, and
// evicting the tenant evicts the pair, so the map is bounded by the cache.
func (c *tenantCache) byExternal(externalID string) *Tenant {
	id, ok := c.byExtern[externalID]
	if !ok {
		return nil
	}
	return c.get(id)
}

func (c *tenantCache) forgetExtern(e *cacheEntry) {
	if c.byExtern[e.tenant.ExternalID] == e.id {
		delete(c.byExtern, e.tenant.ExternalID)
	}
}

// intern finds or creates the shared plan a tenant points at, and records one
// reference. Plans are identified by their UUID; a new id interns a fresh
// object and charges its footprint once.
func (c *tenantCache) intern(p *Plan) *Plan {
	if p == nil {
		return nil
	}
	if interned, ok := c.plans[p.internKey()]; ok {
		interned.refs++
		return interned
	}
	cp := &Plan{ID: p.ID, Key: p.Key, Version: p.Version, Entitlements: p.Entitlements, refs: 1}
	c.plans[p.internKey()] = cp
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
		delete(c.plans, p.internKey())
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

// pinSet is one interned set of pins and the number of cached tenants on it.
type pinSet struct {
	key  string
	pins map[string]Pin
	refs int
}

// pinKey is a set of pins' content, in feature order, so two tenants pinned
// alike share one set.
func pinKey(pins map[string]Pin) string {
	features := make([]string, 0, len(pins))
	for f := range pins {
		features = append(features, f)
	}
	sort.Strings(features)
	var b strings.Builder
	for _, f := range features {
		p := pins[f]
		b.WriteString(f)
		b.WriteByte('=')
		b.WriteString(strconv.Itoa(int(p.Interval)))
		if p.Archived {
			b.WriteString(",a")
		}
		if p.RetiredLimit != nil {
			b.WriteString(",r")
			b.WriteString(strconv.FormatInt(*p.RetiredLimit, 10))
		}
		if p.RetiredUntil != nil {
			b.WriteString(",u")
			b.WriteString(strconv.FormatInt(p.RetiredUntil.UnixNano(), 10))
		}
		b.WriteByte(';')
	}
	return b.String()
}

// internPins finds or creates the shared set equal to pins, records one
// reference, and charges a new set's footprint once.
func (c *tenantCache) internPins(pins map[string]Pin) (map[string]Pin, string) {
	k := pinKey(pins)
	if set, ok := c.pinSets[k]; ok {
		set.refs++
		// The set's own key, so a tenant does not keep a copy of it.
		return set.pins, set.key
	}
	if pins == nil {
		pins = map[string]Pin{}
	}
	c.pinSets[k] = &pinSet{key: k, pins: pins, refs: 1}
	c.planMem += pinBytes(pins)
	return pins, k
}

// releasePins drops one reference and retires an unused set.
func (c *tenantCache) releasePins(k string) {
	set, ok := c.pinSets[k]
	if !ok {
		return
	}
	set.refs--
	if set.refs <= 0 {
		delete(c.pinSets, k)
		c.planMem -= pinBytes(set.pins)
	}
}

func pinBytes(pins map[string]Pin) int64 {
	n := int64(0)
	for f := range pins {
		n += int64(len(f)) + 40
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
	Version      int64
	Entitlements map[string]Entitlement
	refs         int
}

// internKey is a plan's identity in the cache: its id at one version, so an
// entitlement edit, which bumps the version, is a new object and never a
// change under tenants still holding the old one (IP-10).
func (p *Plan) internKey() string { return p.ID + "@" + strconv.FormatInt(p.Version, 10) }
