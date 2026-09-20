package collector

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestFetchInventoryCachesWithinTTL 锁住最基本的命中语义：TTL 内第二次取数
// 不得再调 load。
//
// 反向验证：把 get() 里的新鲜度判断改成「永远命中」之外的任何失效形状（例如
// 删掉 put），这条都会报红 —— 具体地，若实现忘了回填，calls 会是 2。
func TestFetchInventoryCachesWithinTTL(t *testing.T) {
	cache := NewInventoryCache()

	var calls atomic.Int32
	load := func() ([]string, error) {
		calls.Add(1)
		return []string{"dc1", "dc2"}, nil
	}

	first, err := FetchInventory(cache, time.Minute, "k", load)
	if err != nil {
		t.Fatalf("first fetch failed: %s", err)
	}

	second, err := FetchInventory(cache, time.Minute, "k", load)
	if err != nil {
		t.Fatalf("second fetch failed: %s", err)
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("load called %d times, want 1 (second fetch must hit the cache)", got)
	}

	if len(first) != 2 || len(second) != 2 {
		t.Errorf("got %d/%d items, want 2/2", len(first), len(second))
	}
}

// TestFetchInventoryExpiresAfterTTL 断言过期后重新拉取。
//
// 这是缓存正确性的另一半。一个「写进去就永不过期」的实现同样能通过上一条
// 测试，但拓扑变更（新建数据中心/集群）将永远不可见 —— 那正是 P0-2 要避免
// 的静默陈旧。TTL 取极短值而不是 sleep 整个 5m 默认窗口。
func TestFetchInventoryExpiresAfterTTL(t *testing.T) {
	cache := NewInventoryCache()

	var calls atomic.Int32
	load := func() ([]int, error) {
		n := calls.Add(1)
		return []int{int(n)}, nil
	}

	first, _ := FetchInventory(cache, 5*time.Millisecond, "k", load)
	time.Sleep(15 * time.Millisecond)
	second, _ := FetchInventory(cache, 5*time.Millisecond, "k", load)

	if got := calls.Load(); got != 2 {
		t.Errorf("load called %d times, want 2 (entry must expire after TTL)", got)
	}

	if first[0] != 1 || second[0] != 2 {
		t.Errorf("stale data served after expiry: first=%v second=%v", first, second)
	}

	// 过期项应被顺手删除：map 不应随被淘汰的 key 无限增长。
	cache.mu.Lock()
	entries := len(cache.entries)
	cache.mu.Unlock()

	if entries != 1 {
		t.Errorf("cache holds %d entries after expiry, want 1 (expired entry must be evicted)", entries)
	}
}

// TestFetchInventoryDisabledBypassesCache 覆盖两个关闭开关。
//
// 两条路径都必须落到 load：
//   - cache==nil：/probe 多租户路径的形态，越权读防护全靠这个 nil；
//   - ttl<=0：用户显式 -scrape.inventory-cache-ttl=0 回到实时行为。
//
// 反向验证：若 FetchInventory 开头的 nil 判断被删，nil cache 会直接 panic；
// 若 ttl<=0 仍走缓存，calls 会是 1 而非 3。
func TestFetchInventoryDisabledBypassesCache(t *testing.T) {
	var callsNil atomic.Int32
	for i := 0; i < 3; i++ {
		got, err := FetchInventory[string](nil, time.Minute, "k", func() ([]string, error) {
			callsNil.Add(1)
			return []string{"x"}, nil
		})
		if err != nil || len(got) != 1 {
			t.Fatalf("nil cache fetch %d failed: %v %v", i, got, err)
		}
	}

	if got := callsNil.Load(); got != 3 {
		t.Errorf("nil cache bypass: load called %d times, want 3", got)
	}

	cache := NewInventoryCache()

	var callsZero atomic.Int32
	for i := 0; i < 3; i++ {
		if _, err := FetchInventory(cache, 0, "k", func() ([]string, error) {
			callsZero.Add(1)
			return []string{"x"}, nil
		}); err != nil {
			t.Fatalf("ttl=0 fetch %d failed: %s", i, err)
		}
	}

	if got := callsZero.Load(); got != 3 {
		t.Errorf("ttl=0 bypass: load called %d times, want 3", got)
	}

	// 旁路时一条都不该写进缓存。
	cache.mu.Lock()
	entries := len(cache.entries)
	cache.mu.Unlock()

	if entries != 0 {
		t.Errorf("bypassed fetches wrote %d cache entries, want 0", entries)
	}
}

