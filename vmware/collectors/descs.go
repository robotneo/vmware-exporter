package vmwareCollectors

import (
	"fmt"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/vim25/types"
)

// 本文件把 host / vm collector 以及性能指标的 *prometheus.Desc 从采集热路径里
// 提出来，只构造一次（P2-4）。
//
// 原实现在实体循环内部反复调用 prometheus.NewDesc(...)，并把实体的 label 值塞进
// constLabels 参数 —— 这个写法决定了每个实体都必须构造一个独立的 Desc。以 1000
// 台 VM 计，vm collector 单轮采集要造约 4000 个 Desc；性能指标那边更多，是
// 实体数 × 计数器数。每个 Desc 构造都要做 label 校验、排序与 fqName 拼接，
// 全部是可以避免的开销。
//
// 改法：把实体相关的 label 从 constLabels 移到 variableLabels，Desc 复用，
// 采集时通过 MustNewConstMetric 的可变参数传值。
//
// 这不改变任何指标的对外形态 —— Prometheus 文本输出里 const label 与 variable
// label 完全无法区分，两者只在 client_golang 内部有别。
//
// 改造范围只覆盖 host、vm 和性能指标：datacenter / cluster / datastore 的实体数
// 是个位到几十，收益可忽略，而 variableLabels 的顺序错配是静默故障（见下），
// 没必要为了形式统一去扩大风险面。
//
// !!! variableLabels 的顺序即 MustNewConstMetric 传值的顺序 !!!
// 顺序错了不会报错，只会静默把值配到错误的 label 上。每组 Desc 的顺序都写在
// 下面的字段注释里，并由 descs_test.go 断言。

// deprecatedSuffix 是被替换指标 help 文案的统一后缀。
//
// 为什么保留旧指标而不直接改名：它们被随仓库分发的 dashboard 大量引用
// （vmware_host_mem_capacity 29 处、vmware_host_cpu_capacity 20 处）。直接改名会
// 在升级瞬间让所有面板和告警规则失效，而且没有任何报错 —— 用户只会看到图变空。
// 双写一个版本周期给出迁移窗口。
const deprecatedSuffix = " DEPRECATED: use %s instead, this metric will be removed in a future release."

type hostDescs struct {
	// hostmo, host, cmo, vcenter
	info *prometheus.Desc
	// hostmo, host, vendor, model, cpu_type, vcenter
	hardwareInfo *prometheus.Desc
	// hostmo, host, software, version, build, vcenter
	softwareInfo *prometheus.Desc
	// hostmo, host, vcenter
	cpuCoreCount *prometheus.Desc
	// hostmo, host, vcenter
	cpuThreadCount *prometheus.Desc
	// hostmo, host, vcenter -- deprecated, name carries no unit
	cpuCapacity *prometheus.Desc
	// hostmo, host, vcenter
	cpuCapacityMHz *prometheus.Desc
	// hostmo, host, vcenter -- deprecated, help claimed MB but the value is bytes
	memCapacity *prometheus.Desc
	// hostmo, host, vcenter
	memCapacityBytes *prometheus.Desc
}

type vmDescs struct {
	// vmmo, vm, hostmo, vcenter
	info *prometheus.Desc
	// vmmo, vm, hostmo, vcenter
	cpuCoreCount *prometheus.Desc
	// vmmo, vm, hostmo, vcenter
	memCapacity *prometheus.Desc
	// vmmo, vm, vcenter, dsmo -- deprecated, help was copy-pasted from mem_capacity
	dsCapacityUsed *prometheus.Desc
	// vmmo, vm, vcenter, dsmo
	dsCapacityUsedBytes *prometheus.Desc
	// vmmo, vm, vcenter, name
	snapshotInfo *prometheus.Desc
}

// collectorDescs 按 namespace 缓存。namespace 是 Update() 的运行时入参而非编译期
// 常量，所以不能用包级 var 直接构造；上游框架允许调用方覆盖它，把它当常量是错的。
// 实践中全程只有 "vmware" 一个值，测试里会用别的值。
type collectorDescs struct {
	host hostDescs
	vm   vmDescs
}

