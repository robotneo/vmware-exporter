package vmwareCollectors

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prezhdarov/prometheus-exporter/pkg/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

const (
	hostSubsystem = "host"
)

var hostCollectorFlag = flag.Bool(fmt.Sprintf("collector.%s", hostSubsystem), collector.DefaultEnabled, fmt.Sprintf("Enable the %s collector (default: %v)", hostSubsystem, collector.DefaultEnabled))

var (
	cHostCounters = []string{"cpu.usagemhz.average", "cpu.demand.average", "cpu.latency.average", "cpu.entitlement.latest",
		"cpu.ready.summation", "cpu.readiness.average", "cpu.costop.summation", "cpu.maxlimited.summation",
		"mem.entitlement.average", "mem.active.average", "mem.shared.average", "mem.vmmemctl.average",
		"mem.swapped.average", "mem.consumed.average", "sys.uptime.latest",
	} //Common or generic counters that need not be instanced
	iHostCounters = []string{"net.bytesRx.average", "net.bytesTx.average", "net.errorsRx.summation", "net.errorsTx.summation", "net.droppedRx.summation", "net.droppedTx.summation",
		"datastore.read.average", "datastore.write.average", "datastore.numberReadAveraged.average",
		"datastore.numberWriteAveraged.average", "datastore.totalReadLatency.average", "datastore.totalWriteLatency.average",
	} //Counters that come in multiple instances

)

type hostCollector struct {
	logger *slog.Logger
}

func init() {
	collector.RegisterCollector("host", hostCollectorFlag, NewhostCollector)
}

func NewhostCollector(logger *slog.Logger) (collector.Collector, error) {
	return &hostCollector{logger}, nil
}

func (c *hostCollector) Update(ch chan<- prometheus.Metric, namespace string, clientAPI collector.ClientAPI, loginData map[string]interface{}, params map[string]string) error {

	var (
		hosts     []mo.HostSystem
		hostRefs  []types.ManagedObjectReference
		hostNames = make(map[string]string)
	)

	begin := time.Now()

	descs := descsFor(namespace).host
	target := loginData["target"].(string)

	err := fetchProperties(
		loginData["ctx"].(context.Context), loginData["view"].(*view.Manager), loginData["client"].(*vim25.Client),
		[]string{"HostSystem"}, []string{"parent", "summary", "runtime"}, &hosts, c.logger,
	)
	if err != nil {
		return err

	}

	wg := sync.WaitGroup{}

	for _, host := range hosts {

		if host.Runtime.PowerState == "poweredOn" && host.Runtime.ConnectionState == "connected" && !host.Runtime.InMaintenanceMode {

			hostRefs = append(hostRefs, host.Self)

			hostNames[host.Self.Value] = host.Summary.Config.Name

			c.logger.Debug("gathering metrics for host", "host", host.Summary.Config.Name, "host_moref", host.Self.Value)

			moid := host.Self.Value
			name := host.Summary.Config.Name
			hw := host.Summary.Hardware
			product := host.Summary.Config.Product

			ch <- prometheus.MustNewConstMetric(descs.info,
				prometheus.GaugeValue, 1.0,
				moid, name, host.Parent.Value, target)

			ch <- prometheus.MustNewConstMetric(descs.hardwareInfo,
				prometheus.GaugeValue, 1.0,
				moid, name, hw.Vendor, hw.Model, hw.CpuModel, target)

			ch <- prometheus.MustNewConstMetric(descs.softwareInfo,
				prometheus.GaugeValue, 1.0,
				moid, name, product.Name, product.Version, product.Build, target)

			ch <- prometheus.MustNewConstMetric(descs.cpuCoreCount,
				prometheus.GaugeValue, float64(hw.NumCpuCores),
				moid, name, target)

			ch <- prometheus.MustNewConstMetric(descs.cpuThreadCount,
				prometheus.GaugeValue, float64(hw.NumCpuThreads),
				moid, name, target)

			// 双写过渡：无单位后缀的旧指标与带 _mhz / _bytes 的新指标同时输出。
			// 旧指标的 help 已标注 deprecated，值不变 —— 唯一变化是文案，
			// 所以现有 dashboard 不需要任何改动就能继续工作。
			ch <- prometheus.MustNewConstMetric(descs.cpuCapacity,
				prometheus.GaugeValue, float64(hw.CpuMhz),
				moid, name, target)

			ch <- prometheus.MustNewConstMetric(descs.cpuCapacityMHz,
				prometheus.GaugeValue, float64(hw.CpuMhz),
				moid, name, target)

			ch <- prometheus.MustNewConstMetric(descs.memCapacity,
				prometheus.GaugeValue, float64(hw.MemorySize),
				moid, name, target)

			ch <- prometheus.MustNewConstMetric(descs.memCapacityBytes,
				prometheus.GaugeValue, float64(hw.MemorySize),
				moid, name, target)

		}
	}

	c.logger.Debug("time to process property collector for host", "duration_seconds", time.Since(begin).Seconds())

	c.logger.Debug("max perf samples configured", "samples", loginData["samples"].(int32))

	begin = time.Now()

	if len(hostRefs) > 0 {

		// 采样间隔向服务端协商。ESXi 上用户传入的 -vmware.interval 可能与
		// 服务端 RefreshRate 不符，此时以服务端为准 —— 请求一个服务端没有的
		// 间隔只会得到空结果。
		//
		// 用 ForTarget 变体而非裸的 resolvePerfInterval：ESXi 的
		// ProviderSummary 可能声称 SummarySupported，但它不跑汇总服务，
		// 落到 300s 历史间隔上就查不到数据。详见 targettype.go 的说明。
		interval := resolvePerfIntervalForTarget(
			loginData["ctx"].(context.Context),
			loginData["perf"].(*performance.Manager),
			hostRefs[0],
			loginData["interval"].(int32),
			loginData["interval"].(int32),
			targetType(loginData),
			c.logger,
		)

		wg.Add(2)
		for i := 0; i < 2; i++ {
			switch i {
			case 0:
				go func(i int) {
					scrapePerformance(loginData["ctx"].(context.Context), ch, c.logger, loginData["samples"].(int32), interval, loginData["perf"].(*performance.Manager),
						target, "HostSystem", namespace, hostSubsystem, "", cHostCounters,
						loginData["counters"].(map[string]*types.PerfCounterInfo), hostRefs, hostNames)
					wg.Done()
				}(i)

			case 1:
				go func(i int) {
					scrapePerformance(loginData["ctx"].(context.Context), ch, c.logger, loginData["samples"].(int32), interval, loginData["perf"].(*performance.Manager),
						target, "HostSystem", namespace, hostSubsystem, "*", iHostCounters,
						loginData["counters"].(map[string]*types.PerfCounterInfo), hostRefs, hostNames)
					wg.Done()
				}(i)
			}

		}

		wg.Wait()
	}
	c.logger.Debug("time to process perfman for host", "duration_seconds", time.Since(begin).Seconds())

	return nil
}
