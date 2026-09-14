# 采集面 P1/P2/P3 设计与取舍方案

> 编写日期：2026-09-10
> 基线：`master` @ `ab582ce`（含无弹窗复制修复），tag `v0.1.20` 指向 `d22d435`
> 上游文档：`docs/COVERAGE-GAP-ANALYSIS.md`（缺口原始清单，2026-09-03 局部更新）
> 本文目的：回答"P1/P2/P3 里哪些现在做、哪些暂缓、哪些不做"，并给"做"的条目
> 落到**指标名、属性来源、测试方式、基数评估、兼容策略**可执行的粒度。
>
> **阅读约定**：
> - 缺口分析文档里的 **P0 四项（停止过滤关机实体 / overallStatus / UUID / 集群容量）
>   是本方案的前置批次，不是 P1**。本批次的状态见第 1 章，做完才轮到 P1。
> - 已明确出范围的 **Network / DVS**、已交付的 **ResourcePool / vSAN** 不再重复讨论。
> - 每条结论都标注了 v0.1.20 源码证据，行号随代码漂移，核对时以符号名为准。

---

## 0. 结论先行（评审只看这张表也够）

| 批次 | 条目 | 结论 | 理由一句话 |
| --- | --- | --- | --- |
| 前置 P0 | 生命周期状态指标（缺陷 A） | **做，最先做** | 不做的话"主机进维护"和"主机被删"在 Prometheus 里无法区分 |
| 前置 P0 | overallStatus 全覆盖（缺陷 B） | **做，与状态指标同批** | 零新增往返，复用 resourcepool 已有的 `overall_status` 形态 |
| 前置 P0 | 集群容量指标 | **做，与状态指标同批** | cluster collector 现在只有 `_info`，容量视图缺分母 |
| 前置 P0 | 稳定 UUID label | **做，但仅加 label 不改主键** | 为跨 vCenter 场景铺路；无下游消费前不动 join 语义 |
| **P1** | VMware Tools / Guest 心跳 / Guest 身份信息 | **做** | 属性零新增往返，业务可用性信号比 PowerState 更真实 |
| **P1** | 触发告警 triggeredAlarmState | **做，按状态序列建模** | vCenter 已算好的健康结论，不采等于浪费；形态是 gauge 不是新资源 |
| **P1** | HA/DRS 配置与状态 | **做** | 集群可用性的核心配置，属性成本低 |
| **P1** | 采集器实体计数 / 跳过计数自监控 | **做（配套）** | 缺陷 A 落地后的必要配套，否则"少采了"不可见 |
| **P2** | 快照大小 / 快照数 / 链深度 | **做，默认启用** | 最常见的 datastore 容量事故源；成本一次额外属性 |
| **P2** | Datastore 类型/可访问性增强 | **已做，复核即可** | `datastore_info` 已有 type label、已有 `datastore_accessible` |
| **P2** | Folder 业务路径 | **暂缓** | 价值取决于客户是否用文件夹组织业务；实现要递归 parent 链 |
| **P2** | StoragePod（Storage DRS） | **暂缓** | 仅 Storage DRS 环境有意义，用户环境待确认 |
| **P2** | 硬件传感器 / NTP / 固件（档 3） | **暂缓，做也默认禁用** | 逐主机 SOAP，开销随主机数线性增长，沿用 esxcli 模式 |
| **P2** | 分层抓取端点 `?class=` | **暂缓** | 与现有 TTL inventory cache 目标重叠，先观察缓存版效果 |
| **P3** | vApp | **暂缓，并入 ResourcePool 扩展** | 是 ResourcePool 子类，未来加 `type="vapp"` 即可，不立独立项 |
| **P3** | Tag / Category（REST vAPI） | **暂不做** | 需引入第二套认证会话；若云管平台已对接 vCenter tag 则重复建设 |
| **P3** | License 许可证 | **暂缓，低频独立抓取** | 到期风险真实但变化极慢，不值得进每轮 scrape |
| **P3** | 事件 / 任务（EventManager/TaskManager） | **不做进 Prometheus** | 离散日志，属 Loki/ES 管道，硬塞成指标只剩无上下文计数 |
| **P3** | vCenter VAMI appliance 健康 | **不做（本仓库）** | 又一套认证与端点，应由独立 appliance exporter 承担 |

