# vSphere 采集面缺口分析

本文回答一个问题：**除 Datacenter / Cluster / Host / VM / Datastore / Network / vSAN / DVS 之外，vSphere 上还有哪些值得采集的对象与信号。**

产出用途是给统一资源模型（URM）做适配审计的输入，因此每一项都标注了它在
URM 里的**归属形态** —— 是新增一类资源、给已有资源加属性、还是根本不该进
资源模型（属于事件/状态流）。这个区分比"要不要采"更重要：把告警硬塞成一类
"资源"是常见的建模错误。

> 结论先行：**清单里的 Network / vSAN / DVS 三项本仓库目前并未采集**，
> 见第一节实测。缺口分析因此分两层 —— 已列出但未实现的，和未列出也未实现的。

---

## 一、现状基线（实测，不是文档承诺）

### 已注册的采集器：7 个

来源 `vmware/collectors/registry.go:26-37`，这份清单是全项目唯一来源，
同时驱动 `/metrics` 与 `/probe`：

| collector | 默认 | vSphere 对象 | 拉取的属性 |
|---|---|---|---|
| `datacenter` | 启用 | Datacenter | name, parent |
| `cluster` | 启用 | ClusterComputeResource（回退 ComputeResource） | name, summary, datastore, parent |
| `datastore` | 启用 | Datastore | summary, host, vm, parent |
| `host` | 启用 | HostSystem | summary, runtime（+config/hardware 由 esxcli 需求并集带入） |
| `vm` | 启用 | VirtualMachine | summary, runtime, storage, snapshot |
| `esxcli.host.nic` | **禁用** | 逐主机 esxcli | 网卡列表与统计 |
| `esxcli.storage` | **禁用** | 逐主机 esxcli | 存储适配器/路径 |

esxcli 两个默认禁用的原因是逐主机串行发 SOAP，开销随主机数线性增长。

### 性能计数器：host 27 个、vm 23 个、datastore 2 个

host 与 vm 的计数器集合几乎相同（`host.go:23-31`、`vm.go` 对应段），
差异只有 host 多采 `net.errorsRx/Tx`、`net.droppedRx/Tx`。
datastore 只有 `disk.provisioned.latest`、`disk.used.latest` 两个。

**cluster 与 datacenter 没有任何性能计数器**，只出 `_info` 关系型指标。

### 关键：清单里的三项其实是空的

`Network`、`vSAN`、`DVS` 在本仓库**没有对应采集器，也没有对应指标**：

- 没有 `Network` / `DistributedVirtualPortgroup` / `DistributedVirtualSwitch`
  这三类 ManagedEntity 的属性检索。
- 唯一的网络信号是 host/vm 上的 `net.*` 性能计数器 —— 那是**虚机与主机视角
  的流量**，不是网络对象自身的配置与健康。
- vSAN 需要单独的 `vsan-health` / `vsanPerfSystem` 端点，本仓库连
  govmomi 的 vsan 包都没引入（`go.mod` 只有 `govmomi` 主模块）。

所以"除了这 8 项"这个前提需要先修正为"除了这 5 项已实现 + 3 项待实现"。

### 已确认完全未采集的维度

用 Grep 全量核对 `vmware/`（排除测试）后确认，以下关键词零命中：
`alarm` / `triggeredAlarmState` / `EventManager` / `TaskManager` /
`licens` / `ResourcePool`。

`snapshot` 有命中但只用了 `CreateTime`（`vm.go:113-126`），
快照的**大小、层数、链深度**都没采。

---

## 二、先于"加新对象"的两个口径缺陷

在讨论新增采集面之前，有两处**已实现对象的口径问题**。它们的优先级高于任何
新采集器，因为新增对象不会修复这两点，反而会在错误的基线上继续放大。

### 缺陷 A：关机 / 断连实体直接从指标里消失

`vm.go:70` 只处理 `PowerState == "poweredOn"`：

```go
for _, vm := range vms {
    if vm.Runtime.PowerState == "poweredOn" {
```

`host.go:71` 更严格，同时要求三个条件：

```go
if host.Runtime.PowerState == "poweredOn" &&
   host.Runtime.ConnectionState == "connected" &&
   !host.Runtime.InMaintenanceMode {
```

