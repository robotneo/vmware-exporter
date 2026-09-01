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

	var clusters []mo.ClusterComputeResource

	err := fetchProperties(
		ctx, s.View, s.Client,
		[]string{"ClusterComputeResource"}, []string{"name", "summary", "datastore", "parent"}, &clusters, c.logger,
	)
	if err != nil {
		return err

	}

	for _, cluster := range clusters {

		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(s.Namespace, clusterSubsystem, "info"),
				"Basic cluster info, for joining on parent references.", nil,
				map[string]string{"cmo": cluster.Self.Value, "vmwcluster": cluster.Name, "foldermo": cluster.Parent.Value,
					"vcenter": s.Target},
			), prometheus.GaugeValue, 1.0,
		)

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

		var compute []mo.ComputeResource

		err = fetchProperties(
			ctx, s.View, s.Client,
			[]string{"ComputeResource"}, []string{"name", "summary", "datastore", "parent"}, &compute, c.logger,
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
	}

	return nil
}
