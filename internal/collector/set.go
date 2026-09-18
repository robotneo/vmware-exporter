package collector

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
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

// SOAP 请求结果标签取值（vmware_soap_requests_total{result=...}）。
const (
	resultLabelOK    = "ok"
	resultLabelError = "error"
)

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
	entitiesFound    *prometheus.Desc
	entitiesEmitted  *prometheus.Desc
	entitiesSkipped  *prometheus.Desc
	soapRequests     *prometheus.Desc
	soapInflight     *prometheus.Desc
	soapWaitSeconds  *prometheus.Desc
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

		// 以下三个是实体级的本轮快照（gauge，不是 counter）：一个 collector
		// 一轮只上报一次，实体数随环境变化而上下波动，套 rate() 没有意义。
		// 因此名字刻意**不带 _total** —— promlint 禁止非 counter 使用此后缀，
		// 而 DESIGN-p1-p3-roadmap 3.4 草案里的 *_total 命名会被门拦下。
		//
		// 它们带 vcenter label，与 success/duration 不同：后者描述「这次
		// 抓取动作本身」，由 HTTP target 即可定位；实体计数要回答「这个
		// vCenter 上有多少 VM 被跳过」，/probe 多租户下必须自带后端标识。
		entitiesFound: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "scrape", "entities_found"),
			"Number of entities discovered in the inventory during the last scrape, by collector and entity kind.",
			[]string{"vcenter", "collector", "kind"},
			nil,
		),

		entitiesEmitted: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "scrape", "entities_emitted"),
			"Number of entities for which data-plane metrics (performance counters, esxcli) were emitted during the last scrape.",
			[]string{"vcenter", "collector", "kind"},
			nil,
		),

		// 一个实体可能同时命中多个跳过原因（维护中断连），所以各 reason
		// 之和可能大于 found-emitted，help 里说明这一点以免用户拿它对账。
		entitiesSkipped: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "scrape", "entities_skipped"),
			"Number of entities skipped by a data-plane metric during the last scrape, by reason. "+
				"An entity can be counted under more than one reason, so summing across reasons can exceed the number of skipped entities.",
			[]string{"vcenter", "collector", "kind", "reason"},
			nil,
		),

		// 以下三个是 SOAP 通道自监控（P-09）。前两个（请求计数、等闸耗时）是
		// 跨轮累计的 counter/histogram，用来量化每轮给 vCenter 的往返压力与
		// 并发闸排队成本；inflight 是「最近一轮在飞峰值」的 gauge —— 采集
		// 时刻在飞数恒为 0，只有峰值对容量规划有意义。
		soapRequests: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "soap_requests_total"),
			"Total number of SOAP round trips issued to the target, by result. \"error\" covers transport and SOAP fault failures; requests that never acquired the concurrency token are not counted.",
			[]string{"vcenter", "result"},
			nil,
		),

		soapInflight: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "soap_inflight"),
			"Peak number of SOAP round trips simultaneously in flight during the last scrape. Capped by -collector.max-concurrency when that is greater than zero.",
			[]string{"vcenter"},
			nil,
		),

		soapWaitSeconds: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "", "soap_throttle_wait_seconds"),
			"Time spent waiting for the SOAP concurrency limiter token before a round trip. Only populated when -collector.max-concurrency is greater than zero.",
			[]string{"vcenter"},
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
	inventoryCache *InventoryCache
	inventoryTTL   time.Duration

	// errors 是跨请求共享的错误计数器，由调用方持有并注入。
	// 不能是本结构体拥有的状态 —— 见 errors.go 顶部关于「每请求一实例」
	// 与 counter 单调性的说明。
	errors *ScrapeErrors

	// soap 是跨请求共享的 SOAP 往返统计，同理必须在请求外持有：请求计数是
	// counter，放进每请求新建的 CollectorSet 会每轮归零（P-09）。
	soap *SOAPStats
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

	// InventoryCache 与 InventoryTTL 配置拓扑/容量类清单检索的进程级缓存。
	//
	// 只应由使用单一服务级凭证的 /metrics 路径注入；多租户 /probe 路径必须
	// 留 nil（InventoryCache=nil 即完全旁路缓存），否则一个凭证拉回的对象
	// 清单会被另一个凭证的请求读到 —— 那是越权读。InventoryTTL<=0 时即便
	// 传了缓存实例也一律实时检索。
	InventoryCache *InventoryCache
	InventoryTTL   time.Duration

	// Errors 是跨请求累积的错误计数器。必填。
	//
	// 必填而不是「nil 时自动新建一个」：自动新建会让每个请求得到一个
	// 独立的计数器，于是 errors_total 每轮都从 0 开始 —— 一个坏得很
	// 安静的 counter，测试与人眼都不容易发现。宁可让忘记传的调用方
	// 在构造时就拿到 error。
	Errors *ScrapeErrors

	// SOAP 是跨请求累积的 SOAP 往返统计（P-09）。必填，理由同 Errors。
	SOAP *SOAPStats
}

