package vmwareCollectors

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	vsantypes "github.com/vmware/govmomi/vsan/types"
)

// lintAdapter 把 collector.Collector 包成 prometheus.Collector，好让 promlint
// 能对它 gather。
//
// 之所以需要适配器：本包的 collector 签名是 Update(ctx, ch, s) —— ctx 与
// Scrape 都得从外面传进来，而 prometheus.Collector 的 Collect(ch) 没有地方
// 放它们。适配器把这两样捕获成字段。
type lintAdapter struct {
	ctx context.Context
	s   *collector.Scrape
	c   collector.Collector
	t   *testing.T
}

// Describe 故意不发送任何 Desc，也就是把自己声明为 unchecked collector。
//
// 不是偷懒：本包的性能指标 Desc 在 Update 内部按 vCenter 返回的计数器名
// 动态构造（见 descs.go 的 perfDesc），Describe 阶段拿不到完整清单。声明成
// unchecked 之后 registry 不再要求「Collect 产出的指标必须事先声明」，但同名
// 指标之间 help / label 一致性的校验照旧生效 —— 那部分才是这里想要的。
func (a *lintAdapter) Describe(chan<- *prometheus.Desc) {}

func (a *lintAdapter) Collect(ch chan<- prometheus.Metric) {
	if err := a.c.Update(a.ctx, ch, a.s); err != nil {
		a.t.Errorf("Update() returned error: %v", err)
	}
}

// knownProblems 是显式豁免的既有问题，key 为 "<指标名>: <promlint 文本>"。
//
// 每一条都必须写明理由 —— 空着理由的豁免和没有豁免一样没用，半年后没人知道
// 它是「暂缓」还是「认可」。
//
// 清单会被反向校验：豁免了却没触发的条目会让测试失败。**这条校验在 Stage 10b
// 真的起了作用** —— 改名落地的那一刻它立刻报出 4 个死条目并要求删除，而不是
// 让它们留下来无声地放过将来某个新引入的驼峰指标。
//
// 目前为空：10b 的双写改名把 net.bytes{Rx,Tx} 系列的驼峰一并解决了。
var knownProblems = map[string]string{}

// lintMustCover 是必须被 promlint 实际检查到的指标。
//
// 存在的理由是一次真实的假绿：resync 三条指标加进来时，替身与
// hostFetcher 都配好了，promlint 却依然 PASS —— 因为 vcsim 报的
// API 版本是 6.5.0，被 collectResync 的 6.7 门槛挡掉，三条指标压根
// 没被输出。把其中一条故意改成驼峰再跑，测试仍然绿，那道门是空的。
//
// 所以这里列出"容易因为运行时条件不满足而静默缺席"的指标：条件分支
// 后面的、需要替身特定响应才出现的。全量列举没有意义（那等于重写一遍
// descs.go），列举有条件的那些才抓得住这类缺口。
var lintMustCover = []string{
	// resync：要过版本门槛 + 集群内有 poweredOn 主机 + 替身有响应。
	"vmware_vsan_resync_bytes",
	"vmware_vsan_resync_objects",
	"vmware_vsan_resync_recovery_seconds",

	// 盘级：要求替身响应里 Capacity > 0。
	"vmware_vsan_disk_capacity_bytes",
	"vmware_vsan_disk_capacity_used_bytes",
	"vmware_vsan_disk_health",

	// 容量：要求 vSAN 已启用（替身 config 的 Enabled 为 true）。
	"vmware_vsan_capacity_bytes",
	"vmware_vsan_capacity_used_bytes",
	"vmware_vsan_health_status",
}

// metricNameOf 从一条 prometheus.Metric 里取出指标名。
func metricNameOf(t *testing.T, m prometheus.Metric) string {
	t.Helper()

	var pb dto.Metric
	if err := m.Write(&pb); err != nil {
		t.Fatalf("writing metric: %v", err)
	}

	// Desc().String() 形如 `Desc{fqName: "vmware_vsan_resync_bytes", ...}`，
	// 取引号之间的部分。没有更直接的取法 —— Desc 的 fqName 字段未导出。
	desc := m.Desc().String()

	const marker = `fqName: "`
	i := strings.Index(desc, marker)
	if i < 0 {
		t.Fatalf("unexpected Desc format: %s", desc)
	}

	rest := desc[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("unexpected Desc format: %s", desc)
	}

	return rest[:j]
}

