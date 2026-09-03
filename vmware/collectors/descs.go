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

// resourcePoolDescs 覆盖 resourcepool collector 的全部指标。
//
// 这个 collector 的 Desc 从一开始就提到这里，不走 cluster/datacenter 的内联
// 写法。理由是实体数不可控：DRS 给每个 vApp 建池，按租户批量建池的自动化平台
// 也常见，而 vm 那条指标是「一虚机一序列」—— 循环规模跟 VM 数同阶，正是
// host/vm 当初被提出来的那个量级。
//
// !!! 每个字段上方注释里的 label 顺序即 MustNewConstMetric 的传值顺序 !!!
type resourcePoolDescs struct {
	// rpmo, rp, parentmo, ownermo, vcenter
	info *prometheus.Desc
	// rpmo, rp, parentmo, ownermo, vcenter -- ESXi 的 ha-root-pool 伪对象
	infoSynthetic *prometheus.Desc
	// rpmo, rp, status, vcenter
	overallStatus *prometheus.Desc
	// rpmo, rp, vmmo, vcenter
	vm *prometheus.Desc

	// 以下 8 条用量指标的 label 集合相同：rpmo, rp, vcenter
	cpuUsageHertz           *prometheus.Desc
	cpuMaxUsageHertz        *prometheus.Desc
	cpuReservationUsedHertz *prometheus.Desc
	cpuUnreservedHertz      *prometheus.Desc
	memUsageBytes           *prometheus.Desc
	memMaxUsageBytes        *prometheus.Desc
	memReservationUsedBytes *prometheus.Desc
	memUnreservedBytes      *prometheus.Desc

	// 配置项。label 集合同为 rpmo, rp, vcenter
	cpuReservationHertz *prometheus.Desc
	cpuLimitHertz       *prometheus.Desc
	cpuLimited          *prometheus.Desc
	memReservationBytes *prometheus.Desc
	memLimitBytes       *prometheus.Desc
	memLimited          *prometheus.Desc

	// rpmo, rp, level, vcenter
	cpuShares *prometheus.Desc
	// rpmo, rp, level, vcenter
	memShares *prometheus.Desc
}

// vsanDescs 覆盖 vsan collector（组 A：健康与容量）的全部指标。
//
// !!! 每个字段上方注释里的 label 顺序即 MustNewConstMetric 的传值顺序 !!!
type vsanDescs struct {
	// 集群级，label 集合同为 cmo, vmwcluster, vcenter
	enabled           *prometheus.Desc
	dedupEnabled      *prometheus.Desc
	capacityBytes     *prometheus.Desc
	capacityFreeBytes *prometheus.Desc
	capacityUsedBytes *prometheus.Desc

	// cmo, vmwcluster, status, vcenter
	healthStatus *prometheus.Desc

	// cmo, vmwcluster, host, device, uuid, state, vcenter
	diskHealth *prometheus.Desc

	// 盘级容量。label 集合同为 cmo, vmwcluster, host, device, vcenter
	// —— 刻意不含 uuid 与 state：容量是数值指标，把会变化的 state 放进
	// label 会让盘状态一变就产生一条新序列，旧序列变僵尸。
	diskCapacityBytes     *prometheus.Desc
	diskCapacityUsedBytes *prometheus.Desc

	// resync 三条，label 集合同为 cmo, vmwcluster, vcenter。
	// 需要 API >= 6.7，低版本不输出（见 vsan.go 的 collectResync）。
	resyncBytes           *prometheus.Desc
	resyncObjects         *prometheus.Desc
	resyncRecoverySeconds *prometheus.Desc
}