var (
	descsMu    sync.Mutex
	descsCache = map[string]*collectorDescs{}
)

// descsFor 返回给定 namespace 的 Desc 集合，每个 namespace 只构造一次。
//
// 整个 check-then-act 放在同一把锁内。这里不用 RWMutex 双检：调用频率是
// 每 collector 每轮一次（个位数），锁竞争可以忽略，而 Stage 2 已经踩过
// "读检查在锁外" 的坑，不值得为无关紧要的性能再引入一次。
func descsFor(namespace string) *collectorDescs {
	descsMu.Lock()
	defer descsMu.Unlock()

	if d, ok := descsCache[namespace]; ok {
		return d
	}

	d := buildDescs(namespace)
	descsCache[namespace] = d

	return d
}

func buildDescs(namespace string) *collectorDescs {
	return &collectorDescs{
		host: buildHostDescs(namespace),
		vm:   buildVMDescs(namespace),
	}
}

func buildHostDescs(namespace string) hostDescs {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(
			prometheus.BuildFQName(namespace, hostSubsystem, name),
			help, labels, nil,
		)
	}

	return hostDescs{
		info: d("info",
			"Basic host info.",
			"hostmo", "host", "cmo", "vcenter"),

		hardwareInfo: d("hardware_info",
			"Host hardware information.",
			"hostmo", "host", "vendor", "model", "cpu_type", "vcenter"),

		softwareInfo: d("software_info",
			"Host software information.",
			"hostmo", "host", "software", "version", "build", "vcenter"),

		cpuCoreCount: d("cpu_corecount",
			"Number of physical CPU cores on the host.",
			"hostmo", "host", "vcenter"),

		cpuThreadCount: d("cpu_threadcount",
			"Number of physical CPU threads on the host, i.e. cores times SMT width.",
			"hostmo", "host", "vcenter"),

		cpuCapacity: d("cpu_capacity",
			"Average CPU core frequency in MHz."+
				deprecatedFor(namespace, hostSubsystem, "cpu_capacity_mhz"),
			"hostmo", "host", "vcenter"),

		cpuCapacityMHz: d("cpu_capacity_mhz",
			"Average CPU core frequency in MHz. Multiply by cpu_corecount for total host capacity.",
			"hostmo", "host", "vcenter"),

		// 原 help 写 "Amount of RAM in MB"，但 govmomi 的
		// HostHardwareSummary.MemorySize 是字节（vim25/types/types.go:38904
		// 注释明确写 "The physical memory size in bytes"）。值一直是对的，
		// 错的是文档 —— 所以新指标不做任何换算，只是把单位说清楚。
		memCapacity: d("mem_capacity",
			"Total physical memory of the host in bytes."+
				deprecatedFor(namespace, hostSubsystem, "mem_capacity_bytes"),
			"hostmo", "host", "vcenter"),

		memCapacityBytes: d("mem_capacity_bytes",
			"Total physical memory of the host in bytes.",
			"hostmo", "host", "vcenter"),
	}
}