// TestFetchInventoryErrorNotCached 断言失败结果不进缓存。
//
// 一次瞬态超时若被记住，整个 TTL 内该拓扑类型都会持续报同一个错，即便
// vCenter 早已恢复。失败必须下一轮立即重试。
func TestFetchInventoryErrorNotCached(t *testing.T) {
	cache := NewInventoryCache()

	var calls atomic.Int32
	boom := errors.New("soap timeout")

	load := func() ([]string, error) {
		n := calls.Add(1)
		if n == 1 {
			return nil, boom
		}

		return []string{"recovered"}, nil
	}

	if _, err := FetchInventory(cache, time.Minute, "k", load); !errors.Is(err, boom) {
		t.Fatalf("first fetch err = %v, want %v", err, boom)
	}

	got, err := FetchInventory(cache, time.Minute, "k", load)
	if err != nil {
		t.Fatalf("second fetch must retry and succeed, got %s", err)
	}

	if got[0] != "recovered" {
		t.Errorf("got %v, want recovered data after error", got)
	}

	if n := calls.Load(); n != 2 {
		t.Errorf("load called %d times, want 2 (errors must not be cached)", n)
	}
}

// TestFetchInventoryCollapsesConcurrentMisses 断言 singleflight：同一 key 上
// 并发的未命中只允许一次真实拉取。
//
// 抓取闸放开后，同刻多个 /metrics 请求（或同一轮内 propSpec 相同的多个
// collector，如 vsan 与 vsan.perf）可能在缓存刚过期时一起未命中。不合并就会
// 把同一份 ContainerView 检索重放 N 次 —— 缓存恰恰在最该挡住峰值的时候失效。
func TestFetchInventoryCollapsesConcurrentMisses(t *testing.T) {
	cache := NewInventoryCache()

	const goroutines = 16

	var calls atomic.Int32
	release := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()
			<-release

			_, _ = FetchInventory(cache, time.Minute, "k", func() ([]string, error) {
				calls.Add(1)
				time.Sleep(30 * time.Millisecond) // 撑开竞态窗口，见 TestHostsFetchesOnlyOnce 同理
				return []string{"dc1"}, nil
			})
		}()
	}

	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("concurrent misses triggered %d loads, want 1 (singleflight must collapse them)", got)
	}
}

