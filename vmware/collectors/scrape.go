package vmwareCollectors

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
	"golang.org/x/sync/errgroup"
)

func Load(logger *slog.Logger) {
	logger.Info("Loading VMware vSphere collector set")
}

// fetchHosts 是注入给 Scrape.Hosts 的属性检索实现。
//
// 这个函数存在的唯一理由是打破包依赖环：属性检索要用 fetchProperties，
// 它在本包；而 Scrape 在 internal/collector，本包已经 import 了它。
// 于是把实现以函数值的形式传进去，方向就只有一条。
//
// logger 从闭包捕获而非参数传入，是为了让 Scrape.Hosts 的签名保持最小。
func fetchHosts(logger *slog.Logger) collector.HostFetcher {
	return func(ctx context.Context, s *collector.Scrape, props []string, out *[]mo.HostSystem) error {
		return fetchProperties(ctx, s.View, s.Client, []string{"HostSystem"}, props, out, logger)
	}
}

func fetchProperties(ctx context.Context, viewManager *view.Manager, vmwClient *vim25.Client, moTypes, propSpec []string, dataContainer interface{}, logger *slog.Logger) error {

	view, err := viewManager.CreateContainerView(
		ctx, vmwClient.ServiceContent.RootFolder,
		moTypes, true,
	)
	if err != nil {
		return err

	}

	defer func() {
		if err := view.Destroy(ctx); err != nil {
			logger.Error("failed to destroy container view", "error", err)
		}
	}()

	begin := time.Now()

	err = view.Retrieve(ctx, moTypes, propSpec, dataContainer)
	if err != nil {
		return err
	}

	logger.Debug("time to fetch property collector", "types", moTypes, "duration_seconds", time.Since(begin).Seconds())

	return nil

}

// fetchInventoryCached 是拓扑/容量类 collector 的清单检索入口：经 Scrape 上的
// 进程级 TTL 缓存，命中时整轮 ContainerView 检索被跳过，未命中时回退到
// fetchProperties 实时拉取并回填。
//
// 适用范围刻意限定在「变化慢、不含运行态过滤」的清单类型 —— datacenter、
// folder、cluster、compute resource、datastore、resourcepool、以及 vSAN 用的
// 集群名列表。host 与 vm 不走这里：它们的检索结果同时承载 runtime
// （电源/连接/维护态），而这些状态决定哪些实体参与 perf 查询，缓存它们会让
// 开关机与维护进出最多延迟一个 TTL 才反映，且与后续 P1 的状态指标改造耦合。
//
// 返回的切片归缓存所有，调用方必须只读（遍历、读字段），不得 append 或改元素。
func fetchInventoryCached[T any](ctx context.Context, s *collector.Scrape, moTypes, propSpec []string, logger *slog.Logger) ([]T, error) {
	key := collector.InventoryKey(s.Target, moTypes, propSpec)

	return collector.FetchInventory(s.InventoryCache, s.InventoryTTL, key, func() ([]T, error) {
		var out []T
		if err := fetchProperties(ctx, s.View, s.Client, moTypes, propSpec, &out, logger); err != nil {
			return nil, err
		}

		return out, nil
	})
}

