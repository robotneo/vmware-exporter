package vmwareCollectors

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	vimtypes "github.com/vmware/govmomi/vim25/types"
	"github.com/vmware/govmomi/vsan"
	vsanmethods "github.com/vmware/govmomi/vsan/methods"
	vsantypes "github.com/vmware/govmomi/vsan/types"
)

const vsanSubsystem = "vsan"

// vsanHealthUnknown 是健康状态取不到时的显式取值。
//
// 与 telegraf 的差别：它在两次查询都拿不到可用值时 return nil（vsan.go:362），
// 于是"健康检查失效"和"vSAN 不存在"在指标上完全无法区分 —— 用户看到序列缺失，
// 却分不清是没启用 vSAN 还是健康服务坏了。这里输出 unknown，让前者有个显式表达。
const vsanHealthUnknown = "unknown"

// vSAN 管理对象的 MoRef。
//
// !!! 这两个字面量是抄来的，govmomi 没有提供 !!!
//
// vsan/client.go 只定义了 VsanVcClusterConfigSystemInstance、
// VsanPerformanceManagerInstance、VsanQueryObjectIdentitiesInstance 与
// VsanVcStretchedClusterSystem —— 因为 vsan.Client 只包装了这几个方法。
// 容量与健康没有包装，只有包级的 methods.VsanQuerySpaceUsage 等函数，
// 而 This 参数要调用方自己填。
//
// 取值来自 telegraf plugins/inputs/vsphere/vsan.go：queryDiskUsage 的
// spaceManagerRef 与 queryHealthSummary 的 healthSystemRef，逐字核对。
//
// 这两个值无法从 govmomi 的类型系统推导，也无法用 vcsim 验证
// （vsan/simulator 只注册 ClusterConfigSystem 与 StretchedClusterSystem）。
// 改动它们的唯一验证途径是真实 vCenter —— 所以别改。
var (
	vsanSpaceReportSystemRef = vimtypes.ManagedObjectReference{
		Type:  "VsanSpaceReportSystem",
		Value: "vsan-cluster-space-report-system",
	}
	vsanClusterHealthSystemRef = vimtypes.ManagedObjectReference{
		Type:  "VsanVcClusterHealthSystem",
		Value: "vsan-cluster-health-system",
	}
)

// vsanClusterConfigSystemRef 与上面两个不同：govmomi **提供**了这个常量
// （vsan/client.go 的 VsanVcClusterConfigSystemInstance），所以引用它而不是
// 自己抄一份字面量。少一处可能抄错的地方。
var vsanClusterConfigSystemRef = vsan.VsanVcClusterConfigSystemInstance

// vsanNewClient 是 vsan.NewClient 的间接层，便于在测试里替换。
//
// vsan.NewClient 的返回值 *vsan.Client 满足 soap.RoundTripper
// （它有 RoundTrip 方法，client.go:64），所以生产路径直接用它即可。
var vsanNewClient = func(ctx context.Context, s *collector.Scrape) (vsanRoundTripper, error) {
	return vsan.NewClient(ctx, s.Client)
}

// vsanHealthFields 是健康摘要请求的 Fields 参数。
//
// telegraf 只传 overallHealth 与 overallHealthDescription（vsan.go:346），
// 因为它不采盘级指标。我们多传 physicalDisksHealth —— 盘健康就在这个响应里，
// 不必再调 VsanQueryClusterPhysicalDiskHealthSummary（那个要 ESXi root 密码，
// 见 docs/DESIGN-resourcepool-vsan.md 的 2.2.1 节）。
//
// **这是本文件唯一无法自动验证的假设**：vcsim 不实现该方法，替身测试只能
// 验证"我们正确解析了响应"，不能验证"vCenter 真的会按这个 Fields 值返回
// 盘健康"。真实环境若返回空的 PhysicalDisksHealth，盘级指标会静默缺失
// 而集群健康仍然正常 —— README 的前提条件里写了这一点。
var vsanHealthFields = []string{
	"overallHealth",
	"overallHealthDescription",
	"physicalDisksHealth",
}

var vsanCollectorFlag = flag.Bool(fmt.Sprintf("collector.%s", vsanSubsystem), collector.DefaultDisabled, fmt.Sprintf("Enable the %s collector (default: %v)", vsanSubsystem, collector.DefaultDisabled))

func init() {
	collector.RegisterFlag(vsanSubsystem, vsanCollectorFlag)
}