**后果**：一台主机进维护模式，它的 `vmware_host_info` 连同所有容量指标一起
从 `/metrics` 消失。在 Prometheus 侧这与"主机被删除"**完全无法区分** ——
序列同样是停止上报。于是：

- `absent()` 告警无法区分维护窗口与真实失联。
- 容量类看板的分母会在维护期间悄悄变小，利用率虚高。
- 库存类查询（"我一共有多少台主机"）在维护期间少数。

**这对 URM 的影响是结构性的**：资源模型需要"资源存在但状态异常"这个态，
而当前实现把它退化成了"资源不存在"。

**建议口径**：把生命周期状态导成**独立的状态指标**，而非过滤条件：

```
vmware_host_power_state{moid,name,state="poweredOn|poweredOff|standBy"}      1
vmware_host_connection_state{moid,name,state="connected|disconnected|notResponding"} 1
vmware_host_maintenance_mode{moid,name}                                       0|1
vmware_vm_power_state{moid,name,state="poweredOn|poweredOff|suspended"}       1
```

`_info` 与容量类指标对所有实体无条件输出；只有**性能计数器**才跳过关机实体
（那是 vCenter 侧确实无数据，不是我们的选择）。

### 缺陷 B：`overallStatus` 从未采集

vSphere 给每个 ManagedEntity 维护一个 `overallStatus`（green / yellow /
red / gray），这是 vCenter 自己算出的健康汇总，覆盖硬件、配置、告警多个来源。
本仓库的属性列表里没有它 —— 意味着 vCenter UI 上一片红，Prometheus 这边
一切正常。

`gray` 尤其值得单列：它表示"状态未知"，通常是 vCenter 拿不到该实体的信息，
是个先于真实故障出现的信号。

**建议**：给 host / vm / datastore / cluster 统一加

```
vmware_<entity>_overall_status{moid,name,status="green|yellow|red|gray"} 1
```

代价极小 —— `overallStatus` 是 ManagedEntity 的基础属性，加进现有
`fetchProperties` 的属性列表即可，不增加任何往返。

---

## 三、缺失的采集面清单

按**在 URM 里的归属形态**分四类。这个分类是审计的重点：形态错了，后面所有
join 逻辑都要重写。

### 形态一：应作为独立资源类型（有稳定身份、有生命周期、可被引用）

| # | 对象 | vSphere 类型 | 为什么需要 | URM 归属 |
|---|---|---|---|---|
| 1 | **资源池** | `ResourcePool` | CPU/内存的 reservation / limit / shares 实际生效在这一层。没有它，"VM 为什么拿不到 CPU"这个问题在指标上无法回答 | 新增资源，是 Cluster 与 VM 之间**缺失的一层父子关系** |
| 2 | **vApp** | `VirtualApp` | 是 ResourcePool 的子类，多层应用的部署单元 | 可与 ResourcePool 合并为一类，用 `type` 区分 |
| 3 | **文件夹** | `Folder` | vCenter 的实际组织维度，很多客户的业务归属信息只存在于文件夹路径里 | 新增资源，或作为所有实体的 `folder_path` 属性 |
| 4 | **标签 / 分类** | Tag / Category（**REST API**） | 云管平台的业务归属、成本中心、责任人几乎都落在 tag 上。这是 URM 里"业务视角"的唯一可靠来源 | **强烈建议独立**，见下方风险说明 |
| 5 | **分布式交换机** | `DistributedVirtualSwitch` | 清单已列（DVS）但未实现。MTU、上行链路、版本、健康检查结果 | 新增资源 |
| 6 | **端口组** | `DistributedVirtualPortgroup` / `Network` | 清单已列（Network）但未实现。VLAN ID、端口数、可用端口数 | 新增资源，是 VM 网卡的引用目标 |
| 7 | **标准交换机** | `HostVirtualSwitch`（host.config.network） | 未上 DVS 的环境全靠它 | Host 的子资源 |
| 8 | **存储适配器 / 多路径** | `HostHostBusAdapter` / `HostMultipathInfo` | 路径数、路径状态。单路径故障是最典型的"降级但不告警"场景 | Host 的子资源（esxcli.storage 部分覆盖，但默认禁用且未建模） |
| 9 | **vSAN 磁盘组 / 磁盘** | vSAN Disk（需 vsan SDK） | 清单已列（vSAN）但未实现 | 新增资源，见第五节可行性 |
| 10 | **虚拟磁盘（VMDK）** | `VirtualDisk`（vm.config.hardware.device） | 单盘容量、精简/厚置、所在 datastore。当前只有 VM 级的 `PerDatastoreUsage` 汇总 | VM 的子资源 |
| 11 | **许可证** | `LicenseManager` | 到期时间、已用/授权容量。到期会直接导致功能停摆 | 新增资源（vCenter 级） |

