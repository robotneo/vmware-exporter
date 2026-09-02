package collector

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestErrorsTotalAccumulatesAcrossScrapes 是本组测试里唯一不可替代的一个。
//
// 它锁住的不是「失败会不会被计数」，而是「计数会不会在两轮抓取之间归零」。
// 这两件事的区别就是 counter 与 gauge 的区别：
//
// CollectorSet 每个 HTTP 请求构造一个新实例（见 set.go 的类型注释）。如果
// 把计数放进实例字段，单轮内的自增照样正常，任何只跑一轮的测试都会通过 ——
// 而 Prometheus 看到的是一条每次抓取都回到 0 或 1 的序列，rate() 把每次归零
// 当作 counter reset，算出来的速率完全不可用。
//
// 所以这里必须跑两轮，且第二轮的期望值是 2 而不是 1。只跑一轮的测试对这个
// 缺陷完全无感。
func TestErrorsTotalAccumulatesAcrossScrapes(t *testing.T) {
	// 共享的计数器，模拟进程级状态（生产里是 vmware-exporter.go 的
	// 包级 scrapeErrors）。
	shared := NewScrapeErrors()

	newSet := func() *CollectorSet {
		t.Helper()

		defs, _ := stubDefinitions(1, func(ctx context.Context, s *Scrape) error {
			return errors.New("vCenter timed out")
		})

		cs, err := NewCollectorSet(context.Background(), defs, Options{
			Namespace: "vmware",
			Target:    "vcenter.example.com",
			Login:     &stubLogin{scrape: &Scrape{Target: "vcenter.example.com"}},
			Errors:    shared,
		})
		if err != nil {
			t.Fatalf("NewCollectorSet failed: %s", err)
		}

		return cs
	}

	first := gatherText(t, newSet())
	if want := `vmware_scrape_errors_total{collector="c0"} 1`; !strings.Contains(first, want) {
		t.Errorf("after the first scrape, %s was missing; body:\n%s", want, first)
	}

	// 第二轮用一个全新的 CollectorSet，与生产行为一致。
	second := gatherText(t, newSet())
	if want := `vmware_scrape_errors_total{collector="c0"} 2`; !strings.Contains(second, want) {
		t.Errorf("after the second scrape the counter did not accumulate; want %s, body:\n%s", want, second)
	}
}