// vsanRoundTripper 是本 collector 对 vSAN SOAP 通道的全部要求。
//
// 刻意收窄到 soap.RoundTripper 这个单方法接口，而不是接 *vsan.Client：
// govmomi 的 vsan/methods 包级函数第二参正是这个接口，所以生产路径传
// vsan.NewClient(...) 的返回值、测试路径传替身，两边都不需要改造。
//
// 这个决定必须在第一个 commit 里定下 —— 事后从 *vsan.Client 改成接口会
// 牵动所有测试。而 vcsim 只实现了 vSAN 的 3 个方法（组 A 用到的 5 个里
// 只覆盖 1 个），没有替身就等于这个 collector 的绝大部分逻辑不可测，
// 按本项目标准那是不该合入的。
type vsanRoundTripper = soap.RoundTripper

// vsanClientFactory 构造本轮抓取要用的 vSAN 通道。
//
// 做成字段而非直接在 Update 里调 vsan.NewClient，同样是为了可测性 ——
// 测试把它替换成返回替身的函数。默认值 newVsanClient 走真实 SOAP。
type vsanClientFactory func(ctx context.Context, s *collector.Scrape) (vsanRoundTripper, error)

type vsanCollector struct {
	logger *slog.Logger

	// newClient 允许测试注入替身。nil 时用 newVsanClient。
	newClient vsanClientFactory
}

// NewvsanCollector 沿用本包多数派命名（New<小写子系统名>Collector）。
//
// 注意不能在构造期解引用 logger：registry_test.go 的
// TestCreatorsProduceCollectors 会传 nil logger 调用每个 Creator 并断言
// 不返回 error、不返回 nil。现有 collector 都是直接存指针，照抄。
func NewvsanCollector(logger *slog.Logger) (collector.Collector, error) {
	return &vsanCollector{logger: logger}, nil
}

// Update 采集 vSAN 组 A：集群启用状态、去重压缩、容量与健康（含盘健康）。
//
// 默认禁用（registry.go 里是 DefaultDisabled），这不只是保守而是正确的默认：
// 绝大多数 vSphere 环境没启用 vSAN，默认开启会让这些环境每轮 scrape 都白跑
// 一遍容量与健康查询。虽然会优雅降级，但 SOAP 往返是实打实花掉的。
//
// 三条降级路径（设计稿 2.4 节），每条都有反向验证：
//
//  1. ESXi 直连 —— 整体跳过。ESXi 上不存在 ClusterComputeResource，而且
//     vsan/simulator.go:20 只在 IsVPX() 时注册 vSAN 端点，真实 ESXi 同理
//     没有 /vsanHealth。硬查只会得到 404。
//  2. 集群未启用 vSAN —— 输出 enabled 0 后**不再查其余 API**。
//  3. 单个集群查询失败 —— 记日志继续下一个，不让一个坏集群拖垮整轮。
func (c *vsanCollector) Update(ctx context.Context, ch chan<- prometheus.Metric, s *collector.Scrape) error {
	// 降级 1：ESXi 直连。留 Debug 日志而不是静默返回 —— 用户开了
	// -collector.vsan 却一条指标都没有时，日志是唯一的解释来源。
	if isESXi(s) {
		c.logger.Debug("skipping vsan collector on a direct ESXi connection",
			"target_type", targetTypeESXi)
		return nil
	}

	var clusters []mo.ClusterComputeResource

	// 只要 name —— cmo 从 Self 拿，其余属性本 collector 用不到。
	// 刻意不检索 configurationEx：那是 D8 的 A 方案，而 A 方案拿不到 dedup
	// （vim25 的 VsanClusterConfigInfo 没有 DataEfficiencyConfig），
	// 走 VsanClusterGetConfig 才能一次拿到 enabled 与 dedup 两样。
	err := fetchProperties(
		ctx, s.View, s.Client,
		[]string{"ClusterComputeResource"}, []string{"name"}, &clusters, c.logger,
	)
	if err != nil {
		return err
	}

	if len(clusters) == 0 {
		c.logger.Debug("no cluster found, nothing to collect for vsan")
		return nil
	}

	factory := c.newClient
	if factory == nil {
		factory = vsanNewClient
	}

	client, err := factory(ctx, s)
	if err != nil {
		return fmt.Errorf("creating vsan client: %w", err)
	}

	d := descsFor(s.Namespace).vsan

	for _, cluster := range clusters {
		// 降级 3：单集群失败不中断整轮。返回 error 会让整个 collector 被
		// 记为失败，而 vSAN 的常见故障（某个集群的健康服务没起来）是集群
		// 局部的 —— 其余集群的数据仍然有价值。
		if err := c.collectCluster(ctx, ch, s, d, client, cluster); err != nil {
			c.logger.Warn("vsan collection failed for cluster",
				"cluster", cluster.Name, "cmo", cluster.Self.Value, "err", err)
		}
	}

	return nil
}