建议交付节奏：**Batch 0（缺口 P0）→ Batch 1（P1 三项 + 自监控配套）
→ Batch 2（P2 快照）**，每批独立 dev 分支、`--no-ff` 合并。其余 P2/P3 维持
"需要时再立项"，不排期。

---

## 1. 前置：缺口文档 P0 四项的现状（v0.1.20 实测）

这四项在缺口文档里排 P0，**目前代码里只做了零头**，是 P1 的逻辑前置，必须先交代。

| 项 | 现状 | 证据 |
| --- | --- | --- |
| 停止过滤关机/维护实体 | **未做**。vm 仍只处理 `poweredOn`；host 仍要求 `poweredOn && connected && !InMaintenanceMode`；esxcli 两个 collector 同条件 | `vmware/collectors/vm.go:77`、`host.go:74`、`esxclihostnic.go:75`、`esxclistoragelist.go:64` |
| 独立状态指标 | **不存在**。全库无 `vmware_vm_power_state` / `host_connection_state` / `maintenance_mode` | 全库 grep 零命中 |
| overallStatus | **仅 resourcepool 做了**（`vmware_resourcepool_overall_status`，状态进 label、值恒 1）；vm/host/datastore/cluster 未采 | `resourcepool.go:86,142`、`descs.go:414` |
| 稳定 UUID | **未做**。vm 属性仅 summary/runtime/storage/snapshot；host 取了 `hardware` 但未输出 `systemInfo.uuid`；无 uuid label | `vm.go:61`、`internal/collector/scrape.go:177` |
| 集群容量 | **未做**。cluster 取了 `summary` 但只发 `cluster_info` 与 `cluster_datastore` | `cluster.go:36` |

> 因此本方案把"缺口 P0 四项"列为 **Batch 0**。它不改架构，都是属性列表 +
> emit 逻辑的增量，但缺陷 A 是 **breaking change**（关机 VM/维护主机会开始出现在
> `_info` 里），需要 CHANGELOG 显式声明并给一个版本的迁移窗口。

---

## 2. Batch 0 设计（缺口 P0，建议先于一切 P1 交付）

### 2.1 生命周期状态指标（缺陷 A）

**当前问题**：关机 VM、断连/维护中的主机连同 `_info` 与容量指标一起消失，
Prometheus 无法区分"不存在"与"存在但不可用"；维护窗口里容量看板分母悄悄变小。

**指标设计**（全部 gauge，值恒 1，状态进 `state` label —— 对齐 resourcepool 已
确立的 `overall_status` 形态，避免一个实体多条布尔序列的散弹写法）：

```
vmware_vm_power_state{vcenter, vm, vmmo, state="poweredOn|poweredOff|suspended"} 1
vmware_host_power_state{vcenter, host, hostmo, state="poweredOn|poweredOff|standBy|unknown"} 1
vmware_host_connection_state{vcenter, host, hostmo, state="connected|disconnected|notResponding"} 1
vmware_host_maintenance_mode{vcenter, host, hostmo} 0|1
```

**口径规则（关键，避免半吊子修复）**：

- `_info` 与配置/容量类指标对**所有**实体无条件输出（维护中、关机都输出）。
- 只有 **perf 性能计数器**跳过关机/断连实体 —— 那是 vCenter 侧确实无实时数据，
  不是 exporter 的选择；跳过处加 debug 日志并计入 `entities_skipped_total`（见 3.4）。
- esxcli 两个 collector 的同条件过滤保留（主机不可达时 esxcli 本来就调不通），
  但跳过同样要计数。

