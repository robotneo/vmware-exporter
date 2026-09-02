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

### 1. `vsan` 包已在 govmomi 主模块内，不需要新依赖

实测 `govmomi@v0.56.0/vsan/`：

```
client.go  methods/  mo/  simulator/  types/  vsanfs/
```

上一版缺口分析里我把 vSAN 列在"档 4：需要新 SDK"，**这个判断需要修正** ——
`go.mod` 不需要增加任何 require 行。vSAN 因此从档 4 降到档 2.5。

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

### 2.2 采什么：三组，按代价排序

#### 组 A：集群健康与容量（`summary.*` 等价物）—— 建议第一批做

来源实测可用的 API：

| API | 用途 |
|---|---|
| `VsanQuerySpaceUsage` | 集群容量：总量、已用、去重压缩节省 |
| `VsanQueryVcClusterHealthSummary` | 集群整体健康（含各项 health check 结果） |
| `VsanQueryClusterPhysicalDiskHealthSummary` | 物理盘健康 |
| `VsanQueryObjectIdentities` | 对象健康与 resync 状态 |
| `Client.VsanClusterGetConfig` | vSAN 是否启用、去重压缩开关、加密开关 |

指标设计：

```
vmware_vsan_enabled{cmo, vmwcluster, vcenter}                                  0|1
vmware_vsan_capacity_bytes{cmo, vmwcluster, vcenter}
vmware_vsan_capacity_used_bytes{cmo, vmwcluster, vcenter}
vmware_vsan_health_status{cmo, vmwcluster, status="green|yellow|red|unknown", vcenter}   1
vmware_vsan_disk_health{cmo, vmwcluster, host, device, state, vcenter}          1
vmware_vsan_resync_bytes{cmo, vmwcluster, vcenter}
vmware_vsan_resync_objects{cmo, vmwcluster, vcenter}
vmware_vsan_dedup_enabled{cmo, vmwcluster, vcenter}                            0|1
```

`cmo` / `vmwcluster` 沿用 `cluster.go:52` 已有的 label 名，**保证能与
`vmware_cluster_info` join**。这点必须一致，否则 vSAN 指标成了孤岛。

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
没有启用 vSAN，默认开启会让这些环境每轮 scrape 都白跑一遍
`VsanClusterGetConfig`（虽然会优雅降级成 `vsan_enabled 0`，但往返是实打实
花掉的）。

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

`Client.VsanClusterGetConfig` 返回的 `VsanConfigInfoEx` 里有性能服务的配置，
可以直接读出来，不必靠"查询返回空"来推断。

### 2.5 vSAN 前提条件（写进 README）

telegraf 文档列的前提，本项目同样适用：
- vSphere 6.5+（本项目 CI 用 vcsim，实际部署环境需注意）
- 集群已启用 vSAN
- **vSAN 性能服务已开启**（组 B 的前提）
- 采集账号需要 vSAN 相关只读权限 —— 现有的只读账号可能不够，
  这点要在 README 里写明，否则用户会遇到看不懂的权限错误

---

## 三、测试策略（含一个必须先解决的障碍）

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

### 3.2 vSAN：vcsim 覆盖不了，这是个必须先决策的障碍

实测 `govmomi@v0.56.0/vsan/simulator/simulator.go`，它只模拟了两个方法：

```
ClusterConfigSystem.VsanClusterGetConfig
ClusterConfigSystem.VsanClusterReconfig
```

**没有** `VsanQuerySpaceUsage`、`VsanQueryVcClusterHealthSummary`、
`VsanQueryClusterPhysicalDiskHealthSummary`、`VsanQueryObjectIdentities`、
`VsanPerfQueryPerf` —— 也就是说组 A 的 5 个 API 里 vcsim 只支持 1 个，
组 B 完全不支持。

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

## 四、需要你拍板的七个决策点

默认值（ResourcePool 启用 / vSAN 禁用）你已经定了，不在下列之内。
D7 是补做实证时新发现的，初稿没有。

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

### D4：vSAN 性能实体类型先做哪几个？

建议 5 个（`cluster-domclient` / `host-domclient` / `disk-group` /
`capacity-disk` / `cache-disk`）。若你的环境有 iSCSI 或文件服务，
告诉我，我把对应实体类型加进第一批。

### D5：vSAN 测试走 SOAP 替身还是先只做 vcsim 支持的部分？

这是**最需要你决定的一条**，因为它决定 vSAN 这轮的工作量。
建议出路 1（自建替身）。若你接受"vSAN 部分暂时依赖真实环境手工验证、
不进 CI"，则工作量会显著下降 —— 但这与本项目"不留一进 CI 就红的摊子"
以及反向验证的既有标准冲突，需要你明确豁免。

### D6：分几轮做？

建议三轮，每轮独立可验证、独立合并：

| 轮次 | 内容 | 依赖 |
|---|---|---|
| **R1** | ResourcePool collector（含测试、README、CHANGELOG） | 无 |
| **R2** | vSAN 测试基建（SOAP 替身 + 预置响应）+ 组 A（健康容量） | D5 |
| **R3** | vSAN 组 B（性能，CSV 解析） | R2 |

拆分依据沿用本项目既有原则：**按"验证方法是否适用"拆轮**。R1 用 vcsim 即可，
R2 要建全新的替身基建，R3 的 CSV 解析又是另一套验证方式 —— 三者的验证手段
不同，因此是三轮。

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
