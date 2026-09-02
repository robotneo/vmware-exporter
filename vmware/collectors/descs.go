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
	// hostmo, host, vcenter -- legacy, name carries no unit
	cpuCapacity *prometheus.Desc
	// hostmo, host, vcenter -- legacy, MHz is not a Prometheus base unit
	cpuCapacityMHz *prometheus.Desc
	// hostmo, host, vcenter
	cpuCapacityHertz *prometheus.Desc
	// hostmo, host, vcenter -- legacy, name carries no unit
	memCapacity *prometheus.Desc
	// hostmo, host, vcenter
	memCapacityBytes *prometheus.Desc
}

type vmDescs struct {
	// vmmo, vm, hostmo, vcenter
	info *prometheus.Desc
	// vmmo, vm, hostmo, vcenter
	cpuCoreCount *prometheus.Desc
	// vmmo, vm, hostmo, vcenter -- legacy, value is MB
	memCapacity *prometheus.Desc
	// vmmo, vm, hostmo, vcenter
	memCapacityBytes *prometheus.Desc
	// vmmo, vm, vcenter, dsmo -- legacy, name carries no unit
	dsCapacityUsed *prometheus.Desc
	// vmmo, vm, vcenter, dsmo
	dsCapacityUsedBytes *prometheus.Desc
	// vmmo, vm, vcenter, name
	snapshotInfo *prometheus.Desc
}

// datastoreDescs 覆盖 datastore collector 的静态指标。
//
// 原实现在实体循环里内联 NewDesc 并把实体标识塞进 constLabels，与 host/vm
// 改造前一样。datastore 数量是几十级别，性能不是理由 —— 提出来是因为
// capacity / free 要做 legacy 双写，而双写要求新旧两条的 help 引用同一个
// 替代名，内联写法做不到这一点而不重复字符串。
type datastoreDescs struct {
	// dsmo, ds, type, pfinstance, foldermo, vcenter
	info *prometheus.Desc
	// dsmo, ds, vcenter -- legacy, name carries no unit
	capacity *prometheus.Desc
	// dsmo, ds, vcenter
	capacityBytes *prometheus.Desc
	// dsmo, ds, vcenter -- legacy, name carries no unit
	free *prometheus.Desc
	// dsmo, ds, vcenter
	freeBytes *prometheus.Desc
	// dsmo, ds, vcenter
	accessible *prometheus.Desc
}

