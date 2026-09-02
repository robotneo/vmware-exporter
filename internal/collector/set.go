package collector

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
)

// loginBucket 是登录阶段在 collector 标签下使用的取值。
//
// 登录不是一个 collector，但它需要出现在 errors_total 与
// collector_duration_seconds 的 collector 标签里 —— 它是最常见的失败点。
// 提成常量是因为这个字符串出现在三处（duration、errors、Snapshot 的 seed），
// 手写三遍时改动一处漏掉另两处不会有任何编译错误，只会让某条序列
// 悄悄用上另一个标签值。
const loginBucket = "login"

// ScrapeMetrics 是自监控指标的描述符集合。
//
// 名称与标签必须与框架产出的完全一致（框架 collector.go:84-96），
// 否则同一套 Prometheus 查询无法跨版本工作。up 与 scrape_duration_seconds
// 是本次新增的，见 newScrapeMetrics 的说明。
type ScrapeMetrics struct {
	up               *prometheus.Desc
	duration         *prometheus.Desc
	collectorSuccess *prometheus.Desc
	collectorSeconds *prometheus.Desc
	errorsTotal      *prometheus.Desc
}

// Login 抽象登录/登出，由 vmware/api 实现。
//
// 返回 *Scrape 而非 map[string]any：这是把 13 个字符串 key 的运行期契约
// 换成编译期契约的另一半。
type Login interface {
	// Login 建立会话。返回的 cleanup 必须在抓取结束后调用，无论成功与否。
	Login(ctx context.Context, target string) (s *Scrape, cleanup func(), err error)
}

func newScrapeMetrics(namespace string) ScrapeMetrics {
	return ScrapeMetrics{
		// up 是本次新增。改动前登录失败会让整轮抓取产出零个指标
		// （框架 collect.go:15-19 直接 return），Prometheus 收到一份空响应，
		// 无法区分「target 挂了」与「exporter 没配好」。
		//
		// 注意这不是 Prometheus 自己生成的那个 up —— 那个只表示「HTTP 请求
		// 成功」。对多 target exporter 来说 HTTP 成功而 vCenter 登录失败是
		// 常态，所以需要一个表示「后端是否可达」的独立指标。
		up: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "up"),
			"Whether the vCenter or ESXi target could be logged into. 0 means the scrape produced no inventory data.",
			nil,
			nil,
		),

		// scrape_duration_seconds 是 exporter 的标准自监控指标名。
		// 框架只有带 collector="all_collectors" 标签的版本，那不是通行写法 ——
		// 通用的 exporter 告警规则查的是无标签的 <namespace>_scrape_duration_seconds。
		duration: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "scrape", "duration_seconds"),
			"Total duration of the last scrape, including login and logout.",
			nil,
			nil,
		),

		// 以下两个的名称与标签必须与框架逐字一致（框架 collector.go:84-96），
		// 既有的 dashboard 与告警规则直接引用它们。
		collectorSeconds: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "scrape", "collector_duration_seconds"),
			"Duration of a collector scrape.",
			[]string{"collector"},
			nil,
		),

		collectorSuccess: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "scrape", "collector_success"),
			"Whether a collector succeeded.",
			[]string{"collector"},
			nil,
		),

		// errors_total 是全库第一个 counter。此前 45+ 个指标全是 gauge，
		// 包括 collector_success —— 而 success 只能回答「最近一轮成不成」，
		// 答不出「过去一小时失败了几次」。间歇性故障（vCenter 偶发超时）
		// 在 gauge 上表现为抓取之间的抖动，Prometheus 按 scrape_interval
		// 取样，两次采样之间的失败完全看不见；counter 不会漏。
		//
		// collector="login" 是登录失败的桶。登录不属于任何 collector，
		// 但它是最需要计数的失败点，且这个标签值与
		// collector_duration_seconds{collector="login"} 已有的用法一致。
		errorsTotal: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "scrape", "errors_total"),
			"Total number of scrape errors, by collector. The value \"login\" covers failures to authenticate against the target.",
			[]string{"collector"},
			nil,
		),
	}
}

