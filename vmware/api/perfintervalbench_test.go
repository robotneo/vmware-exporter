package vmware

// 采样间隔协商缓存（ProviderSummary）的前后基准。
//
// 运行：
//
//	go test ./vmware/api/ -run '^$' \
//	  -bench 'ProviderSummary' -benchmem -benchtime=50x -count=2
//
// 场景复刻生产：每轮抓取都重新登录（新建 Manager），登录后对 HostSystem、
// VirtualMachine、Datastore 三类各协商一次采样间隔。
//   - Live   = NewAPI()，s.Perf.ProviderSummary 各发一次真实 SOAP（3 次/轮）
//   - Cached = NewAPIWithCaches(同一进程级缓存)，s.ProviderSummary 按
//     target+版本+entity.Type 命中（仅第一轮 miss，后续 0 次协商 SOAP）
//
// 两轮都包含一次完整 Login/Logout，Login 开销相同；差异只在 3 次协商往返。

import (
	"context"
	"testing"
	"time"

	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/types"
)

func setupPerfIntervalBench(b *testing.B) (string, func()) {
	b.Helper()

	model := simulator.VPX()
	if err := model.Create(); err != nil {
		b.Fatalf("create model: %v", err)
	}

	server := model.Service.NewServer()

	creds := Credentials{
		Username: "user",
		Password: "pass",
		Target:   server.URL.Host,
		Schema:   server.URL.Scheme,
		Insecure: true,
	}

	// 先登录一次拿三类真实 ref（计时区外）。
	s, cleanup, err := NewAPI().LoginWithCredentials(context.Background(), creds, discardLogger())
	if err != nil {
		b.Fatalf("probe login: %v", err)
	}

	refsByType := func(moType string) types.ManagedObjectReference {
		mgr := view.NewManager(s.Client)
		cv, err := mgr.CreateContainerView(context.Background(), s.Client.ServiceContent.RootFolder, []string{moType}, true)
		if err != nil {
			b.Fatalf("view: %v", err)
		}
		defer func() { _ = cv.Destroy(context.Background()) }()
		refs, err := cv.Find(context.Background(), []string{moType}, nil)
		if err != nil || len(refs) == 0 {
			b.Fatalf("find %s: %v (refs=%d)", moType, err, len(refs))
		}
		return refs[0]
	}

	host := refsByType("HostSystem")
	vm := refsByType("VirtualMachine")
	ds := refsByType("Datastore")
	cleanup()

	benchRefsList = []types.ManagedObjectReference{host, vm, ds}
	benchCreds = creds

	return server.URL.Host, func() {
		server.Close()
		model.Remove()
	}
}

var (
	benchRefsList []types.ManagedObjectReference
	benchCreds    Credentials
)

// Live 基线刻意只开「已交付的 CounterCache」、不开本批的 ProviderSummary
// 缓存：这样两轮的差异被精确隔离到三次采样间隔协商往返，不重复计入 P-03 的
// 计数器元数据缓存收益（否则数字会夸大本批贡献）。
func BenchmarkProviderSummaryLive(b *testing.B) {
	restoreVMwareFlags(b)
	*vmwInterval = 20
	*vmGranularity = 20
	*vmwTimeout = 60

	_, cleanup := setupPerfIntervalBench(b)
	defer cleanup()

	counter := NewCounterCache() // P-03 已交付，基线也开
	ctx := context.Background()
	b.ReportMetric(3, "providers/iter")
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		s, end, err := NewAPIWithCaches(counter, nil).LoginWithCredentials(ctx, benchCreds, discardLogger())
		if err != nil {
			b.Fatalf("login: %v", err)
		}
		// provider 缓存为 nil：三类各实时发一次 QueryPerfProviderSummary SOAP。
		for _, ref := range benchRefsList {
			if _, err := s.Perf.ProviderSummary(ctx, ref); err != nil {
				b.Fatalf("live ProviderSummary: %v", err)
			}
		}
		end()
	}
}

func BenchmarkProviderSummaryCached(b *testing.B) {
	restoreVMwareFlags(b)
	*vmwInterval = 20
	*vmGranularity = 20
	*vmwTimeout = 60
	*vmwProviderCacheTTL = 10 * time.Minute

	_, cleanup := setupPerfIntervalBench(b)
	defer cleanup()

	// 进程级缓存在所有"轮次/登录"间共享 —— 这正是命中的关键（Manager 每轮新建）。
	counter := NewCounterCache()
	provider := NewProviderSummaryCache()

	ctx := context.Background()
	b.ReportMetric(3, "providers/iter")
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		s, end, err := NewAPIWithCaches(counter, provider).LoginWithCredentials(ctx, benchCreds, discardLogger())
		if err != nil {
			b.Fatalf("login: %v", err)
		}
		if s.ProviderSummary == nil {
			b.Fatal("expected the summary closure to be wired")
		}
		// 经闭包协商：仅每种类型第一次 miss，之后命中进程级缓存，零协商 SOAP。
		for _, ref := range benchRefsList {
			if _, err := s.ProviderSummary(ctx, ref); err != nil {
				b.Fatalf("cached ProviderSummary: %v", err)
			}
		}
		end()
	}
}
