package vmwareCollectors

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

const (
	datastoreSubsystem = "datastore"
)

var datastoreCollectorFlag = flag.Bool(fmt.Sprintf("collector.%s", datastoreSubsystem), collector.DefaultEnabled, fmt.Sprintf("Enable the %s collector (default: %v)", datastoreSubsystem, collector.DefaultEnabled))

var datastoreCounters = []string{"disk.provisioned.latest", "disk.used.latest"}

type datastoreCollector struct {
	logger *slog.Logger
}

func init() {
	collector.RegisterFlag("datastore", datastoreCollectorFlag)
}

func NewdatastoreCollector(logger *slog.Logger) (collector.Collector, error) {
	return &datastoreCollector{logger}, nil
}

func (c *datastoreCollector) Update(ctx context.Context, ch chan<- prometheus.Metric, s *collector.Scrape) error {

	var (
		datastores     []mo.Datastore
		datastoreRefs  []types.ManagedObjectReference
		datastoreNames = make(map[string]string)
	)
	err := fetchProperties(
		ctx, s.View, s.Client,
		[]string{"Datastore"}, []string{"summary", "host", "vm", "parent"}, &datastores, c.logger,
	)
	if err != nil {
		return err

	}

	re := regexp.MustCompile(`(vmfs)?(volumes)?(ds)?(:)?(/+)`)

	descs := descsFor(s.Namespace).datastore

	// -metrics.legacy 在循环外快照一次，理由见 emitLegacyNames：
	// 循环里裸读既是与 SIGHUP 重载的数据竞争，也会让一次抓取的前后半段
	// 用上不同的值。
	legacy := emitLegacyNames()

	for _, datastore := range datastores {

		datastoreRefs = append(datastoreRefs, datastore.Self)
		datastoreNames[datastore.Self.Value] = datastore.Summary.Name

		dsmo := datastore.Summary.Datastore.Value
		dsName := datastore.Summary.Name

		ch <- prometheus.MustNewConstMetric(descs.info,
			prometheus.GaugeValue, 1.0,
			dsmo, dsName, datastore.Summary.Type,
			re.ReplaceAllString(datastore.Summary.Url, ""),
			datastore.Parent.Value, s.Target)

		ch <- prometheus.MustNewConstMetric(descs.capacityBytes,
			prometheus.GaugeValue, float64(datastore.Summary.Capacity),
			dsmo, dsName, s.Target)

		ch <- prometheus.MustNewConstMetric(descs.freeBytes,
			prometheus.GaugeValue, float64(datastore.Summary.FreeSpace),
			dsmo, dsName, s.Target)

		ch <- prometheus.MustNewConstMetric(descs.accessible,
			prometheus.GaugeValue, boolToFloat64(datastore.Summary.Accessible),
			dsmo, dsName, s.Target)

		if !legacy {
			continue
		}

		// 旧名保留原值：它就是升级前那条序列。
		ch <- prometheus.MustNewConstMetric(descs.capacity,
			prometheus.GaugeValue, float64(datastore.Summary.Capacity),
			dsmo, dsName, s.Target)

		ch <- prometheus.MustNewConstMetric(descs.free,
			prometheus.GaugeValue, float64(datastore.Summary.FreeSpace),
			dsmo, dsName, s.Target)
	}

	// 采样间隔向服务端协商，不再硬编码。
	//
	// 原实现直接传 300（5 分钟历史汇总间隔），注释自称 "A dirty workaround"。
	// 那个值在 vCenter 上碰巧可用，但 ESXi 不聚合历史统计，请求 300 会返回
	// 空结果集 —— 指标静默消失且无任何报错，属于最难排查的一类故障。
	//
	// 这里必须用真实的 datastore 引用探测：QueryPerfProviderSummary 要求实体
	// 存在，伪造 moid 会被服务端以 InvalidArgument 拒绝。
	interval := historicIntervalID
	if len(datastoreRefs) > 0 {
		interval = resolvePerfIntervalForTarget(
			ctx,
			s.Perf,
			datastoreRefs[0],
			s.Interval,
			historicIntervalID,
			targetType(s),
			c.logger,
		)
	}

	scrapePerformance(ctx, ch, c.logger, s.Samples, interval, s.Perf,
		s.Target, "Datastore", s.Namespace, datastoreSubsystem, "", datastoreCounters,
		s.Counters, datastoreRefs, datastoreNames)

	return nil
}