**属性来源**：`runtime.powerState` / `runtime.connectionState` /
`runtime.inMaintenanceMode`，都在已检索的 `runtime` 属性内，**零新增往返**。

**兼容性**：breaking。`vmware_vm_info` / `vmware_host_info` 的序列集合会变大。
旧查询若隐式假设"`_info` 里都是开机实体"，改写为：

```promql
vmware_vm_info
  * on(vcenter, vmmo) group_left(state)
  vmware_vm_power_state{state="poweredOn"}
```

CHANGELOG 给出 host/vm 两组迁移示例，并建议该版本升 minor。

**测试**：用 vcsim 的 power op（`PowerOnVM`/`PowerOffVM`）与 host
`EnterMaintenanceMode` 构造状态，断言：(1) 关机 VM 仍出 `vm_info`；
(2) 状态 label 值正确；(3) perf 指标不为关机实体输出且 skip 计数 +1。

### 2.2 overallStatus 全覆盖

复用 `vmware_resourcepool_overall_status` 的 Desc 形态，给四类实体补齐：

```
vmware_vm_overall_status{...,status="green|yellow|red|gray"} 1
vmware_host_overall_status{...,status="green|yellow|red|gray"} 1
vmware_datastore_overall_status{...,status="green|yellow|red|gray"} 1
vmware_cluster_overall_status{...,status="green|yellow|red|gray"} 1
```

`overallStatus` 是 `ManagedEntity` 基础属性，加进各 collector 的属性列表即可，
**不增加任何 API 往返**。`gray`（未知）必须原样输出，它常先于真实故障出现，
不能归并进 yellow。

### 2.3 集群容量

cluster 已检索 `summary`。其中 `ComputeResourceSummary` 含 `numEffectiveHosts`、
`numCpuCores`、`numCpuThreads`、`totalCpu`（MHz 总量）；`ClusterComputeResourceSummary`
另有 `effectiveCpu`（可用 MHz，扣除 HA 预留/故障切换容量）与 `effectiveMemory`。
建议指标：

```
vmware_cluster_effective_host_count    gauge
vmware_cluster_cpu_capacity_hz         gauge   # totalCpu * 1e6，总量
vmware_cluster_cpu_effective_hz        gauge   # effectiveCpu * 1e6，可调度量
vmware_cluster_memory_capacity_bytes   gauge   # totalMemory * MiB
vmware_cluster_memory_effective_bytes  gauge   # effectiveMemory * MiB
```

**单位口径**刻意对齐 resourcepool 已用的 `_hz` / `_bytes` 后缀
（`resourcepool.go:182,190`），不造裸 MHz/MB 指标，避免重蹈
OPTIMIZATION-PLAN P2-1/P2-2 那种"help 写 MB、值是字节"的旧账。

### 2.4 稳定 UUID（只加 label，不换主键）

- VM：属性加 `config.uuid`、`config.instanceUuid`；Host：已取 `hardware`，输出
  `hardware.systemInfo.uuid`；Datastore：VMFS 用 `info.vmfs.uuid`，其他类型回退
  `summary.url`。
- 作为**额外 label** `uuid` 出现在对应 `_info` 指标上；取不到（权限受限/旧版本/
  非 VMFS）时为空串，不报错、不阻断采集。
- **本批不改任何 join 键**：`moid` + `vcenter` 仍是事实主键。缺口文档 Q1/Q5
  （跨 vCenter 去重、Linked Mode 双份合并）是消费侧建模决策，等出现真实的跨
  vCenter vMotion / Linked Mode 场景再立项；现在换主键只会徒增基数和迁移成本。

---

## 3. Batch 1 设计：P1 三项 + 自监控配套

建议拆成三个可独立评审合并的子批，但同属一个 minor。

### 3.1 P1-a：Tools / Guest 心跳 / Guest 身份（档 1，零新增往返）

VM 属性列表当前没有 `guest`，但它可与现有 summary/runtime 在**同一次**
`RetrieveProperties` 取回，不增加往返：

