package vmwareCollectors

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prezhdarov/vmware-exporter/internal/config"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vsan"
	vsanmethods "github.com/vmware/govmomi/vsan/methods"
	vsantypes "github.com/vmware/govmomi/vsan/types"
)

// vsanPerfSubsystem 是指标名里的子系统段，也是 flag 名的一部分。
//
// 取 "vsan_perf" 而 flag 名取 "vsan.perf"：前者进指标名（Prometheus 不允许点），
// 后者进 flag（与 collector.esxcli.host.nic 的点分风格一致）。
// 2.3 节已验证 "vsan" 与 "vsan.perf" 两个 flag 不会互相误触发。
const (
	vsanPerfSubsystem = "vsan_perf"
	vsanPerfFlagName  = "vsan.perf"
)

// vsanPerfEntityWhitelist 是我们愿意采集的实体类型。
//
// 取自 telegraf README 的推荐清单，但**这里只是候选**：真正采哪些由它与
// VsanPerfGetSupportedEntityTypes 的返回取交集决定（D4 的 C 方案）。
// 环境不支持的类型不会被查询，交集为空时给出明确日志而不是静默返回。
//
// 为什么不全采：telegraf 支持 29 个实体类型，其中 vsan-iscsi-*、*-world-cpu、
// host-memory-* 等要么是 vSAN 内部实现细节（排障时才看），要么依赖未必启用的
// 功能，要么与既有的 datastore 计数器重叠。
var vsanPerfEntityWhitelist = []string{
	"cluster-domclient",
	"host-domclient",
	"disk-group",
	"capacity-disk",
	"cache-disk",
}

// vsanPerfLabelWhitelist 是我们愿意采集的指标 label。
//
// **这道白名单是基数控制的关键，不是可选的优化。** telegraf 文档实测：
// disk-group 一个实体类型就有 79 个 label。一个 10 主机、每主机 2 个磁盘组的
// 集群，仅 disk-group 就是 20 x 79 = 1580 条序列，再加 capacity-disk
// （16 label x 盘数）会更多。默认禁用挡不住这个问题 —— 用户一旦打开就会
// 一次性拿到几千条序列，而其中大部分是 resync 分类计数（iops_resync_read_policy
// 这类共 24 个）与调度器队列细节，只在深度排障时有意义。
//
// 白名单直接作为 VsanPerfQuerySpec.Labels 传给 vCenter，所以是**请求侧**裁剪：
// 多余的数据根本不会下载，不是拿回来再丢掉。
//
// 选取原则：保留能回答"存储快不快、满不满、堵不堵"的指标族。
var vsanPerfLabelWhitelist = []string{
	// IOPS 与吞吐 —— 最常看的一屏。
	"iops_read", "iops_write",
	"throughput_read", "throughput_write",

	// 延迟。latency_avg_* 是 domclient 侧的命名，latency_* 是盘级的。
	"latency_avg_read", "latency_avg_write",
	"latency_read", "latency_write",

	// 拥塞与未完成 IO —— vSAN 特有的瓶颈信号，没有 vSphere 侧等价物。
	"congestion", "oio",

	// 磁盘组容量。与组 A 的集群级容量不同，这是磁盘组粒度。
	"capacity", "capacity_used", "capacity_reserved",

	// 缓存命中率与写缓冲余量 —— 判断缓存层是否成为瓶颈。
	"rc_hit_rate", "wb_free_pct",
}

// vsanPerfInterval 是性能查询的时间窗口，单位秒。
//
// flag 名按 D7 定为 -vmware.vsan.interval 而不是初稿的 -vsan.perf.interval：
// 后者会新开一个顶层命名空间且只装这一个 flag，而 vmware.* 已经是"连接与采集
// 参数"的既有归属地（vmware.interval / vmware.granularity / vmware.timeout）。
//
// 默认 300 秒而不是沿用 -vmware.interval 的 20 秒：vSAN 性能统计的最小采集
// 粒度就是 300 秒（vCenter 侧每 5 分钟落一个点），要更细的窗口 API 也给不出
// 更多数据点，只会让每轮抓取拿回同一个点。
var vsanPerfInterval = flag.Int("vmware.vsan.interval", 300,
	"Time window in seconds for vSAN performance queries. vSAN statistics are collected at a 5-minute granularity, so values below 300 do not yield more data points (default: 300)")