func emitPerformanceMetrics(
	ch chan<- prometheus.Metric,
	vcenter, moType, namespace, subsystem, instance string,
	countersSpec map[string]*types.PerfCounterInfo,
	targetNames map[string]string,
	metrics []performance.EntityMetric,
	logger *slog.Logger,
) {
	entityLabels := perfEntityLabels(moType)
	if entityLabels == nil {
		// 原实现的 switch 没有 default 分支，未知实体类型会静默产出只带
		// vcenter label 的指标 —— 该类型下所有实体的序列互相覆盖，最后只剩
		// 一条，而且看不出哪里出了问题。宁可跳过并留下日志。
		logger.Error("unknown managed object type for performance metrics, skipping", "type", moType)
		return
	}

	// -metrics.legacy 在循环外快照一次。内层循环每个计数器值都要判它，
	// 在那里读既是数据竞争（SIGHUP 重载会写这个 flag），也会让同一个
	// /metrics 响应里一部分指标带旧名、一部分不带 —— 见 emitLegacyNames。
	legacy := emitLegacyNames()

	for _, metric := range metrics {
		for _, value := range metric.Value {
			instanced := value.Instance != ""

			// instance 参数非空表示这一轮只要 instanced 计数器。
			if !instanced && instance != "" {
				continue
			}

			if len(value.Value) == 0 {
				continue
			}

			// 样本数与时间戳数不一致说明这批数据不完整，跳过而不是
			// 按错位的方式求平均。
			if len(value.Value) != len(metric.SampleInfo) {
				continue
			}

			counterInfo, ok := countersSpec[value.Name]
			if !ok {
				continue
			}

			spec, mapped := translatePerfCounter(value.Name, counterInfo)

			// 聚合方式必须按 StatsType 分流 —— 这是本轮修正的数据正确性 bug。
			//
			// 旧实现对所有计数器一律求平均。对 delta 类计数器（vCenter 声明
			// 每个样本是「该采样区间内的增量」，例如 cpu.ready.summation）
			// 那是错的：3 个 20 秒区间各 ready 了 100ms，这一分钟内一共
			// ready 了 300ms，不是 100ms。求平均把窗口长度这个信息丢掉了，
			// 得到的数字既不是速率也不是总量。
			//
			// 这个 bug 有个容易漏掉的性质：默认配置下 samples =
			// interval/granularity = 20/20 = 1，求和与求平均结果相同，所以
			// 它只在用户显式调大 -vmware.interval 时才显形 —— 而那正是想
			// 降低抓取频率的人会做的事。
			var raw float64
			for _, subvalue := range value.Value {
				raw += float64(subvalue)
			}

			if !mapped || !spec.Delta {
				// absolute 与 rate 都是瞬时量，窗口内求平均是合理的降噪。
				//
				// 浮点除法而不是旧实现的 int64 整除：整数除法会额外截断，
				// 例如三个样本 1/1/2 求平均得到 1 而不是 1.33。
				raw /= float64(len(value.Value))
			}

			// label 值按 Desc 声明的顺序拼装：vcenter, <实体标签...>, [pfinstance]
			//
			// labelValues 每次循环重新构造。原实现把 labelMap 提到外层复用，
			// 结果 pfinstance 一旦被设置就再也不会清除 —— 若某个 value 带
			// instance 而下一个不带，陈旧的 pfinstance 会泄漏到后者身上，
			// 产出一条 label 错误的序列。
			labelValues := make([]string, 0, len(entityLabels)+2)
			labelValues = append(labelValues, vcenter, targetNames[metric.Entity.Value], metric.Entity.Value)
			if instanced {
				labelValues = append(labelValues, value.Instance)
			}

			if !mapped {
				// 单位不在已知规则里 —— 按旧命名原样导出并留下日志，而不是
				// 猜一个单位后缀。猜错单位会让数值带着错误的后缀进入 TSDB，
				// 而没有任何东西会报错。
				logger.Warn("performance counter has an unmapped unit, emitting under the legacy name",
					"counter", value.Name,
					"unit", counterInfo.UnitInfo.GetElementDescription().Key)

				ch <- prometheus.MustNewConstMetric(
					perfDesc(namespace, subsystem, value.Name, moType, instanced, true, counterInfo),
					prometheus.GaugeValue,
					raw,
					labelValues...,
				)

				continue
			}

			ch <- prometheus.MustNewConstMetric(
				perfDesc(namespace, subsystem, value.Name, moType, instanced, false, counterInfo),
				spec.ValueType,
				raw*spec.Factor,
				labelValues...,
			)

			if !legacy {
				continue
			}

			// 旧名保留原始取值与原始类型：它就是升级前那条序列。改动它的
			// 数值等于让「双写过渡」这个承诺失效 —— 用户拿旧名做的图会在
			// 升级瞬间跳变，而 legacy 的全部意义就是不让那件事发生。
			//
			// 唯一的例外是 delta 计数器的聚合修正：旧名也用求和后的值。那条
			// 是 bug 修复，把错误的平均值继续导出一个版本周期没有意义 ——
			// 何况默认配置（samples=1）下两者本来就相同。
			ch <- prometheus.MustNewConstMetric(
				perfDesc(namespace, subsystem, value.Name, moType, instanced, true, counterInfo),
				prometheus.GaugeValue,
				raw,
				labelValues...,
			)
		}
	}
}

