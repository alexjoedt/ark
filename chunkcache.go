package ark

import (
	"container/list"
	"sync"
)

// defaultRestoreCache is the cap of the restore chunk cache when the caller
// sets none. Restoring a snapshot with duplicated content re-reads the same
// chunk repeatedly, and this is the budget for avoiding that.
const defaultRestoreCache int64 = 256 << 20

// chunkCache is a byte-bounded LRU over chunk content, keyed by chunk hash.
//
// The entries it hands out are shared with everyone else holding the same
// chunk, so a caller reads them and never writes into them. A nil *chunkCache
// is a valid disabled cache: it stores nothing and never hits.
type chunkCache struct {
	mu    sync.Mutex
	limit int64
	size  int64
	order *list.List // front is most recently used, values are *cacheEntry
	items map[string]*list.Element
}

type cacheEntry struct {
	id   string
	data []byte
}

// newChunkCache returns a cache capped at limit bytes, or nil when limit is
// not positive, which disables caching.
func newChunkCache(limit int64) *chunkCache {
	if limit <= 0 {
		return nil
	}
	return &chunkCache{
		limit: limit,
		order: list.New(),
		items: make(map[string]*list.Element),
	}
}

func (c *chunkCache) get(id string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[id]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	entry, ok := el.Value.(*cacheEntry)
	if !ok {
		return nil, false
	}
	return entry.data, true
}

// put stores data under id and evicts least recently used entries until the
// cache is within its limit. A chunk larger than the whole limit is not
// stored: keeping it would evict everything else for one entry.
func (c *chunkCache) put(id string, data []byte) {
	if c == nil || int64(len(data)) > c.limit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[id]; ok {
		c.order.MoveToFront(el)
		return
	}

	c.items[id] = c.order.PushFront(&cacheEntry{id: id, data: data})
	c.size += int64(len(data))

	for c.size > c.limit {
		oldest := c.order.Back()
		if oldest == nil {
			return
		}
		entry, ok := c.order.Remove(oldest).(*cacheEntry)
		if !ok {
			return
		}
		delete(c.items, entry.id)
		c.size -= int64(len(entry.data))
	}
}