// CollectorSet 调度一轮抓取，并实现 prometheus.Collector。
//
// 每个请求构造一个新的实例。ctx 是字段而不是 Collect 的参数，因为
// prometheus.Collector 接口的 Collect(ch) 签名固定，无法加参数 —— 这是
// client_golang 的既定形状，node_exporter 等官方 exporter 也是同样处理。
// 关键在于实例的生命周期严格等于一次请求，所以字段里的 ctx 不会被复用。
type CollectorSet struct {
	// ctx 来自 http.Request.Context()，客户端断连或 Prometheus 抓取超时
	// 都会让它被取消，进而取消所有正在进行的 vCenter 调用。
	ctx context.Context

	login      Login
	collectors map[string]Collector
	target     string
	logger     *slog.Logger
	metrics    ScrapeMetrics

	// namespace 与 maxConcurrency 保留原始值，而不是只用它们构造出
	// metrics 就丢掉：两者都要在登录成功后写进 *Scrape 交给 collector。
	// 见 Collect 里的说明。
	namespace      string
	maxConcurrency int

	// errors 是跨请求共享的错误计数器，由调用方持有并注入。
	// 不能是本结构体拥有的状态 —— 见 errors.go 顶部关于「每请求一实例」
	// 与 counter 单调性的说明。
	errors *ScrapeErrors
}

// Options 是构造 CollectorSet 所需的参数。
//
// 用结构体而非一长串位置参数：框架的 NewCollectorSet 有 4 个参数，其中
// namespace 与 target 都是 string，传反了不会编译报错。
type Options struct {
	Namespace string
	Target    string
	Login     Login
	Logger    *slog.Logger

	// Enabled 是本次显式指定的开关，未出现的名字回退到 Definition.DefaultEnabled。
	// nil 表示全部走默认值。
	Enabled map[string]bool

	// MaxConcurrency 是同时运行的 collector 上限。<= 0 表示不限制。
	MaxConcurrency int

	// Errors 是跨请求累积的错误计数器。必填。
	//
	// 必填而不是「nil 时自动新建一个」：自动新建会让每个请求得到一个
	// 独立的计数器，于是 errors_total 每轮都从 0 开始 —— 一个坏得很
	// 安静的 counter，测试与人眼都不容易发现。宁可让忘记传的调用方
	// 在构造时就拿到 error。
	Errors *ScrapeErrors
}

