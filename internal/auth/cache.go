package auth

import (
	"container/list"
	"sync"
)

// entry is one cached prefix. A known entry is a row of api_keys; an unknown
// one records that no row has that prefix, so a second request with it costs a
// comparison against the dummy digest rather than another database read, and
// takes the same time as a wrong secret.
type entry struct {
	key   Key
	known bool
}

// keyCache is the bounded key cache, QUOTACORE_KEY_CACHE_ENTRIES. It is a
// plain LRU: a hit moves the prefix to the front, and an insert past the bound
// evicts the least recently used prefix, known or not.
type keyCache struct {
	mu    sync.Mutex
	limit int
	ll    *list.List
	byKey map[string]*list.Element
	// epoch moves on every forget and every invalidation. A read that began
	// before it moved may carry a key as it was before a revoke, so a miss
	// does not cache what it read, and a reconcile does not overwrite an
	// entry put after it began.
	epoch uint64
}

type cacheItem struct {
	prefix string
	entry  entry
	born   uint64 // the epoch the entry was put at
}

func newKeyCache(limit int) *keyCache {
	if limit < 1 {
		limit = 1
	}
	return &keyCache{limit: limit, ll: list.New(), byKey: map[string]*list.Element{}}
}

func (c *keyCache) get(prefix string) (entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byKey[prefix]
	if !ok {
		return entry{}, false
	}
	c.ll.MoveToFront(e)
	return e.Value.(*cacheItem).entry, true
}

// now is the current epoch, taken before a read whose answer will be put or
// applied.
func (c *keyCache) now() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

// bump moves the epoch, for an invalidation: whatever is being read now may
// predate it.
func (c *keyCache) bump() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
}

// put caches what a read that began at since found, unless the epoch moved
// while it was in flight.
func (c *keyCache) put(prefix string, en entry, since uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epoch != since {
		return
	}
	if e, ok := c.byKey[prefix]; ok {
		e.Value.(*cacheItem).entry = en
		e.Value.(*cacheItem).born = c.epoch
		c.ll.MoveToFront(e)
		return
	}
	c.byKey[prefix] = c.ll.PushFront(&cacheItem{prefix: prefix, entry: en, born: c.epoch})
	for c.ll.Len() > c.limit {
		back := c.ll.Back()
		c.ll.Remove(back)
		delete(c.byKey, back.Value.(*cacheItem).prefix)
	}
}

// prefixes lists every cached prefix, for the off-path reconcile.
func (c *keyCache) prefixes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.byKey))
	for p := range c.byKey {
		out = append(out, p)
	}
	return out
}

// apply replaces every listed prefix with what the database now says: a row
// that came back is the new truth, and a prefix that did not is unknown.
// Recency is untouched, because a reconcile is not a use. A prefix evicted
// while the read was in flight is not re-added, and one put after the epoch
// moved past since is newer than the read and is kept.
func (c *keyCache) apply(asked []string, rows map[string]Key, since uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range asked {
		e, ok := c.byKey[p]
		if !ok || e.Value.(*cacheItem).born > since {
			continue
		}
		if k, found := rows[p]; found {
			e.Value.(*cacheItem).entry = entry{key: k, known: true}
		} else {
			e.Value.(*cacheItem).entry = entry{}
		}
	}
}

// forget drops one prefix, so the next request with it misses.
func (c *keyCache) forget(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch++
	if e, ok := c.byKey[prefix]; ok {
		c.ll.Remove(e)
		delete(c.byKey, prefix)
	}
}

func (c *keyCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