func scrapePerformance(ctx context.Context, ch chan<- prometheus.Metric, logger *slog.Logger, sampleCount, sampleInterval int32,
	perfManager *performance.Manager, vcenter, moType, namespace, subsystem, instance string,
	counters []string, countersSpec map[string]*types.PerfCounterInfo,
	targetRefs []types.ManagedObjectReference, targetNames map[string]string,
	chunkSize, concurrency int) {
	if len(targetRefs) == 0 {
		logger.Debug("no targets for perfman scrape", "type", moType)
		return
	}

	if perfManager == nil {
		logger.Error("nil performance manager", "type", moType)
		return
	}

	logger.Debug("gathering perfman metrics", "target_ref", targetRefs[0], "type", moType,
		"entities", len(targetRefs), "chunk_size", chunkSize)

	begin := time.Now()

	requestedCounters := len(counters)
	supportedCounters := make([]string, 0, requestedCounters)
	for _, counter := range counters {
		if _, ok := countersSpec[counter]; ok {
			supportedCounters = append(supportedCounters, counter)
			continue
		}

		logger.Debug("performance counter not available, skipping", "counter", counter, "type", moType)
	}

	if len(supportedCounters) == 0 {
		logger.Debug("no supported performance counters for scrape", "type", moType, "requested_counters", requestedCounters)
		return
	}

	// 直接用计数器 id 构造 PerfMetricId，而不是每块都调 perfManager.SampleByName：
	// 后者内部会对每一次调用先 CounterInfoByName 再发一次 SOAP，分块后这个额外
	// 往返会随块数线性放大。计数器元数据本调用已通过 s.Counters 持有，没有理由
	// 为每个分块重复拉一遍。
	metricIDs := make([]types.PerfMetricId, 0, len(supportedCounters))
	for _, name := range supportedCounters {
		metricIDs = append(metricIDs, types.PerfMetricId{
			CounterId: countersSpec[name].Key,
			Instance:  instance,
		})
	}

	template := types.PerfQuerySpec{
		MaxSample:  sampleCount,    // Number of samples to fetch - if samples are fetched every 20s only one is needed.
		MetricId:   metricIDs,      //Instance takes either null string or * (or in fact any name of an performance manager metric instance)
		IntervalId: sampleInterval, // 20 seconds
	}

	// 历史汇总间隔（>=60s）必须带 StartTime 才能取到 vCenter DB 里的点；
	// govmomi SampleByName 也是这么做的（now - IntervalId*MaxSample*2）。
	// 这里在分块前取一次时间并写进模板，保证各块查询的是同一个时间窗口。
	if template.IntervalId >= 60 && template.MaxSample > 0 {
		now, err := methods.GetCurrentTime(ctx, perfManager.Client())
		if err != nil {
			logger.Error("failed to get current vCenter time for historical query", "error", err, "type", moType)
			return
		}

		window := time.Duration(template.IntervalId) * time.Second * time.Duration(template.MaxSample*2)
		start := now.Add(-window)
		template.StartTime = &start
	}

	// 把实体按 chunkSize 切片，多块在有界并发下查询。单请求装几千实体会撞
	// vCenter 的 vpxd.stats.maxQueryMetrics 上限或单次超时，分块后每块都小而稳；
	// 即便这里不设并发上限，RoundTripper 上的全局 SOAP 闸（ThrottleSOAP）仍会
	// 把真正同时在飞的请求数钉在 -collector.max-concurrency，但显式 SetLimit
	// 避免一次生成与块数等量的 goroutine。
	chunks := chunkRefs(targetRefs, chunkSize)
	parts := make([][]types.BasePerfEntityMetricBase, len(chunks))

	g, gctx := errgroup.WithContext(ctx)
	if concurrency > 0 {
		g.SetLimit(concurrency)
	}

	for i, refs := range chunks {
		i, refs := i, refs

		g.Go(func() error {
			queryBegin := time.Now()

			// 每个实体一条 spec，与 govmomi SampleByName 内部的展开方式一致，
			// 但计数器集只解析一次、StartTime 只算一次。
			specs := make([]types.PerfQuerySpec, 0, len(refs))
			for _, ref := range refs {
				spec := template
				spec.Entity = ref.Reference()
				specs = append(specs, spec)
			}

			series, err := perfManager.Query(gctx, specs)
			if err != nil {
				return fmt.Errorf("perf query chunk %d/%d (%d entities, type %s): %w",
					i+1, len(chunks), len(refs), moType, err)
			}

			parts[i] = series

			logger.Debug("perf chunk scraped", "type", moType, "chunk", i+1, "of", len(chunks),
				"entities", len(refs), "duration_seconds", time.Since(queryBegin).Seconds())

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		// 任一块失败就不产出这一轮（该计数器组的）性能指标 —— 与改造前单个
		// SampleByName 失败时的全有或无语义一致，避免发半份数据。
		logger.Error("error sampling metrics and targets", "error", err, "type", moType)
		return
	}

	var rawSeries []types.BasePerfEntityMetricBase
	for _, part := range parts {
		rawSeries = append(rawSeries, part...)
	}

	// 复刻 govmomi SampleByName 对历史查询的尾部截断：2× 窗口可能取回多于
	// MaxSample 的点，只保留最后 MaxSample 个。不做这一步，datastore 的 300s
	// 历史点会把窗口里的多个点一起平均，数值与升级前不一致。
	if template.IntervalId >= 60 && template.MaxSample > 0 {
		truncateToLastSamples(rawSeries, int(template.MaxSample))
	}

	metrics, err := perfManager.ToMetricSeries(ctx, rawSeries)
	if err != nil {
		logger.Error("error converting perf samples to metric series", "error", err, "type", moType)
		return
	}

	logger.Debug("time to fetch perfman samples", "type", moType, "chunks", len(chunks),
		"duration_seconds", time.Since(begin).Seconds())

	begin = time.Now()

	emitPerformanceMetrics(
		ch,
		vcenter,
		moType,
		namespace,
		subsystem,
		instance,
		countersSpec,
		targetNames,
		metrics,
		logger,
	)

	logger.Debug("time to process perfman metrics", "type", moType, "duration_seconds", time.Since(begin).Seconds())
}

// truncateToLastSamples 把每个实体的历史样本裁到只保留最后 n 个。
//
// 这是 govmomi SampleByName 在「为历史查询回看 2× 窗口」之后做的同一件事：
// 多取的点必须丢掉，否则下游对 Value 求平均时窗口被拉长、数值被稀释。本项目
// 直接调 PerfManager.Query（避免每块重复拉计数器元数据），因此要自己复刻
// 这一步，保证与升级前经 SampleByName 得到的结果逐字一致。
func truncateToLastSamples(series []types.BasePerfEntityMetricBase, n int) {
	for _, base := range series {
		em, ok := base.(*types.PerfEntityMetric)
		if !ok {
			continue
		}

		if diff := len(em.SampleInfo) - n; diff > 0 {
			em.SampleInfo = em.SampleInfo[diff:]
		}

		for _, s := range em.Value {
			v, ok := s.(*types.PerfMetricIntSeries)
			if !ok {
				continue
			}

			if diff := len(v.Value) - n; diff > 0 {
				v.Value = v.Value[diff:]
			}
		}
	}
}

// chunkRefs 把托管对象引用按每片至多 size 个切成连续的片。
//
// size<=0（不分块）或引用数不超过一片时，返回装着完整切片的单片 —— 调用方
// 因此无需区分「分块」与「不分块」两条路径，v0.1.19 及更早的单请求行为就是
// size=0 时这唯一一片。
//
// 切出来的是底层数组的切片视图而非拷贝：QueryPerf 只读这些引用，且这一轮内
// targetRefs 不会被修改，共享底层数组没有别名风险。
func chunkRefs(refs []types.ManagedObjectReference, size int) [][]types.ManagedObjectReference {
	if size <= 0 || len(refs) <= size {
		return [][]types.ManagedObjectReference{refs}
	}

	chunks := make([][]types.ManagedObjectReference, 0, (len(refs)+size-1)/size)

	for start := 0; start < len(refs); start += size {
		end := start + size
		if end > len(refs) {
			end = len(refs)
		}

		chunks = append(chunks, refs[start:end])
	}

	return chunks
}
