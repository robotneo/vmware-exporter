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
// 这份清单就是 Stage 10b 的改名清单。每一条都必须写明为什么不在 10a 修 ——
// 空着理由的豁免和没有豁免一样没用，半年后没人知道它是「暂缓」还是「认可」。
//
// 清单会被反向校验：豁免了却没触发的条目会让测试失败。理由是这份清单一旦
// 允许留死条目，10b 改完 bytesRx 之后这几行会继续躺在这里，而将来谁再引入
// 一个驼峰指标就有可能被它们无声地放过。
var knownProblems = map[string]string{
	// 这 4 条同源：名字来自 vCenter 的性能计数器名（net.bytesRx.average），
	// perfDesc 只把 "." 换成 "_"，驼峰是 vCenter 那边的原始拼写穿透过来的。
	//
	// 修它等于改指标名，属于破坏性变更 —— 必须走 10b 的双写过渡（旧名与
	// 新名同时导出一个发布周期，dashboards 同步更新，之后由 -metrics.legacy
	// 控制是否保留旧名）。在 10a 直接改会让所有已经照着旧名写好的面板和
	// 告警在一次升级里全部失效。
	"vmware_host_net_bytesRx_average: metric names should be written in 'snake_case' not 'camelCase'": "Stage 10b: 双写过渡后重命名（net.bytesRx.average）",
	"vmware_host_net_bytesTx_average: metric names should be written in 'snake_case' not 'camelCase'": "Stage 10b: 双写过渡后重命名（net.bytesTx.average）",
	"vmware_vm_net_bytesRx_average: metric names should be written in 'snake_case' not 'camelCase'":   "Stage 10b: 双写过渡后重命名（net.bytesRx.average）",
	"vmware_vm_net_bytesTx_average: metric names should be written in 'snake_case' not 'camelCase'":   "Stage 10b: 双写过渡后重命名（net.bytesTx.average）",
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
		"cluster":    NewClusterCollector,
		"datacenter": NewdatacenterCollector,
		"datastore":  NewdatastoreCollector,
		"host":       NewhostCollector,
		"vm":         NewvmCollector,
	}

	names := make([]string, 0, len(creators))
	for name := range creators {
		names = append(names, name)
	}
	sort.Strings(names)

	var unexpected []string
	triggered := make(map[string]bool, len(knownProblems))

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