| 新指标 | 来源字段 | 形态 |
| --- | --- | --- |
| `vmware_vm_tools_running{...,state="guestToolsRunning\|guestToolsNotRunning\|guestToolsExecutingScripts"}` | `guest.toolsRunningStatus` | gauge=1，状态 label |
| `vmware_vm_tools_version_status{...,state="guestToolsCurrent\|guestToolsNeedUpgrade\|guestToolsUnmanaged\|guestToolsTooOld\|..."}` | `guest.toolsVersionStatus` | gauge=1，状态 label |
| `vmware_vm_guest_heartbeat{...,status="gray\|green\|red\|yellow"}` | `guestHeartbeatStatus`（在已检索的 summary 内） | gauge=1 |
| `vmware_vm_guest_info{vcenter,vm,vmmo,guest_os,host_name,ip}` | `guest.guestFullName`、`guest.hostName`、`guest.ipAddress` | `_info` 型 gauge=1 |

设计约束：

- **IP/hostname 敏感且可能高基数**。只取主 IP（`guest.ipAddress` 单值），
  **绝不**展开 `guest.net[].ipConfig.ipAddress[]` —— 多网卡 VM 会因此产生多条
  序列，且 IP 变化（DHCP）会制造僵尸序列。多网卡清单不是 Prometheus label 该
  承载的东西。
- 关机 / 无 tools 时：状态序列仍输出（"无数据"本身就是信息，如
  `tools_running{state="guestToolsNotRunning"}`、心跳 `status="gray"`）；
  `guest_info` 的 IP/hostname 留空串。
- 心跳 `gray` 表示 VMware Tools 未上报，**不等于**健康，必须与 green 区分。

**为什么现在做**：PowerState 只反映虚拟化层供电；Tools 心跳才反映**客户机 OS
是否活着**。备份、优雅关机、"主机活着但业务卡死"的告警分流都依赖它，成本为零。

**测试**：vcsim 可对自定义 VM 模型注入 `Guest` 字段，断言四种状态与空值降级。

### 3.2 P1-b：触发告警 triggeredAlarmState（档 1，随 inventory 批量取）

**建模纪律（缺口文档形态三）**：告警是挂在实体上的**状态时间片**，不是一类新
资源。不新建 alarm 资源实体，不回溯历史事件，只导出"此刻仍处于触发态"的告警：

```
vmware_alarm_triggered{vcenter, entity_mo, entity_name, alarm_name,
                       severity="info|warning|error", acknowledged="true|false"} 1
```

- 来源：`managedEntity.triggeredAlarmState`，每个 `AlarmState` 含 `key`
  （entity:alarm）、`overallStatus`（黄/红 → severity）、`acknowledged`、
  `time`，alarm 定义名经 `alarm.info.systemName` 或 entity 上
  `AlarmState.Alarm` 解析。把该属性加进 host/vm/datastore/cluster 的批量
  inventory 检索，**不调用 EventManager**。
- 告警在 vCenter 侧清除后序列自然消失 —— 正是 gauge 状态语义。"历史上何时
  触发过"属日志管道，明确不支持。
- **label 用 alarm 定义名，不用触发 message 文本**：message 含 VM 名/阈值等
  动态值，直接进 label 会造成基数泄漏。
- 实现：新建 `alarm` collector（注册名 `alarm`，**默认启用**，可
  `-collector.alarm=false` 关），从缓存的 inventory 实体上收集告警状态，独立于
  单个资源 collector，避免四个 collector 各发一遍。
- 基数：= 环境中当前活跃告警条数，正常为个位数/实体；告警风暴时受 vCenter
  自身上限约束，可接受。

**测试**：vcsim 对告警的模拟不完整，emit 层用表驱动单测（构造
`mo.ManagedEntity.TriggeredAlarmState`），真实设备联调作为补充验收。

### 3.3 P1-c：HA/DRS 配置与状态（档 1~2）

集群可用性的核心配置，当前完全空白：

