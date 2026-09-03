# 设计：resourcepool 与 vsan 两个新采集器

**状态：设计稿，待确认。本文不含任何代码改动。**

范围由用户界定：
- **要做**：
  - **ResourcePool —— 默认启用**（`DefaultEnabled`）
  - **vSAN —— 默认禁用**（`DefaultDisabled`），按需开启
- **不做**：Network（端口组）、分布式交换机（DVS）—— 从缺口分析的 P1 中移除。
- 参考实现：telegraf `inputs.vsphere` 插件的配置与指标口径。

---

## 〇、先说三个已查证的前提

设计基于实测，不是推测。这三条决定了整个方案的形状。

**本节所有 API 假设都过了编译验证**，不是只读了声明：写了一个临时探针
文件把下面用到的每个调用都写成真实代码，`go vet` 通过后删除。验证覆盖：
`vsan.NewClient` 的返回值可直接当 `soap.RoundTripper` 传给
`vsanmethods.*`（因为 `vsan.Client` 嵌入了 `*soap.Client`）、
`VsanQuerySpaceUsage` / `VsanQueryVcClusterHealthSummary` /
`VsanPerfQueryPerf` / `VsanPerfGetSupportedEntityTypes` 四个方法的
参数与返回结构、以及 `SampleInfo` 与 `Values` 确实是 `string`
（用 `var x string = em.SampleInfo` 这种显式赋值让编译器证明）。

这一步值得做，因为它推翻了一个我原本会写错的地方：**`vsan.Client` 只封装了
5 个方法，容量、健康、支持实体类型这三个都不在其中**，必须像 telegraf 一样
直接调 `vsan/methods` 包的函数。若照初稿的想法去找 `vsanClient.QuerySpaceUsage()`
这类方法，会在实现时才发现不存在。

### 1. `vsan` 包已在 govmomi 主模块内，不需要新依赖

实测 `govmomi@v0.56.0/vsan/`：

```
client.go  methods/  mo/  simulator/  types/  vsanfs/
```

上一版缺口分析里我把 vSAN 列在"档 4：需要新 SDK"，**这个判断需要修正** ——
`go.mod` 不需要增加任何 require 行。vSAN 因此从档 4 降到档 2.5。

`vsan/methods/methods.go` 实测有 **432 个导出函数**，本设计需要的
5 个全在里面（行号：`VsanPerfGetSupportedEntityTypes` 402、
`VsanQueryVcClusterHealthSummary` 2142、`VsanQuerySpaceUsage` 2642）。

### 2. vSAN 客户端复用现有会话，不需要二次认证

`vsan/client.go:58-61`：

```go
func NewClient(ctx context.Context, c *vim25.Client) (*Client, error) {
	sc := c.Client.NewServiceClient(Path, Namespace)
	return &Client{sc, sc, c}, nil
}
```

它从已登录的 `vim25.Client` 派生一个 service client，走同一个 vCenter 地址的
不同 SOAP 路径（vsan-health endpoint）。**这与 Tag/vAPI 的情况完全不同** ——
后者要建独立会话，前者只是同一连接上的另一个 namespace。

`s.Client` 在 `Scrape` 里已经有了，直接传进去即可。

**但 `vsan.Client` 的方法集不够用**，这点初稿没写清。它只封装了 5 个：
`VsanClusterGetConfig`、`VsanClusterReconfig`、`VsanPerfQueryPerf`、
`VsanQueryObjectIdentities`、`VsanHostGetConfig`。
本设计要的容量（`VsanQuerySpaceUsage`）、健康
（`VsanQueryVcClusterHealthSummary`）、实体类型
（`VsanPerfGetSupportedEntityTypes`）**都不在其中**。

用法与 telegraf 一致：把 `*vsan.Client` 当 `soap.RoundTripper` 传给
`vsan/methods` 的包级函数。之所以可行，是因为 `vsan.Client` 嵌入了
`*soap.Client` 且有 `RoundTrip` 方法（`vsan/client.go:50-67`）：

```go
vc, _ := vsan.NewClient(ctx, s.Client)
resp, err := vsanmethods.VsanQuerySpaceUsage(ctx, vc, &vsantypes.VsanQuerySpaceUsage{
    This:    vsanSpaceReportSystemRef,   // 固定 MoRef，见 2.2
    Cluster: clusterRef,
})
```

**这个形态对测试是好消息**：`methods` 函数的第二个参数是接口
`soap.RoundTripper`，所以替身只要实现
`RoundTrip(ctx, req, res soap.HasFault) error` 一个方法就能注入 ——
不需要起 HTTP 服务，也不需要 vcsim 支持这些 API。详见第三节。

### 3. vSAN 性能数据是 CSV 字符串，不能复用现有 perf 路径

`vsan/types/types.go`：

```go
type VsanPerfEntityMetricCSV struct {
	EntityRefId string                    `xml:"entityRefId"`
	SampleInfo  string                    `xml:"sampleInfo,omitempty"`   // ← 字符串
	Value       []VsanPerfMetricSeriesCSV `xml:"value,omitempty"`
}

type VsanPerfMetricSeriesCSV struct {
	MetricId VsanPerfMetricId `xml:"metricId"`
	Values   string           `xml:"values,omitempty"`               // ← 字符串
}
```

对比现有的 `performance.EntityMetric` —— 那边 `Value` 是 `[]int64`，
`SampleInfo` 是 `[]types.PerfSampleInfo`，都是结构化的。

**后果**：`emitPerformanceMetrics` / `translatePerfCounter` / `perfDesc`
这条链路**一行都不能复用**。vSAN 需要自己的解析与命名逻辑，包括：
- 按逗号切分 `Values`，按逗号切分 `SampleInfo`（后者形如
  `2026-09-02 10:00:00,2026-09-02 10:05:00`）
- 自己校验两者长度一致（现有 perf 路径有这个检查，vSAN 路径要重写一遍）
- 自己决定单位换算与 counter/gauge 判定 —— vSAN 的 `VsanPerfMetricId` 有
  `StatsType` 和 `RollupType` 字段，但语义与 vSphere 的 `PerfCounterInfo` 不同

这是本设计里**最大的一块工作量**，也是我建议把 vSAN 拆成独立一轮做的原因。

#### telegraf 的实际解析步骤（源码验证，比初稿的描述更精确）

`vsan.go:256-304` 的完整流程，抄的时候有三处必须注意：

```go
// 1. 时间戳格式是固定的，无时区
utcTimeStamp, err := time.Parse("2006-01-02 15:04:05", t)
// 解析失败时 telegraf 塞一个零值占位，而不是跳过整个序列：
timeStamps = append(timeStamps, time.Time{})

// 2. 用 timeStamps[i] 按下标对齐 Values 的第 i 个
for i, values := range strings.Split(counter.Values, ",") {
    ts := timeStamps[i]        // ← 这里有越界风险
    if ts.IsZero() { continue }

// 3. 值一律按 float32 解析，失败则该点静默丢弃
if v, err := strconv.ParseFloat(values, 32); err == nil {
    bucket.fields[metricLabel] = v
}
```

**三处要注意的**：

1. **`timeStamps[i]` 会 panic**。若某个 counter 的 `Values` 比
   `SampleInfo` 长，这里就越界。telegraf 用"零值占位"保证了两者等长
   （只要都来自同一次 split），但这依赖 vCenter 返回的两者严格等长 ——
   没有显式校验。**我们必须先比长度再循环**，这就是前提③里说的
   "自己校验两者长度一致"，现在有了具体的失败形态。
2. **时间戳没有时区**。`"2006-01-02 15:04:05"` 配 `time.Parse`（非
   `ParseInLocation`）会解析成 UTC。telegraf 变量名 `utcTimeStamp` 说明
   这是有意的。**我们的实现要注意**：Prometheus 的 exporter 一般不自带
   时间戳（由 scrape 时刻决定），所以这些历史采样点**根本无法原样导出** ——
   这是 vSAN 性能数据与 Prometheus 模型的一个根本张力。
3. **`ParseFloat(values, 32)` 用的是 32 位**。对 IOPS、延迟这类值
   float32 只有约 7 位有效数字，大集群的累计字节数会丢精度。
   我们应当用 64。

**第 2 点是需要在 R3 认真处理的设计问题**，不是实现细节：
telegraf 是时序数据库客户端，可以给每个采样点带上自己的时间戳；
Prometheus exporter 不能（`prometheus.NewMetricWithTimestamp` 存在，但带
时间戳的样本会被大多数配置拒绝或产生乱序）。**已核实本项目全程只用
`MustNewConstMetric`，从未用过带时间戳的变体。**

所以窗口内的多个采样点必须**聚合成一个值**。怎么聚合有现成答案 ——
`vmware.go:119-130` 是既有的处理方式，且带着一条 bug 修复的记录：

```go
var raw float64
for _, subvalue := range value.Value {
	raw += float64(subvalue)          // 先一律求和
}
if !mapped || !spec.Delta {
	raw /= float64(len(value.Value))  // 非 delta 才求平均
}
```

注释里解释了为什么不能一律求平均：delta 计数器（如
`cpu.ready.summation`）每个样本是**区间增量**，3 个 20 秒区间各
ready 100ms 意味着这一分钟共 300ms，求平均得到 100ms 是错的。

**vSAN 路径必须沿用同一套规则**，而不是我上一版写的"取最后一个采样点"。
取最后一个对 delta 类指标同样是错的（丢掉了前面几个区间的量），
而且与现有 host/vm 指标的口径不一致 —— 同一个 exporter 里两种聚合语义
是维护灾难。

