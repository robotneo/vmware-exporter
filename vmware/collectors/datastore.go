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

// datastorePathRE 把 datastore URL 里的协议/路径成分剥掉，只留规范名。
// 必须是包级变量：它原来在 Update 内 MustCompile，等于每轮抓取都重新编译
// 同一个正则（P-07），datastore collector 默认开启，这是确定的纯浪费。
var datastorePathRE = regexp.MustCompile(`(vmfs)?(volumes)?(ds)?(:)?(/+)`)

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
		datastoreRefs  []types.ManagedObjectReference
		datastoreNames = make(map[string]string)
	)

	// 走清单 TTL 缓存：datastore 的 summary（容量/剩余/可访问）与 host/vm/parent
	// 关联都属于慢变的容量与拓扑面 —— 容量监控按分钟级粒度观察足矣。accessible
	// 最多滞后一个 TTL，但 datastore 不可达时同一轮的 perf 查询仍会实时失败并
	// 留日志，不会被缓存静默吞掉。perf 计数器（disk.used/provisioned）不经过
	// 这里，始终实时查询。
	//
	// overallStatus 是 mo.ManagedEntity 基础属性（DatastoreSummary 上没有它），
	// 单独加进属性列表，不增加任何往返。
	datastores, err := fetchInventoryCached[mo.Datastore](
		ctx, s,
		[]string{"Datastore"}, []string{"summary", "host", "vm", "parent", "overallStatus"}, c.logger,
	)
	if err != nil {
		return err

	}

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
			datastorePathRE.ReplaceAllString(datastore.Summary.Url, ""),
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

		// overallStatus 取自 mo.ManagedEntity 基础属性。gray（未知）原样
		// 输出 —— 它常先于真实故障出现（尤其 APD/PDL 场景），不能归并进 yellow。
		ch <- prometheus.MustNewConstMetric(descs.overallStatus,
			prometheus.GaugeValue, 1.0,
			dsmo, dsName, string(datastore.OverallStatus), s.Target)

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
			s.ProviderSummary,
			datastoreRefs[0],
			s.Interval,
			historicIntervalID,
			targetType(s),
			c.logger,
		)
	}

	scrapePerformance(ctx, ch, c.logger, s.Samples, interval, s.Perf,
		s.Target, "Datastore", s.Namespace, datastoreSubsystem, "", datastoreCounters,
		s.Counters, datastoreRefs, datastoreNames, s.PerfChunkSize, s.HostConcurrency())

	// datastore 数据面不按状态过滤：disk.used/provisioned 是历史汇总查询，
	// 不可达存储也会被 vCenter 返回（值可能为 0）。found 与 emitted 相同。
	s.RecordEntities(datastoreSubsystem, "datastore", len(datastores), len(datastores), nil)

	return nil
}
