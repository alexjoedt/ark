package ark

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNodeCacheLoadsAMissOnce(t *testing.T) {
	c := newNodeCache(10)
	release := make(chan struct{})
	var calls atomic.Int32
	load := func() (*treeNode, error) { //nolint:unparam // the loader signature of nodeCache.load
		calls.Add(1)
		<-release
		return &treeNode{}, nil
	}
	var wg, started sync.WaitGroup
	started.Add(8)
	for range 8 {
		wg.Go(func() {
			started.Done()
			if _, err := c.load("h", load); err != nil {
				t.Errorf("load: %v", err)
			}
		})
	}
	started.Wait()
	time.Sleep(10 * time.Millisecond) // let every caller reach load before the first returns
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Errorf("loaded %d times, want 1", n)
	}
}