难点在于**判断 vSAN 指标是不是 delta**：现有路径靠
`PerfCounterInfo.UnitInfo` + `perfnames.go` 的映射表得出 `spec.Delta`，
而 vSAN 的 `VsanPerfMetricId` 有自己的 `StatsType` / `RollupType` 字段
（语义与 vSphere 的不同，见前提③）。**R3 实现时已确认 telegraf 完全不读
`StatsType`/`RollupType`（只用 `Label` 做蛇形字段名），且白名单的 15 个
label 全部是瞬时量语义（iops/throughput 已是速率、latency 已是平均延迟、
congestion/oio/capacity 是瞬时读数），因此最终采用**全部按窗口内求平均**
的聚合策略，不建 delta 映射表。**这是设计稿该处唯一被实证推翻的假设**：从
"需要建映射表"回退为"全平均，反被 telegraf 证据简化了"。

---

## 一、ResourcePool 采集器设计

### 1.1 数据来源：runtime 属性，不需要性能计数器

这是个重要的设计选择。telegraf 走的是性能计数器路径（`cpu.usagemhz.average`
等 47 个计数器），但 vSphere 的 `ResourcePool` **runtime 属性里已经有全部关键
数值**，实测 `mo.ResourcePool`：

```go
type ResourcePool struct {
	ManagedEntity
	Summary  types.BaseResourcePoolSummary
	Runtime  types.ResourcePoolRuntimeInfo   // ← 用量在这里
	Owner    types.ManagedObjectReference    // ← 所属 Cluster/ComputeResource
	ResourcePool []types.ManagedObjectReference  // ← 子资源池
	Vm       []types.ManagedObjectReference  // ← 池内虚机
	Config   types.ResourceConfigSpec        // ← reservation/limit/shares
}
```

`Runtime.Cpu` 与 `Runtime.Memory` 都是 `ResourcePoolResourceUsage`，字段实测：

```go
ReservationUsed      int64
ReservationUsedForVm int64
UnreservedForPool    int64
UnreservedForVm      int64
OverallUsage         int64
MaxUsage             int64
```

**内存值的单位是字节**（govmomi 注释明确 "Values are in bytes"），
CPU 值的单位是 MHz。

**选属性路径而非性能计数器的理由**：
1. 属性检索一次 ContainerView 拿全部资源池，性能计数器要额外一轮
   `QueryPerf`，往返翻倍。
2. `reservation` / `limit` / `shares` 这三个**最能解释"VM 为什么拿不到 CPU"
   的配置项，只在 `Config` 里，性能计数器里没有**。
3. 属性值是当前瞬时值，没有采样窗口与 rollup 的歧义 —— 而 vSphere 资源池的
   性能计数器有 `.average` / `.minimum` / `.maximum` 三套，混用容易出错。

**取舍**：放弃了 telegraf 有的 `mem.compressed`、`power.energy` 等细项。
这些对"资源池是否成为瓶颈"的判断没有必要性，且 power 类在资源池层级本身就是
下层主机的汇总，语义可疑。

### 1.1.1 telegraf 的资源池实现有个 O(VM×池) 问题，我们不抄

读源码时发现的。telegraf 需要给每台 VM 打上 `rpname` 标签，
它的做法（`endpoint.go:737-745`）是：

```go
func getResourcePoolName(rp types.ManagedObjectReference, rps objectMap) string {
	for _, r := range rps {          // ← 线性扫整个资源池 map
		if r.ref == rp {
			return r.name
		}
	}
	return "Resources"
}
```

而这个函数在 `getVMs` 的**每台 VM 循环里**被调用（`endpoint.go:801`）：

```go
for i := range resources {                                  // 每台 VM
	rpname := getResourcePoolName(*r.ResourcePool, resourcePools)  // O(池数)
```

所以整体是 **O(VM 数 × 资源池数)**。1000 台 VM × 50 个池 = 5 万次比较，
每次还是结构体比较。它本该是一个 `map[MoRef]string` 的 O(1) 查表。

**我们的方向本来就不同，天然避开了这个问题**：`mo.ResourcePool` 自带
`Vm []ManagedObjectReference`（上面结构体的第 6 个字段，已实测确认），
**反向关系直接挂在资源池对象上**，不需要从 VM 侧反查。1.3 节的
`vmware_resourcepool_vm` 一虚机一序列就是直接遍历这个数组产出的。

顺带记一个 telegraf 的真实缺陷：`endpoint.go:801` 的 `*r.ResourcePool`
是**裸解引用**，而 `mo.VirtualMachine.ResourcePool` 的类型是
`*types.ManagedObjectReference`（`vim25/mo/mo.go:113`，已核实是指针）。
它只在 794 行过滤了 `PowerState != poweredOn`，没有 nil 检查。
vSphere 里模板（template）与孤立（orphaned）VM 的 `resourcePool` 可以为 nil。

**我们的实现要有 nil 检查**，这不是抄不抄的问题，是别踩同一个坑。

### 1.2 属性列表

```go
[]string{
	"name", "parent", "owner",
	"summary", "runtime", "config",
	"vm", "resourcePool",
	"overallStatus",     // ← 顺带把缺口分析 #12 在这个 collector 上落实
}
```

`overallStatus` 在这里是免费的，新 collector 不背历史包袱，直接采上。

### 1.3 指标设计

subsystem 用 `resourcepool`（全小写，与现有 `datacenter` / `datastore`
风格一致，不用下划线）。

**关系与身份：**

```
vmware_resourcepool_info{rpmo, rp, parentmo, ownermo, vcenter}                1
vmware_resourcepool_overall_status{rpmo, rp, status, vcenter}                 1
vmware_resourcepool_vm{rpmo, rp, vmmo, vcenter}                              1
```

`vmware_resourcepool_vm` 一个虚机一条序列 —— 这是缺口分析 Q2 里"VM 与 Cluster
之间缺失的一层"的补齐点，让 `vm → resourcepool → cluster` 可以 join 起来。
**沿用 `cluster_datastore` 的既有做法**（`cluster.go:57-69` 已经因为
"逗号拼接会产生僵尸序列"这个理由改成一对一了），不重犯那个错。

**CPU 与内存用量（gauge，label 相同故合并列出）：**

```
vmware_resourcepool_cpu_usage_hertz{rpmo, rp, vcenter}                        # Runtime.Cpu.OverallUsage × 1e6
vmware_resourcepool_cpu_max_usage_hertz{rpmo, rp, vcenter}                    # Runtime.Cpu.MaxUsage × 1e6
vmware_resourcepool_cpu_reservation_used_hertz{rpmo, rp, vcenter}
vmware_resourcepool_cpu_unreserved_hertz{rpmo, rp, vcenter}                   # UnreservedForVm
vmware_resourcepool_mem_usage_bytes{rpmo, rp, vcenter}                        # Runtime.Memory.OverallUsage
vmware_resourcepool_mem_max_usage_bytes{rpmo, rp, vcenter}
vmware_resourcepool_mem_reservation_used_bytes{rpmo, rp, vcenter}
vmware_resourcepool_mem_unreserved_bytes{rpmo, rp, vcenter}
```

**注意 CPU 的 `× 1e6`**：vSphere 给的是 MHz，本项目已确立"导出基础单位"的
约定（`perfnames.go:60` 的 `megaHertz → hertz, factor 1e6`）。资源池这里必须
一致，否则同一个 dashboard 上 host 的 hertz 与资源池的 MHz 会差 6 个数量级。

**配置项（reservation / limit / shares）：**

```
vmware_resourcepool_cpu_reservation_hertz{rpmo, rp, vcenter}                  # Config.CpuAllocation.Reservation × 1e6
vmware_resourcepool_cpu_limit_hertz{rpmo, rp, vcenter}                        # -1 表示 unlimited，见下
vmware_resourcepool_cpu_shares{rpmo, rp, level, vcenter}
vmware_resourcepool_mem_reservation_bytes{rpmo, rp, vcenter}                  # MB → ×1048576
vmware_resourcepool_mem_limit_bytes{rpmo, rp, vcenter}
vmware_resourcepool_mem_shares{rpmo, rp, level, vcenter}
```

**`limit = -1` 的处理需要拍板**（见第四节 D2）。vSphere 用 `-1` 表示无限制，
直接导出 `-1` 会让 `limit - usage` 这类查询算出负数。

`shares` 的 `level` label 取 `low` / `normal` / `high` / `custom`，
value 取实际 share 数 —— 两者都有用：level 是用户配的语义，数值才能算相对权重。

### 1.4 ESXi 上的行为

ESXi 直连时存在一个根资源池（`ha-root-pool`）。参照 `datacenter.go:85` 对
`ha-datacenter` 的既有处理，**打 synthetic 标记**：

```go
if isESXi(s) && rp.Self.Value == "ha-root-pool" {
	labels = syntheticLabels(labels)
}
```

理由与那边一致：让"这不是用户创建的资源池"在指标层面可见，而不是静默混进
真实数据里。

### 1.5 规模风险（默认启用后需要正面处理）

资源池数量通常远小于 VM 数，但**嵌套资源池在某些环境里会很多** —— DRS 会为
每个 vApp 建池，某些自动化平台也会按租户/项目批量建池。一次 ContainerView
检索的成本与实体数线性相关。

**默认启用意味着这个风险不能再靠"用户没开"来回避**，需要三点应对：

1. **只检索必要属性**。属性列表见 1.2，刻意不含 `childConfiguration`
   （嵌套池的配置数组，深层嵌套时体积可观）。
2. **序列基数要评估清楚**。每个资源池产出约 14 条固定序列，加上
   `resourcepool_vm` 的一虚机一条。**后者是基数主项** —— 1000 台 VM 就是
   1000 条 `resourcepool_vm` 序列。这个量级本身可接受（与
   `vmware_vm_info` 同级），但要在 README 里写明，不能让用户被 TSDB
   增长意外。
3. **性能验证要覆盖大规模模型**。`simulator.VPX()` 默认模型太小，
   测试里应显式调大实体数（`model.Pool` / `model.Machine`），
   确认检索耗时随实体数的增长是线性而非二次 —— 二次增长通常意味着写了
   嵌套遍历。