// collectorDescs 按 namespace 缓存。namespace 是 Update() 的运行时入参而非编译期
// 常量，所以不能用包级 var 直接构造；上游框架允许调用方覆盖它，把它当常量是错的。
// 实践中全程只有 "vmware" 一个值，测试里会用别的值。
type collectorDescs struct {
	host         hostDescs
	vm           vmDescs
	datastore    datastoreDescs
	resourcePool resourcePoolDescs
	vsan         vsanDescs
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
		host:         buildHostDescs(namespace),
		vm:           buildVMDescs(namespace),
		datastore:    buildDatastoreDescs(namespace),
		resourcePool: buildResourcePoolDescs(namespace),
		vsan:         buildVsanDescs(namespace),
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

// buildResourcePoolDescs 构造 resourcepool collector 的 Desc 集合。
//
// CPU 值一律换算成 hertz：vSphere 给的是 MHz，而本项目已确立「导出基础单位」
// 的约定（perfnames.go 的 megaHertz → hertz, factor 1e6）。资源池这里必须
// 一致，否则同一张 dashboard 上 host 的 hertz 与资源池的 MHz 会差 6 个数量级。
//
// 内存的两个来源单位不同，别混：Runtime.Memory.* 是**字节**
// （types.go ResourcePoolRuntimeInfo 注释 "Values are in bytes"），而
// Config.MemoryAllocation.{Reservation,Limit} 是 **MB**
// （ResourceAllocationInfo 注释 "Units are MB for memory, MHz for CPU"）。
// 前者原样导出，后者要 ×1048576。
func buildResourcePoolDescs(namespace string) resourcePoolDescs {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(
			prometheus.BuildFQName(namespace, resourcePoolSubsystem, name),
			help, labels, nil,
		)
	}

	const infoHelp = "Resource pool info, for joining on parent and owner references."

	return resourcePoolDescs{
		info: d("info", infoHelp,
			"rpmo", "rp", "parentmo", "ownermo", "vcenter"),

		// synthetic 变体是独立的 Desc 而非在采集时改 label map。
		// 原因：label 集合一旦进了 variableLabels 就是 Desc 的一部分，
		// 同一个 Desc 不能有时带 synthetic 有时不带 —— client_golang 会以
		// 「label 数量不匹配」panic。cluster/datacenter 那边用的是
		// constLabels + 每实体造 Desc，所以能靠 syntheticLabels() 动态加；
		// 这里既然复用 Desc，就得为伪对象单独备一个。
		//
		// help 与上面那条完全相同：promlint 会校验同名指标的 help 一致性，
		// 而这两个 Desc 的 fqName 是同一个。
		infoSynthetic: d("info", infoHelp,
			"rpmo", "rp", "parentmo", "ownermo", "vcenter", "synthetic"),

		overallStatus: d("overall_status",
			"Resource pool overall status as reported by vSphere. "+
				"The status label carries the colour: gray, green, yellow or red.",
			"rpmo", "rp", "status", "vcenter"),

		// 一虚机一条序列。沿用 cluster_datastore 的既有做法 —— 那边已经因为
		// 「逗号拼接的 label 值会在成员变动时产生僵尸序列」改成一对一了。
		vm: d("vm",
			"Resource pool to virtual machine mapping, one series per virtual machine.",
			"rpmo", "rp", "vmmo", "vcenter"),

		cpuUsageHertz: d("cpu_usage_hertz",
			"Current CPU usage of the resource pool and its descendants, in hertz.",
			"rpmo", "rp", "vcenter"),

		cpuMaxUsageHertz: d("cpu_max_usage_hertz",
			"Maximum CPU usage available to the resource pool, in hertz.",
			"rpmo", "rp", "vcenter"),

		cpuReservationUsedHertz: d("cpu_reservation_used_hertz",
			"CPU reservation consumed by all descendants of the resource pool, in hertz.",
			"rpmo", "rp", "vcenter"),

		cpuUnreservedHertz: d("cpu_unreserved_hertz",
			"CPU still available for reservation by virtual machines in the resource pool, in hertz.",
			"rpmo", "rp", "vcenter"),

		memUsageBytes: d("mem_usage_bytes",
			"Current memory usage of the resource pool and its descendants, in bytes.",
			"rpmo", "rp", "vcenter"),

		memMaxUsageBytes: d("mem_max_usage_bytes",
			"Maximum memory usage available to the resource pool, in bytes.",
			"rpmo", "rp", "vcenter"),

		memReservationUsedBytes: d("mem_reservation_used_bytes",
			"Memory reservation consumed by all descendants of the resource pool, in bytes.",
			"rpmo", "rp", "vcenter"),

		memUnreservedBytes: d("mem_unreserved_bytes",
			"Memory still available for reservation by virtual machines in the resource pool, in bytes.",
			"rpmo", "rp", "vcenter"),

		cpuReservationHertz: d("cpu_reservation_hertz",
			"Configured CPU reservation of the resource pool, in hertz.",
			"rpmo", "rp", "vcenter"),

		// unlimited 时这条序列**不输出**，见 emitResourceLimit 的注释。
		cpuLimitHertz: d("cpu_limit_hertz",
			"Configured CPU limit of the resource pool, in hertz. "+
				"Not emitted when the pool is unlimited; check cpu_limited instead.",
			"rpmo", "rp", "vcenter"),

		cpuLimited: d("cpu_limited",
			"Whether the resource pool has a configured CPU limit (1) or is unlimited (0).",
			"rpmo", "rp", "vcenter"),

		memReservationBytes: d("mem_reservation_bytes",
			"Configured memory reservation of the resource pool, in bytes.",
			"rpmo", "rp", "vcenter"),

		memLimitBytes: d("mem_limit_bytes",
			"Configured memory limit of the resource pool, in bytes. "+
				"Not emitted when the pool is unlimited; check mem_limited instead.",
			"rpmo", "rp", "vcenter"),

		memLimited: d("mem_limited",
			"Whether the resource pool has a configured memory limit (1) or is unlimited (0).",
			"rpmo", "rp", "vcenter"),

		// level 与数值都留着：level 是用户在 UI 里配的语义（low/normal/high/
		// custom），数值才能算相对权重。只有 custom 时数值是用户自定的，
		// 其余三档 vSphere 映射到预设值。
		cpuShares: d("cpu_shares",
			"Configured CPU shares of the resource pool. "+
				"The level label is the vSphere allocation level: low, normal, high or custom.",
			"rpmo", "rp", "level", "vcenter"),

		memShares: d("mem_shares",
			"Configured memory shares of the resource pool. "+
				"The level label is the vSphere allocation level: low, normal, high or custom.",
			"rpmo", "rp", "level", "vcenter"),
	}
}

