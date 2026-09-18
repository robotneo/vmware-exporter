package vmware

import (
	"context"
	"testing"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
)

// benchVCSim 在 api 包内自举一个最小 vcsim，返回共享 client 与清理函数。
// 生产路径每次登录都新建一个 performance.Manager（见 vmware.go 的
// performance.NewManager），govmomi 的计数器缓存挂在 Manager 实例上、不跨
// 抓取复用；因此 benchmark 每轮都必须新建 Manager，否则两组都会命中 govmomi
// 自带的实例内缓存，对照就失真了。
func benchVCSim(b *testing.B) (context.Context, *vim25.Client, func()) {
	b.Helper()

	model := simulator.VPX()
	if err := model.Create(); err != nil {
		b.Fatalf("model.Create: %v", err)
	}
	server := model.Service.NewServer()

	ctx := context.Background()
	client, err := govmomi.NewClient(ctx, server.URL, true)
	if err != nil {
		server.Close()
		model.Remove()
		b.Fatalf("govmomi.NewClient: %v", err)
	}

	cleanup := func() {
		server.Close()
		model.Remove()
	}
	return ctx, client.Client, cleanup
}

// BenchmarkCounterCacheLive 是 P-03 的「前」：复刻生产「每次登录新建
// PerfManager 再裸调 CounterInfoByName」—— 一次 SOAP 往返 + XML 解析 +
// 重建 by-name map。
func BenchmarkCounterCacheLive(b *testing.B) {
	ctx, client, cleanup := benchVCSim(b)
	defer cleanup()

	// sanity：simulator 确实有计数器元数据，否则基准在测空转。
	warm, err := performance.NewManager(client).CounterInfoByName(ctx)
	if err != nil || len(warm) == 0 {
		b.Fatalf("warm-up CounterInfoByName failed: %v (len=%d)", err, len(warm))
	}

	b.ResetTimer()
	var n int
	for i := 0; i < b.N; i++ {
		// 每轮新建 Manager，精确对应每次抓取登录后的状态（govmomi 的实例内
		// 缓存因此不会跨轮复用）。
		m, err := performance.NewManager(client).CounterInfoByName(ctx)
		if err != nil {
			b.Fatalf("CounterInfoByName: %v", err)
		}
		if len(m) == 0 {
			b.Fatal("empty counter map")
		}
		n = len(m)
	}
	b.StopTimer()
	b.ReportMetric(float64(n), "counters")
}

// BenchmarkCounterCacheHit 是 P-03 的「后」稳态：首次预热后，每次登录都在
// TTL 内命中进程级缓存 —— SOAP 往返/解析/建表整段消失，只剩一次加锁读 map。
// now 用真实时钟：TTL 取 10 分钟，benchmark 秒级跑完，期间不会过期。
func BenchmarkCounterCacheHit(b *testing.B) {
	ctx, client, cleanup := benchVCSim(b)
	defer cleanup()

	cache := NewCounterCache()
	const ttl = 10 * time.Minute
	const key = "vcsim|7.0.0|bench-build"

	perf := performance.NewManager(client)
	warm, err := cache.Get(ctx, key, perf, ttl, time.Now())
	if err != nil || len(warm) == 0 {
		b.Fatalf("warm-up cache.Get failed: %v (len=%d)", err, len(warm))
	}

	b.ResetTimer()
	var n int
	for i := 0; i < b.N; i++ {
		// 即便每轮传一个全新 Manager（与生产一致），命中时根本不会触达它。
		m, err := cache.Get(ctx, key, performance.NewManager(client), ttl, time.Now())
		if err != nil {
			b.Fatalf("cache.Get: %v", err)
		}
		if len(m) == 0 {
			b.Fatal("empty counter map")
		}
		n = len(m)
	}
	b.StopTimer()
	b.ReportMetric(float64(n), "counters")
}