若实测发现深层嵌套确实构成问题，再考虑加深度上限 flag；
**现在不预先加**，那属于没有证据支撑的复杂度。

---

## 二、vSAN 采集器设计

### 2.1 telegraf 的口径与本项目的差异

telegraf 把 vSAN 指标分成两类前缀，这个划分值得照搬：

| 前缀 | 性质 | telegraf 建议间隔 |
|---|---|---|
| `summary.*` | 实时（disk-usage / health / resync） | 30s |
| `performance.*` | 历史，vSAN 性能服务 5 分钟滚动 | 300s |

telegraf 的配置是 `vsan_metric_include` 白名单 + `vsan_metric_exclude`
（**默认 `["*"]`，即默认不采**）+ `vsan_interval`（默认 `5m`）。

**本项目的差异**：我们没有 telegraf 那套 include/exclude 通配符机制，
collector 的粒度是 `-collector.<name>` 布尔开关。所以口径要换个形式表达 ——
见下面 2.3 的 flag 设计。

### 2.1.1 读完 telegraf 源码后的修正（第二轮参考）

初稿只读了 telegraf 的 README。这轮通读了 `vsan.go`（554 行）、
`endpoint.go` 的资源池部分、`sample.conf` 与 README 的 vSAN 章节，
得到**六处该抄、两处该反着做**。

源码取自 `influxdata/telegraf` master 分支的
`plugins/inputs/vsphere/`，本节所有行号都指该处文件。

#### 该抄的 ①：健康值映射成数值，且要处理缓存空值

telegraf `vsan.go:339` 把健康状态映射成 `{"red":2,"yellow":1,"green":0}`
的整数。更重要的是 341-365 行那段**两阶段读取**：

```go
// 先读 vCenter 缓存的健康摘要
summary.FetchFromCache = new(true)
cached, err := VsanQueryVcClusterHealthSummary(...)
if _, found := healthMap[cached.Returnval.OverallHealth]; !found {
    // 缓存是空的 —— 关掉缓存强制重算
    summary.FetchFromCache = new(false)
    uncached, err := ...
}
```

**这个降级路径我完全没想到。** vCenter 的健康摘要缓存在某些时刻是空的
（刚重启、刚启用 vSAN、健康服务刚重载），此时 `OverallHealth` 返回的是
空字符串或未知值。只读缓存会静默产出一条错误的 `unknown` 序列。

代价是明确的：`FetchFromCache=false` 会触发 vCenter 真的去跑一遍健康检查，
慢得多。所以顺序必须是"先缓存、失败才回退"，不能反。

**采纳**，但本项目要在此之上多做一步：telegraf 在第二次也失败时是
`return nil`（`vsan.go:362-363` 静默跳过）。我们应当输出
`vmware_vsan_health_status{status="unknown"} 1` —— 静默跳过会让"健康检查
失效"和"vSAN 不存在"在指标上无法区分，这正是缺口分析里批评过的口径缺陷 B。

#### 该抄的 ②：`resync` 要 API 版本门槛，且 MoRef 得手工拼

telegraf `queryResyncSummary`（`vsan.go:376-380`）先查 API 版本：
**低于 6.7 直接跳过 resync**。我的设计稿没有任何版本判断。

更值得注意的是 386-397 行：`VsanSystemEx` 这个 MO 没有公开的查询入口，
telegraf 的做法是**从集群第一台主机的 MoRef 手工拼出来**：

```go
hostRefValue := hosts[0].Reference().Value   // "host-42"
vsanSystemEx := types.ManagedObjectReference{
    Type:  "VsanSystemEx",
    Value: "vsanSystemEx-" + strings.Split(hostRefValue, "-")[1],  // "vsanSystemEx-42"
}
```

它还专门校验了 `host-<num>` 这个形态（`len(parts) != 2` 就报错退出）——
说明这个假设脆到需要防御。

**结论：resync 三个指标降到组 B 或更后**。它需要版本门槛 + 拼 MoRef +
依赖"集群至少有一台可达主机"，三个前提都可能不成立。组 A 应当只保留
容量与健康这两个真正稳的。这是对我原设计的降级，不是加码。

**R3 落地记录（三处刻意不照抄 telegraf）：**

1. **多主机轮询而不是只试 `hosts[0]`。** telegraf 取集群第一台主机就
   收工，那台恰好在维护模式时 resync 直接没有数据。我们遍历集群内
   所有 `poweredOn` 主机，逐台试到某台成功为止——这正是 telegraf 自己
   在 CMMDS 上用的容错模式，只是它没用在 resync 上。

2. **`host-<num>` 形态校验要求数字段真的是数字。** telegraf 只检查
   `len(parts) != 2`，所以 `host-abc` 能通过并拼出不存在的
   `vsanSystemEx-abc`。而且它校验失败时 `return err`，此刻 err 恒为
   nil（上一次赋值是成功的 `clusterObj.Hosts`）——静默跳过且不记为失败。

3. **版本号无法解析时不做 resync。** telegraf 的 `versionLowerThan` 在
   主版本解析失败时 `return false`（判定为"不低于"），于是畸形版本号会
   继续往下调一个可能不存在的方法。对 exporter 来说宁可少一条指标，
   也不要每轮抓取都产生一次注定失败的往返和一条错误日志。

指标单位：`totalRecoveryETA` 的单位是**秒**，由 vSAN Management API 文档
明确规定（"The estimated time in seconds to recover all vSAN objects"），
所以指标名是 `vmware_vsan_resync_recovery_seconds` 而不是照抄 API 的 eta。

#### 该抄的 ③：CMMDS 查询要在多台主机上轮询重试

telegraf `getCmmdsMap`（`vsan.go:169-185`）的注释写得很直白：
"Some esx host can be down or in maintenance mode. Hence cmmds query might
fail on such hosts. We iterate until we get proper api response."

它遍历集群所有主机，**逐台尝试直到某台成功**，全失败才报错。
这与缺口分析里的口径缺陷 A（关机/维护主机被过滤）是同一类问题的两面：
telegraf 是在采集侧容忍主机不可达，我们是在过滤侧误删了实体。

**若做组 B 的盘级指标就必须抄这个**，因为盘的 hostname/devicename 只能从
CMMDS 拿到。但这也说明组 B 的复杂度比我初稿估的更高。

**R3 实测后的修正：CMMDS 不做。** R2 落地时发现健康摘要
（`VsanQueryVcClusterHealthSummary` 的 `physicalDisksHealth`）已经同时给出
`Hostname`、盘的 `Name` 与 `Uuid`——也就是 CMMDS 那张映射表的全部内容，
而且是一次调用拿到，不必逐台主机轮询。组 A 的 `vmware_vsan_disk_health`
正是用它输出的（`vsan.go` 的 `emitDiskHealth`）。

组 B 的 `entityid` label 携带的就是同一个盘 uuid，所以要把性能数据关联到
盘名与主机名，PromQL 侧一个 join 即可：

```promql
vmware_vsan_perf_latency_read{entity="capacity-disk"}
  * on (entityid) group_left(host, device)
  label_replace(vmware_vsan_disk_health, "entityid", "$1", "uuid", "(.*)")
```

这比在 exporter 里多打一套 CMMDS 轮询更划算：少一类 SOAP 调用、少一处
"所有主机都不可达"的失败模式，而代价只是查询侧多写一行 join。

**但 telegraf 的多主机轮询模式本身仍然抄了**，用在 resync 上——见上面
"该抄的②"，resync 的 `VsanSystemEx` MoRef 同样是从主机 MoRef 拼出来的，
同样会因单台主机不可达而失败。

#### 该抄的 ④：`GetSupportedEntityTypes` 让实体清单不必硬编码

我的 D4 在纠结"先做哪 5 个实体类型"。telegraf 根本不硬编码 —— 
`getVsanMetadata`（`vsan.go:135-148`）调
`VsanPerfGetSupportedEntityTypes` 问 vCenter **这个环境支持什么**，
再用用户的 include/exclude 过滤。

已核实该方法在 govmomi v0.56.0 可用：
`vsan/methods/methods.go:402 func VsanPerfGetSupportedEntityTypes(...)`。

telegraf 还留了 `vsan_metric_skip_verify` 逃生门，README 的解释是
"some performance entities are not returned by the API, but we want to offer
the flexibility if you really need the stats" —— 即这个 API 本身不完备。
（原文在 `README.md:1045-1051`，已逐字核对；`endpoint.go:253` 处
vsan 的 `simple` 字段直接接 `VSANMetricSkipVerify`，不像其他资源那样
走 `isSimple()` 推导 —— 说明这是专为 vSAN 开的例外。）

**这改变了 D4 的性质**：不再是"我们选哪几个"，而是"要不要按环境自适应"。
详见改写后的 D4。

#### 该抄的 ⑤：telegraf 官方推荐的实体清单，恰好就是我选的那几个

README.md:1039 给的示例配置是：

```toml
vsan_metric_include = ["summary.*", "performance.host-domclient",
  "performance.cache-disk", "performance.disk-group", "performance.capacity-disk"]
```

去掉 `summary.*` 之后是 4 个性能实体类型：`host-domclient`、`cache-disk`、
`disk-group`、`capacity-disk`。我的 2.2 节选了 5 个 —— 上面 4 个
**外加 `cluster-domclient`**。

这是个有用的交叉验证：官方推荐清单与我的选择重合 4/5，说明这几个确实是
"最该先做的"。多出的 `cluster-domclient` 我建议保留 —— 集群级 IOPS/延迟
是最常看的第一眼指标，telegraf 的示例省掉它可能只是因为 `host-domclient`
可以在 PromQL 里聚合出来。**但这条要写进 README**：说明集群级值可由
主机级聚合得到，用户若嫌基数高可以只留一个。

