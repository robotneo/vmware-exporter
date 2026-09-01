package vmwareCollectors

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"strings"

	"github.com/prezhdarov/vmware-exporter/vmware/esxcli"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/vim25/mo"
	"golang.org/x/sync/errgroup"
)

type StorageInfo struct {
	Vendor   string `xml:"Vendor"`
	Model    string `xml:"Model"`
	Revision string `xml:"Revision"`
}

type StorageResponse struct {
	DataObject []StorageInfo `xml:"DataObject"`
}

var (
	esxclistoragelistSubsystem = "esxcli_storage"
)

var esxclistoragelistCollectorFlag = flag.Bool("collector.esxcli.storage", collector.DefaultDisabled, fmt.Sprintf("Enable the %s collector (default: %v)", esxclistoragelistSubsystem, collector.DefaultDisabled))

type esxclistoragelistCollector struct {
	logger *slog.Logger
}

func init() {
	collector.RegisterFlag("esxcli.storage", esxclistoragelistCollectorFlag)
}

func NewesxcliStorageListCCollector(logger *slog.Logger) (collector.Collector, error) {
	return &esxclistoragelistCollector{logger}, nil
}

func (c *esxclistoragelistCollector) Update(ctx context.Context, ch chan<- prometheus.Metric, s *collector.Scrape) error {

	// 与 host / esxcli.host.nic 共享同一份 HostSystem 检索结果。
	hosts, err := s.Hosts(ctx, fetchHosts(c.logger))
	if err != nil {
		return err

	}

	dCounter := 0

	// 与 esxcli.host.nic 同理：per-host fan-out 必须有上限，
	// 否则并发 goroutine 数直接等于 vCenter 里的主机数。
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.HostConcurrency())

	for _, host := range hosts {

		if host.Runtime.PowerState == "poweredOn" && host.Runtime.ConnectionState == "connected" && !host.Runtime.InMaintenanceMode {

			dCounter++

			g.Go(func() error {
				esxcliStorageDriverInfo(gctx, ch, c.logger, s, host, &esxclistoragelistSubsystem)
				return nil
			})

		}
	}

	c.logger.Debug("dispatched storage driver routines", "count", dCounter,
		"max_concurrency", s.HostConcurrency())

	// 单台主机失败不中断其他主机，同 esxcli.host.nic。
	_ = g.Wait()

	return nil
}

func esxcliStorageDriverInfo(ctx context.Context, ch chan<- prometheus.Metric, logger *slog.Logger,
	s *collector.Scrape, host mo.HostSystem, subsystem *string) {

	var (
		data StorageResponse
		// 同 esxclihostnic：原先的 map+Mutex 是 check-then-act 非原子写法，
		// 换成读写同锁的 versionSet。这里遍历是串行的（单次 SOAP 返回全部设备），
		// 但用同一个抽象可以避免两处实现分叉。
		revisions = newVersionSet()
	)

	mme, err := esxcli.GetHostMME(ctx, s.Client, &host.Self)
	if err != nil {
		logger.Error("error retrieving host MME", "error", err, "host", host.Name)
		return
	}

	request := esxcli.ExecuteSoapRequest{
		This:    *mme,
		Moid:    "ha-cli-handler-storage-core-device",
		Method:  "vim.EsxCLI.storage.core.device.list",
		Version: "urn:vim25/5.0",
	}

	err = esxcli.GetSOAP(ctx, s.Client, &request, &data)
	if err != nil {
		logger.Error("error fetching soap data", "error", err, "host", host.Name)
		return
	}

	for _, storage := range data.DataObject {

		// 同一 model 的同一 revision 只产出一次，避免重复时间序列。
		if !revisions.Add(storage.Model, storage.Revision) {
			continue
		}

		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(s.Namespace, *subsystem, "driver"),
				"Storage device driver info", nil, map[string]string{"mo": host.Self.Value, "host": host.Name, "vendor": strings.TrimSpace(storage.Vendor), "model": strings.TrimSpace(storage.Model), "revision": strings.TrimSpace(storage.Revision)},
			), prometheus.GaugeValue, float64(1),
		)
	}

}
