package collector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	"github.com/vmware/govmomi/vim25/mo"
)

// stubLogin 是 Login 接口的测试替身。
//
// 用替身而不是 simulator：这里要测的是 CollectorSet 的调度行为（登录失败
// 怎么处理、并发上限有没有生效），与「能不能连上 vCenter」无关。
// simulator 会把一个调度层的单元测试变成集成测试，还会引入 10 秒级的耗时。
type stubLogin struct {
	scrape  *Scrape
	err     error
	cleaned atomic.Int32
}

func (l *stubLogin) Login(ctx context.Context, target string) (*Scrape, func(), error) {
	// cleanup 必须永不为 nil，与 vmware/api 的契约一致：调用方无条件 defer。
	cleanup := func() { l.cleaned.Add(1) }

	if l.err != nil {
		return nil, cleanup, l.err
	}

	return l.scrape, cleanup, nil
}

// stubCollector 记录自己被调用时看到的 Scrape，并可选地阻塞一段时间。
type stubCollector struct {
	// seen 保存 Update 收到的 *Scrape 指针，供断言 namespace 等注入字段。
	seen atomic.Pointer[Scrape]

	// onUpdate 在 Update 内部调用，用于观测并发峰值或注入错误。
	onUpdate func(ctx context.Context, s *Scrape) error
}

func (c *stubCollector) Update(ctx context.Context, ch chan<- prometheus.Metric, s *Scrape) error {
	c.seen.Store(s)

	if c.onUpdate != nil {
		return c.onUpdate(ctx, s)
	}

	return nil
}

// stubDefinitions 构造 n 个名为 c0..c<n-1> 的默认启用 collector。
func stubDefinitions(n int, onUpdate func(ctx context.Context, s *Scrape) error) ([]Definition, []*stubCollector) {
	defs := make([]Definition, 0, n)
	instances := make([]*stubCollector, 0, n)

	for i := 0; i < n; i++ {
		c := &stubCollector{onUpdate: onUpdate}
		instances = append(instances, c)

		defs = append(defs, Definition{
			Name:           fmt.Sprintf("c%d", i),
			Creator:        func(logger *slog.Logger) (Collector, error) { return c, nil },
			DefaultEnabled: DefaultEnabled,
		})
	}

	return defs, instances
}

// gatherText 通过真实的 prometheus.Registry 采集并渲染成文本格式。
//
// 走 Registry 而不是直接调 Collect 并读 channel：Registry 会做重复指标、
// 标签一致性等校验，而那些校验失败恰恰是这类改动最容易引入的问题。
// 直接读 channel 会跳过全部校验。
func gatherText(t *testing.T, cs *CollectorSet) string {
	t.Helper()

	registry := prometheus.NewRegistry()
	if err := registry.Register(cs); err != nil {
		t.Fatalf("could not register collector set: %s", err)
	}

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather failed: %s", err)
	}

	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))

	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			t.Fatalf("encode failed: %s", err)
		}
	}

	return buf.String()
}

// TestLoginFailureEmitsUpZero 锁住登录失败时的指标产出。
//
// 这是 Stage 9 修掉的框架缺陷之一（pkg/collector/collect.go:15-19：登录失败
// 直接 return）。改动前整轮抓取产出零个指标，Prometheus 收到一份空响应，
// 于是「vCenter 拒绝了凭证」与「exporter 自己挂了」在监控上完全无法区分 ——
// 两者都表现为「没有数据」。
//
// 断言分三部分，缺一不可：
//   - up=0：回答「这个 target 能不能连上」
//   - 每个 collector 的 success=0：回答「哪些采集没跑成」。只发 up=0 会让
//     collector_success 序列凭空消失，依赖它的告警从「触发」变成「无数据」，
//     这两种状态在 Alertmanager 里行为完全不同
//   - cleanup 被调用：登录失败时 api 层可能已经建立了部分资源
func TestLoginFailureEmitsUpZero(t *testing.T) {
	login := &stubLogin{err: errors.New("invalid credentials")}
	defs, _ := stubDefinitions(3, nil)

	cs, err := NewCollectorSet(context.Background(), defs, Options{
		Namespace: "vmware",
		Target:    "vcenter.example.com",
		Login:     login,
		Errors:    NewScrapeErrors(),
	})
	if err != nil {
		t.Fatalf("NewCollectorSet failed: %s", err)
	}

	body := gatherText(t, cs)

	if !strings.Contains(body, "vmware_up 0") {
		t.Errorf("vmware_up 0 was not emitted; body:\n%s", body)
	}

	for _, def := range defs {
		want := `vmware_scrape_collector_success{collector="` + def.Name + `"} 0`
		if !strings.Contains(body, want) {
			t.Errorf("%s was not emitted; body:\n%s", want, body)
		}
	}

	// duration 也必须产出：「抓取有多慢」在失败时反而最需要有数据。
	if !strings.Contains(body, "vmware_scrape_duration_seconds ") {
		t.Errorf("vmware_scrape_duration_seconds was not emitted; body:\n%s", body)
	}

	if got := login.cleaned.Load(); got != 1 {
		t.Errorf("cleanup was called %d times, want 1", got)
	}
}