// vsanPerfSkipVerify 关掉实体类型协商。
//
// 这是 telegraf 的 vsan_metric_skip_verify 的等价物。它存在的理由不是"用户
// 想跳过检查"，而是**这个 API 本身不完备** —— telegraf README 的原话是
// "some performance entities are not returned by the API, but we want to offer
// the flexibility if you really need the stats"。
//
// 也就是说取交集会漏掉真实可查但 API 不申报的实体类型。开启本 flag 后直接用
// 白名单查询，不问 vCenter 支持什么。
var vsanPerfSkipVerify = flag.Bool("collector.vsan.perf.skip-verify", false,
	"Skip vSAN performance entity type negotiation and query the built-in whitelist directly. Needed because VsanPerfGetSupportedEntityTypes does not report every queryable entity type (default: false)")

var vsanPerfCollectorFlag = flag.Bool(fmt.Sprintf("collector.%s", vsanPerfFlagName), collector.DefaultDisabled, fmt.Sprintf("Enable the %s collector (default: %v)", vsanPerfFlagName, collector.DefaultDisabled))

// vsanPerfSettings 是本 collector 一轮抓取用到的两个 flag 的快照。
//
// 与 emitLegacyNames 同理：SIGHUP 重载通过 flag.Set 裸写这两个变量，抓取
// 协程同时在读。两个一起快照而不是各配一个 getter，是因为它们在同一轮
// 抓取里被用于同一个决定（查哪些实体、查多长的窗口）—— 分开读会让一次
// 落在中间的重载把这两个参数配成一份从未存在过的组合。
type vsanPerfSettings struct {
	interval   int
	skipVerify bool
}

func currentVsanPerfSettings() vsanPerfSettings {
	var s vsanPerfSettings

	config.Snapshot(func() {
		s = vsanPerfSettings{
			interval:   *vsanPerfInterval,
			skipVerify: *vsanPerfSkipVerify,
		}
	})

	return s
}

func init() {
	collector.RegisterFlag(vsanPerfFlagName, vsanPerfCollectorFlag)
}

// vsanPerfNewClient 与 vsan.go 的 vsanNewClient 平行，测试用它注入替身。
var vsanPerfNewClient = func(ctx context.Context, s *collector.Scrape) (vsanRoundTripper, error) {
	return vsan.NewClient(ctx, s.Client)
}

type vsanPerfCollector struct {
	logger *slog.Logger

	// newClient 允许测试注入替身。nil 时用 vsanPerfNewClient。
	newClient vsanClientFactory
}

// NewvsanPerfCollector 沿用本包多数派命名（New<小写子系统名>Collector）。
//
// 与 vsan.go 同样不能在构造期解引用 logger：registry_test.go 的
// TestCreatorsProduceCollectors 会传 nil logger 调用每个 Creator。
func NewvsanPerfCollector(logger *slog.Logger) (collector.Collector, error) {
	return &vsanPerfCollector{logger: logger}, nil
}

