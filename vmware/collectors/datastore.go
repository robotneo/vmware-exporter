package vmwareCollectors

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/prezhdarov/prometheus-exporter/pkg/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
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
	collector.RegisterCollector("datastore", datastoreCollectorFlag, NewdatastoreCollector)
}

func NewdatastoreCollector(logger *slog.Logger) (collector.Collector, error) {
	return &datastoreCollector{logger}, nil
}

func (c *datastoreCollector) Update(ch chan<- prometheus.Metric, namespace string, clientAPI collector.ClientAPI, loginData map[string]interface{}, params map[string]string) error {

	var (
		datastores     []mo.Datastore
		datastoreRefs  []types.ManagedObjectReference
		datastoreNames = make(map[string]string)
	)
	err := fetchProperties(
		loginData["ctx"].(context.Context), loginData["view"].(*view.Manager), loginData["client"].(*vim25.Client),
		[]string{"Datastore"}, []string{"summary", "host", "vm", "parent"}, &datastores, c.logger,
	)
	if err != nil {
		return err

	}

	re := regexp.MustCompile(`(vmfs)?(volumes)?(ds)?(:)?(/+)`)

	for _, datastore := range datastores {

		datastoreRefs = append(datastoreRefs, datastore.Self)
		datastoreNames[datastore.Self.Value] = datastore.Summary.Name

		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(namespace, datastoreSubsystem, "info"),
				"This is datastore info to be used for parent reference", nil,
				map[string]string{"dsmo": datastore.Summary.Datastore.Value, "ds": datastore.Summary.Name, "type": datastore.Summary.Type,
					"pfinstance": re.ReplaceAllString(datastore.Summary.Url, ""), "foldermo": datastore.Parent.Value, "vcenter": loginData["target"].(string)},
			), prometheus.GaugeValue, 1.0,
		)

		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(namespace, datastoreSubsystem, "capacity"),
				"Datastore capacity in bytes", nil,
				map[string]string{"dsmo": datastore.Summary.Datastore.Value, "ds": datastore.Summary.Name,
					"vcenter": loginData["target"].(string)},
			), prometheus.GaugeValue, float64(datastore.Summary.Capacity),
		)

		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(namespace, datastoreSubsystem, "free"),
				"Datastore available space in bytes", nil,
				map[string]string{"dsmo": datastore.Summary.Datastore.Value, "ds": datastore.Summary.Name,
					"vcenter": loginData["target"].(string)},
			), prometheus.GaugeValue, float64(datastore.Summary.FreeSpace),
		)

		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(namespace, datastoreSubsystem, "accessible"),
				"Whether the datastore is accessible", nil,
				map[string]string{"dsmo": datastore.Summary.Datastore.Value, "ds": datastore.Summary.Name,
					"vcenter": loginData["target"].(string)},
			), prometheus.GaugeValue,
			func(accessible bool) float64 {
				if accessible {
					return 1
				}
				return 0
			}(datastore.Summary.Accessible),
		)
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
			loginData["ctx"].(context.Context),
			loginData["perf"].(*performance.Manager),
			datastoreRefs[0],
			loginData["interval"].(int32),
			historicIntervalID,
			targetType(loginData),
			c.logger,
		)
	}

	scrapePerformance(loginData["ctx"].(context.Context), ch, c.logger, loginData["samples"].(int32), interval, loginData["perf"].(*performance.Manager),
		loginData["target"].(string), "Datastore", namespace, datastoreSubsystem, "", datastoreCounters,
		loginData["counters"].(map[string]*types.PerfCounterInfo), datastoreRefs, datastoreNames)

	return nil
}
