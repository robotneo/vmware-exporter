package vmwareCollectors

import (
	"context"
	"flag"
	"fmt"
	"log/slog"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/vim25/mo"
)

const (
	clusterSubsystem = "cluster"
)

var clusterCollectorFlag = flag.Bool(fmt.Sprintf("collector.%s", clusterSubsystem), collector.DefaultEnabled, fmt.Sprintf("Enable the %s collector (default: %v)", clusterSubsystem, collector.DefaultEnabled))

type clusterCollector struct {
	logger *slog.Logger
}

func init() {
	collector.RegisterFlag("cluster", clusterCollectorFlag)
}

func NewClusterCollector(logger *slog.Logger) (collector.Collector, error) {
	return &clusterCollector{logger}, nil
}

func (c *clusterCollector) Update(ctx context.Context, ch chan<- prometheus.Metric, s *collector.Scrape) error {

	descs := descsFor(s.Namespace).cluster

	clusters, err := fetchInventoryCached[mo.ClusterComputeResource](
		ctx, s,
		[]string{"ClusterComputeResource"}, []string{"name", "summary", "datastore", "parent"}, c.logger,
	)
	if err != nil {
		return err

	}

	found := 0
	for _, cluster := range clusters {
		found++

		// Summary 是 BaseComputeResourceSummary 接口：真实集群的动态类型是
		// *types.ClusterComputeResourceSummary（嵌入 ComputeResourceSummary）。
		// GetComputeResourceSummary 是 govmomi 生成的类型访问器，避免在这里
		// 手写类型断言并丢掉 ClusterComputeResourceSummary 的嵌入字段。
		summary := cluster.Summary.GetComputeResourceSummary()

		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(s.Namespace, clusterSubsystem, "info"),
				"Basic cluster info, for joining on parent references.", nil,
				map[string]string{"cmo": cluster.Self.Value, "vmwcluster": cluster.Name, "foldermo": cluster.Parent.Value,
					"vcenter": s.Target},
			), prometheus.GaugeValue, 1.0,
		)

		// overall_status：状态进 label、值恒为 1，与 resourcepool 的
		// overall_status 同一形态。gray 原样输出（它常先于真实故障出现）。
		// summary 在集群被断连主机拖成异常时仍会返回，OverallStatus 空值
		// 会输出空状态字符串 —— vCenter 正常不会这样，这里不做猜测映射。
		ch <- prometheus.MustNewConstMetric(descs.overallStatus,
			prometheus.GaugeValue, 1.0,
			cluster.Self.Value, cluster.Name,
			string(summary.OverallStatus), s.Target)

		// 集群容量。单位对齐 resourcepool 约定：CPU 一律 hertz，内存一律
		// 字节。两个内存字段单位不同 —— TotalMemory 本就是字节，
		// EffectiveMemory 是 MB，必须 ×1048576（2^20）。
		ch <- prometheus.MustNewConstMetric(descs.effectiveHosts,
			prometheus.GaugeValue, float64(summary.NumEffectiveHosts),
			cluster.Self.Value, cluster.Name, s.Target)

		ch <- prometheus.MustNewConstMetric(descs.cpuCapacityHertz,
			prometheus.GaugeValue, float64(summary.TotalCpu)*1e6,
			cluster.Self.Value, cluster.Name, s.Target)

		ch <- prometheus.MustNewConstMetric(descs.cpuEffectiveHertz,
			prometheus.GaugeValue, float64(summary.EffectiveCpu)*1e6,
			cluster.Self.Value, cluster.Name, s.Target)

		ch <- prometheus.MustNewConstMetric(descs.memoryCapacityBytes,
			prometheus.GaugeValue, float64(summary.TotalMemory),
			cluster.Self.Value, cluster.Name, s.Target)

		ch <- prometheus.MustNewConstMetric(descs.memoryEffectiveBytes,
			prometheus.GaugeValue, float64(summary.EffectiveMemory)*1048576,
			cluster.Self.Value, cluster.Name, s.Target)

		// 每个 datastore 一条序列。原实现把整个 moid 列表用逗号拼成单个
		// label 值，这有两个问题：查询侧无法用 dsmo 做 join（只能做子串
		// 匹配），且集群增删任一 datastore 都会改变 label 值 —— 产生一条
		// 全新序列，旧序列则变成僵尸留在 TSDB 里。
		for _, ds := range cluster.Datastore {
			ch <- prometheus.MustNewConstMetric(
				prometheus.NewDesc(
					prometheus.BuildFQName(s.Namespace, clusterSubsystem, "datastore"),
					"Cluster to datastore mapping, one series per datastore.", nil,
					map[string]string{"cmo": cluster.Self.Value, "vmwcluster": cluster.Name,
						"dsmo": ds.Value, "vcenter": s.Target},
				), prometheus.GaugeValue, 1.0,
			)
		}
	}

	if len(clusters) == 0 {

		// ESXi 直连时不存在 ClusterComputeResource，只有隐式的
		// ha-compute-res。原实现靠 len(clusters)==0 意外兜到这条路径，
		// 现在把它变成显式分支：既服务 ESXi，也覆盖 vCenter 下主机不在
		// 任何集群里的情况（独立主机会有一个自动生成的 ComputeResource）。
		esxi := isESXi(s)

		if esxi {
			c.logger.Debug("no cluster found, falling back to ComputeResource",
				"target_type", targetTypeESXi)
		}

		compute, err := fetchInventoryCached[mo.ComputeResource](
			ctx, s,
			[]string{"ComputeResource"}, []string{"name", "summary", "datastore", "parent"}, c.logger,
		)
		if err != nil {
			return err

		}

		for _, cr := range compute {

			infoLabels := map[string]string{"cmo": cr.Self.Value, "host": cr.Name, "foldermo": cr.Parent.Value,
				"vcenter": s.Target}

			// 只有 ESXi 的 ha-compute-res 才是伪对象。vCenter 下的独立主机
			// ComputeResource 是真实存在的托管对象，不该被标成 synthetic。
			synthetic := esxi && cr.Self.Value == syntheticComputeMoid
			if synthetic {
				infoLabels = syntheticLabels(infoLabels)
			}

			ch <- prometheus.MustNewConstMetric(
				prometheus.NewDesc(
					prometheus.BuildFQName(s.Namespace, "compute", "info"),
					"Basic compute resource info, for hosts that are not in a cluster.", nil,
					infoLabels,
				), prometheus.GaugeValue, 1.0,
			)

			// 与 cluster_datastore 同理：一个 datastore 一条序列。
			for _, ds := range cr.Datastore {
				dsLabels := map[string]string{"cmo": cr.Self.Value, "host": cr.Name,
					"dsmo": ds.Value, "vcenter": s.Target}
				if synthetic {
					dsLabels = syntheticLabels(dsLabels)
				}

				ch <- prometheus.MustNewConstMetric(
					prometheus.NewDesc(
						prometheus.BuildFQName(s.Namespace, "compute", "datastore"),
						"Compute resource to datastore mapping, one series per datastore.", nil,
						dsLabels,
					), prometheus.GaugeValue, 1.0,
				)
			}
		}

		// 兜底分支统计为 compute 实体，便于与真实集群区分。
		s.RecordEntities(clusterSubsystem, "compute", len(compute), len(compute), nil)
	} else {
		// 集群没有"数据面跳过"概念（指标全部来自已检索的 summary），
		// found 与 emitted 相同，skipped 留空 → 不产出 skipped 序列。
		s.RecordEntities(clusterSubsystem, "cluster", found, found, nil)
	}

	return nil
}