// Update 采集 vSAN 性能指标。
//
// 与 vsan collector（组 A）分开是刻意的：组 A 是三次轻量查询，本 collector
// 是按实体类型逐个查询 CSV 并解析，代价高一个量级。用户可能想要健康与容量
// 却不想要性能数据，两个 flag 才能表达这个组合。
//
// 降级路径与组 A 一致：ESXi 直连整体跳过、单集群失败继续下一个。多一条
// 本 collector 特有的：vSAN 性能服务未开启时 vCenter 返回空数据而不是报错，
// 此时输出空白并留 Debug 日志 —— 这是设计稿 2.5 节写进 README 的前提条件。
func (c *vsanPerfCollector) Update(ctx context.Context, ch chan<- prometheus.Metric, s *collector.Scrape) error {
	if isESXi(s) {
		c.logger.Debug("skipping vsan.perf collector on a direct ESXi connection",
			"target_type", targetTypeESXi)
		return nil
	}

	var clusters []mo.ClusterComputeResource

	err := fetchProperties(
		ctx, s.View, s.Client,
		[]string{"ClusterComputeResource"}, []string{"name"}, &clusters, c.logger,
	)
	if err != nil {
		return err
	}

	if len(clusters) == 0 {
		c.logger.Debug("no cluster found, nothing to collect for vsan.perf")
		return nil
	}

	factory := c.newClient
	if factory == nil {
		factory = vsanPerfNewClient
	}

	client, err := factory(ctx, s)
	if err != nil {
		return fmt.Errorf("creating vsan client: %w", err)
	}

	// 两个 vsan.perf flag 在集群循环外快照一次。放循环里等于让同一轮抓取
	// 的不同集群用上不同的窗口长度或不同的协商策略 —— 见 vsanPerfSettings。
	cfg := currentVsanPerfSettings()

	for _, cluster := range clusters {
		if err := c.collectCluster(ctx, ch, s, client, cluster, cfg); err != nil {
			c.logger.Warn("vsan performance collection failed for cluster",
				"cluster", cluster.Name, "cmo", cluster.Self.Value, "err", err)
		}
	}

	return nil
}

