package vmwareCollectors

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/types"
)

func Load(logger *slog.Logger) {
	logger.Info("Loading VMware vSphere collector set")
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

			var sum int64
			for _, subvalue := range value.Value {
				sum += subvalue
			}
			avg := sum / int64(len(value.Value))

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

			ch <- prometheus.MustNewConstMetric(
				perfDesc(namespace, subsystem, value.Name, moType, instanced, counterInfo),
				prometheus.GaugeValue,
				float64(avg),
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