// collectCluster 采集单个集群的组 A 指标。
func (c *vsanCollector) collectCluster(
	ctx context.Context,
	ch chan<- prometheus.Metric,
	s *collector.Scrape,
	d vsanDescs,
	client vsanRoundTripper,
	cluster mo.ClusterComputeResource,
) error {
	cmo := cluster.Self.Value
	name := cluster.Name

	gauge := func(desc *prometheus.Desc, value float64) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value,
			cmo, name, s.Target)
	}

	cfg, err := vsanmethods.VsanClusterGetConfig(ctx, client, &vsantypes.VsanClusterGetConfig{
		This:    vsanClusterConfigSystemRef,
		Cluster: cluster.Self,
	})
	if err != nil {
		return fmt.Errorf("querying vsan config: %w", err)
	}

	enabled := vsanConfigEnabled(cfg)
	gauge(d.enabled, boolToFloat64(enabled))

	// 降级 2：未启用就到此为止。这里 return nil 而不是继续查 —— 未启用的
	// 集群上那些查询要么报错要么返回空，每个都是一次白花的 SOAP 往返。
	// 混合环境里这是实际负担：telegraf 为此专门开了 vsan_cluster_include
	// 让用户手工筛集群，我们靠这个提前返回天然解决，且不会因为清单过期而
	// 漏采新建的 vSAN 集群。
	if !enabled {
		c.logger.Debug("vsan not enabled on cluster, skipping the remaining queries",
			"cluster", name, "cmo", cmo)
		return nil
	}

	// dedup 与 enabled 同源（同一次 VsanClusterGetConfig 的响应），所以
	// 两条指标之间不存在"一个说启用、另一个说没有"的不一致窗口。
	// DataEfficiencyConfig 是指针，未配置去重压缩时为 nil —— 此时输出 0
	// 而不是省略序列：vSAN 已启用的前提下"没开去重"是个确定的事实，
	// 不是"信息缺失"。
	gauge(d.dedupEnabled, boolToFloat64(vsanDedupEnabled(cfg)))

	// 容量与健康各自独立降级：容量查询失败不该让健康也拿不到。
	// 两个错误都收集起来一起返回，这样日志里能同时看到两个问题，
	// 而不是修好一个才发现还有另一个。
	var errs []error

	if err := c.collectCapacity(ctx, gauge, d, client, cluster); err != nil {
		errs = append(errs, fmt.Errorf("capacity: %w", err))
	}

	if err := c.collectHealth(ctx, ch, s, d, client, cluster); err != nil {
		errs = append(errs, fmt.Errorf("health: %w", err))
	}

	return errors.Join(errs...)
}

// collectCapacity 采集集群容量。
//
// API 只给 total 与 free，没有 used —— used 是这里算出来的。三条都导出的
// 理由见设计稿 2.2.2：原始值是可信基准，used 是运维实际要看的那个数
// （容量告警写的是"已用超过 80%"，不是"剩余低于 20%"）。
func (c *vsanCollector) collectCapacity(
	ctx context.Context,
	gauge func(*prometheus.Desc, float64),
	d vsanDescs,
	client vsanRoundTripper,
	cluster mo.ClusterComputeResource,
) error {
	resp, err := vsanmethods.VsanQuerySpaceUsage(ctx, client, &vsantypes.VsanQuerySpaceUsage{
		This:    vsanSpaceReportSystemRef,
		Cluster: cluster.Self,
	})
	if err != nil {
		return err
	}
	if resp == nil {
		return errors.New("empty space usage response")
	}

	total := resp.Returnval.TotalCapacityB
	free := resp.Returnval.FreeCapacityB

	gauge(d.capacityBytes, float64(total))
	gauge(d.capacityFreeBytes, float64(free))

	// FreeCapacityB 是 omitempty，缺失时为 0，此时 used == total。
	// 语义上正确（"没有可用空间"就是"全部已用"），也与 total 为 0 时
	// 三条全 0 自然吻合，不需要额外的标志位。
	gauge(d.capacityUsedBytes, float64(total-free))

	return nil
}