// collectCluster 采集单个集群的性能指标。
func (c *vsanPerfCollector) collectCluster(
	ctx context.Context,
	ch chan<- prometheus.Metric,
	s *collector.Scrape,
	client vsanRoundTripper,
	cluster mo.ClusterComputeResource,
	cfg vsanPerfSettings,
) error {
	entities, err := c.resolveEntityTypes(ctx, client, cluster, cfg.skipVerify)
	if err != nil {
		return err
	}

	if len(entities) == 0 {
		// 交集为空。这里必须留下明确的日志：静默返回会让用户以为
		// collector 没生效，而实际情况是"这个环境不支持我们想采的任何
		// 实体类型"——两者的处置完全不同（后者该考虑 skip-verify）。
		c.logger.Warn("no supported vsan performance entity type matched the whitelist; "+
			"the cluster may not have the vSAN performance service enabled, "+
			"or try -collector.vsan.perf.skip-verify",
			"cluster", cluster.Name, "cmo", cluster.Self.Value,
			"whitelist", strings.Join(vsanPerfEntityWhitelist, ","))

		return nil
	}

	// 时间窗口以当前时刻为终点回看。startTime/endTime 是 *time.Time
	// （非 omitempty），必须都给值。
	end := time.Now().UTC()
	start := end.Add(-time.Duration(cfg.interval) * time.Second)

	specs := make([]vsantypes.VsanPerfQuerySpec, 0, len(entities))
	for _, entity := range entities {
		specs = append(specs, vsantypes.VsanPerfQuerySpec{
			// EntityRefId 形如 "cluster-domclient:*" —— 星号是通配，
			// 让 vCenter 返回该类型下的全部实体。telegraf 用同一形态
			// （vsan.go 的 "%s:*"）。
			EntityRefId: fmt.Sprintf("%s:*", entity),
			StartTime:   &start,
			EndTime:     &end,

			// Labels 是请求侧裁剪：白名单直接传给 vCenter，多余的指标
			// 根本不会下载。这是控制基数最有效的位置 —— 比拿回来再丢掉
			// 省掉了传输与解析。
			Labels: vsanPerfLabelWhitelist,
		})
	}

	clusterRef := cluster.Self

	resp, err := vsanmethods.VsanPerfQueryPerf(ctx, client, &vsantypes.VsanPerfQueryPerf{
		This:       vsan.VsanPerformanceManagerInstance,
		Cluster:    &clusterRef,
		QuerySpecs: specs,
	})
	if err != nil {
		return fmt.Errorf("querying vsan performance: %w", err)
	}

	if resp == nil || len(resp.Returnval) == 0 {
		// 空响应不是错误。vSAN 性能服务未开启时 vCenter 就返回空，
		// 这是最常见的"打开了 flag 却没有指标"的原因，所以留日志。
		c.logger.Debug("vsan performance query returned no data; "+
			"the vSAN performance service is probably not enabled on this cluster",
			"cluster", cluster.Name, "cmo", cluster.Self.Value)

		return nil
	}

	var errs []error

	for _, entityMetric := range resp.Returnval {
		if err := c.emitEntity(ch, s, cluster, entityMetric); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// resolveEntityTypes 决定本轮实际查询哪些实体类型。
//
// D4 的 C 方案：问 vCenter 支持什么，与白名单取交集。这把"环境支持什么"和
// "我们想采什么"分开了 —— 交集为空时能明确告诉用户原因，而硬编码清单在这种
// 情况下只会静默返回空数据。
func (c *vsanPerfCollector) resolveEntityTypes(
	ctx context.Context,
	client vsanRoundTripper,
	cluster mo.ClusterComputeResource,
	skipVerify bool,
) ([]string, error) {
	if skipVerify {
		// 逃生门：不问 vCenter，直接用白名单。见 flag 定义处的说明 ——
		// 这个 API 不申报全部可查实体类型，取交集会漏。
		c.logger.Debug("skipping vsan performance entity type negotiation",
			"cluster", cluster.Name)

		return vsanPerfEntityWhitelist, nil
	}

	resp, err := vsanmethods.VsanPerfGetSupportedEntityTypes(ctx, client,
		&vsantypes.VsanPerfGetSupportedEntityTypes{
			This: vsan.VsanPerformanceManagerInstance,
		})
	if err != nil {
		return nil, fmt.Errorf("querying supported vsan performance entity types: %w", err)
	}

	if resp == nil {
		return nil, errors.New("empty supported entity types response")
	}

	// 用 map 建索引再按白名单顺序取交集：保持输出顺序稳定（白名单顺序），
	// 而不是跟随 API 返回顺序。顺序稳定让日志与测试断言可复现。
	supported := make(map[string]bool, len(resp.Returnval))
	for _, t := range resp.Returnval {
		supported[t.Name] = true
	}

	matched := make([]string, 0, len(vsanPerfEntityWhitelist))
	for _, want := range vsanPerfEntityWhitelist {
		if supported[want] {
			matched = append(matched, want)
		}
	}

	return matched, nil
}

// emitEntity 解析单个实体的 CSV 性能数据并输出指标。
//
// 这是本 collector 的核心，也是与既有性能采集完全无法复用的地方：
// performance.EntityMetric 那边的值是 []int64 结构化数组，这里是逗号
// 分隔的字符串，两套解析一行都不通用。
func (c *vsanPerfCollector) emitEntity(
	ch chan<- prometheus.Metric,
	s *collector.Scrape,
	cluster mo.ClusterComputeResource,
	em vsantypes.VsanPerfEntityMetricCSV,
) error {
	// EntityRefId 形如 "host-domclient:52ab...uuid"。前半是实体类型，
	// 后半是实体标识（主机 uuid、磁盘 uuid 等）。切一次就够——uuid 本身
	// 不含冒号，用 SplitN 避免 uuid 里出现意外冒号时把标识截断。
	entityType, entityID, ok := strings.Cut(em.EntityRefId, ":")
	if !ok {
		// 没有冒号说明格式与预期不符。跳过而不是猜：把整串当实体类型
		// 会产出一条实体标识为空的序列，那比缺失更难排查。
		return fmt.Errorf("unexpected entity ref id %q", em.EntityRefId)
	}

	// SampleInfo 是逗号分隔的时间戳。我们并不把它当时间用（指标值取窗口
	// 内的平均，时间戳由 Prometheus 抓取时刻决定），但**必须解析它**：
	// 它的长度是校验 Values 长度的唯一依据。
	//
	// telegraf 在这里有一个越界隐患：它按 values 的下标去索引 timeStamps
	// （vsan.go 的 timeStamps[i]），两者长度不等时直接 panic。我们先校验
	// 长度再循环，不给这个可能性留位置。
	timestamps := splitCSV(em.SampleInfo)
	if len(timestamps) == 0 {
		c.logger.Debug("vsan performance entity has no sample info",
			"cluster", cluster.Name, "entity", em.EntityRefId)

		return nil
	}

	// 白名单在请求侧已经传给 vCenter（QuerySpec.Labels），这里再过一遍是
	// 因为 API 并不保证严格遵守：telegraf README 提到某些版本会返回未请求
	// 的 label。请求侧裁剪省流量，响应侧过滤保证基数上限是硬的。
	allowed := vsanPerfAllowedLabels()

	var errs []error

	for _, series := range em.Value {
		label := series.MetricId.Label
		if label == "" {
			continue
		}

		if !allowed[label] {
			c.logger.Debug("dropping a vsan performance label outside the whitelist",
				"cluster", cluster.Name, "entity", em.EntityRefId, "label", label)

			continue
		}

		values := splitCSV(series.Values)

		// 长度校验：不等就整条序列跳过，不做截断对齐。
		//
		// 截断看起来更宽容，但那是在猜"前 N 个点是对齐的"——若 API 少给了
		// 中间某个点，截断后每个值都配错了时刻。求平均虽然不用时间戳，
		// 但长度不等本身就说明这条响应不可信，宁可缺这一条指标。
		if len(values) != len(timestamps) {
			errs = append(errs, fmt.Errorf(
				"entity %s label %s: got %d values for %d samples",
				em.EntityRefId, label, len(values), len(timestamps)))

			continue
		}

		avg, n := averageCSVValues(values)
		if n == 0 {
			// 全部采样点都解析失败或为空。不输出 0——那会把"没数据"
			// 伪装成"值是零"，而 IOPS 为 0 和 IOPS 未知在告警上完全不同。
			c.logger.Debug("no parsable value in a vsan performance series",
				"cluster", cluster.Name, "entity", em.EntityRefId, "label", label)

			continue
		}

		desc := vsanPerfDesc(s.Namespace, entityType, label)

		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, avg,
			cluster.Self.Value, cluster.Name, entityType, entityID, s.Target)
	}

	return errors.Join(errs...)
}

// splitCSV 切分 vSAN 的逗号分隔字符串。
//
// 空串单独处理：strings.Split("", ",") 返回长度 1 的 [""]，会让
// "没有数据" 看起来像 "有一个空值"，长度校验就此失效。
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}

	return strings.Split(s, ",")
}

