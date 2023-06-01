package vmwareCollectors

import (
	"context"
	"flag"
	"fmt"
	"regexp"

	"github.com/go-kit/log"
	"github.com/prezhdarov/prometheus-exporter/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
)

const (
	datastoreSubsystem = "datastore"
)

var datastoreCollectorFlag = flag.Bool(fmt.Sprintf("collector.%s", datastoreSubsystem), collector.DefaultEnabled, fmt.Sprintf("Enable the %s collector (default: %v)", datastoreSubsystem, collector.DefaultEnabled))

type datastoreCollector struct {
	logger log.Logger
}

func init() {
	collector.RegisterCollector("datastore", datastoreCollectorFlag, NewdatastoreCollector)
}

func NewdatastoreCollector(logger log.Logger) (collector.Collector, error) {
	return &datastoreCollector{logger}, nil
}

func (c *datastoreCollector) Update(ch chan<- prometheus.Metric, namespace string, clientAPI collector.ClientAPI, loginData map[string]interface{}, params map[string]string) error {

	var datastores []mo.Datastore

	err := fetchProperties(
		loginData["ctx"].(context.Context), loginData["view"].(*view.Manager), loginData["client"].(*vim25.Client),
		[]string{"Datastore"}, []string{"summary", "host", "vm", "parent"}, &datastores, c.logger,
	)
	if err != nil {
		return err

	}

	re := regexp.MustCompile(`(vmfs)?(volumes)?(ds)?(:)?(/+)`)

	for _, datastore := range datastores {

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
	}

	return nil
}
