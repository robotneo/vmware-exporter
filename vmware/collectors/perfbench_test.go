package vmwareCollectors

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
)

// 本文件是性能批次 C/D 的「前后基准」工具（计划 0 节与批次 C 的合并门槛要求先有
// 可对比的基准数字）。普通 `go test` 不会运行 Benchmark；用：
//
//	go test ./vmware/collectors/ -run '^$' -bench 'VMPerf' \
//	    -benchmem -benchtime=20x
//
// vcsim 模型规模在这里集中定义，前后两次跑必须用同一个数才可比。
//
// P-03（计数器元数据缓存）的基准不在这里：govmomi 的计数器缓存挂在
// performance.Manager 实例上，而生产每次登录都新建 Manager。要真实复刻
// 「每次登录」必须每轮新建 Manager，因此 P-03 的 live/hit 对照放在
// vmware/api/countercache_bench_test.go（同包能直接构造 Manager 与 Cache）。
const (
	benchDatacenters = 1
	benchHosts       = 1
	benchClusters    = 0 // standalone host 即可承载大量 VM，减少模型创建噪声
	benchClusterHost = 0
	benchDatastores  = 1
	benchVMs         = 500
)

func benchModel() *simulator.Model {
	m := simulator.VPX()
	m.Datacenter = benchDatacenters
	m.Host = benchHosts
	m.Cluster = benchClusters
	m.ClusterHost = benchClusterHost
	m.Datastore = benchDatastores
	m.Machine = benchVMs
	return m
}

func benchLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// poweredOnVMRefs 复刻 vm.Update 里 perf 引用的构造：只保留 poweredOn 的 VM，
// 返回与生产一致的 (refs, names)。在 benchmark 计时区外只跑一次。
func poweredOnVMRefs(b *testing.B, ctx context.Context, s *collector.Scrape) ([]types.ManagedObjectReference, map[string]string) {
	b.Helper()

	var vms []mo.VirtualMachine
	if err := fetchProperties(ctx, s.View, s.Client,
		[]string{"VirtualMachine"}, []string{"summary", "runtime"}, &vms, benchLogger()); err != nil {
		b.Fatalf("fetchProperties: %v", err)
	}

	refs := make([]types.ManagedObjectReference, 0, len(vms))
	names := make(map[string]string, len(vms))
	for _, vm := range vms {
		if vm.Runtime.PowerState != types.VirtualMachinePowerStatePoweredOn {
			continue
		}
		refs = append(refs, vm.Self)
		names[vm.Self.Value] = vm.Summary.Config.Name
	}
	if len(refs) == 0 {
		b.Fatal("benchmark model produced zero powered-on VMs")
	}
	return refs, names
}

// BenchmarkVMPerfScrape 量化 P-08b：一次大 VM 集合的 perf 分块查询 + 合并 +
// 转换 + 发指标的耗时与分配。channel 用足够大的缓冲，避免消费侧成为瓶颈。
func BenchmarkVMPerfScrape(b *testing.B) {
	ctx, s, cleanup := setupCollectorScrapeWithModel(b, benchModel())
	defer cleanup()

	refs, names := poweredOnVMRefs(b, ctx, s)
	b.ReportMetric(float64(len(refs)), "vms")

	// 每轮一个干净的大缓冲 channel：500 VM × 13 common 计数器 × 1 样本。
	const maxMetrics = 1 << 18

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ch := make(chan prometheus.Metric, maxMetrics)
		scrapePerformance(ctx, ch, benchLogger(), s.Samples, 20, s.Perf,
			s.Target, "VirtualMachine", s.Namespace, vmSubsystem, "", cVMCounters,
			s.Counters, refs, names, 64, 4)
		// 必须排空：scrapePerformance 在同一调用栈向 ch 发指标，丢弃引用让 GC
		// 行为跨轮一致。
		var emitted int
		for {
			select {
			case <-ch:
				emitted++
			default:
				goto done
			}
		}
	done:
		if i == 0 {
			b.ReportMetric(float64(emitted), "metrics/iter")
		}
	}
}
