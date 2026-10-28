package ark

import "testing"

func TestChunkCacheEvictsTheLeastRecentlyUsed(t *testing.T) {
	c := newChunkCache(10)

	c.put("a", []byte("12345"))
	c.put("b", []byte("12345"))
	if _, ok := c.get("a"); !ok {
		t.Fatal("a was evicted although the cache still fits it")
	}

	// a was just read, so b is the least recently used and pays for c.
	c.put("c", []byte("12345"))
	if _, ok := c.get("b"); ok {
		t.Error("b survived, want the least recently used entry evicted")
	}
	if _, ok := c.get("a"); !ok {
		t.Error("a was evicted although it was used more recently than b")
	}
	if c.size > c.limit {
		t.Errorf("cache holds %d bytes, over its limit of %d", c.size, c.limit)
	}
}

func TestChunkCacheRejectsAChunkBiggerThanItself(t *testing.T) {
	c := newChunkCache(4)
	c.put("big", []byte("123456789"))
	if _, ok := c.get("big"); ok {
		t.Error("a chunk larger than the whole cache was stored")
	}
	if c.size != 0 {
		t.Errorf("cache holds %d bytes after rejecting the chunk", c.size)
	}
}

// A disabled cache is the nil cache, and every call on it has to be safe.
func TestDisabledChunkCacheIsUsable(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		c := newChunkCache(limit)
		if c != nil {
			t.Fatalf("newChunkCache(%d) returned a cache, want nil", limit)
		}
		c.put("a", []byte("data"))
		if _, ok := c.get("a"); ok {
			t.Errorf("newChunkCache(%d) served a hit", limit)
		}
	}
}
