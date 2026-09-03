package vmwareCollectors

import (
	"context"
	"flag"
	"fmt"
	"log/slog"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

const (
	resourcePoolSubsystem = "resourcepool"
)

// syntheticRootPoolMoid 是 ESXi 上隐式根资源池的 moid。
//
// 依据：govmomi simulator/esx/resource_pool.go:21 —— ESXi 没有用户创建的资源池
// 概念，但仍以固定 moid 暴露一个根池，让 API 形状与 vCenter 保持一致。
const syntheticRootPoolMoid = "ha-root-pool"

// mhzToHertz 是 MHz → Hz 的换算系数。
//
// vSphere 的 CPU 分配与用量一律以 MHz 计（types.ResourceAllocationInfo 与
// ResourcePoolRuntimeInfo 的注释都写明了），而 Prometheus 约定导出基础单位。
const mhzToHertz = 1e6

// mbToBytes 是 MB → 字节的换算系数。
//
// 2^20 而非 10^6：vSphere 的 MB 是二进制兆字节，vm collector 的
// mem_capacity_bytes 已经用的是这个系数，两边必须一致。
const mbToBytes = 1048576

var resourcepoolCollectorFlag = flag.Bool(fmt.Sprintf("collector.%s", resourcePoolSubsystem), collector.DefaultEnabled, fmt.Sprintf("Enable the %s collector (default: %v)", resourcePoolSubsystem, collector.DefaultEnabled))

type resourcepoolCollector struct {
	logger *slog.Logger
}

func init() {
	collector.RegisterFlag(resourcePoolSubsystem, resourcepoolCollectorFlag)
}

// NewresourcepoolCollector 沿用本包 6:1 的多数派命名（New<小写子系统名>Collector）。
//
// 这个名字读起来别扭，任何 linter 都会觉得可疑 —— 选它的唯一理由是本轮范围是
// 「加 collector」而不是「统一 7 个既有 Creator 的命名」。后者应当是独立一轮，
// 把 NewClusterCollector 一起改掉；半途换风格只会让不一致从 6:1 变成 6:4。
func NewresourcepoolCollector(logger *slog.Logger) (collector.Collector, error) {
	return &resourcepoolCollector{logger}, nil
}

// Update 采集资源池的配置与瞬时用量。
//
// 走属性检索而非性能计数器，这是与 telegraf inputs.vsphere 的主要分歧点。
// 三个理由：
//
//  1. 一次 ContainerView 检索拿全部资源池，性能计数器路径要额外一轮
//     QueryPerf，往返翻倍。
//  2. reservation / limit / shares 这三个最能解释「VM 为什么拿不到 CPU」的
//     配置项只存在于 Config 里，性能计数器根本没有。
//  3. 属性值是当前瞬时值，没有采样窗口与 rollup 的歧义。资源池的性能计数器
//     有 .average / .minimum / .maximum 三套，混用容易出错。
//
// 代价是放弃了 telegraf 有的 mem.compressed、power.energy 等细项。这些对
// 「资源池是否成为瓶颈」的判断没有必要性，且 power 类在资源池层级本身就是
// 下层主机的汇总，语义可疑。
func (c *resourcepoolCollector) Update(ctx context.Context, ch chan<- prometheus.Metric, s *collector.Scrape) error {

	var pools []mo.ResourcePool

	// 属性列表刻意不含 childConfiguration —— 那是嵌套子池的完整
	// ResourceConfigSpec 数组，深层嵌套时体积可观，而本 collector 用不到它
	// （子池自己会作为独立实体被检索到）。默认启用的 collector 更要克制。
	err := fetchProperties(
		ctx, s.View, s.Client,
		[]string{"ResourcePool"},
		[]string{"name", "parent", "owner", "runtime", "config", "vm", "overallStatus"},
		&pools, c.logger,
	)
	if err != nil {
		return err
	}

	d := descsFor(s.Namespace).resourcePool
	esxi := isESXi(s)

	for _, pool := range pools {
		c.emitPool(ch, s, d, pool, esxi)
	}

	return nil
}

// emitPool 产出单个资源池的全部序列。
func (c *resourcepoolCollector) emitPool(
	ch chan<- prometheus.Metric,
	s *collector.Scrape,
	d resourcePoolDescs,
	pool mo.ResourcePool,
	esxi bool,
) {
	rpmo := pool.Self.Value
	name := pool.Name

	// Parent 是 *ManagedObjectReference，根池之上没有父实体时为 nil。
	// 裸解引用会 panic —— telegraf 在自己那条资源池路径上就漏了同类检查
	// （endpoint.go:801 解引用 VirtualMachine.ResourcePool 而不判空）。
	parentmo := ""
	if pool.Parent != nil {
		parentmo = pool.Parent.Value
	}

	// Owner 是值类型而非指针，但它可以是零值 ManagedObjectReference
	// （未设置时 Value 为空串），照样导出即可 —— 空串比伪造一个 moid 诚实。
	ownermo := pool.Owner.Value

	// ESXi 只有隐式的 ha-root-pool。标注 synthetic 让「这不是用户创建的
	// 资源池」在指标层面可见，而不是静默混进真实数据里。做法与
	// datacenter.go 对 ha-datacenter、cluster.go 对 ha-compute-res 一致。
	if esxi && rpmo == syntheticRootPoolMoid {
		ch <- prometheus.MustNewConstMetric(
			d.infoSynthetic, prometheus.GaugeValue, 1.0,
			rpmo, name, parentmo, ownermo, s.Target, "true",
		)
	} else {
		ch <- prometheus.MustNewConstMetric(
			d.info, prometheus.GaugeValue, 1.0,
			rpmo, name, parentmo, ownermo, s.Target,
		)
	}

	ch <- prometheus.MustNewConstMetric(
		d.overallStatus, prometheus.GaugeValue, 1.0,
		rpmo, name, string(pool.OverallStatus), s.Target,
	)

	// 一虚机一条序列。这是 vm → resourcepool → cluster 这条 join 链的补齐点。
	//
	// 反向关系直接挂在资源池对象上（mo.ResourcePool.Vm），所以不需要像
	// telegraf 那样从 VM 侧反查资源池名 —— 它那个 getResourcePoolName 是在
	// 每台 VM 的循环里线性扫资源池 map，整体 O(VM 数 × 池数)。
	for _, vm := range pool.Vm {
		ch <- prometheus.MustNewConstMetric(
			d.vm, prometheus.GaugeValue, 1.0,
			rpmo, name, vm.Value, s.Target,
		)
	}

	c.emitUsage(ch, s, d, pool, rpmo, name)
	c.emitAllocation(ch, s, d, pool, rpmo, name)
}

// emitUsage 产出 Runtime 里的瞬时用量。
//
// 单位差异要记牢：Runtime.Cpu.* 是 MHz，Runtime.Memory.* 是**字节**
// （ResourcePoolRuntimeInfo 的字段注释分别写了 "Values are in Mhz" 与
// "Values are in bytes"）。所以 CPU 侧乘 1e6，内存侧原样导出。
func (c *resourcepoolCollector) emitUsage(
	ch chan<- prometheus.Metric,
	s *collector.Scrape,
	d resourcePoolDescs,
	pool mo.ResourcePool,
	rpmo, name string,
) {
	cpu := pool.Runtime.Cpu
	mem := pool.Runtime.Memory

	gauge := func(desc *prometheus.Desc, value float64) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value,
			rpmo, name, s.Target)
	}

	gauge(d.cpuUsageHertz, float64(cpu.OverallUsage)*mhzToHertz)
	gauge(d.cpuMaxUsageHertz, float64(cpu.MaxUsage)*mhzToHertz)
	gauge(d.cpuReservationUsedHertz, float64(cpu.ReservationUsed)*mhzToHertz)

	// UnreservedForVm 而非 UnreservedForPool：前者才是「还能给虚机预留多少」，
	// 也是排查「VM 开不起来说资源不足」时要看的那个数。
	gauge(d.cpuUnreservedHertz, float64(cpu.UnreservedForVm)*mhzToHertz)

	gauge(d.memUsageBytes, float64(mem.OverallUsage))
	gauge(d.memMaxUsageBytes, float64(mem.MaxUsage))
	gauge(d.memReservationUsedBytes, float64(mem.ReservationUsed))
	gauge(d.memUnreservedBytes, float64(mem.UnreservedForVm))
}

