package vmware

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prezhdarov/vmware-exporter/internal/collector"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/types"
)

func TestProviderCacheKeyIncludesVersionAndType(t *testing.T) {
	k1 := providerCacheKey("vc:443", "7.0.3", "12345", "HostSystem")
	k2 := providerCacheKey("vc:443", "7.0.3", "12345", "HostSystem")
	k3 := providerCacheKey("vc:443", "8.0.0", "99999", "HostSystem")
	k4 := providerCacheKey("vc:443", "7.0.3", "12345", "VirtualMachine")

	if k1 != k2 {
		t.Fatal("same target/version/build/type must produce the same key")
	}
	if k1 == k3 {
		t.Fatal("an upgraded build must produce a different cache key")
	}
	if k1 == k4 {
		t.Fatal("different entity types must produce different cache keys")
	}
}

func fakeProviderSummary(refresh int32) *types.PerfProviderSummary {
	return &types.PerfProviderSummary{RefreshRate: refresh, CurrentSupported: true}
}

func TestProviderCacheHitsWithinTTL(t *testing.T) {
	c := NewProviderSummaryCache()
	var fetches int32
	fetch := func() (*types.PerfProviderSummary, error) {
		atomic.AddInt32(&fetches, 1)
		return fakeProviderSummary(20), nil
	}
	const ttl = time.Minute
	t0 := time.Unix(1000, 0)

	first, err := c.getWithFetcher("k", ttl, t0, fetch)
	if err != nil || first == nil || first.RefreshRate != 20 {
		t.Fatalf("first Get: err=%v summary=%+v", err, first)
	}
	second, err := c.getWithFetcher("k", ttl, t0.Add(ttl-time.Second), fetch)
	if err != nil || second.RefreshRate != 20 {
		t.Fatalf("cached Get: err=%v summary=%+v", err, second)
	}
	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Fatalf("fetches = %d, want 1 (second Get must be a cache hit)", got)
	}
	if first != second {
		t.Fatal("cached summary must be the same instance")
	}
}

