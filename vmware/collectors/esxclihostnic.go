package vmwareCollectors

import (
	"context"
	"flag"
	"fmt"
	"log/slog"

	"github.com/prezhdarov/vmware-exporter/vmware/esxcli"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/vim25/mo"
	"golang.org/x/sync/errgroup"
)

type DriverInfo struct {
	Driver   string `xml:"Driver"`
	Version  string `xml:"Version"`
	Firmware string `xml:"FirmwareVersion"`
}

type NicResponse struct {
	DriverInfo DriverInfo `xml:"DriverInfo"`
}

type NicListInfo struct {
	Name        string `xml:"Name"`
	Description string `xml:"Description"`
}

type NicListResponse struct {
	DataObject []NicListInfo `xml:"DataObject"`
}

var (
	esxclihostnicSubsystem = "esxcli_host_nic"
)

var esxclihostnicCollectorFlag = flag.Bool("collector.esxcli.host.nic", collector.DefaultDisabled, fmt.Sprintf("Enable the %s collector (default: %v)", esxclihostnicSubsystem, collector.DefaultDisabled))

type esxclihostnicCollector struct {
	logger *slog.Logger
}

func init() {
	collector.RegisterFlag("esxcli.host.nic", esxclihostnicCollectorFlag)
}

func NewesxcliHostNICCollector(logger *slog.Logger) (collector.Collector, error) {
	return &esxclihostnicCollector{logger}, nil
}

func (c *esxclihostnicCollector) Update(ctx context.Context, ch chan<- prometheus.Metric, s *collector.Scrape) error {

	// 与 host collector 共享同一份 HostSystem 检索结果。改动前这里自己
	// 又拉一遍（属性集是 runtime/name/config/hardware），三个主机相关的
	// collector 全开时同一份清单被检索三次。
	hosts, err := s.Hosts(ctx, fetchHosts(c.logger))
	if err != nil {
		return err

	}

	// 每主机一个 goroutine，每张网卡再一个 —— 这是无界 fan-out 的第二和
	// 第三层。500 主机 × 4 网卡 = 2500 个 goroutine 同时打同一个 vCenter。
	//
	// errgroup.SetLimit 给这两层都加了上限。用 s.MaxConcurrency 而非一个
	// 新参数：并发预算是整轮抓取的属性，不该每层各配一个旋钮。
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.HostConcurrency())

	for _, host := range hosts {

		if host.Runtime.PowerState == "poweredOn" && host.Runtime.ConnectionState == "connected" && !host.Runtime.InMaintenanceMode {

			g.Go(func() error {
				esxcliHostNicInfo(gctx, ch, c.logger, s, host, &esxclihostnicSubsystem)
				return nil
			})

		}

	}

	// 闭包永远返回 nil：单台主机的 esxcli 失败不该让其他主机的采集也中断。
	// 失败已经通过日志暴露，且整个 collector 的成败由上层的 success 指标表达。
	_ = g.Wait()

	return nil
}

func esxcliHostNicInfo(ctx context.Context, ch chan<- prometheus.Metric, logger *slog.Logger,
	s *collector.Scrape, host mo.HostSystem, subsystem *string) {

	var (
		data     NicListResponse
		drivers  = newVersionSet()
		firmware = newVersionSet()
	)

	mme, err := esxcli.GetHostMME(ctx, s.Client, &host.Self)
	if err != nil {
		logger.Error("error retrieving host MME", "error", err, "host", host.Name)
		return
	}

	request := esxcli.ExecuteSoapRequest{
		This:    *mme,
		Moid:    "ha-cli-handler-network-nic",
		Method:  "vim.EsxCLI.network.nic.list",
		Version: "urn:vim25/5.0",
	}

	err = esxcli.GetSOAP(ctx, s.Client, &request, &data)
	if err != nil {
		logger.Error("error retrieving nic list", "error", err, "host", host.Name)
		return
	}

	request.Method = "vim.EsxCLI.network.nic.get"

	// 每张网卡一次 SOAP 往返，串行会让网卡多的主机显著拖慢整轮抓取。
	// 并发是安全的：versionSet 内部读写在同一把锁内完成，
	// 且 prometheus.Metric channel 本身支持多 goroutine 写入。
	//
	// 这一层同样受 HostConcurrency 限制。注意上层已经占用了并发预算，
	// 所以这里是「每台主机内部再限流」而非全局限流 —— 全局精确限流需要
	// 一个跨层共享的 semaphore，那是 Stage 13 批量化 nic.get 时要做的事，
	// 届时这一层会整体消失。
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.HostConcurrency())

	for _, nic := range data.DataObject {
		g.Go(func() error {
			esxcliGetNicInfo(gctx, ch, logger, s, request,
				&host.Self.Value, &host.Name, &s.Namespace, subsystem, &nic,
				drivers, firmware)
			return nil
		})
	}

	_ = g.Wait()
}

func esxcliGetNicInfo(ctx context.Context, ch chan<- prometheus.Metric, logger *slog.Logger,
	s *collector.Scrape, request esxcli.ExecuteSoapRequest,
	hostRef, hostName, namespace, subsystem *string, nic *NicListInfo, drivers, firmware *versionSet) {

	var data NicResponse

	request.Argument = esxcli.ConfigArguments(map[string]string{"nicname": nic.Name})

	err := esxcli.GetSOAP(ctx, s.Client, &request, &data)
	if err != nil {
		logger.Error("error fetching soap data", "error", err, "host", *hostName)
		return
	}

	// 两个 Add 都要执行，不能短路：driver 版本与 firmware 版本各自独立去重，
	// 任一为首次出现就应当产出指标。用 | 而非 || 正是为此。
	newDriver := drivers.Add(data.DriverInfo.Driver, data.DriverInfo.Version)
	newFirmware := firmware.Add(data.DriverInfo.Driver, data.DriverInfo.Firmware)

	if newDriver || newFirmware {
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(*namespace, *subsystem, "driver"),
				"NIC Info", nil, map[string]string{"mo": *hostRef, "host": *hostName, "descr": nic.Description, "driver": data.DriverInfo.Driver, "version": data.DriverInfo.Version, "firmware": data.DriverInfo.Firmware},
			), prometheus.GaugeValue, float64(1),
		)
	}

}