// TestInventoryKeySeparatesTargetsAndSpecs 锁住缓存键的两段安全语义：
//
//   - 不同 target 绝不共享 —— 这是同进程抓多个 vCenter（即便只走 /metrics）
//     时的隔离底线；
//   - 同类型但不同 propSpec 绝不共享 —— 命中「属性更少」的旧结果会让调用方
//     读到一堆零值字段，且没有任何报错。
//
// 反向验证：若 InventoryKey 退化成只按对象类型拼键，第二、三条断言会报红。
func TestInventoryKeySeparatesTargetsAndSpecs(t *testing.T) {
	cache := NewInventoryCache()

	mkLoad := func(marker string) func() ([]string, error) {
		return func() ([]string, error) { return []string{marker}, nil }
	}

	a, _ := FetchInventory(cache, time.Minute,
		InventoryKey("vc-a", []string{"Datacenter"}, []string{"name"}), mkLoad("A"))
	b, _ := FetchInventory(cache, time.Minute,
		InventoryKey("vc-b", []string{"Datacenter"}, []string{"name"}), mkLoad("B"))

	if a[0] != "A" || b[0] != "B" {
		t.Errorf("targets share cache entries: a=%v b=%v", a, b)
	}

	full, _ := FetchInventory(cache, time.Minute,
		InventoryKey("vc-a", []string{"ClusterComputeResource"}, []string{"name", "summary"}),
		mkLoad("full"))
	nameOnly, _ := FetchInventory(cache, time.Minute,
		InventoryKey("vc-a", []string{"ClusterComputeResource"}, []string{"name"}),
		mkLoad("nameonly"))

	if full[0] != "full" || nameOnly[0] != "nameonly" {
		t.Errorf("different property specs share a cache entry: full=%v nameOnly=%v", full, nameOnly)
	}

	// 同一 target + 同 propSpec 的第二次取数必须命中第一条（marker 不会变）。
	aAgain, _ := FetchInventory(cache, time.Minute,
		InventoryKey("vc-a", []string{"Datacenter"}, []string{"name"}), mkLoad("A2"))
	if aAgain[0] != "A" {
		t.Errorf("identical key missed the cache: got %v, want A", aAgain)
	}
}

// TestCollectorSetInjectsInventoryCache 锁住 Options → Scrape 的缓存注入。
//
// 与 TestCollectInjectsNamespaceAndConcurrency 同样的理由：漏注入不会让任何
// 上层测试变红，只是缓存悄悄永远不生效（命中率 0，性能改进静默消失）。
//
// 另一条断言覆盖 /probe 形态：Options 不传缓存时 Scrape.InventoryCache 必须
// 为 nil —— 业务层靠这个 nil 旁路缓存，nil 丢了就打开了越权读的口子。
func TestCollectorSetInjectsInventoryCache(t *testing.T) {
	cache := NewInventoryCache()
	ttl := 5 * time.Minute

	login := &stubLogin{scrape: &Scrape{Target: "vcenter.example.com", TargetType: TargetTypeVCenter}}
	defs, instances := stubDefinitions(1, nil)

	cs, err := NewCollectorSet(t.Context(), defs, Options{
		Namespace:      "vmware",
		Login:          login,
		InventoryCache: cache,
		InventoryTTL:   ttl,
		Errors:         NewScrapeErrors(),
		SOAP:           NewSOAPStats(),
	})
	if err != nil {
		t.Fatalf("NewCollectorSet failed: %s", err)
	}

	gatherText(t, cs)

	seen := instances[0].seen.Load()
	if seen == nil {
		t.Fatal("collector was never invoked")
	}

	// 用指针相等而不是「非 nil」：注入的必须就是调用方持有的那一个进程级
	// 单例，而不是 CollectorSet 自己 new 出来的新实例（那样跨请求命中率为 0）。
	if seen.InventoryCache != cache {

		t.Errorf("Scrape.InventoryCache = %p, want the injected singleton %p", seen.InventoryCache, cache)
	}

	if seen.InventoryTTL != ttl {
		t.Errorf("Scrape.InventoryTTL = %s, want %s", seen.InventoryTTL, ttl)
	}

	// /probe 形态：不传缓存字段。
	probeLogin := &stubLogin{scrape: &Scrape{Target: "other.example.com", TargetType: TargetTypeVCenter}}
	probeDefs, probeInstances := stubDefinitions(1, nil)

	probeSet, err := NewCollectorSet(t.Context(), probeDefs, Options{
		Namespace: "vmware",
		Login:     probeLogin,
		Errors:    NewScrapeErrors(),
		SOAP:      NewSOAPStats(),
	})
	if err != nil {
		t.Fatalf("probe NewCollectorSet failed: %s", err)
	}

	gatherText(t, probeSet)

	if probeSeen := probeInstances[0].seen.Load(); probeSeen.InventoryCache != nil {
		t.Errorf("probe path got a non-nil inventory cache: cross-credential reads would be possible")
	}
}