// newLintVsanCollector 造一个注入了替身的 vsan collector，专门给 promlint 用。
//
// 为什么不能直接用 NewvsanCollector：那样这道门只会覆盖 9 个 vSAN 指标里的
// 1 个。vcsim 的 vsan/simulator.go 虽然实现了 VsanClusterGetConfig，但返回的
// 是空的 VsanConfigInfoEx（simulator.go:72），其 Enabled 为 nil —— collector
// 会正确地走降级 2 提前返回，只产出 vsan_enabled 0。剩下 8 个指标（含盘级
// 那 3 个 label 最多的）就完全逃过了 promlint 的规范检查。
//
// 这不是钻空子：promlint 检查的是指标名与 help 的静态规范（单位、后缀、
// 驼峰、help 缺失），跟数据是真是假无关。用替身喂一份"全字段都有值"的响应，
// 恰好是让这道门覆盖面最大的做法。
//
// 替身响应刻意包含物理盘且 Capacity > 0 —— 否则 disk_capacity_bytes 与
// disk_capacity_used_bytes 不会被输出（vsan.go 的 Capacity > 0 判断），
// 又少覆盖 2 个。
//
// resync 三条同理需要两个前提：注入的 hostFetcher 给出一台属于被采集
// 集群的 poweredOn 主机，以及 Scrape 的 API 版本达到 6.7。后者由
// TestBusinessMetricsPassPromlint 里的 liftAPIVersion 负责 ——
// **vcsim 的 ServiceContent.About.Version 是 "6.5.0"**
// （simulator/vpx/service_content.go:25），不抬就会被版本门槛挡掉，
// 这三条指标的命名就逃过了 promlint。
func newLintVsanCollector(logger *slog.Logger) (collector.Collector, error) {
	stub := &vsanStub{
		config: vsanEnabledConfig(true),
		space: &vsantypes.VsanQuerySpaceUsageResponse{
			Returnval: vsantypes.VsanSpaceUsage{
				TotalCapacityB: 10 << 40,
				FreeCapacityB:  3 << 40,
			},
		},
		health: vsanHealthResponse("green", []vsantypes.VsanPhysicalDiskHealthSummary{{
			Hostname: "esx1.example.com",
			Disks: []vsantypes.VsanPhysicalDiskHealth{{
				Name:          "naa.disk1",
				Uuid:          "52a1-0001",
				SummaryHealth: "green",
				Capacity:      2 << 40,
				UsedCapacity:  1 << 40,
			}},
		}}),
		resyncByRef: resyncByRefForAnyHost(4<<30, 12, 900),
	}

	return &vsanCollector{
		logger: logger,
		newClient: func(context.Context, *collector.Scrape) (vsanRoundTripper, error) {
			return stub, nil
		},
		// 刻意不注入 hostFetcher：Scrape.Hosts 用 sync.Once，而这个
		// Scrape 在多个 collector 之间共享 —— host collector 先跑就会
		// 用真实 fetcher 填满缓存，之后任何注入都不再被调用。
		// 所以这里走真实检索，让替身按 vcsim 分配的 MoRef 应答。
	}, nil
}

// resyncByRefForAnyHost 让替身对任何 VsanSystemEx MoRef 都给出同一份响应。
//
// 为什么不能按具体 MoRef 建表：vcsim 的主机编号在运行时分配，而
// Scrape.Hosts 的 sync.Once 又让测试无法注入自己的主机清单（见
// newLintVsanCollector 的说明）。这里要的只是"resync 三条指标被输出、
// 从而被 promlint 检查到"，具体查的是哪台主机无关紧要。
//
// 轮询与 MoRef 拼接的正确性由 vsan_test.go 的替身测试保证，那里能
// 精确控制每台主机的成败。
func resyncByRefForAnyHost(bytes, objects, eta int64) map[string]*vsantypes.VsanQuerySyncingVsanObjectsResponse {
	// nil map 在 stub 里会走"查不到"分支，所以用一个哨兵 key 表达
	// "全部命中"。stub 的查表逻辑对此有专门处理。
	return map[string]*vsantypes.VsanQuerySyncingVsanObjectsResponse{
		vsanStubAnyRef: resyncResponse(bytes, objects, eta),
	}
}

