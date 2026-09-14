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

		// 实体级自监控：本轮发现的 VM 数、perf 抓取因非开机态跳过的数量。
		// 跳过的是性能计数器（vCenter 对关机 VM 本就没有实时数据），
		// info/容量/状态指标对全部 VM 无条件输出 —— 见 DESIGN-p1-p3-roadmap 2.1。
		totalVMs   int
		skippedVMs int
		// 预填 vm 数据面关心的全部跳过原因：正常状态下也输出 0 值序列，
		// 这样「有 VM 被跳过」的告警不必用 absent()/or 兜底。
		skipReasons = map[string]int{
			collector.SkipReasonPoweredOff: 0,
			collector.SkipReasonSuspended:  0,
		}
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

	// -metrics.legacy 在循环外快照一次，理由见 emitLegacyNames：
	// 循环里裸读既是与 SIGHUP 重载的数据竞争，也会让一次抓取的前后半段
	// 用上不同的值 —— 这里尤其明显，两处读点分别在 VM 层与 datastore
	// 内层循环，一次落在中间的重载能让同一台 VM 的内存旧名有、
	// 存储旧名没有。
	legacy := emitLegacyNames()

	for _, vm := range vms {
		totalVMs++

		moid := vm.Self.Value
		name := vm.Summary.Config.Name

		// runtime.host 在关机/孤立 VM 上可能为空引用，hostmo 退化为空串
		// 而不是解引用一个零值 ManagedObjectReference（那会得到字符串 "<nil>"）。
		hostMoid := ""
		if vm.Runtime.Host != nil {
			hostMoid = vm.Runtime.Host.Value
		}

		// uuid 取自 summary.config.uuid（VirtualMachineConfigSummary 自带，
		// 不需要额外的 config 往返）。不可访问的 VM 上 summary.config 为
		// 空对象，uuid 就是空串 —— 降级为无值 label，不阻断采集。
		uuid := vm.Summary.Config.Uuid

		// 电源状态对每台 VM 输出（含关机/挂起）。正是这条让 Prometheus
		// 能区分"VM 不存在"与"VM 存在但没开机"。
		powerState := string(vm.Runtime.PowerState)
		if powerState == "" {
			powerState = "unknown"
		}

		ch <- prometheus.MustNewConstMetric(descs.powerState,
			prometheus.GaugeValue, 1.0,
			moid, name, powerState, target)

		ch <- prometheus.MustNewConstMetric(descs.overallStatus,
			prometheus.GaugeValue, 1.0,
			moid, name, string(vm.Summary.OverallStatus), target)

		// _info 与配置/容量类指标对所有 VM 无条件输出，包括关机 VM。
		// 旧版本这些指标被电源态门控，v0.2.0 起显式可见 —— 这是 breaking
		// change，迁移写法见 CHANGELOG。
		ch <- prometheus.MustNewConstMetric(descs.info,
			prometheus.GaugeValue, 1.0,
			moid, name, hostMoid, uuid, target)

		ch <- prometheus.MustNewConstMetric(descs.cpuCoreCount,
			prometheus.GaugeValue, float64(vm.Summary.Config.NumCpu),
			moid, name, hostMoid, target)

		// MemorySizeMB 的 MB 是 2^20 字节。
		ch <- prometheus.MustNewConstMetric(descs.memCapacityBytes,
			prometheus.GaugeValue, float64(vm.Summary.Config.MemorySizeMB)*1048576,
			moid, name, hostMoid, target)

		if legacy {
			ch <- prometheus.MustNewConstMetric(descs.memCapacity,
				prometheus.GaugeValue, float64(vm.Summary.Config.MemorySizeMB),
				moid, name, hostMoid, target)
		}

		// PerDatastoreUsage 在关机 VM 上通常为空；Storage 本身是指针，
		// VM 不可访问时为 nil，必须判空（旧代码只有开机分支会走到这里）。
		if vm.Storage != nil {
			for _, datastore := range vm.Storage.PerDatastoreUsage {

				ch <- prometheus.MustNewConstMetric(descs.dsCapacityUsedBytes,
					prometheus.GaugeValue, float64(datastore.Committed),
					moid, name, target, datastore.Datastore.Value)

				// 旧名保留原值（本来就是字节，换算是 ×1）。
				if legacy {
					ch <- prometheus.MustNewConstMetric(descs.dsCapacityUsed,
						prometheus.GaugeValue, float64(datastore.Committed),
						moid, name, target, datastore.Datastore.Value)
				}
			}
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

		// 只有 perf 计数器仍按电源态跳过：vCenter 对非开机 VM 没有实时
		// 采样，查询它们只会得到空结果并浪费分块配额。
		if vm.Runtime.PowerState != "poweredOn" {
			skippedVMs++
			reason := collector.SkipReasonPoweredOff
			if vm.Runtime.PowerState == "suspended" {
				reason = collector.SkipReasonSuspended
			}
			skipReasons[reason]++
			c.logger.Debug("skipping perf counters for non-powered-on vm",
				"vm", name, "vm_moref", moid, "power_state", powerState)
			continue
		}

		vmRefs = append(vmRefs, vm.Self)
		vmNames[vm.Self.Value] = name
	}

	s.RecordEntities(vmSubsystem, "vm", totalVMs, totalVMs-skippedVMs, skipReasons)

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
				s.Counters, vmRefs, vmNames, s.PerfChunkSize, s.HostConcurrency())
		}()

		go func() {
			defer wg.Done()
			scrapePerformance(ctx, ch, c.logger, s.Samples, interval, s.Perf,
				target, "VirtualMachine", s.Namespace, vmSubsystem, "*", iVMCounters,
				s.Counters, vmRefs, vmNames, s.PerfChunkSize, s.HostConcurrency())
		}()

		wg.Wait()

	}

	c.logger.Debug("time to process perfman for vm", "duration_seconds", time.Since(begin).Seconds())

	return nil
}
