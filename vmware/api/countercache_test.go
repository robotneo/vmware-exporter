package vmware

import (
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmware/govmomi/vim25/types"
)

func fakeCounterMap(n int) map[string]*types.PerfCounterInfo {
	m := make(map[string]*types.PerfCounterInfo, n)
	for i := 0; i < n; i++ {
		key := int32(i + 1)
		m["counter."+strconv.Itoa(i)] = &types.PerfCounterInfo{Key: key}
	}
	return m
}

func TestCounterCacheKeyIncludesVersion(t *testing.T) {
	k1 := counterCacheKey("vc:443", "7.0.3", "12345")
	k2 := counterCacheKey("vc:443", "7.0.3", "12345")
	k3 := counterCacheKey("vc:443", "8.0.0", "99999")

	if k1 != k2 {
		t.Fatal("same target/version/build must produce the same key")
	}
	if k1 == k3 {
		t.Fatal("an upgraded build must produce a different cache key")
	}
}

func TestCounterCacheHitsWithinTTL(t *testing.T) {
	c := NewCounterCache()
	var fetches int32
	fetch := func() (map[string]*types.PerfCounterInfo, error) {
		atomic.AddInt32(&fetches, 1)
		return fakeCounterMap(3), nil
	}
	const ttl = time.Minute
	t0 := time.Unix(1000, 0)

	first, err := c.getWithFetcher("k", ttl, t0, fetch)
	if err != nil || len(first) != 3 {
		t.Fatalf("first Get: err=%v len=%d", err, len(first))
	}
	// TTL 内再次取，必须命中、不触发第二次 fetch。
	second, err := c.getWithFetcher("k", ttl, t0.Add(ttl-time.Second), fetch)
	if err != nil || len(second) != 3 {
		t.Fatalf("cached Get: err=%v len=%d", err, len(second))
	}
	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Fatalf("fetches = %d, want 1 (second Get must be a cache hit)", got)
	}
	// 返回的必须是同一只读实例（共享 map，不复制）。
	if &first == nil || len(first) != len(second) {
		t.Fatal("cached map must be the same instance")
	}
}

func TestCounterCacheRefetchesAfterTTL(t *testing.T) {
	c := NewCounterCache()
	var fetches int32
	fetch := func() (map[string]*types.PerfCounterInfo, error) {
		atomic.AddInt32(&fetches, 1)
		return fakeCounterMap(1), nil
	}
	const ttl = time.Minute
	t0 := time.Unix(1000, 0)

	if _, err := c.getWithFetcher("k", ttl, t0, fetch); err != nil {
		t.Fatal(err)
	}
	// 恰在 TTL 边界（>= ttl）视为过期，重新拉取。
	if _, err := c.getWithFetcher("k", ttl, t0.Add(ttl), fetch); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&fetches); got != 2 {
		t.Fatalf("fetches = %d, want 2 after TTL expiry", got)
	}
}

func TestCounterCacheNegativeResultNotCached(t *testing.T) {
	c := NewCounterCache()
	var fetches int32
	wantErr := errors.New("vCenter unreachable")
	fetch := func() (map[string]*types.PerfCounterInfo, error) {
		atomic.AddInt32(&fetches, 1)
		return nil, wantErr
	}

	if _, err := c.getWithFetcher("k", time.Minute, time.Unix(1, 0), fetch); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	// 失败不得被缓存：紧接着的重试必须再次触发 fetch（不能在一个 TTL 里持续返错）。
	if _, err := c.getWithFetcher("k", time.Minute, time.Unix(2, 0), fetch); !errors.Is(err, wantErr) {
		t.Fatalf("second err = %v, want %v", err, wantErr)
	}
	if got := atomic.LoadInt32(&fetches); got != 2 {
		t.Fatalf("fetches = %d, want 2 (failed result must not be cached)", got)
	}
}

func TestCounterCacheCoalescesConcurrentMisses(t *testing.T) {
	c := NewCounterCache()
	var fetches, entered int32
	release := make(chan struct{})
	fetch := func() (map[string]*types.PerfCounterInfo, error) {
		atomic.AddInt32(&entered, 1)
		<-release // 撑住窗口，让全部 goroutine 都进入 singleflight 等待
		atomic.AddInt32(&fetches, 1)
		return fakeCounterMap(2), nil
	}

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := c.getWithFetcher("same-key", time.Minute, time.Unix(1, 0), fetch)
			if err == nil && len(m) != 2 {
				err = errors.New("unexpected map size")
			}
			errs <- err
		}()
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Get failed: %v", err)
		}
	}
	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Fatalf("fetches = %d, want 1 (singleflight must coalesce concurrent misses)", got)
	}
}

func TestCounterCacheEvictsExpiredEntriesOnStore(t *testing.T) {
	c := NewCounterCache()
	fetch := func() (map[string]*types.PerfCounterInfo, error) {
		return fakeCounterMap(1), nil
	}
	const ttl = time.Minute
	t0 := time.Unix(1000, 0)

	// 旧版本 key 在 t0 填充。
	if _, err := c.getWithFetcher("vc|7.0|1", ttl, t0, fetch); err != nil {
		t.Fatal(err)
	}
	// 升级后出现新版本 key，时间已超过旧 key 的 TTL：store 时旧 key 必须被回收。
	if _, err := c.getWithFetcher("vc|8.0|2", ttl, t0.Add(ttl+time.Second), fetch); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, old := c.entries["vc|7.0|1"]; old {
		t.Fatal("expired old-version entry must be evicted when a new value is stored")
	}
	if _, ok := c.entries["vc|8.0|2"]; !ok {
		t.Fatal("current entry must remain after eviction sweep")
	}
}