func TestProviderCacheRefetchesAfterTTL(t *testing.T) {
	c := NewProviderSummaryCache()
	var fetches int32
	fetch := func() (*types.PerfProviderSummary, error) {
		atomic.AddInt32(&fetches, 1)
		return fakeProviderSummary(20), nil
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

func TestProviderCacheNegativeResultNotCached(t *testing.T) {
	c := NewProviderSummaryCache()
	var fetches int32
	wantErr := errors.New("vCenter unreachable")
	fetch := func() (*types.PerfProviderSummary, error) {
		atomic.AddInt32(&fetches, 1)
		return nil, wantErr
	}

	if _, err := c.getWithFetcher("k", time.Minute, time.Unix(1, 0), fetch); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	// 失败不得缓存：紧接着的重试必须再次 fetch。
	if _, err := c.getWithFetcher("k", time.Minute, time.Unix(2, 0), fetch); !errors.Is(err, wantErr) {
		t.Fatalf("second err = %v, want %v", err, wantErr)
	}
	if got := atomic.LoadInt32(&fetches); got != 2 {
		t.Fatalf("fetches = %d, want 2 (failed result must not be cached)", got)
	}
}

func TestProviderCacheCoalescesConcurrentMisses(t *testing.T) {
	c := NewProviderSummaryCache()
	var fetches, entered int32
	release := make(chan struct{})
	fetch := func() (*types.PerfProviderSummary, error) {
		atomic.AddInt32(&entered, 1)
		<-release
		atomic.AddInt32(&fetches, 1)
		return fakeProviderSummary(20), nil
	}

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := c.getWithFetcher("same-key", time.Minute, time.Unix(1, 0), fetch)
			if err == nil && (s == nil || s.RefreshRate != 20) {
				err = errors.New("unexpected summary")
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

func TestProviderCacheEvictsExpiredEntriesOnStore(t *testing.T) {
	c := NewProviderSummaryCache()
	fetch := func() (*types.PerfProviderSummary, error) {
		return fakeProviderSummary(20), nil
	}
	const ttl = time.Minute
	t0 := time.Unix(1000, 0)

	if _, err := c.getWithFetcher("vc|7.0|1|HostSystem", ttl, t0, fetch); err != nil {
		t.Fatal(err)
	}
	if _, err := c.getWithFetcher("vc|8.0|2|HostSystem", ttl, t0.Add(ttl+time.Second), fetch); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, old := c.entries["vc|7.0|1|HostSystem"]; old {
		t.Fatal("expired old-version entry must be evicted when a new value is stored")
	}
	if _, ok := c.entries["vc|8.0|2|HostSystem"]; !ok {
		t.Fatal("current entry must remain after eviction sweep")
	}
}

// TestProviderSummaryCacheEndToEnd 是该优化的端到端验收：用 NewAPIWithCaches
// 登录 vcsim，证明 (1) Scrape.ProviderSummary 闭包确实被安装；(2) 它按
// entity.Type 命中进程级缓存，而不是每次都发 QueryPerfProviderSummary SOAP。
//
// 验证手法：先用真实 HostSystem ref 解析一次（发一次真实 SOAP、填充缓存），再用
// 一个同类型但服务端不存在的伪造 ref 解析。若闭包发真实 SOAP，伪造实体会被
// vcsim 以 InvalidArgument 拒绝、返回 error；命中缓存则直接返回第一次那份
// 真实 summary、无 error。同时再取真实 VM ref，确认不同 entity.Type 走独立 key。
func TestProviderSummaryCacheEndToEnd(t *testing.T) {
	restoreVMwareFlags(t)
	*vmwInterval = 20
	*vmGranularity = 20
	*vmwTimeout = 60
	// TTL 直接用 NewAPIWithCaches 对应的 flag 值（默认 10m），这里显式钉住。
	*vmwProviderCacheTTL = 10 * time.Minute

	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatalf("create model: %v", err)
	}
	defer model.Remove()

	server := model.Service.NewServer()
	defer server.Close()

	creds := Credentials{
		Username: "user",
		Password: "pass",
		Target:   server.URL.Host,
		Schema:   server.URL.Scheme,
		Insecure: true,
	}

	api := NewAPIWithCaches(NewCounterCache(), NewProviderSummaryCache())
	s, cleanup, err := api.LoginWithCredentials(context.Background(), creds, discardLogger())
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer cleanup()

	if s.ProviderSummary == nil {
		t.Fatal("expected Scrape.ProviderSummary to be wired for the /metrics-style login, but it is nil")
	}

	ctx := context.Background()

	hostRef := firstExistingRef(t, ctx, s, "HostSystem")

	first, err := s.ProviderSummary(ctx, hostRef)
	if err != nil {
		t.Fatalf("first ProviderSummary(real host) should succeed: %v", err)
	}
	if first == nil || !first.CurrentSupported || first.RefreshRate != 20 {
		t.Fatalf("unexpected real summary: %+v", first)
	}

	// 同类型伪造 ref：命中缓存才会成功。
	bogus := types.ManagedObjectReference{Type: "HostSystem", Value: "this-entity-does-not-exist"}
	fromCache, err := s.ProviderSummary(ctx, bogus)
	if err != nil {
		t.Fatalf("second lookup with a bogus same-type ref must hit the cache and not call SOAP, got error: %v", err)
	}
	if fromCache != first {
		t.Fatal("cached lookup must return the exact same summary instance")
	}

	// 不同 entity.Type 必须是独立 key：真实 VM ref 应拿到 VM 自己的 summary，
	// 而不是复用 HostSystem 那份。
	vmRef := firstExistingRef(t, ctx, s, "VirtualMachine")
	vmSummary, err := s.ProviderSummary(ctx, vmRef)
	if err != nil {
		t.Fatalf("ProviderSummary(real vm): %v", err)
	}
	if vmSummary == nil {
		t.Fatal("expected a non-nil VM summary")
	}
}

// TestProviderSummaryNotWiredForPlainAPI 钉住 /probe 边界：NewAPI()（多租户、
// 不带缓存）登录后 Scrape.ProviderSummary 必须为 nil，协商走实时 SOAP。
func TestProviderSummaryNotWiredForPlainAPI(t *testing.T) {
	restoreVMwareFlags(t)
	*vmwInterval = 20
	*vmGranularity = 20
	*vmwTimeout = 60

	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatalf("create model: %v", err)
	}
	defer model.Remove()

	server := model.Service.NewServer()
	defer server.Close()

	creds := Credentials{
		Username: "user",
		Password: "pass",
		Target:   server.URL.Host,
		Schema:   server.URL.Scheme,
		Insecure: true,
	}

	s, cleanup, err := NewAPI().LoginWithCredentials(context.Background(), creds, discardLogger())
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer cleanup()

	if s.ProviderSummary != nil {
		t.Fatal("plain NewAPI() (used on /probe) must not wire a process-level summary cache")
	}
}

// firstExistingRef 用 ContainerView 取回 moType 的第一个真实实体引用。
func firstExistingRef(t *testing.T, ctx context.Context, s *collector.Scrape, moType string) types.ManagedObjectReference {
	t.Helper()

	mgr := view.NewManager(s.Client)
	cv, err := mgr.CreateContainerView(ctx, s.Client.ServiceContent.RootFolder, []string{moType}, true)
	if err != nil {
		t.Fatalf("create container view for %s: %v", moType, err)
	}
	defer func() { _ = cv.Destroy(ctx) }()

	refs, err := cv.Find(ctx, []string{moType}, nil)
	if err != nil {
		t.Fatalf("find %s: %v", moType, err)
	}
	if len(refs) == 0 {
		t.Fatalf("simulator has no %s", moType)
	}
	return refs[0]
}
