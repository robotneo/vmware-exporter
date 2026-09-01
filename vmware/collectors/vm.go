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
	collector.RegisterCollector(vmSubsystem, vmCollectorFlag, NewvmCollector)
}

// NewMeminfoCollector returns a new Collector exposing memory stats.
func NewvmCollector(logger *slog.Logger) (collector.Collector, error) {
	return &vmCollector{logger}, nil
}

func (c *vmCollector) Update(ch chan<- prometheus.Metric, namespace string, clientAPI collector.ClientAPI, loginData map[string]interface{}, params map[string]string) error {

	var (
		vms     []mo.VirtualMachine
		vmRefs  []types.ManagedObjectReference
		vmNames = make(map[string]string)
	)

	begin := time.Now()

	descs := descsFor(namespace).vm
	target := loginData["target"].(string)

	err := fetchProperties(
		loginData["ctx"].(context.Context), loginData["view"].(*view.Manager), loginData["client"].(*vim25.Client),
		[]string{"VirtualMachine"}, []string{"summary", "runtime", "storage", "snapshot", "snapshot.rootSnapshotList", "snapshot.currentSnapshot"}, &vms, c.logger,
	)
	if err != nil {
		return err

	}

	wg := sync.WaitGroup{}

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
			loginData["ctx"].(context.Context),
			loginData["perf"].(*performance.Manager),
			vmRefs[0],
			loginData["interval"].(int32),
			loginData["interval"].(int32),
			targetType(loginData),
			c.logger,
		)

		wg.Add(2)
		for i := 0; i < 2; i++ {
			switch i {
			case 0:
				go func() {
					scrapePerformance(loginData["ctx"].(context.Context), ch, c.logger, loginData["samples"].(int32), interval, loginData["perf"].(*performance.Manager),
						target, "VirtualMachine", namespace, vmSubsystem, "", cVMCounters,
						loginData["counters"].(map[string]*types.PerfCounterInfo), vmRefs, vmNames)
					wg.Done()
				}()

			case 1:
				go func() {
					scrapePerformance(loginData["ctx"].(context.Context), ch, c.logger, loginData["samples"].(int32), interval, loginData["perf"].(*performance.Manager),
						target, "VirtualMachine", namespace, vmSubsystem, "*", iVMCounters,
						loginData["counters"].(map[string]*types.PerfCounterInfo), vmRefs, vmNames)
					wg.Done()
				}()
			}

		}

		wg.Wait()

	}

	c.logger.Debug("time to process perfman for vm", "duration_seconds", time.Since(begin).Seconds())

	return nil
}
