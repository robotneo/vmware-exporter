package collector

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestScrapeMetricsPassPromlint 把自监控指标交给 Prometheus 官方的规范检查器。
//
// promlint 检查的是「人写不出来的那类问题」：counter 少 _total 后缀、gauge 多了
// _total 后缀、单位不是基础单位（用 ms 而非 seconds）、help 缺失、指标名带
// 大写或复数。这些问题不会让任何测试失败，也不会让 exporter 报错 —— 只会让
// 下游写 PromQL 的人踩坑，而那时改名已经是破坏性变更了。
//
// 这是全库首次使用 promlint。它的意义在 10a 阶段就是「立一道门」：Stage 10b
// 要批量给指标改名并引入更多 counter，届时每一个改动都会先经过这里。
//
// 只覆盖自监控指标（本包产出的那些）。业务指标由 vmware/collectors 产出，
// 它们的 Desc 在采集时按 vCenter 返回的计数器名动态构造，需要一个活的
// simulator 才能 gather —— 那属于 vmware/collectors 的测试范围。
func TestScrapeMetricsPassPromlint(t *testing.T) {
	// 用会失败的 collector：这样 errors_total 才有非零值，同时
	// collector_success / collector_duration_seconds 也都产出。
	// 全成功的场景下 errors_total 只有 seed 出的 0，覆盖面反而更窄。
	defs, _ := stubDefinitions(2, func(ctx context.Context, s *Scrape) error {
		return context.DeadlineExceeded
	})

	cs, err := NewCollectorSet(context.Background(), defs, Options{
		Namespace: "vmware",
		Target:    "vcenter.example.com",
		Login:     &stubLogin{scrape: &Scrape{Target: "vcenter.example.com"}},
		Errors:    NewScrapeErrors(),
	})
	if err != nil {
		t.Fatalf("NewCollectorSet failed: %s", err)
	}

	problems, err := testutil.CollectAndLint(cs)
	if err != nil {
		t.Fatalf("CollectAndLint failed: %s", err)
	}

	if len(problems) == 0 {
		return
	}

	// 排序后输出，让失败信息在多次运行间稳定 —— gather 的顺序不保证。
	lines := make([]string, 0, len(problems))
	for _, p := range problems {
		lines = append(lines, p.Metric+": "+p.Text)
	}

	sort.Strings(lines)

	t.Errorf("promlint reported %d problem(s):\n%s", len(problems), strings.Join(lines, "\n"))
}