### 形态二：应作为已有资源的属性 / 附加指标（不新增资源类型）

| # | 信号 | 来源 | 说明 |
|---|---|---|---|
| 12 | **`overallStatus`** | 所有 ManagedEntity | 见第二节缺陷 B。**最高性价比的一项** |
| 13 | **电源 / 连接 / 维护状态** | host.runtime / vm.runtime | 见第二节缺陷 A |
| 14 | **VMware Tools 状态** | `vm.guest.toolsStatus` / `toolsVersionStatus` | Tools 过期或未运行会导致优雅关机、备份、心跳全部失效 |
| 15 | **Guest 心跳** | `vm.guestHeartbeatStatus` | vCenter 视角的"VM 是否活着"，比 PowerState 更接近业务可用性 |
| 16 | **Guest OS / IP / 主机名** | `vm.guest.*` | URM 里把虚机关联到 CMDB / 监控 agent 的关键字段 |
| 17 | **HA 状态** | `cluster.configuration.dasConfig` + `host.runtime.dasHostState` | HA 是否启用、准入控制策略、故障切换容量。集群级最重要的配置项之一 |
| 18 | **DRS 状态与建议** | `cluster.configuration.drsConfig` / `drsRecommendation` | DRS 是否启用、自动化级别、当前不均衡度 |
| 19 | **集群容量汇总** | `cluster.summary`（`effectiveCpu` / `effectiveMemory` / `numEffectiveHosts`） | **当前 cluster collector 完全没出容量指标**，只有 `_info` 和 datastore 映射 |
| 20 | **EVC 模式** | `cluster.summary.currentEVCModeKey` | 决定 vMotion 兼容性边界 |
| 21 | **主机硬件传感器** | `host.runtime.healthSystemRuntime.systemHealthInfo` | 风扇、电源、温度、内存 ECC。硬件故障的第一手信号 |
| 22 | **主机 NTP / 服务状态** | `host.config.dateTimeInfo` / `serviceInfo` | 时间漂移会让所有时序数据失真 |
| 23 | **主机固件 / 驱动版本** | `host.config.product` + esxcli | 补丁合规审计 |
| 24 | **快照大小与链深度** | `vm.layoutEx.snapshot` | 当前只采了 `CreateTime`。快照吃掉 datastore 是最常见的容量事故，而"有几个快照"不足以预警，需要"占了多少空间" |
| 25 | **Datastore 类型与 SIOC** | `datastore.summary.type` / `iormConfiguration` | VMFS / NFS / vSAN / vVol 的容量语义不同，混在一起算利用率会失真 |
| 26 | **Datastore 可访问性** | `datastore.summary.accessible` | APD / PDL 场景 |
| 27 | **Datastore 集群** | `StoragePod` | Storage DRS 的容量与均衡单元 |
| 28 | **VM 配置合规** | `vm.config.*`（CPU/mem hot-add、硬件版本、CBT） | 备份能力与升级路径依赖这些开关 |

### 形态三：不应进资源模型（事件流 / 状态流，另建通道）

这三项**最容易被错误建模成资源**，需要单独说明：

| # | 对象 | 为什么不该进 URM |
|---|---|---|
| 29 | **触发的告警**（`triggeredAlarmState`） | 告警是**状态的时间片**，不是资源。它的正确形态是 `vmware_alarm_triggered{entity_moid, alarm_name, severity} 1` —— 一条随告警消失而消失的序列，挂在已有资源上，而不是一类新资源 |
| 30 | **事件**（`EventManager`） | 事件是**离散日志**，天然不适合 Prometheus。vMotion 发生、配置变更、登录失败这些应该走日志管道（Loki / ES）。硬塞成指标只能退化成"最近一分钟事件计数"，丢掉全部上下文 |
| 31 | **任务**（`TaskManager`） | 同上。可以派生一个"当前运行中的长任务数"作为指标，但任务本身属于日志 |