// TestBusinessMetricsPassPromlint 把业务指标交给 Prometheus 官方的规范检查器。
//
// 这道门开在这里才有意义。internal/collector 里的那个 lint 测试只覆盖 6 个
// 自监控指标，而 Stage 10b 要批量改名的是本包产出的 50+ 个业务指标 —— 门
// 只开在自监控指标上，对改名毫无约束力。
//
// promlint 抓的是「不会让任何测试失败、也不会让 exporter 报错，只会让下游写
// PromQL 的人踩坑」的那类问题：counter 缺 _total 后缀、单位不是基础单位、
// 名字带驼峰或复数、help 缺失。等到有人已经照着名字写好了告警，改名就成了
// 破坏性变更 —— 所以门要在改名之前立好。
//
// 五个 collector 汇总成一轮判断而不是拆成 subtest：knownProblems 的反向校验
// 需要看到「全部问题」才能判断哪些豁免没被触发，拆成 subtest 后用
// -run .../host 单跑一个就会让其余豁免全部显示为未触发。失败信息里的指标名
// 自带 vmware_host_ / vmware_vm_ 前缀，本来就指明了来源。
func TestBusinessMetricsPassPromlint(t *testing.T) {
	ctx, s, cleanup := setupCollectorScrape(t)
	defer cleanup()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// esxcli 的两个 collector 不在这里：simulator 不实现 esxcli 通道，它们
	// 在这个环境下产不出任何指标，列进来只会让覆盖面看起来更宽。
	creators := map[string]func(*slog.Logger) (collector.Collector, error){
		"cluster":      NewClusterCollector,
		"datacenter":   NewdatacenterCollector,
		"datastore":    NewdatastoreCollector,
		"host":         NewhostCollector,
		"vm":           NewvmCollector,
		"resourcepool": NewresourcepoolCollector,
		"vsan":         newLintVsanCollector,
	}

	names := make([]string, 0, len(creators))
	for name := range creators {
		names = append(names, name)
	}
	sort.Strings(names)

	// vcsim 报的是 6.5.0（simulator/vpx/service_content.go:25），会被
	// resync 的 6.7 门槛挡掉，那三条指标就不会进入 promlint 的视野。
	// 抬到 8.0 让它们被输出 —— 门槛本身在 vsan_test.go 里单独覆盖。
	s.Client.ServiceContent.About.Version = "8.0.3"

	var unexpected []string
	triggered := make(map[string]bool, len(knownProblems))

	// covered 记录 lint 实际看到的指标名。没有它，"某条指标忘了被
	// 替身喂出来"就会表现为静默的覆盖缺口 —— 测试照常绿，而那条指标
	// 的命名从未被检查过。这正是 resync 三条差点掉进去的坑：
	// vcsim 的版本号让它们被门槛挡掉，promlint 一无所知却依然 PASS。
	covered := map[string]bool{}

	for _, name := range names {
		c, err := creators[name](logger)
		if err != nil {
			t.Fatalf("%s constructor returned error: %v", name, err)
		}

		problems, err := testutil.CollectAndLint(&lintAdapter{ctx: ctx, s: s, c: c, t: t})
		if err != nil {
			t.Fatalf("CollectAndLint(%s) returned error: %v", name, err)
		}

		for _, p := range problems {
			key := p.Metric + ": " + p.Text
			if reason, ok := knownProblems[key]; ok {
				triggered[key] = true
				t.Logf("known problem: %s -- %s", key, reason)
				continue
			}
			unexpected = append(unexpected, key)
		}
	}

	// 覆盖面自检：这些指标必须真的被 gather 到，否则上面的 lint 是空转。
	// 单独再 collect 一遍而不是复用 CollectAndLint 的结果 —— 后者只返回
	// 问题清单，没问题的指标不会出现在里面。
	for _, name := range names {
		c, err := creators[name](logger)
		if err != nil {
			t.Fatalf("%s constructor returned error: %v", name, err)
		}

		ch := make(chan prometheus.Metric, 2048)
		if err := c.Update(ctx, ch, s); err != nil {
			t.Fatalf("%s Update returned error: %v", name, err)
		}
		close(ch)

		for m := range ch {
			covered[metricNameOf(t, m)] = true
		}
	}

	for _, want := range lintMustCover {
		if !covered[want] {
			t.Errorf("metric %q was never emitted, so promlint never checked it; "+
				"fix the stub or the scrape setup instead of dropping this assertion", want)
		}
	}

	if len(unexpected) > 0 {
		// 排序后输出：gather 顺序不保证，不排序会让同一个失败在多次运行间
		// 呈现出不同的行序，看起来像是问题本身在变。
		sort.Strings(unexpected)
		t.Errorf("promlint reported %d unexpected problem(s):\n%s",
			len(unexpected), strings.Join(unexpected, "\n"))
	}

	var stale []string
	for key := range knownProblems {
		if !triggered[key] {
			stale = append(stale, key)
		}
	}

	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("knownProblems has %d stale entry(ies) that no longer trigger; "+
			"delete them instead of leaving them to silently excuse future regressions:\n%s",
			len(stale), strings.Join(stale, "\n"))
	}
}