#### 该抄的 ⑥：vSAN 只在集群级采集，且集群范围要可筛

README.md:1053-1056 明确 "vSAN metrics are only collected on the cluster
level"，并提供 `vsan_cluster_include`（默认 `/*/host/**`）来限定
**哪些集群参与 vSAN 采集**。

我的设计稿默认对所有集群逐个查 vSAN，没有这个筛选维度。这在混合环境里
是实际负担：**只有部分集群启用了 vSAN，其余集群每轮都白跑一次
`VsanClusterGetConfig`**。

不过本项目不需要照抄成一个新 flag ——「该反着做的 ⑦」的 `configurationEx`
方案已经天然解决了它：集群属性里读到 `Enabled == false` 就直接跳过，
不发任何 vsan 端点请求。**这比 telegraf 的路径筛选更好**：不需要用户
手工维护集群清单，也不会因为清单过期而漏采新建的 vSAN 集群。

这一条记在这里是为了说明：telegraf 之所以需要 `vsan_cluster_include`，
正因为它的 `vsanEnabled` 判断本身就要一次额外往返（见下条），
筛掉集群才能省掉那次往返。我们不欠这笔债，所以不需要这个 flag。

#### 该反着做的 ⑦：vSAN 启用判断不该用 vsan 端点

telegraf `vsanEnabled`（`vsan.go:106-113`）走的是 **vim25 侧**：

```go
config, err := clusterObj.Configuration(ctx)   // 属性 configurationEx
enabled := config.VsanConfigInfo.Enabled
```

我的初稿写的是 `Client.VsanClusterGetConfig`（vsan SOAP 端点）。
实测核对了类型链：

- `object.ClusterComputeResource.Configuration()`
  （`object/cluster_compute_resource.go:26-35`）只检索一个属性
  `configurationEx`，返回 `*types.ClusterConfigInfoEx`。
- `ClusterConfigInfoEx.VsanConfigInfo *VsanClusterConfigInfo`
  —— 确认存在于 v0.56.0。
- 注意**不在** `ClusterConfigInfo` 里（我查过，那个结构体没有 vSAN 字段），
  必须是 `...Ex`，走 `configurationEx` 而非 `configuration`。

**这条比我的方案严格更优，理由是它能合并进现有请求。**
`cluster.go:38` 已经在检索 `ClusterComputeResource` 的
`{name, summary, datastore, parent}`。把 `configurationEx` 加进这个列表，
vSAN 启用状态就是**零额外往返**拿到的 —— 而 `VsanClusterGetConfig` 是
每集群一次独立的 SOAP 调用。

连带的好处：`vmware_vsan_enabled` 可以由 **cluster collector** 输出，
于是"vSAN 默认禁用"时用户**仍然知道哪些集群启用了 vSAN**。这解决了默认
禁用带来的发现性问题 —— 用户不必先猜着开一次 vSAN collector 才知道自己
有没有 vSAN。

代价：`configurationEx` 是个不小的结构（含完整 DAS/DRS 配置）。需要在
R1/R2 实测这次检索的体积增长，若明显则改回独立调用。**这是需要实测而非
现在拍板的**。

#### 该反着做的 ⑧：不抄它的资源池反查

见 1.1 节末尾新增的说明 —— telegraf 的资源池名查找是 O(VM×池)。

### 2.2 采什么：三组，按代价排序

#### 组 A：集群健康与容量（`summary.*` 等价物）—— 建议第一批做

来源实测可用的 API：

| API | 用途 |
|---|---|
| `VsanClusterGetConfig` | vSAN 是否启用、去重压缩是否启用 |
| `VsanQuerySpaceUsage` | 集群容量：总量、可用 |
| `VsanQueryVcClusterHealthSummary` | 集群整体健康 **+ 物理盘健康**（一次拿两样） |
| ~~`VsanQueryClusterPhysicalDiskHealthSummary`~~ | ~~物理盘健康~~ → **要 ESXi root 密码，走不通，见 2.2.1** |
| ~~`VsanQueryObjectIdentities`~~ | ~~对象健康与 resync 状态~~ → 推迟到 R3 |

指标设计：

```
vmware_vsan_enabled{cmo, vmwcluster, vcenter}                                  0|1
vmware_vsan_capacity_bytes{cmo, vmwcluster, vcenter}
vmware_vsan_capacity_free_bytes{cmo, vmwcluster, vcenter}
vmware_vsan_capacity_used_bytes{cmo, vmwcluster, vcenter}      # = total - free，见 2.2.2
vmware_vsan_health_status{cmo, vmwcluster, status="green|yellow|red|unknown", vcenter}   1
vmware_vsan_disk_health{cmo, vmwcluster, host, device, uuid, state, vcenter}    1
vmware_vsan_disk_capacity_bytes{cmo, vmwcluster, host, device, vcenter}
vmware_vsan_disk_capacity_used_bytes{cmo, vmwcluster, host, device, vcenter}
vmware_vsan_dedup_enabled{cmo, vmwcluster, vcenter}                            0|1

# 以下三条原计划推迟到 R3（理由见 2.1.1 节②：版本门槛 + 拼 MoRef +
# 依赖主机可达）。R3 已实现，代码在 vsan.go 的 collectResync，
# 仍归属 vsan collector（组 A 的 flag），不占用 vsan.perf 那个开关。
vmware_vsan_resync_bytes{cmo, vmwcluster, vcenter}
vmware_vsan_resync_objects{cmo, vmwcluster, vcenter}
vmware_vsan_resync_recovery_seconds{cmo, vmwcluster, vcenter}   # 单位：秒，API 文档明确
```

#### 2.2.1 盘健康不能用 `VsanQueryClusterPhysicalDiskHealthSummary`（R2 实测发现）

这是对本设计稿的**实质修正**。初稿把该 API 列进组 A，但核对 v0.56.0 的请求
体之后发现它走不通：

```go
// vsan/types/types.go:4081
type VsanQueryClusterPhysicalDiskHealthSummaryRequestType struct {
    This            types.ManagedObjectReference `xml:"_this"`
    Hosts           []string                     `xml:"hosts"`
    EsxRootPassword string                       `xml:"esxRootPassword"`   // ← 硬阻断
}
```

**它要 ESXi 的 root 密码。** 本项目的采集账号是只读 vCenter 账号（README 的
权限章节就是这么写的），拿不到也不该拿 ESXi root 密码 —— 让一个 exporter
持有集群所有主机的 root 凭据，是比"少一个指标"严重得多的问题。而且这会引入
一个新的必填配置项，与"只读账号即可运行"的定位冲突。

**不需要它。** `VsanQueryVcClusterHealthSummary` 的响应里已经带了盘健康：

```go
// vsan/types/types.go:7385 VsanClusterHealthSummary
PhysicalDisksHealth []VsanPhysicalDiskHealthSummary `xml:"physicalDisksHealth,omitempty"`

// vsan/types/types.go:7432 VsanPhysicalDiskHealthSummary
Hostname string                   // 主机名
Disks    []VsanPhysicalDiskHealth // 该主机的盘

// vsan/types/types.go:5389 VsanPhysicalDiskHealth
Name           string  // 设备名，如 mpx.vmhba1:C0:T1:L0
Uuid           string  // vSAN 盘 UUID
SummaryHealth  string  // 汇总健康（非 omitempty，必有值）
CapacityHealth string  // 容量健康
Capacity       int64   // 容量，字节
UsedCapacity   int64   // 已用，字节
```

所以盘健康是**零额外往返**的：健康摘要那一次调用同时给出集群健康与盘健康。
比初稿的方案严格更优 —— 少一次 SOAP 往返、不需要 root 密码、还多拿到了
每盘容量。

一个前提：`Fields` 参数要包含 `physicalDisksHealth`。telegraf 只传
`["overallHealth", "overallHealthDescription"]`，因为它不采盘级指标。我们要
多传一个字段名。**这一处是 R2 唯一无法用 vcsim 验证的假设**（vcsim 不实现
这个方法），替身测试只能验证"我们正确解析了响应"，不能验证"vCenter 真的会
按这个 Fields 值返回盘健康"。已在 README 的前提条件里记下这一点。

另外 CMMDS 那条路（2.1.1 节③）也因此不必进 R2：盘的 hostname 与 devicename
从健康摘要里就能拿到，不需要逐台主机轮询 CMMDS。**这把组 B 的一个前置依赖
提前解决了**。

#### 2.2.2 容量：API 只给 total 与 free，used 要自己算

`VsanSpaceUsage`（`vsan/types/types.go:5050`）的字段是：

```go
TotalCapacityB int64 `xml:"totalCapacityB"`            // 非 omitempty
FreeCapacityB  int64 `xml:"freeCapacityB,omitempty"`   // omitempty
```

**没有 used 字段。** telegraf 也只导出 `total_capacity_byte` 与
`free_capacity_byte` 两个原始值，不做推导。

本项目**两个都导出，再加一条推导出来的 used**：

- `capacity_bytes` = `TotalCapacityB`（原始值）
- `capacity_free_bytes` = `FreeCapacityB`（原始值）
- `capacity_used_bytes` = `TotalCapacityB - FreeCapacityB`（推导）

为什么三条都要：原始值不会因为我们的推导逻辑出错而失真，是可信的基准；
而 `used` 是运维实际要看的那个数（容量告警写的是"已用超过 80%"，不是
"剩余低于 20%"）。只导原始值会让每个用户在 PromQL 里重复写同一个减法，
而那正是最容易写错 label matcher 的地方。

推导的边界情况：`FreeCapacityB` 是 `omitempty`，缺失时为 0，此时 used 会
等于 total。这在语义上是对的（"没有可用空间"就是"全部已用"），但要注意它
与"真的用满了"在指标上不可区分。不额外加标志位 —— total 为 0 时（vSAN 未
就绪）三条都是 0，用户看到的是"没有容量"，不会误判。