对 URM 的建议：**给资源模型留一个 `health` / `alarm` 关联面，但不要把告警和
事件建成资源实体**。它们的基数（cardinality）行为完全不同 —— 资源数量稳定，
事件数量随时间无界增长。

### 形态四：vCenter 自身（常被遗漏）

| # | 信号 | 说明 |
|---|---|---|
| 32 | **vCenter 服务健康** | `ServiceInstance.content.about` + VAMI/appliance health。vCenter 自己挂了，所有采集一起瞎 |
| 33 | **vCenter 数据库 / 存储水位** | appliance API。vCenter 的 VCDB 满了会静默停止记录性能数据 |
| 34 | **采集器自身可观测性** | 本仓库已有 `up` 与 scrape 时长。可补：每类对象的实体数、被跳过的实体数、perf 查询失败数 |

第 34 项值得强调：如果第二节缺陷 A 按建议修复，就需要一个
`vmware_entities_skipped_total{reason}` 来暴露"我因为什么原因少采了东西"。

---

## 四、给 URM 审计的六个待决问题

下面每一条都是**需要你拍板的建模决策**，不是实现细节。选错的代价是后续所有
查询和关联逻辑重写。

### Q1：资源身份用什么？MoRef 还是 UUID？

当前实现全部用 **MoRef**（`host.Self.Value`，形如 `host-42`）作为 `moid` label。

MoRef 的问题：**它只在单个 vCenter 内唯一，且跨 vCenter 迁移后会变**。
- 同一台 VM 从 vCenter A 迁到 B，MoRef 从 `vm-101` 变成 `vm-887`。
  在 URM 里这会被识别成"旧资源消失 + 新资源出现"。
- 两个 vCenter 各有一个 `host-42`，只靠 `moid` 无法区分。当前靠额外的
  `vcenter` label 兜住了，但这意味着**资源主键是复合键** `(vcenter, moid)`。

替代方案：VM 有 `config.uuid` 与 `config.instanceUuid`，Host 有
`hardware.systemInfo.uuid`，Datastore 有 `summary.url` 里的 UUID。
这些跨 vCenter 稳定。

**建议**：URM 的资源主键用稳定 UUID，MoRef 降为一个"当前位置"属性。
但注意 —— 这需要**新增属性采集**（当前 UUID 一个都没采），
且 Prometheus 侧的 label 基数会增加。

### Q2：父子关系怎么表达？当前是残缺的

现在的关系表达方式是在 `_info` 指标上挂父引用：
- `vmware_host_info{moid, name, parentmo}` —— parentmo 指向 ComputeResource
- `vmware_vm_info{moid, name, hostmo}` —— 指向 Host
- `vmware_cluster_datastore{cmo, dsmo}` —— 集群到 datastore，一条一序列

问题有三个：
1. **中间层缺失**：VM → Host → ComputeResource，但 ResourcePool 和 Folder
   这两层不存在，所以 `vm_info.hostmo` 跳过了实际的资源分配层级。
2. **Datacenter 关联断裂**：datacenter collector 只出 name 和 parent，
   而 host/vm 无法直接关联到 datacenter，必须多跳 join。
3. **vCenter 层级不完整**：Folder 树没采，所以无法还原 vCenter UI 里看到的
   实际组织结构。

**待决**：URM 是要还原完整的 vSphere 树（Datacenter → Folder →
Cluster → ResourcePool → VM），还是压平成"资源 + 若干归属标签"？
前者忠实但 join 深，后者好查但丢结构。

### Q3：`_info` 模式还是 label 富化？

当前是 `_info` 模式：`vmware_host_info` 值恒为 1，元信息全在 label，
需要用 `* on(moid) group_left(...)` 关联到指标上。

这是 Prometheus 的标准做法，但对 URM 有个含义：**资源属性与资源指标是两套
序列**，URM 侧需要自己做这个 join。如果 URM 期望"一个资源一条记录、属性和
指标在一起"，需要在采集器和 URM 之间加一层聚合。

**待决**：URM 消费的是原始 Prometheus 序列，还是经过 join 的资源视图？

### Q4：设备实例 label —— 已查清，此项无需决策

这是 vSphere exporter 的经典陷阱，但**本仓库没有踩**。记录在此以免审计时
重复排查。

