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
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

const (
	vmSubsystem = "vm"
)

var vmCollectorFlag = flag.Bool(fmt.Sprintf("collector.%s", vmSubsystem), collector.DefaultEnabled, fmt.Sprintf("Enable the %s collector (default: %v)", vmSubsystem, collector.DefaultEnabled))
var (
	cVMCounters = []string{"cpu.usagemhz.average", "cpu.demand.average", "cpu.latency.average", "cpu.entitlement.latest",
		"cpu.ready.summation", "cpu.readiness.average", "cpu.costop.summation", "cpu.maxlimited.summation",
		"mem.entitlement.average", "mem.active.average", "mem.shared.average", "mem.vmmemctl.average",
		"mem.swapped.average", "mem.consumed.average", "sys.uptime.latest",
	} //Common or generic counters that need not be instanced
	iVMCounters = []string{"net.bytesRx.average", "net.bytesTx.average",
		"datastore.read.average", "datastore.write.average", "datastore.numberReadAveraged.average",
		"datastore.numberWriteAveraged.average", "datastore.totalReadLatency.average", "datastore.totalWriteLatency.average"} //Counters that come in multiple i
)

type vmCollector struct {
	logger *slog.Logger
}

func init() {
	collector.RegisterFlag(vmSubsystem, vmCollectorFlag)
}

// NewvmCollector returns a new Collector exposing virtual machine stats.
func NewvmCollector(logger *slog.Logger) (collector.Collector, error) {
	return &vmCollector{logger}, nil
}

func (c *vmCollector) Update(ctx context.Context, ch chan<- prometheus.Metric, s *collector.Scrape) error {

	var (
		vms     []mo.VirtualMachine
		vmRefs  []types.ManagedObjectReference
		vmNames = make(map[string]string)
	)

	begin := time.Now()

	descs := descsFor(s.Namespace).vm
	target := s.Target

	err := fetchProperties(
		ctx, s.View, s.Client,
		[]string{"VirtualMachine"}, []string{"summary", "runtime", "storage", "snapshot", "snapshot.rootSnapshotList", "snapshot.currentSnapshot"}, &vms, c.logger,
	)
	if err != nil {
		return err

	}

	for _, vm := range vms {

		if vm.Runtime.PowerState == "poweredOn" {

			vmRefs = append(vmRefs, vm.Self)

			vmNames[vm.Self.Value] = vm.Summary.Config.Name

			moid := vm.Self.Value
			name := vm.Summary.Config.Name
			hostMoid := vm.Runtime.Host.Value

			ch <- prometheus.MustNewConstMetric(descs.info,
				prometheus.GaugeValue, 1.0,
				moid, name, hostMoid, target)

			ch <- prometheus.MustNewConstMetric(descs.cpuCoreCount,
				prometheus.GaugeValue, float64(vm.Summary.Config.NumCpu),
				moid, name, hostMoid, target)

			ch <- prometheus.MustNewConstMetric(descs.memCapacity,
				prometheus.GaugeValue, float64(vm.Summary.Config.MemorySizeMB),
				moid, name, hostMoid, target)

			for _, datastore := range vm.Storage.PerDatastoreUsage {

				// 双写过渡：旧指标名保留（dashboard 有引用），help 已从
				// 错抄的 "Virtual memory configured in MB" 改为正确描述。
				ch <- prometheus.MustNewConstMetric(descs.dsCapacityUsed,
					prometheus.GaugeValue, float64(datastore.Committed),
					moid, name, target, datastore.Datastore.Value)

				ch <- prometheus.MustNewConstMetric(descs.dsCapacityUsedBytes,
					prometheus.GaugeValue, float64(datastore.Committed),
					moid, name, target, datastore.Datastore.Value)
			}

			// 有快照时把创建时间的 Unix 秒数作为 metric value 输出。
			if vm.Snapshot != nil {
				c.logger.Debug("vm has snapshots", "vm", name, "vm_moref", moid)
				for _, rootSnap := range vm.Snapshot.RootSnapshotList {

					// created label 已移除（P1-4）：它是同一个时间戳的
					// RFC3339 形式，而 value 就是 Unix 秒数，label 里那份
					// 纯属冗余。时间戳做 label 会让每个快照占一条独立序列，
					// 快照删除后序列仍以僵尸形式留在 TSDB 里直到过期。
					ch <- prometheus.MustNewConstMetric(descs.snapshotInfo,
						prometheus.GaugeValue, float64(rootSnap.CreateTime.Unix()),
						moid, name, target, rootSnap.Name)
				}
			}
		}

	}

	c.logger.Debug("time to process property collector for vm", "duration_seconds", time.Since(begin).Seconds())

	begin = time.Now()

	if len(vmRefs) > 0 {

		// 与 host 一致：采样间隔以服务端 RefreshRate 为准，
		// 并对 ESXi 额外拦掉 300s 历史间隔。
		interval := resolvePerfIntervalForTarget(
			ctx,
			s.Perf,
			vmRefs[0],
			s.Interval,
			s.Interval,
			targetType(s),
			c.logger,
		)

		// 固定 2 个 goroutine，不随 VM 数量增长。
		wg := sync.WaitGroup{}
		wg.Add(2)

		go func() {
			defer wg.Done()
			scrapePerformance(ctx, ch, c.logger, s.Samples, interval, s.Perf,
				target, "VirtualMachine", s.Namespace, vmSubsystem, "", cVMCounters,
				s.Counters, vmRefs, vmNames)
		}()

		go func() {
			defer wg.Done()
			scrapePerformance(ctx, ch, c.logger, s.Samples, interval, s.Perf,
				target, "VirtualMachine", s.Namespace, vmSubsystem, "*", iVMCounters,
				s.Counters, vmRefs, vmNames)
		}()

		wg.Wait()

	}

	c.logger.Debug("time to process perfman for vm", "duration_seconds", time.Since(begin).Seconds())

	return nil
}