`cmo` / `vmwcluster` 沿用 `cluster.go:52` 已有的 label 名，**保证能与
`vmware_cluster_info` join**。这点必须一致，否则 vSAN 指标成了孤岛。

`resync_eta_seconds` 是读 telegraf 源码后加的：它的 `queryResyncSummary`
（`vsan.go:412-414`）一次拿三个值 `TotalBytesToSync` / `TotalObjectsToSync` /
`TotalRecoveryETA`，我原设计漏了第三个。**同一次调用已经返回它，不导出纯属浪费**
—— 而且 ETA 恰好是运维最关心的那个（"还要多久恢复完"比"还剩多少字节"可读）。

**`_seconds` 这个后缀在 R3 实现时已查证落实。**
三个字段在 govmomi 里都是 `int64`
（`vsan/types/types.go:4772-4774`，结构体 `VsanHostVsanObjectSyncQueryResult`），
**源码里没有任何单位注释**，telegraf 也只是原样导出成 `total_recovery_eta`
而不做换算 —— 它是 InfluxDB 口径，不像 Prometheus 那样要求单位进指标名。

写这段时列了三种可能：秒、毫秒、或"预计完成时刻的时间戳"。**R3 实施时在
Broadcom 官方 vSAN Management API 文档中查到了确定答案**：
`totalRecoveryETA` 的定义是 "The estimated time **in seconds** to recover
all vSAN objects of specified types."（`vim.vsan.host.VsanSyncingObjectQueryResult`
数据对象说明，8.0 与 9.x 两版文档一致）。

所以最终指标名直接定为 `vmware_vsan_resync_recovery_seconds`，
不需要走"先无后缀、确认后再补"的两步路径。

**这一组的价值最高**：容量满和盘故障是 vSAN 最常见的两类事故，而它们都在
这一组里。而且这组不涉及 CSV 解析，实现代价明显低于组 B。

#### 组 B：性能指标（`performance.*` 等价物）—— 建议独立一轮

走 `Client.VsanPerfQueryPerf`，返回 CSV 字符串（见前提 3）。

telegraf 列了 28 个实体类型。**我建议只做其中 5 个**：

| 实体类型 | 理由 |
|---|---|
| `cluster-domclient` | 集群级 IOPS / 吞吐 / 延迟，最常看的一屏 |
| `host-domclient` | 主机级同上，用于定位热点主机 |
| `disk-group` | 磁盘组级，缓存层是否成为瓶颈 |
| `capacity-disk` | 容量盘级，单盘异常 |
| `cache-disk` | 缓存盘级，同上 |

砍掉的 23 个（`vsan-iscsi-*`、`*-world-cpu`、`host-memory-*`、`vscsi`、
`virtual-disk` 等）理由：要么是 vSAN 内部实现细节（world-cpu 是 ESXi 调度器
的世界，排障时才看），要么依赖未必启用的功能（iSCSI），要么与 VM 层已有的
`datastore.*` 计数器重叠。

**先做 5 个再按需扩**比一次上 28 个更合理 —— 每个实体类型的 label 集不同
（见 telegraf 的 vSAN Tags 表：`disk-group` 要 `hostname`+`deviceName`+`ssdUuid`，
`vsan-pnic-net` 要 `pnic`），28 个意味着 28 套 label 映射要逐个验证。

#### 组 C：文件服务、iSCSI、拉伸集群 —— 不做

`VsanClusterQueryFileShares`、`VsanClusterQueryFsDomains`、
`VimClusterVsanVcStretchedClusterSystem` 这些只在启用了对应功能时有意义。
**不做，等有明确需求再说。**

### 2.3 flag 设计：一个还是两个 collector？

telegraf 用两个插件实例分别以 30s / 300s 抓 summary 与 performance。
本项目的 collector 是同频的（一次 scrape 全跑），所以有个选择：

**方案 A：一个 collector `vsan`，内部两组都采**
- 优点：一个开关，简单。
- 缺点：性能组的数据 5 分钟才更新一次，但每次 scrape 都去查
  —— 若 scrape 间隔 30s，10 次里有 9 次拿到的是同一批数据，纯浪费往返。

**方案 B：两个 collector `vsan` 与 `vsan.perf`**（推荐）
- `vsan`：组 A，健康与容量，实时，代价低。
- `vsan.perf`：组 B，性能，CSV 解析，代价高。
- 命名沿用现有 `esxcli.host.nic` / `esxcli.storage` 的点分风格，
  已有先例，`check_config.py` 的 flag 一致性检查也能直接覆盖。
- 用户可以只开 `vsan` 不开 `vsan.perf` —— 这恰好对应"我只想知道容量和盘
  健康"这个最常见需求。

两者都 `DefaultDisabled` —— 这是用户明确要求的：vSAN 默认禁用，
ResourcePool 默认启用。

对 vSAN 来说默认禁用不只是保守，而是**正确的默认**：绝大多数 vSphere 环境
没有启用 vSAN，默认开启会让这些环境每轮 scrape 都白跑一遍容量与健康查询
（虽然会优雅降级，但往返是实打实花掉的）。

注意这个理由**只对组 A/B 的那几个 vsan 端点调用成立**，不包括
`vmware_vsan_enabled` 本身 —— 后者若按 D8 的 A 方案放进 cluster collector，
就是零额外往返，默认输出也不构成浪费。这两件事要分开看。

**额外 flag**：性能窗口需要可配，因为 vSAN 性能服务的滚动窗口是 5 分钟，
vSAN 8 U1 起可降到 30 秒（telegraf 文档明确）。
**不复用 `-vmware.interval`** —— 那个是 vSphere perf 的窗口，两者语义不同、
取值范围也不同。

命名建议 **`-vmware.vsan.interval`**（默认 300 秒），理由见 D7：
初稿写的 `-vsan.perf.interval` 会开出一个只装一个 flag 的新顶层命名空间。

### 2.4 未启用 vSAN 时必须优雅降级

这是 vSAN collector 最容易出错的地方，也是**必须反向验证的点**。

三种"没有 vSAN"的情形，行为都不能是报错刷屏：

| 情形 | 预期行为 |
|---|---|
| 集群存在但未启用 vSAN | 输出 `vmware_vsan_enabled 0`，不再查其余 API |
| 目标是 ESXi 直连 | 整个 collector 跳过（无 ClusterComputeResource），留一条 Debug 日志 |
| 启用了 vSAN 但**性能服务未开** | 组 A 正常，组 B 返回空；输出一个显式指标标记这个状态 |

第三种最隐蔽 —— telegraf 文档专门警告过："When you create a vSAN cluster,
the performance service is disabled." 用户会看到组 A 有数据、组 B 空白，
如果没有显式标记就会误以为是 exporter 的 bug。因此建议：

```
vmware_vsan_perf_service_enabled{cmo, vmwcluster, vcenter}   0|1
```

**怎么判断这个状态：初稿的方案不可靠，改用 telegraf 的做法。**

初稿写的是"从 `VsanConfigInfoEx` 里读性能服务的配置"。读源码后发现
telegraf 是靠**错误码识别**（`vsan.go:235-240`）：

```go
resp, err := vsanmethods.VsanPerfQueryPerf(ctx, vsanClient, &perfRequest)
if err != nil {
    if err.Error() == "ServerFaultCode: NotFound" {
        // 性能服务没开
        e.parent.Log.Errorf("[vSAN] Is vSAN performance service enabled for %s? Skipping ...", clusterRef.name)
        commonError = err
        break        // ← 注意是 break 不是 continue
    }
    // 其它错误：只跳过这个实体类型
    continue
}
```

两个细节值得抄：

1. **`NotFound` 才 break，其它错误 `continue`**。性能服务没开是集群级
   的事实，继续试其余实体类型全都会失败 —— 早退是对的。而单个实体类型
   查询失败（比如这个环境没有 iSCSI）不该影响其它类型。
2. **它比读配置可靠**。`VsanConfigInfoEx` 里的性能服务字段在不同 vSAN
   版本上的位置和语义不一致，而 `NotFound` 这个 fault 是 API 契约。

但字符串比较 `err.Error() == "ServerFaultCode: NotFound"` 是脆的写法 ——
govmomi 的错误串格式变一下它就静默失效，退化成"所有错误都当作其它错误处理"。

**本项目应当用结构化判断**，已核实这几个都在 v0.56.0 里：

```go
// vim25/soap/error.go:86,91
soap.IsSoapFault(err)           // 是不是 SOAP fault
soap.ToSoapFault(err).VimFault() // 取出具体 fault

// vim25/types/types.go:57956
types.NotFound{}                 // 目标 fault 类型（内嵌 VimFault）
```

即 `if f, ok := soap.ToSoapFault(err).VimFault().(types.NotFound); ok`
这个形态。**具体断言写法要在 R3 实现时用替身响应验证** —— 我没有真实
vCenter 可以确认 vSAN 性能服务未开时返回的确实是 `NotFound` 而非
`InvalidArgument` 之类。telegraf 的字符串证明了 fault 名是 `NotFound`，
但它是 `ServerFaultCode` 包装的，解包后的具体类型需要实测。

### 2.5 vSAN 前提条件（写进 README）

telegraf 文档列的前提，本项目同样适用：
- vSphere 6.5+（本项目 CI 用 vcsim，实际部署环境需注意）
- 集群已启用 vSAN
- **vSAN 性能服务已开启**（组 B 的前提）
- 采集账号需要 vSAN 相关只读权限 —— 现有的只读账号可能不够，
  这点要在 README 里写明，否则用户会遇到看不懂的权限错误

---

## 三、测试策略（初稿说的"障碍"已实测解决）

