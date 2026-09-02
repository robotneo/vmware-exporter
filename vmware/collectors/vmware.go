package vmwareCollectors

import (
	"context"
	"log/slog"
	"time"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
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

			if !*legacyMetrics {
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
	targetRefs []types.ManagedObjectReference, targetNames map[string]string) {
	if len(targetRefs) == 0 {
		logger.Debug("no targets for perfman scrape", "type", moType)
		return
	}

	if perfManager == nil {
		logger.Error("nil performance manager", "type", moType)
		return
	}

	logger.Debug("gathering perfman metrics", "target_ref", targetRefs[0], "type", moType)

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

	spec := types.PerfQuerySpec{
		MaxSample:  sampleCount,                                // Number of samples to fetch - if samples are fetched every 20s only one is needed.
		MetricId:   []types.PerfMetricId{{Instance: instance}}, //Instance takes either null string or * (or in fact any name of an performance manager metric instance)
		IntervalId: sampleInterval,                             // 20 seconds
	}

	sample, err := perfManager.SampleByName(ctx, spec, supportedCounters, targetRefs)
	if err != nil {
		logger.Error("error sampling metrics and targets", "error", err, "type", moType)
		return
	}

	metrics, err := perfManager.ToMetricSeries(ctx, sample)
	if err != nil {
		logger.Error("error converting perf samples to metric series", "error", err, "type", moType)
		return
	}

	logger.Debug("time to fetch perfman samples", "type", moType, "duration_seconds", time.Since(begin).Seconds())

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