// NewCollectorSet 按 definitions 与 opts.Enabled 构造本轮要运行的 collector 集合。
func NewCollectorSet(ctx context.Context, definitions []Definition, opts Options) (*CollectorSet, error) {
	if opts.Login == nil {
		return nil, fmt.Errorf("collector set requires a Login implementation")
	}

	if opts.Errors == nil {
		return nil, fmt.Errorf("collector set requires a ScrapeErrors counter")
	}

	if opts.SOAP == nil {
		return nil, fmt.Errorf("collector set requires a SOAPStats counter")
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
		inventoryCache: opts.InventoryCache,
		inventoryTTL:   opts.InventoryTTL,
		logger:         logger,
		metrics:        newScrapeMetrics(opts.Namespace),
		errors:         opts.Errors,
		soap:           opts.SOAP,
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
	ch <- cs.metrics.entitiesFound
	ch <- cs.metrics.entitiesEmitted
	ch <- cs.metrics.entitiesSkipped
	ch <- cs.metrics.soapRequests
	ch <- cs.metrics.soapInflight
	ch <- cs.metrics.soapWaitSeconds
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

	// s 在 defer 之前声明：SOAP 统计的 flush 放在最外层 defer 里，而该 defer
	// 是本函数第一个注册的，LIFO 下**最后**执行 —— 那时 cleanup（Logout）已经
	// 跑完，登出这次往返也已计入 recorder。登录失败或未开始时 s 为 nil，
	// emitSOAP 仍导出零值/累计序列，保证序列不缺失。
	var s *Scrape
	var err error

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
		cs.emitSOAP(ch, s)
	}()

	loginBegin := time.Now()

	var cleanup func()
	s, cleanup, err = cs.login.Login(cs.ctx, cs.target)
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
	s.InventoryCache = cs.inventoryCache
	s.InventoryTTL = cs.inventoryTTL

	// 实体统计是本轮快照：每个请求新建一个 EntityStats，随 Scrape 传给
	// 并发运行的 collector，g.Wait() 之后再统一输出（见方法尾部）。
	// 时序与上面的字段注入相同 —— 必须在 g.Go 之前赋值，否则 collector
	// 并发上报与这里的写入构成数据竞争。
	entityStats := NewEntityStats()
	s.entityStats = entityStats

	// 在登录成功、任何 collector 起跑之前，给本轮抓取的 SOAP 通道装一个
	// 全局闸。它与 collector/主机层的 SetLimit 不同：闸放在 vim25 client 的
	// RoundTripper 上，令牌只包住单次网络往返，因此把 esxcli "每主机 × 每网卡"
	// 这种嵌套 fan-out 真正同时在飞的请求总数钉死在 maxConcurrency，而不是
	// 两层各自 SetLimit(n) 后最坏仍有 n×n 个并发。详见 throttle.go。
	//
	// 时序与上面两个字段相同的约束：必须在 g.Go 之前，否则安装写入与
	// Update 里的并发读取构成数据竞争；必须在 Login 之后，client 是登录产物。
	s.ThrottleSOAP(cs.maxConcurrency)

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

			err := safeUpdate(cs.ctx, c, ch, s)

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

	// 实体计数必须在 g.Wait() 之后输出：所有 collector 的 RecordEntities
	// 都发生在各自的 goroutine 里，提前读会漏掉还没跑完的 collector。
	for _, snap := range entityStats.snapshot() {
		ch <- prometheus.MustNewConstMetric(cs.metrics.entitiesFound, prometheus.GaugeValue,
			float64(snap.found), cs.target, snap.collectorName, snap.kind)
		ch <- prometheus.MustNewConstMetric(cs.metrics.entitiesEmitted, prometheus.GaugeValue,
			float64(snap.emitted), cs.target, snap.collectorName, snap.kind)

		// skipped map 里的每个原因都输出，包括本轮计数为 0 的：collector
		// 上报时预填自己关心的全部原因，于是正常状态也有稳定的 0 值序列，
		// 「跳过数 == 0」这类告警不需要再用 or 兜底。一个实体可命中多个
		// 原因（维护中断连），所以跨 reason 求和可能大于实际跳过实体数。
		for reason, n := range snap.skipped {
			ch <- prometheus.MustNewConstMetric(cs.metrics.entitiesSkipped, prometheus.GaugeValue,
				float64(n), cs.target, snap.collectorName, snap.kind, reason)
		}
	}

	// all_collectors 标签沿用框架的取值，既有 dashboard 有引用。
	// 它与新增的无标签 scrape_duration_seconds 的差别是不含 login/logout。
	ch <- prometheus.MustNewConstMetric(cs.metrics.collectorSeconds, prometheus.GaugeValue,
		time.Since(begin).Seconds(), "all_collectors")
}