```
vmware_cluster_ha_enabled{...} 0|1
vmware_cluster_ha_admission_control_enabled{...} 0|1
vmware_cluster_drs_enabled{...} 0|1
vmware_cluster_drs_automation_level{...,level="manual|partiallyAutomated|fullyAutomated|disabled"} 1
vmware_cluster_evc_enabled{...} 0|1
```

- 来源：`configuration.dasConfig.enabled` /
  `dasConfig.admissionControlEnabled`、`configuration.drsConfig.enabled` /
  `drsConfig.defaultVmBehavior`；EVC 用已在 summary 中的
  `currentEVCModeKey`（空串即未启用，不额外发 mode 字符串 label，避免每个 EVC
  模式名变成新序列）。
- `configurationEx` 响应体明显大于 summary。两种取法二选一，建议先 A：
  - **A（推荐）**：并入现有 cluster collector 的属性集 —— 集群数量通常远小于
    VM/host，多取的配置体可忽略，且不增加 collector 数量。
  - B：独立 `-collector.cluster.config`（默认关）—— 仅当实测大环境响应体不可
    接受时再拆。
- 基数：每集群一条，可忽略。

**测试**：vcsim 的 `ClusterComputeResource` 支持 `ModifyClusterConfiguration`
风格的配置设置（或直接对 simulator model 赋值），断言开关与自动化级别；ESXi
直连（无真实集群）下该 collector 对伪集群不输出这些指标。

### 3.4 配套：实体计数 / 跳过计数自监控

Batch 0 让"被跳过的实体"从"消失"变为"显式状态"，但还需要让**跳过的原因**可
观测，否则告警配置无的放矢：

```
vmware_scrape_entities_total{vcenter, collector, kind}        gauge   # 本轮发现的实体数
vmware_scrape_entities_emitted_total{vcenter, collector, kind} gauge   # 实际输出的实体数
vmware_scrape_entities_skipped_total{vcenter, collector, reason="poweredOff|disconnected|maintenance|unsupported|error"} gauge
```

- 与已有的 `vmware_scrape_duration_seconds`、`vmware_scrape_errors_total{collector}`
  同属 scrape 自监控族，在统一调度层（`internal/collector`）聚合，不让每个
  collector 各造一份计数。
- 这些序列按 collector/kind/reason 分维，**基数与实体数无关**，是安全的。
- 可直接支撑告警：`rate(vmware_scrape_entities_skipped_total{reason="error"}[10m]) > 0`。

---

## 4. Batch 2 设计：P2 —— 快照空间指标（建议做），其余暂缓

### 4.1 快照大小 / 数量 / 链深度（做，默认启用）

**为什么值得做**：快照吃满 datastore 是 vSphere 运维最高频的容量事故，而当前只有
`vmware_vm_snapshot_info`（值=创建时间戳，`vm.go:121-131`），"有快照"但不知道
占了多少空间、链多深，无法预警。

**数据来源与取舍**：

- 快照大小没有单一权威字段。`VirtualMachineStorageSummary` 不按快照拆分；
  需要额外取 `layoutEx.snapshot`（每个快照文件的实际大小）或对 `layoutEx.file`
  中的 delta 盘大小聚合。`layoutEx` 体积较大，VM 数量多时随 VM inventory 一起
  取会明显撑大单次响应。
- 因此做成 **vm collector 内的可选属性组**，由独立开关
  `-collector.vm.snapshot-size` 控制，默认启用但可关；与 v0.1.20 已落地的
  **chunked QueryPerf + TTL inventory cache** 叠加后，大环境的增量成本主要在
  缓存刷新周期，不在每轮抓取。

**指标设计**：