本项目的既有约定是**反向验证**：每个检查先证明它在缺陷版上会失败，
再确认它在正确版上不误报。新 collector 必须遵守。

### 3.1 ResourcePool：vcsim 可完整覆盖

`simulator.VPX()` 自带资源池对象（每个 ComputeResource 有一个根池），
现有测试基建（`collectors_test.go:214` 的 `setupCollectorScrapeWithModel`）
直接可用。

可反向验证的缺陷注入：

| 注入的缺陷 | 应当被哪个断言抓住 |
|---|---|
| CPU 忘了 `× 1e6`（导 MHz 而非 Hz） | 断言 `cpu_usage_hertz` 与 host 的 `cpu_capacity_hertz` 同量纲 |
| 内存误乘 `1048576`（属性本已是字节） | 断言内存值不超过物理内存总量 |
| `vm` 列表用逗号拼成单个 label | 断言 N 个虚机产出 N 条 `resourcepool_vm` 序列 |
| `limit = -1` 直接导出 | 断言 limit 指标非负（取决于 D2 的决定） |
| ESXi 的 `ha-root-pool` 未标 synthetic | 断言 ESXi 模型下带 synthetic label |

**第一条最重要** —— 它正是本项目 Stage 系列反复出现的量纲缺陷类型，
而且错了之后数值"看起来仍然合理"。

### 3.2 vSAN：vcsim 覆盖不了，但替身方案已实测可行

实测 `govmomi@v0.56.0/vsan/simulator/simulator.go`（**整个文件只有 108 行**），
它注册了两个管理对象、共三个方法：

```
ClusterConfigSystem.VsanClusterGetConfig
ClusterConfigSystem.VsanClusterReconfig
StretchedClusterSystem.VSANVcConvertToStretchedCluster
```

（初稿说"两个方法"，漏了第三个。不过第三个是拉伸集群转换，与采集无关，
所以对本设计的结论没有影响。）

**没有** `VsanQuerySpaceUsage`、`VsanQueryVcClusterHealthSummary`、
`VsanQueryClusterPhysicalDiskHealthSummary`、`VsanQueryObjectIdentities`、
`VsanPerfQueryPerf` —— 也就是说组 A 的 5 个 API 里 vcsim 只支持 1 个，
组 B 完全不支持。

还有三个更麻烦的细节，前两个在那 108 行里，第三个是实测撞出来的：

1. **`simulator.go:20` 只在 `r.IsVPX()` 时注册 vSAN 端点。**
   所以 `simulator.ESX()` 模型下连 `VsanClusterGetConfig` 都没有 ——
   这恰好是 2.4 节"ESXi 直连要整体跳过"那条降级路径，可以用 vcsim 直接测，
   不需要替身。**这是个意外的好消息**：三种降级里有一种能用现有基建覆盖。
   已实测确认，ESX 模型下调用返回 `POST "/vsanHealth": 404 Not Found`。
2. **`VsanClusterGetConfig` 返回的是空的 `VsanConfigInfoEx`**
   （`simulator.go:72` `info = &types.VsanConfigInfoEx{}`），
   其 `Enabled` 字段是 nil 而非 false。已实测确认：VPX 模型下拿到的
   `cfg.Enabled == nil`。所以 vcsim 环境下"vSAN 未启用"走的是 **nil 分支**，
   不是 false 分支。实现时 `Enabled == nil` 与 `*Enabled == false`
   都要当作未启用 —— 这正是「该反着做的 ⑦」里强调两层 nil 检查的实际场景。
3. **`model.Service.RegisterEndpoints = true` 必须显式打开**，
   否则连 VPX 都是 404。这一条是实测撞出来的：第一次跑验证时 VPX 也返回
   `404 Not Found`，查到 `simulator/simulator.go:819-823` 才发现
   `endpoints` 只在 `s.RegisterEndpoints` 为真时才注册，而它**默认是 false**。

   **第 3 条对 R2 是硬性前提**：本项目现有测试（`vmware_test.go:366`、
   `vmware-exporter_test.go:271` 等）全都是
   `server := model.Service.NewServer()`，**没有任何一处设过这个字段**。
   所以 vSAN 相关测试必须在 `NewServer()` 之前加上这一行，否则会得到
   一个"看起来像 vSAN 未启用、实际是端点没挂上"的假绿测试 ——
   这是最坏的一类测试缺陷：它验证的降级路径是对的，但触发原因是错的。

而本项目**全部测试都建立在 vcsim 上**（`vmware/api/vmware_test.go:360`、
`collectors_test.go:214`、`vmware-exporter_test.go` 等处均为 `simulator.VPX()`
/ `simulator.ESX()`）。

这带来一个直接后果：**按现有测试基建，vSAN collector 的绝大部分逻辑
无法被测试覆盖**。而无法反向验证的代码，按本项目的既有标准是不该合入的。

三个可选出路：

**出路 1：自己写 SOAP 响应替身**（推荐）
在测试里挂一个 `soap.RoundTripper`，对 vSAN 的方法返回预置的响应体。
- 优点：能完整测 CSV 解析、单位换算、降级路径 —— 而这些正是最容易错的部分。
  尤其组 B 的 CSV 解析，用固定字符串做输入反而比 vcsim 更好测。
- 缺点：需要准备真实响应样本。可以从 govmomi 的 `vsan/types` 结构手工构造，
  不必真连 vSAN 环境。
- 与现有做法的关系：本项目已有替身的先例（`internal/collector` 的
  `HostFetcher` 就是为可测性设计的注入点），风格一致。

**这条出路的成本比初稿估计的低得多**，因为注入点是个只有一个方法的接口。
已编译验证：`vsan/methods` 的每个函数签名都是

```go
func VsanQuerySpaceUsage(ctx context.Context, r soap.RoundTripper,
    req *types.VsanQuerySpaceUsage) (*types.VsanQuerySpaceUsageResponse, error)
```

第二个参数是接口 `soap.RoundTripper`，而它只要求一个方法：

```go
type RoundTripper interface {
    RoundTrip(ctx context.Context, req, res HasFault) error
}
```

所以替身的骨架是这样，**不需要 HTTP 服务、不需要 XML、不碰网络**：

```go
type vsanStub struct {
    space  *vsantypes.VsanQuerySpaceUsageResponse
    health *vsantypes.VsanQueryVcClusterHealthSummaryResponse
    fault  error   // 用于测降级路径
}

func (s *vsanStub) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
    if s.fault != nil {
        return s.fault
    }
    switch body := res.(type) {
    case *vsanmethods.VsanQuerySpaceUsageBody:
        body.Res = s.space
    case *vsanmethods.VsanQueryVcClusterHealthSummaryBody:
        body.Res = s.health
    }
    return nil
}
```

注意 `methods` 包里那些 `xxxBody` 类型是**导出的**（`VsanQuerySpaceUsageBody`
等），所以 switch 能直接写。这是替身可行的关键前提，已核对
`vsan/methods/methods.go:2642` 附近的 body 类型声明。

**这也意味着 collector 的构造函数要接受 `soap.RoundTripper` 而不是
`*vsan.Client`**，否则替身注不进去。生产路径传 `vsan.NewClient(...)` 的返回值
（它满足该接口，已编译验证），测试路径传 `&vsanStub{...}`。
这个设计决定要在 R2 的第一个 commit 里就定下来 —— 事后再改会牵动所有测试。

**出路 2：给 vcsim 提交上游 PR**
补齐 vSAN 模拟。技术上正确，但**不该阻塞本项目** —— 上游合入周期不可控。

**出路 3：只做组 A 里 vcsim 支持的部分**
即只做 `VsanClusterGetConfig` 能覆盖的 `vsan_enabled` / `dedup_enabled`。
覆盖面太小，价值有限，不推荐。

**我的建议**：出路 1，且**把它作为 vSAN 实现的第一步而非最后一步** ——
先把 SOAP 替身与一组预置响应建起来，再写 collector。顺序反了的话，
很容易写出"只在真实环境里手工验证过"的代码。

### 3.3 CI 影响

- `check_config.py` 会自动发现新 flag（它从 binary 的 `-h` 输出提取，
  实测输出 `config check OK (26 flags known via binary, 24 files scanned)`），
  新增 `-collector.resourcepool` / `-collector.vsan` /
  `-collector.vsan.perf` / `-vmware.vsan.interval`（命名见 D7）后
  **必须同步更新两份 README 的 flag 表**，否则 config job 会红 ——
  这是它的设计意图。注意它是**双向**检查：写了文档但没注册也会红。
- `TestDefinitionsMatchRegisteredFlags`（`registry_test.go`）会强制
  `definitions` 清单与 `RegisterFlag` 注册的开关一致，漏一处就编译期外的
  测试失败。这是好事，不需要改。
  但要知道它**只查正向**（清单里的名字有没有对应 flag），反向不查 ——
  注册了 flag 却忘了进清单，这个测试是绿的，`/probe` 却少一个 collector。
  实现时两处一起改，别依赖测试兜住反向。
- `TestCreatorsProduceCollectors` 会对每个 `Creator` 传 **nil logger** 调用
  并断言不返回 error、不返回 nil。三个新 collector 的构造函数因此
  **不能在构造期解引用 logger**（现有 collector 都是直接把指针存进结构体，
  照抄即可）。这条初稿没写，是读 `registry_test.go:81-94` 时发现的。
- 新增 dashboard panel（若有）要过 `migrate_dashboards.py --check` 与
  `patch_dashboards.py --check`。

---

## 四、需要你拍板的决策点（8 条，其中 D5 已由实测解决）

默认值（ResourcePool 启用 / vSAN 禁用）你已经定了，不在下列之内。
D7 来自 flag 实证，D8 与改写后的 D4 来自通读 telegraf 源码 —— 初稿都没有。