当前 `scrapePerformance` 对 instanced 计数器传 `"*"`（`host.go:176`），
每个网卡、每个 datastore 各一条序列。承载设备实例的 label 名是
**`pfinstance`**（`descs.go:373-375`）：

```go
labels := append([]string{"vcenter"}, perfEntityLabels(moType)...)
if instanced {
    labels = append(labels, "pfinstance")
}
```

没有占用 `instance` —— 后者在 Prometheus 里指抓取目标，若被 exporter 占用会
在服务发现重打标签时被覆盖。实体标识也已分开（`descs.go:333-344`）：
HostSystem → `host` + `hostmo`，VirtualMachine → `vm` + `vmmo`，
Datastore → `ds` + `dsmo`。名称与 MoRef 各占一个 label，这对 URM 是好事：
改名不会导致序列断裂，因为 join 可以走 `*mo`。

**唯一要留意的**：`pfinstance` 是本仓库自造的名字，URM 侧若要对接其他
vSphere exporter（如 vmware_exporter、telegraf vsphere 插件）需要做 label
映射 —— 它们通常叫 `instance` 或 `device`。

### Q5：多 vCenter 的资源合并策略

本仓库支持多 target（`vcenter` label 区分）。但同一份物理资源可能在两个
vCenter 里出现：
- Linked Mode 下多个 vCenter 共享清单，同一 VM 会被两边都采到。
- 跨 vCenter vMotion 期间会短暂双份。

**待决**：URM 是按 `(vcenter, moid)` 存两份，还是按 UUID 去重合并？
这个决策直接依赖 Q1。

### Q6：采集频率分层 —— 配置类与性能类不该同频

当前所有 collector 在同一次 scrape 里跑完。但：
- **配置/清单类**（`_info`、容量、许可、HA 配置）分钟级甚至小时级足够，
  它们几乎不变。
- **性能类**（cpu/mem/net/disk）需要 20s~1min。

现在混在一起的代价：每次 scrape 都要重新检索全量属性。清单里的
`fetchProperties` 对每类对象都是一次 ContainerView 创建 + RetrieveProperties
+ 销毁，实体多时这是主要开销。

**待决**：URM 是否需要采集器提供**分层抓取端点**（如
`/metrics?class=inventory` 与 `?class=performance`），
让 Prometheus 用不同 `scrape_interval` 分别抓？
本仓库已有 `/probe` 与 collector 开关，实现代价不大。

---

## 五、实现可行性分级

不是所有缺口的代价都一样。按**接入方式**分四档，这直接决定要不要做。

### 档 1：零新增往返 —— 只是往属性列表里加字段

改一行 `fetchProperties` 的属性数组即可，**不增加任何 API 调用**。
现有代码已经在检索 `summary` / `runtime`，多取几个字段是免费的。

覆盖：`overallStatus`（#12）、电源/连接/维护状态（#13）、Tools 状态（#14）、
心跳（#15）、Guest 信息（#16）、集群容量汇总（#19）、EVC（#20）、
Datastore 类型与可访问性（#25/#26）、UUID（Q1 所需）。

> **这一档应该优先全部做掉。** 它同时修复第二节的两个口径缺陷，
> 而且是 URM 建模最急需的"状态"与"身份"两类信息。

### 档 2：新增一次 ContainerView 检索 —— 常规新采集器

与现有 5 个 collector 同构，复制 `datacenter.go` 的骨架即可。
每类对象一次 ContainerView 创建 + RetrieveProperties + 销毁。

覆盖：ResourcePool（#1）、vApp（#2）、Folder（#3）、DVS（#5）、
端口组（#6）、StoragePod（#27）、告警（#29，挂在实体上）。

代价可控，但**实体数多时要注意**：ResourcePool 与 Folder 在大环境里
数量可能超过 VM 数。建议做成默认启用但可关闭。

### 档 3：逐主机调用 —— 开销随主机数线性增长

必须逐台主机发请求，无法批量。这就是现有两个 esxcli collector 默认禁用的原因。

覆盖：标准交换机（#7）、存储适配器/多路径（#8）、硬件传感器（#21）、
NTP/服务状态（#22）、固件版本（#23）。

**建议一律默认禁用**，并沿用 esxcli collector 已有的模式。
`host.config.network` 与 `runtime.healthSystemRuntime` 严格说可以随 HostSystem
属性一起批量取，但它们的返回体很大 —— 取全量 host 的 `config` 会显著撑大单次
响应，实测前不要假设它便宜。