// emitAllocation 产出 Config 里的 reservation / limit / shares。
//
// ResourceAllocationInfo 的 Reservation、Limit、Shares **全是指针**
// （types.go:71225/71240/71242），未设置时为 nil。这不是理论风险：
// 属性检索没请求到某个子字段、或对象处于半初始化状态时就会拿到 nil。
// 每一处都判空，缺失即不输出该序列 —— 比填 0 诚实，0 在 CPU reservation
// 上恰好是个有效值，分不出「没配」和「配成 0」。
func (c *resourcepoolCollector) emitAllocation(
	ch chan<- prometheus.Metric,
	s *collector.Scrape,
	d resourcePoolDescs,
	pool mo.ResourcePool,
	rpmo, name string,
) {
	cpuAlloc := pool.Config.CpuAllocation
	memAlloc := pool.Config.MemoryAllocation

	gauge := func(desc *prometheus.Desc, value float64) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value,
			rpmo, name, s.Target)
	}

	if cpuAlloc.Reservation != nil {
		gauge(d.cpuReservationHertz, float64(*cpuAlloc.Reservation)*mhzToHertz)
	}
	if memAlloc.Reservation != nil {
		gauge(d.memReservationBytes, float64(*memAlloc.Reservation)*mbToBytes)
	}

	c.emitLimit(ch, s, d.cpuLimitHertz, d.cpuLimited, cpuAlloc.Limit, mhzToHertz, rpmo, name)
	c.emitLimit(ch, s, d.memLimitBytes, d.memLimited, memAlloc.Limit, mbToBytes, rpmo, name)

	c.emitShares(ch, s, d.cpuShares, cpuAlloc.Shares, rpmo, name)
	c.emitShares(ch, s, d.memShares, memAlloc.Shares, rpmo, name)
}