```
# 每根快照一条：保留时间与身份（现状保留）
vmware_vm_snapshot_info{vcenter,vm,vmmo,snapshotmo,name} = 创建时间 Unix 秒
# VM 级聚合：空间、数量、深度（每 VM 一条，基数安全）
vmware_vm_snapshot_count{vcenter,vm,vmmo}               gauge
vmware_vm_snapshot_chain_depth{vcenter,vm,vmmo}         gauge
vmware_vm_snapshot_size_bytes{vcenter,vm,vmmo}          gauge  # 该 VM 全部快照 delta 总量
vmware_vm_snapshot_oldest_age_seconds{vcenter,vm,vmmo}  gauge
```

`snapshot_info` 现有 label 集合保持不变（`created` 时间戳 label 已移除，时间戳
只留 value）；新增的 VM 级聚合指标不带每快照 label，避免快照链变化制造僵尸序列。

**测试**：vcsim 上连续两次 `CreateSnapshot_Task` 构造多级链，断言 count=2、
depth=2、size >= 0、oldest_age 合理；删除快照后聚合值归零的行为固定并写进测试。

### 4.2 Datastore 类型 / 可访问性（已实现，仅复核）

`datastore_info` 已带 type label、已有独立 `vmware_datastore_accessible`
（`datastore.go:76,88-90`）。本批次不新增工作；Batch 0 补 overallStatus 时
顺带确认 APD/PDL（accessible=false / host mount 不可达）场景的取值与文档。

### 4.3 暂缓项及触发条件

| 项 | 暂缓理由 | 什么情况下重启 |
| --- | --- | --- |
| Folder 业务路径（`folder_path` label 或 Folder collector） | 现在只采 `host`/`datastore` 两个系统文件夹（`datacenter.go:97-118`）。业务文件夹价值取决于客户是否用文件夹承载归属；还原路径要递归 parent 链，label 值也较长 | 出现"按业务部门/系统归集 VM 容量"的真实需求，且 tag 方案不可用时 |
| StoragePod（Storage DRS 容量单元） | 仅启用 Storage DRS 的环境有意义；用户环境尚未确认 | 确认环境使用 Storage DRS |
| 硬件传感器 / NTP / 固件（档 3） | 逐主机 SOAP，开销随主机数线性增长；传感器体系型号差异大、枚举维护成本高 | 有硬件预警强需求时，按 esxcli collector 模式新增且**默认禁用** |
| 标准交换机 / 多路径明细（档 3） | 同上，部分已被默认禁用的 esxcli.storage / esxcli.host.nic 覆盖 | 默认禁用渠道出现真实消费再扩展 |
| 分层抓取 `/probe?class=inventory\|performance`（缺口 Q6） | 分层的核心收益（降低大环境每轮 inventory 开销）已由 v0.1.20 的 **TTL inventory cache** 拿到大部分；缓存版未经大规模实测就拆端点属过早设计 | 缓存版仍无法把 scrape 压进目标间隔时，再按 collector 分组拆端点 |

---

## 5. Batch 3：P3 —— 大部分不做进本仓库

### 5.1 vApp：不立独立项，预留扩展点

`VirtualApp` 是 `ResourcePool` 的子类。未来需要时在 resourcepool collector 的
检索类型中加入 `VirtualApp`，以 `type="resourcepool|vapp"` label 区分即可，
**现在不做**（无需求环境里纯属多余序列）。

### 5.2 Tag / Category：暂不做

- 真正的障碍不是"新 SDK"，而是 **REST vAPI 需要独立的认证会话**
  （`/api/cis/tagging/...`）。当前 `vmware/api` 只建立 SOAP 客户端，接入 tag
  要新增一套会话生命周期、登出与超时管理，与 P0-1 修过的会话泄漏问题同构，
  维护面显著扩大。
- 价值高度依赖消费侧：tag 是云管/CMDB 的业务归属来源。**若 URM/云管平台已有
  途径直接对接 vCenter tag，exporter 再做一遍就是重复建设**。
- 重启条件：确认没有其他系统提供 tag，且业务归属无法用文件夹（4.3）替代时，
  作为独立大项设计（独立 client、独立 collector、默认禁用）。

### 5.3 License 许可证：暂缓，应低频采集