**D5 保留在这里但已有答案** —— 替身方案实测跑通，它从"你得做个有风险的
取舍"变成"确认一个设计约束"。实际需要你权衡的是 **D1~D4、D6~D8 共七条**。

### D1：ResourcePool 走属性还是性能计数器？

我的建议是**属性**（理由见 1.1：一次往返、含 reservation/limit/shares、
无 rollup 歧义）。

telegraf 走性能计数器。若你希望与 telegraf 的指标口径完全对齐（比如已有基于
telegraf 的看板要迁移），则应改走计数器路径 —— 但那样拿不到 limit/shares。

**也可以两者都要**：属性出配置与瞬时用量，性能计数器另开一个
`resourcepool.perf`。我不推荐，因为资源池的性能计数器与其下 VM 的计数器
高度重叠，价值不足以支撑第二个 collector。

### D2：`limit = -1`（unlimited）怎么导出？

vSphere 用 `-1` 表示不限制。三个选项：

| 选项 | 后果 |
|---|---|
| 原样导 `-1` | `limit - usage` 算出负数；但保留了原始语义 |
| 不导出该序列 | PromQL 里 limit 缺失，`limit - usage` 变成空结果而非错误值 |
| 导 `+Inf` | 数学上正确，Prometheus 支持 `+Inf`；但部分可视化工具显示异常 |

**我倾向第二个**（unlimited 时不输出该序列），并额外输出一个
`vmware_resourcepool_cpu_limited{rpmo,rp} 0|1` 显式表达"是否有限制"。
理由：缺失的序列在 PromQL 里的行为（空结果）比一个哨兵值更安全 ——
哨兵值会被误当作真实数值参与计算。

### D3：vSAN 拆一个 collector 还是两个？

建议两个（`vsan` + `vsan.perf`，见 2.3）。若你希望减少 flag 数量，
可以合成一个，但那样"只要容量健康、不要性能"这个常见需求无法表达。

### D4：vSAN 性能实体类型 —— 硬编码清单还是问 vCenter？（读源码后重写）

初稿问的是"先做哪 5 个"。读了 telegraf 源码后我认为**问题问错了**。

telegraf 不硬编码任何实体类型。`getVsanMetadata`（`vsan.go:135-148`）
调 `VsanPerfGetSupportedEntityTypes` 让 vCenter 返回本环境支持的清单，
再用用户配置过滤。已核实这个方法在 govmomi v0.56.0 存在
（`vsan/methods/methods.go:402`）。

三个选项：

| 选项 | 优点 | 代价 |
|---|---|---|
| **A. 硬编码 5 个**（初稿） | 序列基数可预测，测试好写 | vSAN 版本升级后新实体类型采不到；ESA 架构的实体名与 OSA 不同，硬编码清单会在 ESA 集群上大面积空转 |
| **B. 问 vCenter 全采** | 自适应版本与架构 | **序列基数完全不可控** —— telegraf README 列了 29 个实体类型，每个乘以实体数乘以指标数 |
| **C. 问 vCenter + 硬编码白名单取交集** | 自适应且基数可控 | 多一次 API 调用；白名单仍需维护 |

**我倾向 C**，理由是它把"环境支持什么"和"我们想采什么"分开了：
交集为空时能明确告诉用户"你要的实体类型这个环境不支持"，
而 A 方案在这种情况下只是静默返回空数据。

但 C 有个前提要注意：telegraf 专门留了 `vsan_metric_skip_verify` 逃生门，
README 的说明是 "some performance entities are not returned by the API,
but we want to offer the flexibility if you really need the stats"。
**即这个 API 本身不完备** —— 有些真实可查的实体类型它不返回。
所以取交集会漏掉这部分，需要一个"跳过校验"的开关，那又是一个 flag。

**需要你定**：A（简单、可能不准）、B（自适应、基数失控）、
C（正确但多一个 flag + 一次调用）。
若选 C，请一并确认能接受第 5 个新 flag。

### D5：vSAN 测试走 SOAP 替身还是先只做 vcsim 支持的部分？

**这条已经不需要你担风险了 —— 我把替身方案实测跑通了。**

初稿把它列为"最需要你决定的一条"，因为不确定自建替身的工作量。
现在有确切答案：写了一个临时测试文件，用 3.2 节那个替身骨架驱动**真实的**
`vsanmethods.VsanQuerySpaceUsage` 与 `VsanQueryVcClusterHealthSummary`，
断言容量值、健康值，以及注入 `ServerFaultCode: NotFound` 后的降级路径。

```
=== RUN   TestStubDrivesRealMethods
--- PASS: TestStubDrivesRealMethods (0.00s)
ok  github.com/prezhdarov/vmware-exporter  1.370s
```

**0.00s、不碰网络、不需要 vcsim。** 替身总共约 15 行。验证完即删除，
仓库里没留下这个文件 —— 它的作用是证明方案可行，真正的实现属于 R2。

所以出路 1 的成本远低于初稿的估计，出路 2（等上游 vcsim）与出路 3
（只做 vcsim 支持的部分）都不再有理由。

**仍需你确认的只剩一点**：接受"collector 构造函数收
`soap.RoundTripper` 而非 `*vsan.Client`"这个设计约束（见 3.2 末段）。
这是替身能注入的前提，也是唯一侵入生产代码的地方。

### D6：分几轮做？

建议三轮，每轮独立可验证、独立合并：

| 轮次 | 内容 | 依赖 |
|---|---|---|
| **R1** | ResourcePool collector（含测试、README、CHANGELOG） | 无 |
| **R2** | vSAN 测试基建（SOAP 替身 + 预置响应）+ 组 A（**仅容量与健康**） | 无（D5 已实测解决） |
| **R3** | vSAN 组 B（性能，CSV 解析）+ resync + CMMDS 盘标签 | R2 |

拆分依据沿用本项目既有原则：**按"验证方法是否适用"拆轮**。R1 用 vcsim 即可，
R2 要建全新的替身基建，R3 的 CSV 解析又是另一套验证方式 —— 三者的验证手段
不同，因此是三轮。

**读 telegraf 源码后 R2/R3 的边界移动了**：resync 三个指标从组 A 降到 R3。
理由见 2.1.1 节②—— resync 需要 API ≥ 6.7 的版本门槛、需要手工拼
`VsanSystemEx-<n>` 这个 MoRef、还依赖"集群至少一台主机可达"。
这三个前提的验证方式与容量健康完全不同（要造多版本、造主机不可达场景），
按同一条拆分原则它就该和 CMMDS 盘标签一起进 R3。

**默认启用改变了 R1 的 CHANGELOG 归类**：初稿假设两者都默认禁用，
R1 只是"加了个可选 collector"；现在 resourcepool 默认启用，R1 会改变
所有升级用户的序列数，必须进 Added 并写明关闭方式（见四之二末节）。

### D7：性能窗口 flag 叫什么？（实证后新增）

初稿写的是 `-vsan.perf.interval`。跑完 `check_config.py` 拿到真实 flag 清单
之后我认为**这个名字应该改**。现有 26 个 flag 分布在 9 个顶层命名空间：

```
collector(8)  vmware(8)  disable(2)  envflag(2)  log(2)
metrics(1)  http(1)  web(1)  file(1)
```

`-vsan.perf.interval` 会开出**第 10 个顶层命名空间，且只装这一个 flag**。
而 `vmware.*` 已经是"连接与采集参数"的既有归属地 —— `vmware.interval`、
`vmware.granularity`、`vmware.timeout` 全在那里，vSAN 的采样窗口是同一类东西。

| 选项 | 评价 |
|---|---|
| `-vsan.perf.interval` | 初稿方案。新开顶层命名空间只为一个 flag |
| **`-vmware.vsan.interval`** | **推荐**。与 `vmware.interval` 并列，语义归属清楚 |
| `-collector.vsan.perf.interval` | 不行。`collector.*` 前缀在本项目专指布尔开关，塞一个 int 进去会破坏这个约定 |

第三个选项还有个更硬的理由：`RegisteredNames()` 与
`TestDefinitionsMatchRegisteredFlags` 都假设 `collector.<name>` 对应一个
collector。多一个 `collector.vsan.perf.interval` 不会让测试失败（它只查
正向：清单里的名字有没有对应 flag），但会让 `-h` 输出里的 `collector.*`
不再是"开关列表"，读的人得逐个辨认哪个是开关哪个是参数。

**需要你拍板**：接受 `-vmware.vsan.interval`，还是坚持初稿的
`-vsan.perf.interval`。我已按推荐值改了文档，你若不同意我改回去。

### D8：`vmware_vsan_enabled` 由谁输出？（读源码后新增）

这条是 2.1.1 节⑤的直接后果，且**它影响的不是 vSAN collector，而是
默认启用的 cluster collector** —— 所以必须你点头。

telegraf 走 `configurationEx.vsanConfigInfo.enabled` 属性判断 vSAN 是否启用。
cluster collector 已经在检索 `ClusterComputeResource`（`cluster.go:38`），
把 `configurationEx` 加进属性列表就能零额外往返拿到这个标志。

| 选项 | 后果 |
|---|---|
| **A. cluster collector 输出** | vSAN 默认禁用时用户**仍能发现自己有 vSAN**；代价是 cluster 的属性检索体积变大 |
| B. vsan collector 输出（初稿） | 属性检索不变；但不开 vsan collector 就完全看不到"我有没有 vSAN" |
| C. 两边都输出 | 重复序列，不考虑 |

**我倾向 A**，因为它解决了一个默认禁用固有的问题：用户怎么知道该不该开
`-collector.vsan`？现在的答案是"先开一次看看"，A 方案的答案是
"看 `vmware_vsan_enabled` 就行"。

