package vmwareCollectors

import (
	"context"
	"flag"
	"fmt"
	"log/slog"

	"github.com/prezhdarov/prometheus-exporter/pkg/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
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
	collector.RegisterCollector("cluster", clusterCollectorFlag, NewClusterCollector)
}

func NewClusterCollector(logger *slog.Logger) (collector.Collector, error) {
	return &clusterCollector{logger}, nil
}

func (c *clusterCollector) Update(ch chan<- prometheus.Metric, namespace string, clientAPI collector.ClientAPI, loginData map[string]interface{}, params map[string]string) error {

	var clusters []mo.ClusterComputeResource

	err := fetchProperties(
		loginData["ctx"].(context.Context), loginData["view"].(*view.Manager), loginData["client"].(*vim25.Client),
		[]string{"ClusterComputeResource"}, []string{"name", "summary", "datastore", "parent"}, &clusters, c.logger,
	)
	if err != nil {
		return err

	}

	for _, cluster := range clusters {

		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(namespace, clusterSubsystem, "info"),
				"This is basic cluster info to be used for parent reference", nil,
				map[string]string{"cmo": cluster.Self.Value, "vmwcluster": cluster.Name, "foldermo": cluster.Parent.Value,
					"vcenter": loginData["target"].(string)},
			), prometheus.GaugeValue, 1.0,
		)

		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(namespace, clusterSubsystem, "datastores"),
				"This is basic cluster info to be used for parent reference", nil,
				map[string]string{"cmo": cluster.Self.Value, "vmwcluster": cluster.Name, "datastores": *moSliceToString(cluster.Datastore),
					"vcenter": loginData["target"].(string)},
			), prometheus.GaugeValue, 1.0,
		)
	}

	if len(clusters) == 0 {

		// ESXi 直连时不存在 ClusterComputeResource，只有隐式的
		// ha-compute-res。原实现靠 len(clusters)==0 意外兜到这条路径，
		// 现在把它变成显式分支：既服务 ESXi，也覆盖 vCenter 下主机不在
		// 任何集群里的情况（独立主机会有一个自动生成的 ComputeResource）。
		esxi := isESXi(loginData)

		if esxi {
			c.logger.Debug("no cluster found, falling back to ComputeResource",
				"target_type", targetTypeESXi)
		}

		var compute []mo.ComputeResource

		err = fetchProperties(
			loginData["ctx"].(context.Context), loginData["view"].(*view.Manager), loginData["client"].(*vim25.Client),
			[]string{"ComputeResource"}, []string{"name", "summary", "datastore", "parent"}, &compute, c.logger,
		)
		if err != nil {
			return err

		}

		for _, cr := range compute {

			infoLabels := map[string]string{"cmo": cr.Self.Value, "host": cr.Name, "foldermo": cr.Parent.Value,
				"vcenter": loginData["target"].(string)}
			dsLabels := map[string]string{"cmo": cr.Self.Value, "host": cr.Name, "datastores": *moSliceToString(cr.Datastore),
				"vcenter": loginData["target"].(string)}

			// 只有 ESXi 的 ha-compute-res 才是伪对象。vCenter 下的独立主机
			// ComputeResource 是真实存在的托管对象，不该被标成 synthetic。
			if esxi && cr.Self.Value == syntheticComputeMoid {
				infoLabels = syntheticLabels(infoLabels)
				dsLabels = syntheticLabels(dsLabels)
			}

			ch <- prometheus.MustNewConstMetric(
				prometheus.NewDesc(
					prometheus.BuildFQName(namespace, "compute", "info"),
					"This is basic cluster info to be used for parent reference", nil,
					infoLabels,
				), prometheus.GaugeValue, 1.0,
			)

			ch <- prometheus.MustNewConstMetric(
				prometheus.NewDesc(
					prometheus.BuildFQName(namespace, "compute", "datastores"),
					"This is basic cluster info to be used for parent reference", nil,
					dsLabels,
				), prometheus.GaugeValue, 1.0,
			)
		}
	}

	return nil
}