// collectorDescs 按 namespace 缓存。namespace 是 Update() 的运行时入参而非编译期
// 常量，所以不能用包级 var 直接构造；上游框架允许调用方覆盖它，把它当常量是错的。
// 实践中全程只有 "vmware" 一个值，测试里会用别的值。
type collectorDescs struct {
	host      hostDescs
	vm        vmDescs
	datastore datastoreDescs
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
		host:      buildHostDescs(namespace),
		vm:        buildVMDescs(namespace),
		datastore: buildDatastoreDescs(namespace),
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

		// 三个 legacy 指标（cpu_capacity、cpu_capacity_mhz、mem_capacity）都只在
		// -metrics.legacy=true 时输出。此前 cpu_capacity / mem_capacity 是无条件
		// 双写的，那让「默认给出一套干净指标集」这个承诺只对性能指标成立。
		cpuCapacity: d("cpu_capacity",
			"Average CPU core frequency in MHz."+
				deprecatedFor(namespace, hostSubsystem, "cpu_capacity_hertz"),
			"hostmo", "host", "vcenter"),

		// MHz 不是 Prometheus 的基础单位，promlint 会明确点出这一条。
		// _mhz 是上一轮过渡引入的，这轮直接跳到 _hertz，_mhz 一并降级为 legacy。
		cpuCapacityMHz: d("cpu_capacity_mhz",
			"Average CPU core frequency in MHz."+
				deprecatedFor(namespace, hostSubsystem, "cpu_capacity_hertz"),
			"hostmo", "host", "vcenter"),

		cpuCapacityHertz: d("cpu_capacity_hertz",
			"Average CPU core frequency in hertz. Multiply by cpu_corecount for total host capacity.",
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

		// Summary.Config.MemorySizeMB 确实是 MB，所以旧 help 本身没错 ——
		// 错的是把非基础单位暴露给 Prometheus。新指标换算成字节（×1048576，
		// MB 在这里是 2^20 而非 10^6），旧指标降级为 legacy 保留原值。
		memCapacity: d("mem_capacity",
			"Virtual memory configured for the virtual machine in MB."+
				deprecatedFor(namespace, vmSubsystem, "mem_capacity_bytes"),
			"vmmo", "vm", "hostmo", "vcenter"),

		memCapacityBytes: d("mem_capacity_bytes",
			"Virtual memory configured for the virtual machine in bytes.",
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
			"Storage committed by this virtual machine on the datastore, in bytes."+
				" Includes disks, logs, snapshots and configuration files.",
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

func buildDatastoreDescs(namespace string) datastoreDescs {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(
			prometheus.BuildFQName(namespace, datastoreSubsystem, name),
			help, labels, nil,
		)
	}

	return datastoreDescs{
		info: d("info",
			"Datastore info, for joining on parent references.",
			"dsmo", "ds", "type", "pfinstance", "foldermo", "vcenter"),

		// capacity / free 的值本来就是字节，换算是 ×1，改动只在名字上。
		capacity: d("capacity",
			"Datastore capacity in bytes."+
				deprecatedFor(namespace, datastoreSubsystem, "capacity_bytes"),
			"dsmo", "ds", "vcenter"),

		capacityBytes: d("capacity_bytes",
			"Datastore capacity in bytes.",
			"dsmo", "ds", "vcenter"),

		free: d("free",
			"Datastore available space in bytes."+
				deprecatedFor(namespace, datastoreSubsystem, "free_bytes"),
			"dsmo", "ds", "vcenter"),

		freeBytes: d("free_bytes",
			"Datastore available space in bytes.",
			"dsmo", "ds", "vcenter"),

		// accessible 不改名：它是无单位的布尔量，_bytes 之类的后缀不适用，
		// 而 promlint 对这个名字没有意见。
		accessible: d("accessible",
			"Whether the datastore is accessible.",
			"dsmo", "ds", "vcenter"),
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
// instanced 决定是否带 pfinstance label，legacy 决定用新名还是旧名。这五者
// 一致即可复用同一个 Desc。
type perfDescKey struct {
	namespace string
	subsystem string
	counter   string
	moType    string
	instanced bool
	legacy    bool
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
//
// legacy 为 true 时返回旧命名的 Desc（计数器名直接把 "." 换成 "_"），help 带
// DEPRECATED 前缀。双写过渡期内同一个计数器会同时取两个 Desc，因此 legacy
// 必须进 cache key —— 否则先取到的那个会被另一个复用，产出一条名字对不上
// help 的序列。
func perfDesc(namespace, subsystem, counter, moType string, instanced, legacy bool, counterInfo *types.PerfCounterInfo) *prometheus.Desc {
	key := perfDescKey{
		namespace: namespace,
		subsystem: subsystem,
		counter:   counter,
		moType:    moType,
		instanced: instanced,
		legacy:    legacy,
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

	// help 的原实现末尾多一个空格（"%s in %s "），照抄会把这个小毛病永久化。
	// 新旧两条都用去掉空格的版本 —— 修 help 文本不是破坏性变更。
	help := fmt.Sprintf("%s in %s",
		counterInfo.UnitInfo.GetElementDescription().Label,
		counterInfo.NameInfo.GetElementDescription().Summary,
	)

	name := strings.ReplaceAll(counter, ".", "_")

	if legacy {
		// 旧名的 help 追加 deprecation 说明，指向新名。替代名从同一张映射表
		// 取，而不是在这里手拼 —— 手拼会让 help 里的名字和实际注册的新指标
		// 名脱钩，而这种脱钩没有任何东西会报错。
		spec, ok := translatePerfCounter(counter, counterInfo)
		replacement := prometheus.BuildFQName(namespace, subsystem, name)
		if ok {
			replacement = prometheus.BuildFQName(namespace, subsystem, spec.Name)
		}
		help += fmt.Sprintf(deprecatedSuffix, replacement)
	} else if spec, ok := translatePerfCounter(counter, counterInfo); ok {
		name = spec.Name
	}

	d := prometheus.NewDesc(
		prometheus.BuildFQName(namespace, subsystem, name),
		help,
		labels, nil,
	)

	perfDescCache[key] = d

	return d
}

// boolToFloat64 把布尔状态映射为 Prometheus 惯用的 1/0。
func boolToFloat64(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