// averageCSVValues 求窗口内的平均值，返回平均值与参与计算的点数。
//
// **全部指标按瞬时量求平均**，不区分 delta 与 rate。这是刻意的：
// vSAN 的 VsanPerfMetricId 虽然带 StatsType/RollupType 字段，但
// telegraf 完全不读它们（只用 Label 做字段名），也就是说没有任何
// 现成的、经过验证的映射表可以照抄。自己造一张表意味着对每个 label
// 猜它是累加量还是瞬时量，猜错的方向是静默的数值错误。
//
// 而白名单里的 15 个 label 全部是瞬时量语义：iops/throughput 是
// vSAN 侧已经算好的速率（不是累计计数），latency 是平均延迟，
// congestion/oio/capacity/rc_hit_rate/wb_free_pct 都是瞬时读数。
// 对这类值求平均得到的正是"窗口内平均水平"，语义正确。
//
// 返回点数而不只是平均值：调用方要区分"平均值是 0"和"没有可用点"。
func averageCSVValues(values []string) (float64, int) {
	var sum float64
	var n int

	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v == "" {
			// vSAN 在实体刚上线、或某个采样点缺失时给空字段。
			// 跳过单点而不是整条丢弃——窗口里其余点仍然有效。
			continue
		}

		// 64 位而不是 telegraf 的 ParseFloat(v, 32)：32 位浮点只有约
		// 7 位有效十进制数字，大集群的 throughput（字节/秒，轻易过亿）
		// 会丢精度。Prometheus 的值本来就是 float64，没有理由先降精度。
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			// 单点解析失败只跳过这一点。整条丢弃过于严厉：一个畸形
			// 采样点不该让整个窗口的数据消失。
			continue
		}

		sum += f
		n++
	}

	if n == 0 {
		return 0, 0
	}

	return sum / float64(n), n
}