// collectHealth 采集集群健康与物理盘健康。
//
// 两阶段读取，抄自 telegraf queryHealthSummary（vsan.go:341-365）：
// 先读 vCenter 缓存的健康摘要，缓存为空则关掉缓存强制重算。
//
// 为什么需要第二阶段：vCenter 的健康摘要缓存在某些时刻是空的（刚重启、
// 刚启用 vSAN、健康服务刚重载），此时 OverallHealth 是空串或未知值。
// 只读缓存会静默产出一条错误的 unknown 序列。
//
// 顺序不能反：FetchFromCache=false 会让 vCenter 真的跑一遍健康检查，
// 慢得多。所以必须"先缓存、失败才回退"。
func (c *vsanCollector) collectHealth(
	ctx context.Context,
	ch chan<- prometheus.Metric,
	s *collector.Scrape,
	d vsanDescs,
	client vsanRoundTripper,
	cluster mo.ClusterComputeResource,
) error {
	clusterRef := cluster.Self

	req := &vsantypes.VsanQueryVcClusterHealthSummary{
		This:           vsanClusterHealthSystemRef,
		Cluster:        &clusterRef,
		Fields:         vsanHealthFields,
		FetchFromCache: vsanTrue(),
	}

	summary, err := vsanmethods.VsanQueryVcClusterHealthSummary(ctx, client, req)
	if err != nil {
		return fmt.Errorf("reading cached health summary: %w", err)
	}

	// 第二阶段的触发条件是"缓存里的值不是已知健康值"，不是"报错"。
	// 空串、以及将来 vSAN 新增的任何未知取值都会走到这里。
	if summary == nil || !isKnownVsanHealth(summary.Returnval.OverallHealth) {
		req.FetchFromCache = vsanFalse()

		uncached, err := vsanmethods.VsanQueryVcClusterHealthSummary(ctx, client, req)
		if err != nil {
			// 与 telegraf 的分歧点：它这里 return nil 静默跳过
			// （vsan.go:362），我们输出 unknown。静默跳过会让"健康检查
			// 失效"和"vSAN 不存在"在指标上无法区分 —— 而这两种情况
			// 一个该告警、一个不该。
			c.emitHealthStatus(ch, s, d, cluster, vsanHealthUnknown)
			return fmt.Errorf("reading uncached health summary: %w", err)
		}

		if uncached != nil {
			summary = uncached
		}
	}

	status := vsanHealthUnknown
	if summary != nil && isKnownVsanHealth(summary.Returnval.OverallHealth) {
		status = summary.Returnval.OverallHealth
	}

	c.emitHealthStatus(ch, s, d, cluster, status)

	if summary != nil {
		c.emitDiskHealth(ch, s, d, cluster, summary.Returnval.PhysicalDisksHealth)
	}

	return nil
}

// emitHealthStatus 输出集群健康。
//
// 状态进 label、值恒为 1，与 resourcepool_overall_status 同一形态。
// **不抄 telegraf 的 green=0/yellow=1/red=2 数字映射**：那个映射没有
// unknown 的位置，只能把它丢掉（telegraf 正是这么做的）。而 unknown
// 恰恰是最该告警的状态之一 —— 健康服务本身失效了。
func (c *vsanCollector) emitHealthStatus(
	ch chan<- prometheus.Metric,
	s *collector.Scrape,
	d vsanDescs,
	cluster mo.ClusterComputeResource,
	status string,
) {
	ch <- prometheus.MustNewConstMetric(
		d.healthStatus, prometheus.GaugeValue, 1.0,
		cluster.Self.Value, cluster.Name, status, s.Target,
	)
}

