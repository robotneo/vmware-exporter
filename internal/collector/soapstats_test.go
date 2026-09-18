package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/vmware/govmomi/vim25"
)

func TestSOAPRecorderCountsAndPeak(t *testing.T) {
	rt := newFakeRT()
	tr := &throttledRoundTripper{
		next: rt,
		sem:  make(chan struct{}, 2),
		rec:  newSOAPRecorder(),
	}
	close(rt.release) // 所有调用立即返回，不阻塞

	const n = 5
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = tr.RoundTrip(context.Background(), nil, nil) }()
	}
	wg.Wait()

	snap := tr.rec.snapshot()
	if snap.ok != int64(n) || snap.failed != 0 {
		t.Fatalf("ok=%d failed=%d, want ok=%d failed=0", snap.ok, snap.failed, n)
	}
	// 容量 2 的信号量下，5 个并发调用的在飞峰值不可能超过 2。
	if peak := snap.peak; peak < 1 || peak > 2 {
		t.Fatalf("peak inflight = %d, want within [1,2]", peak)
	}
	// 每次经过闸都有一次等待观测（无竞争时落在最小桶附近）。
	if snap.waits != uint64(n) {
		t.Fatalf("wait observations = %d, want %d", snap.waits, n)
	}
	if len(snap.waitBuckets) != len(soapWaitBuckets) {
		t.Fatalf("wait bucket count = %d, want %d", len(snap.waitBuckets), len(soapWaitBuckets))
	}
}

func TestSOAPRecorderSeparatesErrors(t *testing.T) {
	rt := newFakeRT()
	rt.err = errors.New("soap fault")
	tr := &throttledRoundTripper{next: rt, rec: newSOAPRecorder()} // sem=nil：纯透传
	close(rt.release)

	_ = tr.RoundTrip(context.Background(), nil, nil)

	snap := tr.rec.snapshot()
	if snap.ok != 0 || snap.failed != 1 {
		t.Fatalf("ok=%d failed=%d, want ok=0 failed=1", snap.ok, snap.failed)
	}
	if snap.waits != 0 {
		t.Fatalf("passthrough wrapper must not record waits, got %d", snap.waits)
	}
}

// TestSOAPStatsAccumulatesAcrossScrapes 锁住 counter 语义：两轮抓取的请求数
// 必须累加到同一 target 桶，而不是每轮归零（归零会让 rate() 当成 reset）。
func TestSOAPStatsAccumulatesAcrossScrapes(t *testing.T) {
	stats := NewSOAPStats()

	first := stats.Observe("vcenter.example.com", soapScrapeSnapshot{ok: 3, failed: 1, peak: 2, waits: 4, waitSumSecs: 0.01})
	if first.RequestsOK != 3 || first.RequestsFail != 1 {
		t.Fatalf("first view = %+v", first)
	}

	second := stats.Observe("vcenter.example.com", soapScrapeSnapshot{ok: 5, peak: 4, waits: 0})
	if second.RequestsOK != 8 || second.RequestsFail != 1 {
		t.Fatalf("after second scrape requests = ok %v / fail %v, want 8/1",
			second.RequestsOK, second.RequestsFail)
	}
	// 在飞峰值是「最近一轮」快照，不取跨轮 max。
	if second.InflightPeak != 4 {
		t.Fatalf("inflight peak = %v, want latest-scrape value 4", second.InflightPeak)
	}
}

// TestSOAPStatsNormalizesTarget 与 ScrapeErrors 共用同一条规范化路径：
// 大小写变体不能拆成两个桶。
func TestSOAPStatsNormalizesTarget(t *testing.T) {
	stats := NewSOAPStats()

	stats.Observe("VC.Example.com", soapScrapeSnapshot{ok: 1})
	stats.Observe("vc.example.com", soapScrapeSnapshot{ok: 1})

	if got := stats.TargetCount(); got != 1 {
		t.Fatalf("distinct target buckets = %d, want 1", got)
	}

	view := stats.Snapshot("vc.example.com")
	if view.RequestsOK != 2 {
		t.Fatalf("normalized bucket requests = %v, want 2", view.RequestsOK)
	}
}