// TestErrorsTotalSeedsZeroForHealthyCollectors 锁住「从未失败也要导出 0」。
//
// 不导出 0 的后果不是「少一条序列」这么轻：一切正常时序列根本不存在，写好的
// 告警在正常状态下是「无数据」而非「值为 0」；等到第一次失败序列才凭空出现，
// 而 increase() 对一条刚出现的序列算不出增量 —— 第一次故障恰好是漏报的。
//
// 这个测试用「全部成功」的场景，因此如果实现只在 Add 之后才导出，
// 断言会直接失败。
func TestErrorsTotalSeedsZeroForHealthyCollectors(t *testing.T) {
	defs, _ := stubDefinitions(2, nil)

	cs, err := NewCollectorSet(context.Background(), defs, Options{
		Namespace: "vmware",
		Target:    "vcenter.example.com",
		Login:     &stubLogin{scrape: &Scrape{Target: "vcenter.example.com"}},
		Errors:    NewScrapeErrors(),
	})
	if err != nil {
		t.Fatalf("NewCollectorSet failed: %s", err)
	}

	body := gatherText(t, cs)

	// login 也在 seed 里：它是最常见的失败点，同样需要一条恒存在的序列。
	for _, want := range []string{
		`vmware_scrape_errors_total{collector="c0"} 0`,
		`vmware_scrape_errors_total{collector="c1"} 0`,
		`vmware_scrape_errors_total{collector="login"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s was not emitted for a fully healthy scrape; body:\n%s", want, body)
		}
	}
}

// TestErrorsTotalCountsLoginFailure 验证登录失败落进 collector="login" 桶。
//
// 登录不是 collector，但它必须出现在同一个标签维度下 —— 否则「这个 target
// 一小时内失败了几次」这个问题要分两条不同名字的指标去查。标签值与
// collector_duration_seconds{collector="login"} 保持一致，那个用法在框架
// 时代就已经存在。
func TestErrorsTotalCountsLoginFailure(t *testing.T) {
	defs, _ := stubDefinitions(2, nil)

	cs, err := NewCollectorSet(context.Background(), defs, Options{
		Namespace: "vmware",
		Target:    "vcenter.example.com",
		Login:     &stubLogin{err: errors.New("invalid credentials")},
		Errors:    NewScrapeErrors(),
	})
	if err != nil {
		t.Fatalf("NewCollectorSet failed: %s", err)
	}

	body := gatherText(t, cs)

	if want := `vmware_scrape_errors_total{collector="login"} 1`; !strings.Contains(body, want) {
		t.Errorf("%s was not emitted; body:\n%s", want, body)
	}

	// 登录失败时 collector 根本没跑过，所以它们的计数必须还是 0 ——
	// 不能因为「这一轮什么都没采到」就把每个 collector 也记一次错。
	// 那会让一次凭证过期在 errors_total 上表现为 N+1 次故障，
	// 掩盖真正的「某个 collector 单独坏了」。
	for _, name := range []string{"c0", "c1"} {
		want := `vmware_scrape_errors_total{collector="` + name + `"} 0`
		if !strings.Contains(body, want) {
			t.Errorf("%s was not emitted; login failure must not be attributed to collectors; body:\n%s", want, body)
		}
	}
}

// TestErrorsTotalIsPerTarget 验证不同 target 的失败互不串账。
//
// /probe 模式下同一个进程服务多个 vCenter，共用一份 ScrapeErrors。若不按
// target 分桶，A 的凭证错误会抬高 B 的 errors_total，于是针对 B 的告警会在
// B 完全健康时触发 —— 而且查不出原因，因为 B 的 collector_success 全是 1。
func TestErrorsTotalIsPerTarget(t *testing.T) {
	shared := NewScrapeErrors()

	build := func(target string, updateErr error) *CollectorSet {
		t.Helper()

		defs, _ := stubDefinitions(1, func(ctx context.Context, s *Scrape) error { return updateErr })

		cs, err := NewCollectorSet(context.Background(), defs, Options{
			Namespace: "vmware",
			Target:    target,
			Login:     &stubLogin{scrape: &Scrape{Target: target}},
			Errors:    shared,
		})
		if err != nil {
			t.Fatalf("NewCollectorSet failed: %s", err)
		}

		return cs
	}

	// 先让 A 失败三次。
	for i := 0; i < 3; i++ {
		gatherText(t, build("a.example.com", errors.New("boom")))
	}

	// B 从未失败，它的计数必须仍是 0。
	body := gatherText(t, build("b.example.com", nil))

	if want := `vmware_scrape_errors_total{collector="c0"} 0`; !strings.Contains(body, want) {
		t.Errorf("errors from another target leaked into this one; want %s, body:\n%s", want, body)
	}

	// 反过来也要确认 A 的计数确实累到了 3 —— 否则上面的断言可能只是因为
	// 计数根本没工作而通过。
	bodyA := gatherText(t, build("a.example.com", errors.New("boom")))
	if want := `vmware_scrape_errors_total{collector="c0"} 4`; !strings.Contains(bodyA, want) {
		t.Errorf("target A did not accumulate its own errors; want %s, body:\n%s", want, bodyA)
	}
}

// TestNewCollectorSetRequiresErrors 锁住 Errors 是必填项。
//
// 为什么不让它「nil 时自动新建一个」：那样每个请求都会得到一个独立的计数器，
// errors_total 于是每轮从 0 开始 —— 正是 TestErrorsTotalAccumulatesAcrossScrapes
// 要防的缺陷，但形式更隐蔽（没人写错代码，是默认行为在骗人）。
// 宁可让忘记传的调用方在构造时就拿到 error。
func TestNewCollectorSetRequiresErrors(t *testing.T) {
	defs, _ := stubDefinitions(1, nil)

	_, err := NewCollectorSet(context.Background(), defs, Options{
		Namespace: "vmware",
		Login:     &stubLogin{scrape: &Scrape{}},
	})
	if err == nil {
		t.Fatal("NewCollectorSet accepted a nil Errors counter; it must be required")
	}

	if !strings.Contains(err.Error(), "ScrapeErrors") {
		t.Errorf("the error should name the missing field, got: %s", err)
	}
}
