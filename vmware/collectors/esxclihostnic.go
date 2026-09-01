package vmwareCollectors

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"sync"

	"github.com/prezhdarov/vmware-exporter/vmware/esxcli"

	"github.com/prezhdarov/prometheus-exporter/pkg/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
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
	collector.RegisterCollector("esxcli.host.nic", esxclihostnicCollectorFlag, NewesxcliHostNICCollector)
}

func NewesxcliHostNICCollector(logger *slog.Logger) (collector.Collector, error) {
	return &esxclihostnicCollector{logger}, nil
}

func (c *esxclihostnicCollector) Update(ch chan<- prometheus.Metric, namespace string, clientAPI collector.ClientAPI, loginData map[string]interface{}, params map[string]string) error {

	var (
		hosts []mo.HostSystem
	)

	err := fetchProperties(
		loginData["ctx"].(context.Context), loginData["view"].(*view.Manager), loginData["client"].(*vim25.Client),
		[]string{"HostSystem"}, []string{"runtime", "name", "config", "hardware"}, &hosts, c.logger,
	)
	if err != nil {
		return err

	}

	wg := sync.WaitGroup{}

	for _, host := range hosts {

		if host.Runtime.PowerState == "poweredOn" && host.Runtime.ConnectionState == "connected" && !host.Runtime.InMaintenanceMode {

			wg.Add(1)

			go func(host mo.HostSystem) {

				esxcliHostNicInfo(ch, c.logger, loginData["ctx"].(context.Context), loginData["client"].(*vim25.Client),
					host, &namespace, &esxclihostnicSubsystem)
				wg.Done()
			}(host)

		}

	}

	wg.Wait()

	return nil
}

func esxcliHostNicInfo(ch chan<- prometheus.Metric, logger *slog.Logger, ctx context.Context, client *vim25.Client,
	host mo.HostSystem, namespace, subsystem *string) {

	var (
		data     NicListResponse
		drivers  = newVersionSet()
		firmware = newVersionSet()
	)

	mme, err := esxcli.GetHostMME(ctx, client, &host.Self)
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

	err = esxcli.GetSOAP(ctx, client, &request, &data)
	if err != nil {
		logger.Error("error retrieving nic list", "error", err, "host", host.Name)
		return
	}

	request.Method = "vim.EsxCLI.network.nic.get"

	// 每张网卡一次 SOAP 往返，串行会让网卡多的主机显著拖慢整轮抓取。
	// 并发是安全的：versionSet 内部读写在同一把锁内完成，
	// 且 prometheus.Metric channel 本身支持多 goroutine 写入。
	wg := sync.WaitGroup{}

	for _, nic := range data.DataObject {
		wg.Add(1)

		go func(nic NicListInfo) {
			defer wg.Done()

			esxcliGetNicInfo(ch, logger, ctx, client, request,
				&host.Self.Value, &host.Name, namespace, subsystem, &nic,
				drivers, firmware)
		}(nic)
	}

	wg.Wait()
}

func esxcliGetNicInfo(ch chan<- prometheus.Metric, logger *slog.Logger, ctx context.Context, client *vim25.Client, request esxcli.ExecuteSoapRequest,
	hostRef, hostName, namespace, subsystem *string, nic *NicListInfo, drivers, firmware *versionSet) {

	var data NicResponse

	request.Argument = esxcli.ConfigArguments(map[string]string{"nicname": nic.Name})

	err := esxcli.GetSOAP(ctx, client, &request, &data)
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