// TestSOAPStatsSnapshotMissingReturnsZero 登录失败时桶还不存在，Snapshot 必须
// 返回可导出的零值视图而不是 panic / nil map（emitSOAP 会写直方图桶）。
func TestSOAPStatsSnapshotMissingReturnsZero(t *testing.T) {
	view := NewSOAPStats().Snapshot("never-seen.example.com")
	if view.RequestsOK != 0 || view.RequestsFail != 0 || view.InflightPeak != 0 {
		t.Fatalf("missing-target view = %+v, want all zero", view)
	}
	if view.WaitBuckets == nil {
		t.Fatal("WaitBuckets must be a non-nil map for histogram construction")
	}
}

// TestSOAPStatsBoundsDistinctTargets 复现 M-01 的有界化要求：/probe 的 target
// 来自请求，distinct target 集合必须有上限，否则就是无界内存。
func TestSOAPStatsBoundsDistinctTargets(t *testing.T) {
	const limit = 16
	stats := newSOAPStatsWithLimit(limit)

	for i := 0; i < 10000; i++ {
		stats.Observe(fmt.Sprintf("vc-%d.example.com", i), soapScrapeSnapshot{ok: 1})
	}

	if got := stats.TargetCount(); got > limit {
		t.Fatalf("distinct target buckets = %d, must be capped at %d", got, limit)
	}
}

// TestSOAPMetricsEmittedEndToEnd 经真实 Registry 验证三个指标随抓取产出。
// ThrottleSOAP 幂等：RoundTripper 只在第一轮被包装，collector 经它发出的往返
// 被计入本轮 recorder。
func TestSOAPMetricsEmittedEndToEnd(t *testing.T) {
	// collector 在 Update 里经（即将被包装的）RoundTripper 发 3 次往返。
	defs, _ := stubDefinitions(1, func(ctx context.Context, s *Scrape) error {
		for i := 0; i < 3; i++ {
			if err := s.Client.RoundTrip(ctx, nil, nil); err != nil {
				return err
			}
		}
		return nil
	})

	sharedSOAP := NewSOAPStats()

	// 每轮登录都新建一个未包装的 Scrape/Client，复刻生产时序（CollectorSet
	// 在登录成功后调 ThrottleSOAP 装记录层）。
	newSet := func() *CollectorSet {
		t.Helper()
		rt := newFakeRT()
		close(rt.release)
		scrape := &Scrape{
			Client:     &vim25.Client{RoundTripper: rt},
			Target:     "vcenter.example.com",
			TargetType: TargetTypeVCenter,
		}
		cs, err := NewCollectorSet(context.Background(), defs, Options{
			Namespace:      "vmware",
			Target:         "vcenter.example.com",
			Login:          &stubLogin{scrape: scrape},
			MaxConcurrency: 8,
			Errors:         NewScrapeErrors(),
			SOAP:           sharedSOAP,
		})
		if err != nil {
			t.Fatalf("NewCollectorSet failed: %s", err)
		}
		return cs
	}

	body := gatherText(t, newSet())

	// 标签按字母序输出（result 在 vcenter 前）。
	for _, want := range []string{
		`vmware_soap_requests_total{result="ok",vcenter="vcenter.example.com"} 3`,
		`vmware_soap_requests_total{result="error",vcenter="vcenter.example.com"} 0`,
		`vmware_soap_inflight{vcenter="vcenter.example.com"}`,
		`vmware_soap_throttle_wait_seconds_count{vcenter="vcenter.example.com"} 3`,
		`vmware_soap_throttle_wait_seconds_bucket{`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in scrape output:\n%s", want, body)
		}
	}

	// 第二轮：counter 必须跨轮累加到 6（同一份进程级 SOAPStats）。
	second := gatherText(t, newSet())
	if want := `vmware_soap_requests_total{result="ok",vcenter="vcenter.example.com"} 6`; !strings.Contains(second, want) {
		t.Errorf("counter did not accumulate across scrapes; want %s in:\n%s", want, second)
	}
}
