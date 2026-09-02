package mcp

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheHitWithinTTL(t *testing.T) {
	c := newResultCache(time.Minute, 8)
	var calls int32
	fn := func() (string, error) { atomic.AddInt32(&calls, 1); return "v", nil }
	for i := 0; i < 5; i++ {
		if v, err := c.do("k", fn); err != nil || v != "v" {
			t.Fatalf("v=%q err=%v", v, err)
		}
	}
	if calls != 1 {
		t.Fatalf("fn ran %d times, want 1 (cached)", calls)
	}
}

func TestCacheExpires(t *testing.T) {
	c := newResultCache(20*time.Millisecond, 8)
	var calls int32
	fn := func() (string, error) { atomic.AddInt32(&calls, 1); return "v", nil }
	_, _ = c.do("k", fn)
	time.Sleep(40 * time.Millisecond)
	_, _ = c.do("k", fn)
	if calls != 2 {
		t.Fatalf("fn ran %d times, want 2 (expired)", calls)
	}
}

func TestCacheErrorsNotCached(t *testing.T) {
	c := newResultCache(time.Minute, 8)
	var calls int32
	fn := func() (string, error) { atomic.AddInt32(&calls, 1); return "", errors.New("boom") }
	_, _ = c.do("k", fn)
	_, _ = c.do("k", fn)
	if calls != 2 {
		t.Fatalf("errors must not be cached: ran %d, want 2", calls)
	}
}

func TestCacheLRUBounded(t *testing.T) {
	c := newResultCache(time.Minute, 2)
	_, _ = c.do("a", func() (string, error) { return "1", nil })
	_, _ = c.do("b", func() (string, error) { return "2", nil })
	_, _ = c.do("c", func() (string, error) { return "3", nil }) // evicts "a"
	var ran int32
	_, _ = c.do("a", func() (string, error) { atomic.AddInt32(&ran, 1); return "1", nil })
	if ran != 1 {
		t.Fatal("a should have been evicted (LRU cap 2)")
	}
}

func TestCacheSingleflightCollapses(t *testing.T) {
	c := newResultCache(time.Minute, 8)
	var calls int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.do("k", func() (string, error) {
				atomic.AddInt32(&calls, 1)
				time.Sleep(10 * time.Millisecond)
				return "v", nil
			})
		}()
	}
	wg.Wait()
	if calls > 3 {
		t.Fatalf("singleflight should collapse concurrent calls, ran %d", calls)
	}
}
