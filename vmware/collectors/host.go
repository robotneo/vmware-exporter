package vmwareCollectors

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prometheus/client_golang/prometheus"
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
	collector.RegisterFlag("host", hostCollectorFlag)
}

func NewhostCollector(logger *slog.Logger) (collector.Collector, error) {
	return &hostCollector{logger}, nil
}

func (c *hostCollector) Update(ctx context.Context, ch chan<- prometheus.Metric, s *collector.Scrape) error {

	var (
		hostRefs  []types.ManagedObjectReference
		hostNames = make(map[string]string)
	)

	begin := time.Now()

	descs := descsFor(s.Namespace).host
	target := s.Target

	// 请求内共享：host、esxcli.host.nic、esxcli.storage 三个 collector 原先
	// 各自检索一遍 HostSystem。s.Hosts 用 sync.Once 保证一轮抓取里只取一次。
	// 属性集是三者需求的并集，所以这里会多拿到 config 与 hardware ——
	// 代价远小于省下的两次 ContainerView 创建/销毁往返。
	hosts, err := s.Hosts(ctx, fetchHosts(c.logger))
	if err != nil {
		return err

	}

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

			// MHz → hertz、MB → bytes 的换算落在新指标上；旧指标只在
			// -metrics.legacy=true 时输出，且保留原始取值。
			ch <- prometheus.MustNewConstMetric(descs.cpuCapacityHertz,
				prometheus.GaugeValue, float64(hw.CpuMhz)*1e6,
				moid, name, target)

			ch <- prometheus.MustNewConstMetric(descs.memCapacityBytes,
				prometheus.GaugeValue, float64(hw.MemorySize),
				moid, name, target)

			if !*legacyMetrics {
				continue
			}

			ch <- prometheus.MustNewConstMetric(descs.cpuCapacity,
				prometheus.GaugeValue, float64(hw.CpuMhz),
				moid, name, target)

			ch <- prometheus.MustNewConstMetric(descs.cpuCapacityMHz,
				prometheus.GaugeValue, float64(hw.CpuMhz),
				moid, name, target)

			ch <- prometheus.MustNewConstMetric(descs.memCapacity,
				prometheus.GaugeValue, float64(hw.MemorySize),
				moid, name, target)

		}
	}

	c.logger.Debug("time to process property collector for host", "duration_seconds", time.Since(begin).Seconds())

	c.logger.Debug("max perf samples configured", "samples", s.Samples)

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
			ctx,
			s.Perf,
			hostRefs[0],
			s.Interval,
			s.Interval,
			targetType(s),
			c.logger,
		)

		// 两轮采样：一轮非 instanced 计数器，一轮 instanced。
		//
		// 这里的 goroutine 数是固定的 2，不随主机数增长，所以不需要
		// 并发上限 —— 上限管的是 collector 层的 fan-out（见
		// internal/collector.CollectorSet.Collect 的 SetLimit）。
		wg := sync.WaitGroup{}
		wg.Add(2)

		go func() {
			defer wg.Done()
			scrapePerformance(ctx, ch, c.logger, s.Samples, interval, s.Perf,
				target, "HostSystem", s.Namespace, hostSubsystem, "", cHostCounters,
				s.Counters, hostRefs, hostNames)
		}()

		go func() {
			defer wg.Done()
			scrapePerformance(ctx, ch, c.logger, s.Samples, interval, s.Perf,
				target, "HostSystem", s.Namespace, hostSubsystem, "*", iHostCounters,
				s.Counters, hostRefs, hostNames)
		}()

		wg.Wait()
	}
	c.logger.Debug("time to process perfman for host", "duration_seconds", time.Since(begin).Seconds())

	return nil
}