到期/超配风险真实，但许可证数据变化极慢（月级），放进 20s~60s 的每轮 scrape
毫无必要。若要做，正确形态是**独立低频端点或独立缓存 TTL（小时级）**，而不是
塞进现有 collector。当前无明确需求，暂缓。

### 5.4 事件 / 任务：不做进 Prometheus

`EventManager` / `TaskManager` 产生的是**离散日志流**。Prometheus 指标只能退化成
"最近 N 分钟事件计数"，丢掉全部上下文（谁、对哪个对象、什么变更），既查不了
vMotion 历史也追不了配置变更。这类数据应进 Loki / Elasticsearch / SIEM。
唯一可考虑的派生指标"当前运行中的长任务数"价值也很低，不做。

### 5.5 vCenter VAMI appliance 健康 / VCDB 水位：本仓库不做

VAMI 是又一套端点与认证（appliance API），与 vCenter SOAP 采集目标不同。应由
专门的 appliance 监控（或 VAMI 自身的 SNMP/健康端点 + blackbox）承担，避免把
exporter 撑成"vCenter 全家桶采集器"。

---

## 6. 批次、风险与验收总览

| 批次 | 内容 | 新增往返 | 破坏性 | 默认启用 |
| --- | --- | --- | --- | --- |
| Batch 0 | 生命周期状态、overallStatus 全覆盖、集群容量、UUID label、skip 自监控 | 零（状态/overallStatus/容量/UUID） | **是**（关机/维护实体进入 `_info`） | 是 |
| Batch 1a | Tools / 心跳 / Guest 身份 | 零 | 否（纯增量） | 是 |
| Batch 1b | 触发告警 alarm collector | 零（随 inventory） | 否 | 是 |
| Batch 1c | HA/DRS/EVC 配置 | 0~1（并入 cluster 配置检索） | 否 | 是 |
| Batch 2 | 快照空间/数量/深度 | 有（layoutEx，可关） | 否（纯增量） | 是（可关） |
| 暂缓 | Folder / StoragePod / 硬件档3 / 分层端点 | — | — | — |
| 不做 | 事件任务进指标、VAMI 健康 | — | — | — |

**跨批次的通用验收标准**：

1. 每个新增指标在 `docs/METRICS.md` 与 `docs/METRICS-zh.md` 登记，help 文案
   标明单位（沿用 `_bytes`/`_hz`/`_seconds` 后缀约定），`scripts/check_config.py`
   双向一致性检查通过。
2. Desc 一律在 `descs.go` 构造一次，禁止回到热路径里 `prometheus.NewDesc`
   （OPTIMIZATION-PLAN P2-4 的约定）。
3. vcsim（VPX 与 ESX 两种 `targetType`）表驱动测试覆盖：新增序列存在性、
   状态枚举值、空值降级、关机/维护口径；ESXi 直连下不适用的集群指标不输出。
4. 每个批次跑 `CGO_ENABLED=0 go test ./...`；竞态靠 CI Linux 的 `-race`
   （本机无 C 工具链）。
5. 每个批次独立 dev 分支，`--no-ff` 合 master；Batch 0 含 breaking change，
   合并版本升 minor 并在 CHANGELOG 写迁移示例。
6. 不 push、不打 tag，除非明确要求；v0.1.20 的本地 tag 与 systemd 部署包已先行，
   Batch 0 建议从 **v0.2.0** 起编号。

## 7. 需要拍板的事项

1. 是否接受 Batch 0 的 breaking change（关机 VM / 维护主机进入 `_info`），
   并据此从 v0.2.0 起编号？
2. 快照 `layoutEx` 的成本：是否同意"默认启用、可关"，还是首版默认关、观察后再开？
3. HA/DRS 配置取法：并入 cluster collector（推荐）还是独立默认关的子开关？
4. 环境确认：是否使用 Storage DRS、业务文件夹归属、是否已有系统消费 vCenter tag？
   这三个答案决定 4.3 与 5.2 是否永远搁置。