// TestCollectInjectsNamespaceAndConcurrency 锁住 Options 到 Scrape 的注入。
//
// 这两个字段的注入曾经缺失，后果不对称，所以两条都要断言：
//   - Namespace 为空时 prometheus.BuildFQName("", ...) 产出无 vmware_ 前缀的
//     指标名，整套 dashboard 与告警规则全部失效 —— 这个会被上层测试发现
//   - MaxConcurrency 为零时 HostConcurrency() 静默回落到默认 8，
//     -collector.max-concurrency 对 per-host fan-out 完全无效。这个**不会**
//     让任何测试变红，只是并发预算悄悄不生效 —— 正是这类潜伏缺陷需要
//     在注入点本身设一道断言的原因
func TestCollectInjectsNamespaceAndConcurrency(t *testing.T) {
	login := &stubLogin{scrape: &Scrape{Target: "vcenter.example.com", TargetType: TargetTypeVCenter}}
	defs, instances := stubDefinitions(1, nil)

	cs, err := NewCollectorSet(context.Background(), defs, Options{
		Namespace:      "vmware",
		Login:          login,
		MaxConcurrency: 17,
		Errors:         NewScrapeErrors(),
	})
	if err != nil {
		t.Fatalf("NewCollectorSet failed: %s", err)
	}

	gatherText(t, cs)

	seen := instances[0].seen.Load()
	if seen == nil {
		t.Fatal("collector was never invoked")
	}

	if seen.Namespace != "vmware" {
		t.Errorf("Scrape.Namespace = %q, want %q", seen.Namespace, "vmware")
	}

	if seen.MaxConcurrency != 17 {
		t.Errorf("Scrape.MaxConcurrency = %d, want 17", seen.MaxConcurrency)
	}

	// 17 不等于 defaultHostConcurrency，所以这条断言能分辨「注入生效」与
	// 「回落到默认值」—— 若用 8 做期望值，漏注入也会通过。
	if got := seen.HostConcurrency(); got != 17 {
		t.Errorf("HostConcurrency() = %d, want 17", got)
	}
}

// TestMaxConcurrencyRespected 断言同时运行的 collector 数不超过上限。
//
// 这是框架第四个缺陷的验收：-prom.maxRequests 是死参数（exporter.go:20 存进
// handler.go:16 之后再没被读过），框架用 wg.Add(len(cs.Collectors)) 一次性
// 放出全部 goroutine。而 host / vm 内部各自再 wg.Add(2)，esxcli 还有第三层
// （每主机一个、每网卡再一个）—— 500 主机 × 4 网卡的环境下瞬间 2500 个
// goroutine 同时打同一个 vCenter，是自制的 DoS。
//
// 测法是记录**峰值**并发而不是某一瞬间的并发：后者可能恰好采到低谷，
// 于是一个没有上限的实现也能通过。peak 用 CompareAndSwap 循环更新，
// 保证不会因为两个 goroutine 同时刷新而丢掉更大的值。
//
// 每个 collector 里的 sleep 是必要的：没有它，goroutine 可能在下一个被放出
// 之前就跑完了，于是任何实现的观测并发都是 1，测试失去分辨力。
func TestMaxConcurrencyRespected(t *testing.T) {
	const (
		collectors = 12
		limit      = 3
	)

	var (
		current atomic.Int32
		peak    atomic.Int32
	)

	onUpdate := func(ctx context.Context, s *Scrape) error {
		now := current.Add(1)
		defer current.Add(-1)

		// 单调抬高 peak。用 CAS 循环而不是 Store：两个 goroutine 同时看到
		// 自己是新高时，直接 Store 会让较小的那个覆盖较大的那个。
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}

		time.Sleep(20 * time.Millisecond)

		return nil
	}

	defs, _ := stubDefinitions(collectors, onUpdate)
	login := &stubLogin{scrape: &Scrape{Target: "vcenter.example.com", TargetType: TargetTypeVCenter}}

	cs, err := NewCollectorSet(context.Background(), defs, Options{
		Namespace:      "vmware",
		Login:          login,
		MaxConcurrency: limit,
		Errors:         NewScrapeErrors(),
	})
	if err != nil {
		t.Fatalf("NewCollectorSet failed: %s", err)
	}

	gatherText(t, cs)

	if got := peak.Load(); got > limit {
		t.Errorf("peak concurrency was %d, want <= %d", got, limit)
	}

	// 下界断言不可省：若实现变成串行（比如有人误删了 g.Go 改成直接调用），
	// 上界断言仍然通过，但并发就没了。12 个 collector、上限 3、每个睡 20ms
	// 的情况下，正确实现的峰值必然到 2 以上。
	if got := peak.Load(); got < 2 {
		t.Errorf("peak concurrency was %d, collectors did not run concurrently at all", got)
	}
}