// safeUpdate 调用 c.Update，并把 panic 转成一条普通 error。
//
// 为什么必须在这里拦：collector 跑在 errgroup 起的**子协程**里，而
// promhttp 的 recover（HandlerOpts.HTTPErrorOnError 那一套）装在 serve
// 协程上。Go 的 panic 不跨协程传播 —— 子协程里的 panic 不会被父协程的
// recover 捕获，它直接终止整个进程。
//
// 后果的量级值得写下来：一台 ESXi 让某个 collector panic，挂掉的不是这次
// 抓取、也不是这个 target，而是 exporter 进程本身，于是**所有** target 一起
// 失联。相比之下，把它降级成这一个 collector 的 success=0 是显然更好的行为，
// 其余 collector 的数据照常产出。
//
// 保留调用栈：panic 的信息几乎全在栈里，只记 recover() 的返回值会让排查
// 无从下手 —— 那通常只是一句 "runtime error: invalid memory address"，
// 不含出事的文件行号。
func safeUpdate(ctx context.Context, c Collector, ch chan<- prometheus.Metric, s *Scrape) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("collector panicked: %v\n%s", r, debug.Stack())
		}
	}()

	return c.Update(ctx, ch, s)
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

// emitSOAP 把本轮 recorder 的记录并入进程级 SOAPStats，并导出三个 SOAP 自监控
// 指标（P-09）。在 Collect 的最外层 defer 里调用：那时 Logout 已完成，登出
// 往返也被计入。
//
// s 为 nil（登录失败/未开始）或 soapRec 为 nil（理论上不会，ThrottleSOAP
// 总会安装）时不写入新观测，只导出该 target 的既有累计值或零值 —— 与
// errors_total 的 seed 同理，保证序列在「一切正常」时也存在，告警不会无数据。
func (cs *CollectorSet) emitSOAP(ch chan<- prometheus.Metric, s *Scrape) {
	var view SOAPView
	if s != nil && s.soapRec != nil {
		view = cs.soap.Observe(cs.target, s.soapRec.snapshot())
	} else {
		view = cs.soap.Snapshot(cs.target)
	}

	ch <- prometheus.MustNewConstMetric(cs.metrics.soapRequests, prometheus.CounterValue,
		view.RequestsOK, cs.target, resultLabelOK)
	ch <- prometheus.MustNewConstMetric(cs.metrics.soapRequests, prometheus.CounterValue,
		view.RequestsFail, cs.target, resultLabelError)

	ch <- prometheus.MustNewConstMetric(cs.metrics.soapInflight, prometheus.GaugeValue,
		view.InflightPeak, cs.target)

	ch <- prometheus.MustNewConstHistogram(cs.metrics.soapWaitSeconds,
		view.WaitCount, view.WaitSum, view.WaitBuckets, cs.target)
}