// NewCollectorSet 按 definitions 与 opts.Enabled 构造本轮要运行的 collector 集合。
func NewCollectorSet(ctx context.Context, definitions []Definition, opts Options) (*CollectorSet, error) {
	if opts.Login == nil {
		return nil, fmt.Errorf("collector set requires a Login implementation")
	}

	if opts.Errors == nil {
		return nil, fmt.Errorf("collector set requires a ScrapeErrors counter")
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	collectors := make(map[string]Collector)

	for _, def := range definitions {
		enabled := def.DefaultEnabled
		if explicit, ok := opts.Enabled[def.Name]; ok {
			enabled = explicit
		}

		if !enabled {
			logger.Debug("collector disabled", "name", def.Name)
			continue
		}

		instance, err := def.Creator(logger.With("collector", def.Name))
		if err != nil {
			// 构造失败必须让整个请求失败，而不是跳过这个 collector。
			// 静默跳过会让 Prometheus 收到一份看起来正常、实际缺了一部分
			// 数据的响应 —— 那比明确的 500 难排查得多。
			return nil, fmt.Errorf("could not create collector %q: %w", def.Name, err)
		}

		logger.Debug("collector enabled", "name", def.Name)

		collectors[def.Name] = instance
	}

	return &CollectorSet{
		ctx:            ctx,
		login:          opts.Login,
		collectors:     collectors,
		target:         opts.Target,
		namespace:      opts.Namespace,
		maxConcurrency: opts.MaxConcurrency,
		logger:         logger,
		metrics:        newScrapeMetrics(opts.Namespace),
		errors:         opts.Errors,
	}, nil
}

// Names 返回本轮启用的 collector 名字，已排序。
func (cs *CollectorSet) Names() []string {
	names := make([]string, 0, len(cs.collectors))
	for name := range cs.collectors {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// Describe 实现 prometheus.Collector。
//
// 只描述固定的自监控指标。业务指标的 Desc 在各 collector 内部按采集结果
// 动态构造，无法预先枚举 —— 这会让 registry 把本 collector 视为
// unchecked collector 并跳过重复注册检测，是刻意的取舍。
func (cs *CollectorSet) Describe(ch chan<- *prometheus.Desc) {
	ch <- cs.metrics.up
	ch <- cs.metrics.duration
	ch <- cs.metrics.collectorSeconds
	ch <- cs.metrics.collectorSuccess
	ch <- cs.metrics.errorsTotal
}

// Collect 实现 prometheus.Collector：登录、并发跑所有 collector、登出。
//
// 相对框架 Collect（pkg/collector/collect.go）的三处行为修正：
//
//  1. 登录失败时产出 up=0 并为每个启用的 collector 产出 success=0，
//     而不是直接 return 一份空响应。区别在于告警能不能写出来。
//
//  2. 所有 collector 共享一个从 http.Request 派生的 context。客户端断连
//     或抓取超时会真正取消上游的 vCenter 调用，而不是让它们跑到自然结束。
//
//  3. 并发有上限。框架用 wg.Add(len(cs.Collectors)) 一次性放出全部
//     goroutine，而 host / vm 内部各自再 wg.Add(2)，esxcli 还有第三层
//     （每主机一个、每网卡再一个）。500 主机 × 4 网卡的环境下瞬间
//     2500 个 goroutine 同时打同一个 vCenter，是自制的 DoS。
func (cs *CollectorSet) Collect(ch chan<- prometheus.Metric) {
	begin := time.Now()

	// duration 无论走哪条路径都要产出，否则「抓取有多慢」这个问题在失败
	// 的情况下反而没有数据 —— 而那正是最需要它的时候。
	//
	// errors_total 同样放在这里，且必须放在这个 defer 而不是散布在各条
	// 返回路径上：它是本函数注册的第一个 defer，所以 LIFO 下它最后执行,
	// 此时无论走登录失败分支还是走完 g.Wait()，所有 Add 都已经发生。
	// 换成「每条 return 之前手写一次」则新增一条提前返回就会漏掉。
	defer func() {
		ch <- prometheus.MustNewConstMetric(cs.metrics.duration, prometheus.GaugeValue, time.Since(begin).Seconds())

		cs.emitErrors(ch)
	}()

	loginBegin := time.Now()

	s, cleanup, err := cs.login.Login(cs.ctx, cs.target)
	if cleanup != nil {
		defer cleanup()
	}

	ch <- prometheus.MustNewConstMetric(cs.metrics.collectorSeconds, prometheus.GaugeValue,
		time.Since(loginBegin).Seconds(), loginBucket)

	if err != nil {
		cs.logger.Error("login failed", "target", cs.target, "error", err)

		cs.errors.Add(cs.target, loginBucket)

		// up=0 加上每个 collector 的 success=0。两者都需要：up 回答
		// 「这个 target 能不能连上」，success 回答「哪些采集没跑成」。
		// 只发 up=0 会让 collector_success 序列凭空消失，依赖它的告警
		// 从「触发」变成「无数据」，这两种状态在 Alertmanager 里的行为
		// 完全不同。
		ch <- prometheus.MustNewConstMetric(cs.metrics.up, prometheus.GaugeValue, 0)

		for name := range cs.collectors {
			ch <- prometheus.MustNewConstMetric(cs.metrics.collectorSuccess, prometheus.GaugeValue, 0, name)
		}

		return
	}

	ch <- prometheus.MustNewConstMetric(cs.metrics.up, prometheus.GaugeValue, 1)

	// namespace 与 maxConcurrency 由 CollectorSet 注入，而不是由 Login 的
	// 实现填写。
	//
	// 这不只是分工问题：vmware/api 是「怎么连上 vCenter」，指标前缀与并发
	// 预算是「这个进程怎么配置的」，是根包 flag 的产物。让 api 包去填就得
	// 把两个 flag 的值传进 Login —— 于是 api 包多了两个跟登录无关的入参，
	// 而 /metrics 与 /probe 两条路径都得各自记得传对。放在这里则只有一处，
	// 且 Options 已经是这两个值的唯一来源。
	//
	// 时序上必须在 Login 之后：Scrape 是登录的产物，登录前还不存在。
	// 也必须在 g.Go 之前：一旦 collector 起跑，写字段就与 Update 里的读
	// 构成数据竞争。这两个约束把注入点唯一地钉在这里。
	s.Namespace = cs.namespace
	s.MaxConcurrency = cs.maxConcurrency

	cs.logger.Debug("login successful", "target", cs.target, "target_type", s.TargetType,
		"collectors", len(cs.collectors))

	// errgroup 而非裸 WaitGroup，为的是 SetLimit。这里刻意不用
	// errgroup.WithContext：那个变体会在首个 collector 出错时取消 ctx，
	// 于是一个 collector 的失败会连带掐断其他所有 collector。对 exporter
	// 来说这是错的 —— 部分数据远好过没有数据，而每个 collector 的成败
	// 已经由各自的 success 指标独立表达了。
	g := &errgroup.Group{}
	if cs.maxConcurrency > 0 {
		g.SetLimit(cs.maxConcurrency)
	}

	for name, c := range cs.collectors {
		g.Go(func() error {
			collectorBegin := time.Now()

			err := c.Update(cs.ctx, ch, s)

			duration := time.Since(collectorBegin)

			success := float64(1)
			if err != nil {
				success = 0
				cs.errors.Add(cs.target, name)
				cs.logger.Error("collector failed", "collector", name,
					"duration_seconds", duration.Seconds(), "error", err)
			} else {
				cs.logger.Debug("collector scraped successfully", "collector", name,
					"duration_seconds", duration.Seconds())
			}

			ch <- prometheus.MustNewConstMetric(cs.metrics.collectorSeconds, prometheus.GaugeValue,
				duration.Seconds(), name)
			ch <- prometheus.MustNewConstMetric(cs.metrics.collectorSuccess, prometheus.GaugeValue,
				success, name)

			// 永远返回 nil：collector 的成败已经由 success 指标表达，
			// 让它冒泡到 g.Wait() 没有任何额外用处，反而会在将来有人
			// 改用 WithContext 时变成「一个失败拖垮全部」。
			return nil
		})
	}

	// 忽略返回值是安全的：上面的闭包永远返回 nil。
	_ = g.Wait()

	// all_collectors 标签沿用框架的取值，既有 dashboard 有引用。
	// 它与新增的无标签 scrape_duration_seconds 的差别是不含 login/logout。
	ch <- prometheus.MustNewConstMetric(cs.metrics.collectorSeconds, prometheus.GaugeValue,
		time.Since(begin).Seconds(), "all_collectors")
}

// emitErrors 导出本 target 下的累计错误数。
//
// seed 覆盖「所有启用的 collector + login」，而不只是本轮真的出过错的那些。
// 理由见 ScrapeErrors.Snapshot 的说明：不导出 0 会让正常状态下的序列缺失，
// 于是第一次故障反而是漏报的 —— increase() 对一条刚出现的序列算不出增量。
func (cs *CollectorSet) emitErrors(ch chan<- prometheus.Metric) {
	seed := make([]string, 0, len(cs.collectors)+1)
	seed = append(seed, loginBucket)
	seed = append(seed, cs.Names()...)

	for name, count := range cs.errors.Snapshot(cs.target, seed) {
		// CounterValue 而不是 GaugeValue。这是全库唯一一处 —— 类型选错
		// 不会有任何报错，只会让 promtool 的 lint 与 rate() 的语义悄悄失效。
		ch <- prometheus.MustNewConstMetric(cs.metrics.errorsTotal, prometheus.CounterValue, count, name)
	}
}