// TestHostsFetchesOnlyOnce 断言同一个 Scrape 上的主机清单只检索一次。
//
// 改动前 host、esxcli.host.nic、esxcli.storage 三个 collector 各自调用
// fetchProperties 检索一遍 HostSystem，三个全开时同一份清单被拉三次。
// 每次检索都包含 ContainerView 的创建与销毁（两次 SOAP 往返）加上属性
// 检索本身，在大规模环境下是可观的浪费。
//
// 并发调用而非顺序调用：三个 collector 在生产里是并发跑的，顺序调用测不出
// sync.Once 是否真的必要 —— 一个用「if s.hosts == nil」实现的版本在顺序
// 调用下也能通过，并发下就会重复检索。
//
// 两处刻意的时序安排，都是为了让竞态**必然**张开而不是碰运气：
//
//  1. release channel 让所有 goroutine 先各自起好、都阻塞在同一点，再一起
//     放出去。不这样做的话第一个 goroutine 往往在最后一个还没启动时就已经
//     跑完了，于是根本不存在并发。
//
//  2. fetch 内部停一小段。非原子实现的竞态窗口是「判断为 nil」到「写入
//     s.hosts」之间，若 fetch 瞬间返回，这个窗口窄到几乎不可能被撞上。
//
// 这不是过度设计：第一版没有这两处，把 sync.Once 换成 if-nil 实现之后
// 重复跑 10 次只有 3 次检出 —— CI 上跑一次的漏过概率是 70%。
func TestHostsFetchesOnlyOnce(t *testing.T) {
	const goroutines = 8

	var calls atomic.Int32

	release := make(chan struct{})

	fetch := func(ctx context.Context, s *Scrape, props []string, out *[]mo.HostSystem) error {
		calls.Add(1)

		// 撑开「判断」与「写入」之间的窗口。真实的 fetch 是 SOAP 往返，
		// 耗时远大于此，所以这更接近生产时序而非人为放大。
		time.Sleep(30 * time.Millisecond)

		*out = []mo.HostSystem{{}, {}}

		return nil
	}

	s := &Scrape{Target: "vcenter.example.com", TargetType: TargetTypeVCenter}

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			<-release

			hosts, err := s.Hosts(context.Background(), fetch)
			if err != nil {
				t.Errorf("Hosts returned error: %s", err)
			}

			if len(hosts) != 2 {
				t.Errorf("Hosts returned %d hosts, want 2", len(hosts))
			}
		}()
	}

	// 给全部 goroutine 到达 <-release 的时间，然后一次性放开。
	time.Sleep(20 * time.Millisecond)
	close(release)

	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("fetcher was called %d times, want 1", got)
	}
}

// TestHostsRemembersError 断言检索失败也只尝试一次。
//
// 若第一个 collector 因为超时拿不到主机清单，后面两个再试只会同样超时，
// 白白多花两次 SOAP 往返 —— 而此时整轮抓取的时间预算大概已经耗尽了。
// 这条与上一条是同一个 sync.Once 的两个分支：只测成功路径的话，一个
// 「出错时不记住、下次重试」的实现也能通过上一条。
func TestHostsRemembersError(t *testing.T) {
	var calls atomic.Int32

	wantErr := errors.New("context deadline exceeded")
	fetch := func(ctx context.Context, s *Scrape, props []string, out *[]mo.HostSystem) error {
		calls.Add(1)

		return wantErr
	}

	s := &Scrape{Target: "vcenter.example.com"}

	for i := 0; i < 3; i++ {
		if _, err := s.Hosts(context.Background(), fetch); !errors.Is(err, wantErr) {
			t.Fatalf("call %d: err = %v, want %v", i, err, wantErr)
		}
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("fetcher was called %d times, want 1", got)
	}
}