### 档 4：需要新 SDK / 新端点 —— 独立评估

| 项 | 障碍 |
|---|---|
| **vSAN**（#9） | 需 `govmomi/vsan` 包与 `vsan-health` 端点，是独立的 SOAP service。还需处理 vSAN 未启用时的优雅降级 |
| **Tag / Category**（#4） | **REST API（vAPI），不是 SOAP**。需要独立的会话管理与认证路径。当前 `vmware/api/vmware.go` 只建了 SOAP 客户端 |
| **许可证**（#11） | `LicenseManager` 是 SOAP 但走 ServiceContent 单例，不是 ContainerView，取法不同 |
| **VMDK 明细**（#10） | 需取 `vm.config.hardware.device` 并遍历设备树筛 `VirtualDisk`。属性体积大，VM 多时慎用 |
| **事件/任务**（#30/#31） | 见形态三 —— 建议不进 Prometheus |
| **vCenter appliance 健康**（#32/#33） | VAMI REST API，又一套认证 |

**Tag（#4）的特别说明**：它对 URM 的价值极高（业务归属的唯一可靠来源），
但接入代价也最高 —— 要引入第二套认证。如果 URM 已经有其他途径拿到
vSphere tag（比如直接对接 vCenter），**建议不要在 exporter 里做**。

---

## 六、建议的优先级

排序依据是「对 URM 的必要性 ÷ 实现代价」，不是功能完整度。

### P0 —— 修口径，不加对象（档 1）

1. **停止过滤关机/维护实体**，改为导出状态指标（缺陷 A）
2. **加 `overallStatus`**（缺陷 B、#12）
3. **加 UUID**，为 URM 主键做准备（Q1）
4. **补集群容量汇总**（#19）—— 当前 cluster collector 只有 `_info`，
   连 `effectiveCpu` 都没有，这是明显的空洞

这四项都在档 1，改动集中在属性列表和 emit 逻辑，且**能立刻解掉 URM 建模里
"资源状态"和"资源身份"两个基础问题**。

### P1 —— 补齐层级与已列未做项（档 2）

5. **ResourcePool + vApp**（#1/#2）—— 补上 VM 与 Cluster 之间缺失的一层
6. **DVS + 端口组**（#5/#6）—— 你清单里已列，实际未实现
7. **告警**（#29）—— 但按"挂在实体上的状态序列"建模，不是新资源类型
8. **Tools / 心跳 / Guest 信息**（#14/#15/#16）—— 严格说属档 1，
   放这里是因为它们对 URM 的必要性略低于 P0

### P2 —— 看环境决定

9. **快照大小**（#24）—— 若环境里快照管理混乱，这条会跳到 P0
10. **Folder**（#3）—— 若客户的业务归属靠文件夹组织，同样上提
11. **StoragePod**（#27）—— 仅当用了 Storage DRS
12. **硬件传感器**（#21）—— 档 3，默认禁用

### P3 —— 独立立项

13. **vSAN**（#9）—— 若环境用 vSAN 则必做，但工作量是一个独立 collector 加
    新 SDK 引入
14. **Tag**（#4）—— 先确认 URM 是否已有其他途径
15. **许可证**（#11）—— 到期风险高但变化极慢，可低频采集

### 明确不建议做

- **事件、任务**（#30/#31）进 Prometheus —— 走日志管道
- **vCenter appliance 健康**（#32/#33）—— 除非已经在维护 VAMI 认证

---

## 七、需要你确认的事项

1. 第六节的 P0 四项是否认可？特别是**停止过滤关机实体**这条会改变现有指标行为
   （关机 VM 会开始出现在 `vmware_vm_info` 里），属于 breaking change，
   需要进 CHANGELOG。
2. Q1（MoRef vs UUID 作主键）与 Q5（多 vCenter 去重）需要一起定。
3. Q2（是否还原完整 vSphere 树）决定 ResourcePool / Folder 的优先级。
4. Q6（分层抓取）—— 若 URM 期望配置类与性能类分开抓，需要在设计阶段就定，
   事后拆开会改动 collector 接口。
5. 环境实际情况：**是否使用 vSAN、DVS、Storage DRS、Tag**？
   这四项直接决定 P1/P3 里哪些是必做、哪些可以永久搁置。