func buildVMDescs(namespace string) vmDescs {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(
			prometheus.BuildFQName(namespace, vmSubsystem, name),
			help, labels, nil,
		)
	}

	return vmDescs{
		info: d("info",
			"Basic virtual machine info, for joining on parent references.",
			"vmmo", "vm", "hostmo", "vcenter"),

		cpuCoreCount: d("cpu_corecount",
			"Number of virtual CPUs configured for the virtual machine.",
			"vmmo", "vm", "hostmo", "vcenter"),

		// 这个 help 本来就是对的：Summary.Config.MemorySizeMB 确实是 MB。
		// 保持原样，不引入 _bytes 版本 —— 换算单位会改变数值，属于另一类
		// 破坏性变更，不该混进这次的文案修正里。
		memCapacity: d("mem_capacity",
			"Virtual memory configured for the virtual machine in MB.",
			"vmmo", "vm", "hostmo", "vcenter"),

		// 原 help 是从 mem_capacity 错抄过来的 "Virtual memory configured in MB"。
		// 实际数据源是 VirtualMachineUsageOnDatastore.Committed，
		// govmomi 注释为 "Storage space, in bytes, ... actually being used"
		// （vim25/types/types.go:93167）。
		dsCapacityUsed: d("datastore_capacity_used",
			"Storage committed by this virtual machine on the datastore, in bytes."+
				deprecatedFor(namespace, vmSubsystem, "datastore_capacity_used_bytes"),
			"vmmo", "vm", "vcenter", "dsmo"),

		dsCapacityUsedBytes: d("datastore_capacity_used_bytes",
			"Storage committed by this virtual machine on the datastore, in bytes. "+
				"Includes disks, logs, snapshots and configuration files.",
			"vmmo", "vm", "vcenter", "dsmo"),

		// created label 已移除（P1-4）：它是 CreateTime 的 RFC3339 形式，而
		// metric value 就是同一个时间戳的 Unix 秒数，label 里那份完全冗余。
		// 时间戳做 label 是 Prometheus 反模式 —— 每个快照一条独立序列，
		// 快照删掉后序列还会以僵尸形式留在 TSDB 里直到过期。
		snapshotInfo: d("snapshot_info",
			"Unix timestamp of the snapshot creation time.",
			"vmmo", "vm", "vcenter", "name"),
	}
}

// deprecatedFor 生成指向替代指标的 deprecation 说明。
// 替代指标名走 BuildFQName 拼接而不是手写字符串，避免 help 里的名字与
// 实际注册的指标名脱钩。
func deprecatedFor(namespace, subsystem, replacement string) string {
	return fmt.Sprintf(deprecatedSuffix,
		prometheus.BuildFQName(namespace, subsystem, replacement))
}

// perfDescKey 唯一标识一个性能指标的 Desc。
//
// counter 名决定 fqName 与 help，moType 决定 label 集合（host/vm/ds 各不同），
// instanced 决定是否带 pfinstance label。这四者一致即可复用同一个 Desc。
type perfDescKey struct {
	namespace string
	subsystem string
	counter   string
	moType    string
	instanced bool
}

var (
	perfDescMu    sync.Mutex
	perfDescCache = map[perfDescKey]*prometheus.Desc{}
)

// perfEntityLabels 返回给定实体类型的 label 名，顺序即传值顺序。
//
// 返回 nil 表示不认识的类型 —— 调用方据此跳过，而不是产出一条只带 vcenter
// 的指标。原实现的 switch 没有 default，未知类型会静默丢掉实体标识，
// 结果是同一实体类型下所有实体的序列互相覆盖。
func perfEntityLabels(moType string) []string {
	switch moType {
	case "HostSystem":
		return []string{"host", "hostmo"}
	case "VirtualMachine":
		return []string{"vm", "vmmo"}
	case "Datastore":
		return []string{"ds", "dsmo"}
	default:
		return nil
	}
}

// perfDesc 取（或构造）一个性能指标的 Desc。
//
// 这是 P2-4 中收益最大的一处：原实现在 metric × value 的双层循环里每次都
// NewDesc，规模是实体数 × 计数器数。1000 台 VM × 15 个计数器就是 15000 次，
// 远超方案里估算的 4000。
func perfDesc(namespace, subsystem, counter, moType string, instanced bool, counterInfo *types.PerfCounterInfo) *prometheus.Desc {
	key := perfDescKey{
		namespace: namespace,
		subsystem: subsystem,
		counter:   counter,
		moType:    moType,
		instanced: instanced,
	}

	perfDescMu.Lock()
	defer perfDescMu.Unlock()

	if d, ok := perfDescCache[key]; ok {
		return d
	}

	labels := append([]string{"vcenter"}, perfEntityLabels(moType)...)
	if instanced {
		labels = append(labels, "pfinstance")
	}

	d := prometheus.NewDesc(
		prometheus.BuildFQName(namespace, subsystem, strings.ReplaceAll(counter, ".", "_")),
		fmt.Sprintf("%s in %s ",
			counterInfo.UnitInfo.GetElementDescription().Label,
			counterInfo.NameInfo.GetElementDescription().Summary,
		),
		labels, nil,
	)

	perfDescCache[key] = d

	return d
}
