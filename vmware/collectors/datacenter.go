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
	datacenterSubsystem = "datacenter"
)

var datacenterCollectorFlag = flag.Bool(fmt.Sprintf("collector.%s", datacenterSubsystem), collector.DefaultEnabled, fmt.Sprintf("Enable the %s collector (default: %v)", datacenterSubsystem, collector.DefaultEnabled))

type datacenterCollector struct {
	logger *slog.Logger
}

func init() {
	collector.RegisterFlag("datacenter", datacenterCollectorFlag)
}

func NewdatacenterCollector(logger *slog.Logger) (collector.Collector, error) {
	return &datacenterCollector{logger}, nil
}

func (c *datacenterCollector) Update(ctx context.Context, ch chan<- prometheus.Metric, s *collector.Scrape) error {

	client := s.Client
	target := s.Target
	about := client.ServiceContent.About

	// vmware_target_info 是 Stage 3 引入的类型标识指标，也是全部 dashboard
	// 条件渲染的唯一依赖点。它与下面的 vmware_vcenter_info 并存而非替代 ——
	// 后者已被既有面板引用，删掉会直接打断用户的图。
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			prometheus.BuildFQName(s.Namespace, "target", "info"),
			"Scrape target type and version. type is either vcenter or esxi.", nil,
			map[string]string{
				"target":  target,
				"type":    targetType(s),
				"version": about.Version,
				"build":   about.Build,
				"patch":   about.PatchLevel,
			},
		), prometheus.GaugeValue, 1.0,
	)

	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			prometheus.BuildFQName(s.Namespace, "vcenter", "info"),
			"This is basic vcenter info", nil,
			map[string]string{
				"version": about.Version,
				"build":   about.Build,
				"patch":   about.PatchLevel,
				"vcenter": target},
		), prometheus.GaugeValue, 1.0,
	)

	var datacenters []mo.Datacenter

	err := fetchProperties(
		ctx, s.View, client,
		[]string{"Datacenter"}, []string{"name", "parent"}, &datacenters, c.logger,
	)
	if err != nil {
		return err

	}

	for _, datacenter := range datacenters {

		labels := map[string]string{"dcmo": datacenter.Self.Value, "dc": datacenter.Name,
			"vcenter": target}

		// ESXi 只有隐式的 ha-datacenter 伪对象。标注 synthetic 让「这不是
		// 真实数据中心」这件事在指标层面可见，而不是静默混进真实数据里。
		if isESXi(s) && datacenter.Self.Value == syntheticDatacenterMoid {
			labels = syntheticLabels(labels)
		}

		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(s.Namespace, datacenterSubsystem, "info"),
				"This is basic datacenter info to be used for parent reference", nil,
				labels,
			), prometheus.GaugeValue, 1.0,
		)

	}

	var folders []mo.Folder

	err = fetchProperties(
		ctx, s.View, client,
		[]string{"Folder"}, []string{"name", "parent"}, &folders, c.logger,
	)
	if err != nil {
		return err

	}

	for _, folder := range folders {

		if folder.Name == "host" || folder.Name == "datastore" {

			ch <- prometheus.MustNewConstMetric(
				prometheus.NewDesc(
					prometheus.BuildFQName(s.Namespace, "folder", "info"),
					"This is basic datacenter info to be used for parent reference", nil,
					map[string]string{"foldermo": folder.Self.Value, "dc": folder.Name, "dcmo": folder.Parent.Value,
						"vcenter": target},
				), prometheus.GaugeValue, 1.0,
			)
		}
	}

	return nil
}