// TestInventoryCacheEvictsOldestOverLimit 验证 M-02 的硬容量兜底：distinct key
// 超过 maxEntries 时淘汰 fetchedAt 最旧的条目，map 大小被钉在上限。
//
// 直接调 put 并 sleep 拉开 fetchedAt，避免依赖 TTL 过期路径。
func TestInventoryCacheEvictsOldestOverLimit(t *testing.T) {
	const limit = 3
	cache := NewInventoryCacheWithLimit(limit)
	const ttl = time.Hour // 远大于测试时长，保证不是 TTL 过期在删除

	cache.put("a", []string{"a"}, ttl)
	time.Sleep(2 * time.Millisecond)
	cache.put("b", []string{"b"}, ttl)
	time.Sleep(2 * time.Millisecond)
	cache.put("c", []string{"c"}, ttl)
	time.Sleep(2 * time.Millisecond)

	// 第 4 个 key 触发淘汰；a 最旧，应被移除。
	cache.put("d", []string{"d"}, ttl)

	cache.mu.Lock()
	n := len(cache.entries)
	_, aExists := cache.entries["a"]
	_, dExists := cache.entries["d"]
	cache.mu.Unlock()

	if n != limit {
		t.Errorf("entries = %d, want bounded at %d", n, limit)
	}
	if aExists {
		t.Errorf("oldest key %q survived eviction", "a")
	}
	if !dExists {
		t.Errorf("newly written key %q was evicted over an older one", "d")
	}

	// 再写一个，应继续淘汰当前最旧的 b，大小保持上限。
	time.Sleep(2 * time.Millisecond)
	cache.put("e", []string{"e"}, ttl)
	cache.mu.Lock()
	_, bExists := cache.entries["b"]
	gotSize := len(cache.entries)
	cache.mu.Unlock()
	if bExists {
		t.Errorf("oldest key %q survived the second eviction", "b")
	}
	if gotSize != limit {
		t.Errorf("entries = %d after second eviction, want %d", gotSize, limit)
	}
}

// TestInventoryCachePutReapsNeverRevisitedExpired 验证 M-02 的兜底清扫：一个
// 永不再被 get 访问的 key 不会因为惰性删除够不到而永驻 map —— 后续任意一次
// put 都会回收它。
func TestInventoryCachePutReapsNeverRevisitedExpired(t *testing.T) {
	cache := NewInventoryCacheWithLimit(4096)

	cache.put("expired-key", []string{"x"}, 5*time.Millisecond)
	time.Sleep(10 * time.Millisecond)

	// 写一个不相关的新 key，应顺手回收已过期的旧 key。
	cache.put("fresh-key", []string{"y"}, time.Hour)

	cache.mu.Lock()
	_, staleExists := cache.entries["expired-key"]
	_, freshExists := cache.entries["fresh-key"]
	cache.mu.Unlock()

	if staleExists {
		t.Errorf("expired never-revisited key survived a put (no low-frequency sweep bound)")
	}
	if !freshExists {
		t.Errorf("fresh key missing after reaping")
	}
}

// TestInventoryCacheDefaultLimitSane 钉住默认上限是一个"远大于正常 key 数"的
// 有限正值，防止有人把它改成 0（=退化为仅限容量的关闭态）或无界。
func TestInventoryCacheDefaultLimitSane(t *testing.T) {
	cache := NewInventoryCache()
	if cache.maxEntries <= 0 {
		t.Fatalf("default maxEntries = %d, want a positive bound", cache.maxEntries)
	}
	// 单 target 的正常 key 是 mo 类型集 × 属性集的组合，几十量级；默认上限应
	// 远在其上，正常部署绝不触发淘汰。
	if cache.maxEntries < 256 {
		t.Errorf("default maxEntries = %d unexpectedly small; normal scrapes could evict live entries", cache.maxEntries)
	}
}