// emitDiskHealth 输出物理盘健康与盘级容量。
//
// 数据来自健康摘要响应的 PhysicalDisksHealth，**不是**
// VsanQueryClusterPhysicalDiskHealthSummary —— 后者的请求体要
// EsxRootPassword（vsan/types/types.go:4081），只读采集账号拿不到、
// 也不该持有集群所有主机的 root 凭据。详见设计稿 2.2.1 节。
//
// 顺带解决了组 B 的一个前置依赖：盘的 hostname 与 devicename 从这里就能
// 拿到，不必像 telegraf 那样逐台主机轮询 CMMDS（getCmmdsMap，vsan.go:169）。
func (c *vsanCollector) emitDiskHealth(
	ch chan<- prometheus.Metric,
	s *collector.Scrape,
	d vsanDescs,
	cluster mo.ClusterComputeResource,
	hosts []vsantypes.VsanPhysicalDiskHealthSummary,
) {
	cmo := cluster.Self.Value
	name := cluster.Name

	for _, host := range hosts {
		// Error 非 nil 表示这台主机的盘健康取不到（主机关机或维护模式）。
		// 跳过它而不是整个放弃 —— 这与 telegraf 逐台重试 CMMDS 的动机相同：
		// 集群里有主机不可达是常态，不该让其余主机的数据一起消失。
		if host.Error != nil {
			c.logger.Debug("skipping disk health for a host that reported an error",
				"cluster", name, "host", host.Hostname)
			continue
		}

		for _, disk := range host.Disks {
			ch <- prometheus.MustNewConstMetric(
				d.diskHealth, prometheus.GaugeValue, 1.0,
				cmo, name, host.Hostname, disk.Name, disk.Uuid,
				disk.SummaryHealth, s.Target,
			)

			// 容量为 0 时不输出：Capacity 是 omitempty，缓存的健康摘要里
			// 常常没有容量字段（那是 physicalDisksHealth 的可选子字段）。
			// 输出 0 会让"没拿到"看起来像"这块盘容量是 0"。
			if disk.Capacity > 0 {
				ch <- prometheus.MustNewConstMetric(
					d.diskCapacityBytes, prometheus.GaugeValue, float64(disk.Capacity),
					cmo, name, host.Hostname, disk.Name, s.Target,
				)
				ch <- prometheus.MustNewConstMetric(
					d.diskCapacityUsedBytes, prometheus.GaugeValue, float64(disk.UsedCapacity),
					cmo, name, host.Hostname, disk.Name, s.Target,
				)
			}
		}
	}
}

// knownVsanHealthStates 是 vSAN 健康的已知取值。
//
// 用于判断缓存的健康摘要是否可用（telegraf 用 healthMap 的 found 做同一件事）。
// 取值来自 telegraf 的 healthMap（vsan.go:339）——它把这三个映射成 2/1/0。
var knownVsanHealthStates = map[string]bool{
	"green":  true,
	"yellow": true,
	"red":    true,
}

func isKnownVsanHealth(status string) bool {
	return knownVsanHealthStates[status]
}

// vsanConfigEnabled 判断 vSAN 是否启用，两层判空。
//
// Enabled 是 *bool（vim25/types/types.go:99414，通过嵌入的
// VsanClusterConfigInfo 继承而来），nil 与 false 都算未启用。
//
// nil 不是理论情况：vsan/simulator.go:72 返回的是空的 VsanConfigInfoEx，
// 其 Enabled 就是 nil 而非 false —— 也就是说 vcsim 环境下"未启用"走的
// 正是 nil 分支。真实 vCenter 上属性缺失时同理。
func vsanConfigEnabled(cfg *vsantypes.VsanClusterGetConfigResponse) bool {
	if cfg == nil {
		return false
	}

	return cfg.Returnval.Enabled != nil && *cfg.Returnval.Enabled
}

// vsanDedupEnabled 判断去重压缩是否启用。
//
// DataEfficiencyConfig 是指针（vsan/types/types.go:8445），未配置时 nil；
// 其内的 DedupEnabled 是裸 bool，不需要再判一层。
//
// 注意这个字段只存在于 **vsan 包的** VsanConfigInfoEx，不在 vim25 的
// VsanClusterConfigInfo 里 —— 这正是 D8 最终选 B（走 VsanClusterGetConfig
// 而非 cluster collector 的 configurationEx 属性）的决定性理由。
func vsanDedupEnabled(cfg *vsantypes.VsanClusterGetConfigResponse) bool {
	if cfg == nil || cfg.Returnval.DataEfficiencyConfig == nil {
		return false
	}

	return cfg.Returnval.DataEfficiencyConfig.DedupEnabled
}

// vsanTrue / vsanFalse 造 *bool。
//
// FetchFromCache 是 *bool 且**不是** omitempty（vsan/types/types.go:2114），
// 所以 nil 与 false 在线上不等价：nil 会被编码成空元素，服务端行为未定义。
// 两个阶段都必须显式给值。
func vsanTrue() *bool {
	v := true
	return &v
}

func vsanFalse() *bool {
	v := false
	return &v
}