**但 A 动了默认启用的 collector，风险要说清**：`configurationEx` 是个
大结构（含完整 DAS/DRS/DPM 配置）。它是否显著增加 cluster 检索的耗时与
内存，**我没有实测数据** —— vcsim 的响应体积不代表真实 vCenter。

所以我的建议是：**R1 阶段先不动 cluster collector**，在 R2 做 vSAN 时
用 `-vmware.granularity` 那种方式实测一次 `configurationEx` 的体积，
再决定是否合并。若体积不可接受，退回 B。

**需要你定**：A（发现性好、动了默认路径）、B（保守）、
或"先按 B 做、R2 实测后再考虑 A"（我推荐这个）。

**已拍板（R2）**：走 B —— `vsan` collector 输出 `vmware_vsan_enabled`，
数据源是 `VsanClusterGetConfig`（vsan 端点）而非 `configurationEx` 属性。

R2 实测下来 B 反而比 A 更有理由，不只是"保守"：

1. **A 方案拿不到 dedup**。`ClusterConfigInfoEx.VsanConfigInfo` 的类型是
   **vim25 的** `types.VsanClusterConfigInfo`（`vim25/types/types.go:99410`），
   它只有 `Enabled *bool` 与主机默认配置，**没有 `DataEfficiencyConfig`**。
   去重压缩状态只存在于 **vsan 包的** `VsanConfigInfoEx`
   （`vsan/types/types.go:8442`），那是 `VsanClusterGetConfig` 的返回类型。
   所以走 A 之后仍要为 dedup 单独调一次 `VsanClusterGetConfig` ——
   "零额外往返"的好处在 R2 的指标集下不成立。
2. 走 B 之后 `vmware_vsan_enabled` 与 `vmware_vsan_dedup_enabled` 同源，
   两条指标不会出现"一个说启用、另一个说没有"的不一致窗口。

代价照旧：不开 `-collector.vsan` 就看不到"我有没有 vSAN"。这个发现性问题
留给文档解决 —— README 里写明"想知道有没有 vSAN，开一次 `-collector.vsan`
即可，未启用的集群只会输出一条 `enabled 0` 且不再发任何后续请求"。
真要做 A，等 R3 有了 `configurationEx` 的实测体积数据再议。

### D9：两个 MoRef 常量 govmomi 没有提供（R2 实测新增）

`vsan/client.go` 只定义了四个 MoRef 常量：`VsanVcClusterConfigSystemInstance`、
`VsanPerformanceManagerInstance`、`VsanQueryObjectIdentitiesInstance`、
`VsanVcStretchedClusterSystem`（外加一个 `VsanPropertyCollectorInstance`）。

**容量与健康这两个系统的 MoRef 不在其中** —— 因为 govmomi 的 `vsan.Client`
没有包装这两个方法，只有包级的 `methods.VsanQuerySpaceUsage` 等函数，而
`This` 参数要调用方自己填。

取值不能猜。从 telegraf 源码逐字取到（`plugins/inputs/vsphere/vsan.go`，
`queryDiskUsage` 与 `queryHealthSummary` 两个函数的局部变量）：

| 用途 | Type | Value |
|---|---|---|
| 容量 | `VsanSpaceReportSystem` | `vsan-cluster-space-report-system` |
| 健康 | `VsanVcClusterHealthSystem` | `vsan-cluster-health-system` |

**这两个字面量是 R2 唯一"抄来的魔法值"**，无法从 govmomi 的类型系统推导，
也无法用 vcsim 验证（vcsim 不注册这两个管理对象）。实现时必须在常量定义处
写明出处，否则将来没人知道这串字符串是怎么来的、改错了怎么发现。

---

## 四之二、注册清单（实现时的唯一事实来源）

默认值已确定，落到代码上是两处必须一致的注册。**这两处不同步是本项目已知的
易错点**，`TestDefinitionsMatchRegisteredFlags` 就是为它建的。

### `registry.go` 的 `definitions` 追加三行

```go
// 现有 5 个默认启用的基础 collector 之后：
{Name: "resourcepool", Creator: NewresourcepoolCollector, DefaultEnabled: collector.DefaultEnabled},

// 现有两个 esxcli 默认禁用项之后：
{Name: "vsan",      Creator: NewvsanCollector,     DefaultEnabled: collector.DefaultDisabled},
{Name: "vsan.perf", Creator: NewvsanPerfCollector, DefaultEnabled: collector.DefaultDisabled},
```

### 各 collector 的 `init()` 注册对应开关

```go
// resourcepool.go
collector.RegisterFlag("resourcepool", resourcepoolCollectorFlag)
// vsan.go
collector.RegisterFlag("vsan", vsanCollectorFlag)
// vsanperf.go
collector.RegisterFlag("vsan.perf", vsanPerfCollectorFlag)
```

### 构造函数命名：沿用哪个先例？

现有 7 个 Creator 的命名**本身不一致**，这是实测出来的：

```
NewdatacenterCollector          NewhostCollector
NewdatastoreCollector           NewvmCollector
NewesxcliHostNICCollector       NewesxcliStorageListCCollector
NewClusterCollector      ← 唯一一个首字母大写的
```

6 比 1，多数派是 `New<小写子系统名>Collector`。上面的三行按多数派写成
`NewresourcepoolCollector` / `NewvsanCollector` / `NewvsanPerfCollector`。

**我沿用多数派，但要说清这不是因为它更好** —— `NewresourcepoolCollector`
读起来很别扭，任何 Go linter 都会觉得可疑。选它的唯一理由是本轮范围是
"加 collector"，不是"统一 7 个既有函数的命名"。后者是独立的重命名改动，
混进来会让本轮的 diff 无法审阅。

若你想顺手统一（`NewResourcePoolCollector` 这种规范驼峰），那应该是
**另一轮**：把 7 个既有的一起改，`registry.go` 同步，一次做完。
现在半途换风格只会让不一致从 6:1 变成 6:4。

### 最终的 collector 清单（10 个）

| collector | 默认 | 说明 |
|---|---|---|
| `datacenter` | 启用 | 现有 |
| `cluster` | 启用 | 现有 |
| `datastore` | 启用 | 现有 |
| `host` | 启用 | 现有 |
| `vm` | 启用 | 现有 |
| **`resourcepool`** | **启用** | 新增 |
| `esxcli.host.nic` | 禁用 | 现有 |
| `esxcli.storage` | 禁用 | 现有 |
| **`vsan`** | **禁用** | 新增 |
| **`vsan.perf`** | **禁用** | 新增 |

### 新增 flag 共 4 个

```
-collector.resourcepool   （bool，默认 true）
-collector.vsan           （bool，默认 false）
-collector.vsan.perf      （bool，默认 false）
-vmware.vsan.interval     （int，默认 300，单位秒）   ← 命名见下方 D7
```

flag 总数实测 26 → **30**（26 这个数是跑 `check_config.py` 得到的，
不是估算：`config check OK (26 flags known via binary, 24 files scanned)`）。
`check_config.py` 从 binary 的 `-h` 输出提取 flag 并与两份 README 的表格
双向比对 —— 既查"注册了但没写文档"，也查"写了文档但没注册"。
**四个都必须写进 README 与 README-zh 的表格**，漏一个 config job 就红。
这是它的设计意图，不是障碍。

**这个数字取决于 D4**：若采纳 D4 的 C 方案（问 vCenter + 白名单取交集），
需要第 5 个 flag 作为 telegraf `vsan_metric_skip_verify` 的等价物，
届时是 26 → 31。D4 未定之前这里按 4 个算。

### 已验证：`vsan` 同时是 collector 名与 `vsan.perf` 的前缀，不会互相误触发

这是本项目此前没有过的形态。现有的点分名 `esxcli.host.nic` /
`esxcli.storage` 里，`esxcli` **本身不是** collector 名；而 `vsan` 与
`vsan.perf` 是父名本身也是一个 collector。三条路径都查过，都是精确匹配：

| 路径 | 实现 | 结论 |
|---|---|---|
| 命令行开关 | Go `flag` 包以完整字符串为 map 键 | `vsan` 与 `vsan.perf` 是两个独立键 |
| `/probe` 选择 | `set.go:179` `opts.Enabled[def.Name]` | 精确查表，无前缀语义 |
| README 一致性 | `check_config.py:255` 的 `^\|\s*`?-([a-zA-Z][\w.-]*)` | 字符类含 `.`，两个名都能完整提取 |

全仓库 grep `HasPrefix` / `TrimPrefix` 在 `internal/collector/` 与
`vmware-exporter.go` 中**零命中**，所以不存在"开 `vsan` 顺带开
`vsan.perf`"的隐式联动。

**记录这条的意义在于：实现时不要凭直觉去补"父级开关联动"逻辑。**
`-collector.vsan=true` 不应该隐式打开 `vsan.perf` —— 那恰好破坏了 2.3 节
拆两个 collector 的全部理由（"只要容量健康、不要性能"）。

### 默认启用 resourcepool 是 breaking change 吗？

**是，需要进 CHANGELOG 的 Added 而非 Changed。** 理由：升级后用户的 TSDB
会多出一批 `vmware_resourcepool_*` 序列，而他们没有做任何配置变更。
虽然不破坏任何既有序列，但序列数增长属于"用户应当被告知"的范畴 ——
尤其对按序列计费的托管 Prometheus。

CHANGELOG 里要写明：不需要的用户传 `-collector.resourcepool=false` 关闭。

---

## 五、明确不做的（本轮范围外）

- **Network / 端口组 / DVS** —— 用户已明确不需要。
- vSAN 文件服务、iSCSI、拉伸集群（组 C）。
- vSAN 的 23 个细粒度性能实体类型。
- telegraf 那套 include/exclude 通配符配置机制 —— 本项目用 collector 布尔
  开关，粒度更粗但与既有风格一致。若将来确实需要指标级过滤，那是一个独立
  的横切特性，不该塞进这两个 collector。