// vsanPerfAllowedLabels 把白名单切片转成集合，只算一次。
//
// sync.Once 而不是包级 var 初始化：白名单是同一文件里的包级切片，
// 两个包级 var 之间的初始化顺序由编译器按依赖推导，能工作但很脆弱。
// 显式的 Once 让依赖关系写在代码里。
var (
	vsanPerfAllowedOnce sync.Once
	vsanPerfAllowedSet  map[string]bool
)

func vsanPerfAllowedLabels() map[string]bool {
	vsanPerfAllowedOnce.Do(func() {
		vsanPerfAllowedSet = make(map[string]bool, len(vsanPerfLabelWhitelist))
		for _, l := range vsanPerfLabelWhitelist {
			vsanPerfAllowedSet[l] = true
		}
	})

	return vsanPerfAllowedSet
}

// vsanPerfDescKey 唯一标识一条 vSAN 性能指标的 Desc。
//
// 与 perfDescKey 平行但独立：那个 key 带 moType/instanced/legacy，
// 全部与 vSAN 无关；这里只有 namespace 与 label 决定指标名，实体类型
// 进的是 label 值而不是指标名。
type vsanPerfDescKey struct {
	namespace string
	label     string
}

var (
	vsanPerfDescMu    sync.Mutex
	vsanPerfDescCache = map[vsanPerfDescKey]*prometheus.Desc{}
)

// vsanPerfDesc 取（或构造）一条 vSAN 性能指标的 Desc。
//
// 指标名是 <namespace>_vsan_perf_<label>，实体类型作为 label 值而非
// 指标名的一部分。这个选择很关键：
//
// 若把实体类型拼进指标名（vsan_perf_host_domclient_iops_read），那么
// "集群总 IOPS 与各主机 IOPS 的对比" 就要跨两个指标名做 join，而
// PromQL 里跨指标名聚合远比按 label 聚合笨重。放进 label 后，
// sum by (entity) (vmware_vsan_perf_iops_read) 就是一行。
//
// 代价是同名指标下不同实体类型的语义略有差异（cluster-domclient 的
// iops_read 是集群合计，capacity-disk 的是单盘）。这由 entity label
// 显式区分，且 help 里写明了。
//
// entityType 不进 cache key：它不影响指标名也不影响 label 集合，
// 只是运行时的 label 值。进 key 只会让缓存条目数无谓地翻几倍。
func vsanPerfDesc(namespace, entityType, label string) *prometheus.Desc {
	key := vsanPerfDescKey{namespace: namespace, label: label}

	vsanPerfDescMu.Lock()
	defer vsanPerfDescMu.Unlock()

	if d, ok := vsanPerfDescCache[key]; ok {
		return d
	}

	// cmo / vmwcluster 与 vsanDescs 及 cluster.go 的 vmware_cluster_info
	// 一致，保证能 join 到集群信息。entity / entityid 是本 collector 特有：
	// 前者是实体类型（host-domclient 等），后者是该类型下的实体标识
	// （主机 uuid、磁盘 uuid），vSAN API 只给 uuid 不给友好名。
	//
	// !!! 顺序即 MustNewConstMetric 的传值顺序 !!!
	// cmo, vmwcluster, entity, entityid, vcenter
	d := prometheus.NewDesc(
		prometheus.BuildFQName(namespace, vsanPerfSubsystem, label),
		fmt.Sprintf("vSAN performance metric %q, averaged over the query window. "+
			"The entity label is the vSAN performance entity type and entityid "+
			"is the entity UUID reported by vSAN.", label),
		[]string{"cmo", "vmwcluster", "entity", "entityid", "vcenter"}, nil,
	)

	vsanPerfDescCache[key] = d

	return d
}