func buildVsanDescs(namespace string) vsanDescs {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(
			prometheus.BuildFQName(namespace, vsanSubsystem, name),
			help, labels, nil,
		)
	}

	// cmo / vmwcluster 与 cluster.go:52 的 vmware_cluster_info 完全一致，
	// 这是刻意的：vSAN 指标必须能与集群信息 join，否则它们就是一座孤岛
	// —— 用户拿到 cmo 却无法关联到集群名、父文件夹与 datastore。
	return vsanDescs{
		enabled: d("enabled",
			"Whether vSAN is enabled on the cluster (1) or not (0). "+
				"Emitted for every cluster, so a value of 0 distinguishes "+
				"\"vSAN is off\" from \"the vsan collector is not running\".",
			"cmo", "vmwcluster", "vcenter"),

		dedupEnabled: d("dedup_enabled",
			"Whether vSAN deduplication and compression is enabled on the cluster (1) or not (0).",
			"cmo", "vmwcluster", "vcenter"),

		capacityBytes: d("capacity_bytes",
			"Total vSAN datastore capacity of the cluster, in bytes.",
			"cmo", "vmwcluster", "vcenter"),

		capacityFreeBytes: d("capacity_free_bytes",
			"Free vSAN datastore capacity of the cluster, in bytes. "+
				"This is the raw value reported by the API.",
			"cmo", "vmwcluster", "vcenter"),

		// 推导值。API 只给 total 与 free，used 是我们算的 —— help 里说明
		// 这一点，这样用户排查数值可疑时知道该去核对哪两条原始序列。
		capacityUsedBytes: d("capacity_used_bytes",
			"Used vSAN datastore capacity of the cluster, in bytes. "+
				"Derived as capacity_bytes minus capacity_free_bytes; "+
				"the API reports no used value directly.",
			"cmo", "vmwcluster", "vcenter"),

		// 状态进 label、值恒为 1，与 resourcepool_overall_status 同一形态。
		// 不映射成数字（telegraf 用 green=0/yellow=1/red=2）：那个映射把
		// "未知"和"健康"都压进数轴，而 unknown 恰恰是最该告警的状态之一。
		healthStatus: d("health_status",
			"vSAN cluster health, as a label. "+
				"The status label is green, yellow, red or unknown; "+
				"unknown means the health service returned no usable value.",
			"cmo", "vmwcluster", "status", "vcenter"),

		diskHealth: d("disk_health",
			"vSAN physical disk health, as a label. "+
				"The state label is the summary health reported by vSAN.",
			"cmo", "vmwcluster", "host", "device", "uuid", "state", "vcenter"),

		diskCapacityBytes: d("disk_capacity_bytes",
			"Capacity of a vSAN physical disk, in bytes.",
			"cmo", "vmwcluster", "host", "device", "vcenter"),

		diskCapacityUsedBytes: d("disk_capacity_used_bytes",
			"Used capacity of a vSAN physical disk, in bytes.",
			"cmo", "vmwcluster", "host", "device", "vcenter"),

		// resync：集群正在重建/迁移的数据量。三条同源（一次
		// VsanQuerySyncingVsanObjects 的响应），所以彼此之间不存在
		// 不一致窗口。全部是 gauge —— 它们是"还剩多少"的瞬时快照，
		// 不是累计量，套 rate() 是错的。
		//
		// 三条恒一起输出（含全 0 的情况）：resync 完成时值就是 0，而
		// "没有 resync"正是运维要确认的正常态。省略序列会让 absent()
		// 无法区分"集群健康"和"采集失败"。
		resyncBytes: d("resync_bytes",
			"Amount of data left to resync on the vSAN cluster, in bytes. "+
				"Zero means no resync is in progress. Requires vSphere API 6.7 or later.",
			"cmo", "vmwcluster", "vcenter"),

		resyncObjects: d("resync_objects",
			"Number of vSAN objects currently syncing on the cluster. "+
				"Zero means no resync is in progress. Requires vSphere API 6.7 or later.",
			"cmo", "vmwcluster", "vcenter"),

		// 单位是秒，由 vSAN Management API 明确规定
		// （totalRecoveryETA: "The estimated time in seconds to recover
		// all vSAN objects"）。指标名带 _seconds 后缀而不是照抄 API 的
		// eta —— Prometheus 命名规范要求单位进名字。
		resyncRecoverySeconds: d("resync_recovery_seconds",
			"Estimated time to complete the vSAN resync, in seconds. "+
				"Zero means no resync is in progress. Requires vSphere API 6.7 or later.",
			"cmo", "vmwcluster", "vcenter"),
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