// emitLimit 处理 limit 的 unlimited 语义。
//
// vSphere 用 -1 表示不限制（ResourceAllocationInfo.Limit 的注释：
// "If set to -1, then there is no fixed limit on resource usage"）。
// 三种导出方式里选的是「unlimited 时不输出 limit 序列，另出一条 _limited
// 标志位」：
//
//   - 原样导 -1：limit - usage 会算出负数，而且 -1 会被当成真实数值参与
//     聚合运算（sum/avg 都会被污染）。
//   - 导 +Inf：数学上正确，Prometheus 也支持，但部分可视化工具显示异常。
//   - 不输出：PromQL 里 limit 缺失就是空结果，而不是一个看起来合理的错误值。
//
// 缺失的序列在 PromQL 里的行为比任何哨兵值都安全 —— 哨兵值会被误当作真实
// 数值。代价是「有没有配限制」这个信息没了，所以补一条 _limited 显式表达。
func (c *resourcepoolCollector) emitLimit(
	ch chan<- prometheus.Metric,
	s *collector.Scrape,
	limitDesc, limitedDesc *prometheus.Desc,
	limit *int64,
	factor float64,
	rpmo, name string,
) {
	// nil 与 -1 都算「没有限制」。nil 是属性缺失，-1 是 vSphere 的
	// unlimited 哨兵，对下游查询而言二者等价。
	limited := limit != nil && *limit >= 0

	ch <- prometheus.MustNewConstMetric(limitedDesc, prometheus.GaugeValue,
		boolToFloat64(limited), rpmo, name, s.Target)

	if limited {
		ch <- prometheus.MustNewConstMetric(limitDesc, prometheus.GaugeValue,
			float64(*limit)*factor, rpmo, name, s.Target)
	}
}

// emitShares 产出 shares 数值，level 作为 label。
//
// 两者都留着是有意的：level 是用户在 UI 里配的语义，数值才能算相对权重。
// 只有 level 为 custom 时 Shares 字段才是用户自定的值，其余三档由 vSphere
// 映射到预设值（SharesInfo.Shares 的注释说明了这点）—— 但预设值本身也是
// 真实的权重，照样导出，否则 low/normal/high 之间就没法比较了。
func (c *resourcepoolCollector) emitShares(
	ch chan<- prometheus.Metric,
	s *collector.Scrape,
	desc *prometheus.Desc,
	shares *types.SharesInfo,
	rpmo, name string,
) {
	if shares == nil {
		return
	}

	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue,
		float64(shares.Shares), rpmo, name, string(shares.Level), s.Target)
}
