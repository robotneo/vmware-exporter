# 指标参考

[English](./METRICS.md)

本 exporter 能吐出的每一个指标、它带的确切标签集、以及数值代表什么。

**本文档属于契约的一部分。** 新增、改名、删除任何一个指标、标签或 `--collector.*`
flag，都必须在同一次改动里同步更新本文档。`scripts/check_config.py` 会强制执行——
代码里声明了但文档里没有（或文档里列了但代码里找不到）都会导致检查失败。

- Namespace：所有指标的前缀都是 `vmware_`。
- 类型：除 `vmware_scrape_errors_total`（counter）和 `vmware_exporter_build_info`
  （gauge，值恒为 1）之外，**全部是 gauge**。
- `*_info` 指标的数值恒为 `1`，它们存在的意义是携带标签，供你 join 到数值型序列上
  —— 见 [Joining on `_info` 指标](#joining-on-_info-指标)。

## 目录

- [自监控指标](#自监控指标)
- [拓扑与清单](#拓扑与清单) — `datacenter`, `cluster`
- [主机](#主机) — `host`
- [虚拟机](#虚拟机) — `vm`
- [数据存储](#数据存储) — `datastore`
- [资源池](#资源池) — `resourcepool`
- [vSAN](#vsan) — `vsan`, `vsan.perf`
- [ESXi CLI](#esxi-cli) — `esxcli.host.nic`, `esxcli.storage`
- [性能计数器](#性能计数器) — 来自 vCenter Performance Manager 的动态计数器，
  由 `host`、`vm` 和 `datastore` 采集器发出
- [已弃用指标](#已弃用指标)
- [通用标签](#通用标签)
- [Joining on `_info` 指标](#joining-on-_info-指标)

## 自监控指标

无论开启了哪些采集器，每次抓取都会发出这些指标。它们是你要设告警的序列——
告诉你 exporter 本身是否在工作，这与 vSphere 是否健康是两回事。

| 指标 | 标签 | 含义 |
|------|------|------|
| `vmware_up` | — | `1` 表示能登录目标，`0` 表示不能。`0` 意味着这轮抓取**没有产生任何清单数据**——此时应把其他所有指标视为过期。密码错误在这里体现，不会表现为 HTTP 错误。 |
| `vmware_scrape_duration_seconds` | — | 整轮抓取的挂钟耗时（含登录和登出）。对照你的 `scrape_timeout` 评估。 |
| `vmware_scrape_collector_duration_seconds` | `collector` | 单个采集器的耗时。用于定位哪个采集器拖慢了抓取。 |
| `vmware_scrape_collector_success` | `collector` | `1` 表示该采集器完成，`0` 表示出错。单个采集器失败不会导致整轮抓取失败。 |
| `vmware_scrape_errors_total` | `collector` | **Counter。** 每个采集器累积的抓取错误数。`login` 值覆盖认证失败。请对 `rate()` 设告警，不要直接看原始值。 |
| `vmware_exporter_build_info` | `version`, `revision`, `branch`, `goversion`, `goos`, `goarch`, `tags` | 恒为 `1`。回答「当前宿主机跑的是哪个构建？」 |
| `vmware_exporter_config_last_reload_successful` | — | `1` 表示上次 `systemctl reload` 成功，`0` 表示失败。**值得专门设告警：** `systemctl reload` 只要信号送达就返回 0，配置文件被拒绝时不会报错。失败的重载会保留旧配置。 |
| `vmware_exporter_config_last_reload_success_timestamp_seconds` | — | 最后一次**成功**重载的 Unix 时间戳；如果从未重载过，则为进程启动时间。 |

如果 `-disable.exporter.metrics=false`，标准 Go 和进程采集器（`go_*`、`process_*`）
也会附加到 `/metrics`。

### 建议告警

```promql
# exporter 无法登录 —— 其他所有指标都已过期
vmware_up == 0

# 配置重载被拒绝；进程保留了旧配置
vmware_exporter_config_last_reload_successful == 0

# 某个采集器持续失败，但整轮抓取仍然 "成功"
rate(vmware_scrape_errors_total[15m]) > 0
```

## 拓扑与清单

采集器：`datacenter`、`cluster`（均默认启用）。

这些几乎全是 `_info` 指标，其用途是提供 vSphere 自身使用的父对象引用——
你 join 到数值型序列上，回答「这个 VM 在哪个集群里？」这类问题。

| 指标 | 标签 | 含义 |
|------|------|------|
| `vmware_target_info` | `target`, `type`, `version`, `build`, `patch` | 抓取目标。`type` 是 `vcenter` 或 `esxi`，决定了其他采集器能看到什么：独立 ESXi 主机没有集群、没有真正的数据中心、也没有 vSAN 集群健康。Dashboard 依赖此指标做条件渲染。 |
| `vmware_vcenter_info` | `version`, `build`, `patch`, `vcenter` | 端点的构建详情。与 `vmware_target_info` 并存是因为已有面板引用了它。 |
| `vmware_datacenter_info` | `dcmo`, `dc`, `vcenter`, (`synthetic`) | 每个数据中心一条序列。ESXi 上唯一的数据中心是隐式的 `ha-datacenter` 伪对象，标记为 `synthetic="true"`——用 `{synthetic!="true"}` 过滤掉。 |
| `vmware_folder_info` | `foldermo`, `dc`, `dcmo`, `vcenter` | 每个 `host` 或 `datastore` 文件夹一条序列，用于遍历清单树。注意这里的 `dc` 是**文件夹**名，不是数据中心名。 |
| `vmware_cluster_info` | `cmo`, `vmwcluster`, `foldermo`, `vcenter` | 每个集群一条序列。集群名的标签是 `vmwcluster`，不是 `cluster`——`cluster` 在许多 Prometheus 设置中是保留标签。 |
| `vmware_cluster_datastore` | `cmo`, `vmwcluster`, `dsmo`, `vcenter` | 集群可达的数据存储，**每个数据存储一条序列**。 |
| `vmware_compute_info` | `cmo`, `host`, `foldermo`, `vcenter`, (`synthetic`) | 独立计算资源——不在任何集群中的主机。仅在不存在集群时发出。在 ESXi 上这是 `ha-compute-res` 伪对象；在 vCenter 下独立主机有真实的自动生成的 ComputeResource，**不**标记为 synthetic。 |
| `vmware_compute_datastore` | `cmo`, `host`, `dsmo`, `vcenter`, (`synthetic`) | 独立计算资源可达的数据存储，每个数据存储一条序列。 |

## 主机

采集器：`host`（默认启用）。

| 指标 | 标签 | 含义 |
|------|------|------|
| `vmware_host_info` | `hostmo`, `host`, `cmo`, `vcenter` | 每个 ESXi 主机一条序列，带有其父集群或计算资源。 |
| `vmware_host_hardware_info` | `hostmo`, `host`, `vendor`, `model`, `cpu_type`, `vcenter` | 硬件型号和 CPU 类型。 |
| `vmware_host_software_info` | `hostmo`, `host`, `software`, `version`, `build`, `vcenter` | ESXi 版本和构建号——规划补丁时按此分组。 |
| `vmware_host_cpu_corecount` | `hostmo`, `host`, `vcenter` | 物理 CPU 核心数。 |
| `vmware_host_cpu_threadcount` | `hostmo`, `host`, `vcenter` | 物理线程数，即核心数 × SMT 宽度。 |
| `vmware_host_cpu_capacity_hertz` | `hostmo`, `host`, `vcenter` | 平均核心频率（赫兹）。**要乘以 `cpu_corecount`** 才是主机总容量——这是单核值，不是总和。 |
| `vmware_host_mem_capacity_bytes` | `hostmo`, `host`, `vcenter` | 总物理内存（字节）。 |

主机 CPU 和内存的**利用率**属于性能计数器，不在此列——见[性能计数器](#性能计数器)。

## 虚拟机

采集器：`vm`（默认启用）。这通常是指标最多的采集器：一台主机只有几条序列，一台
VM 有数条序列加上它的性能计数器。

| 指标 | 标签 | 含义 |
|------|------|------|
| `vmware_vm_info` | `vmmo`, `vm`, `hostmo`, `vcenter` | 每个 VM 一条序列，带它所在的主机。通过 `hostmo` 可 join 到 `vmware_host_info`。 |
| `vmware_vm_cpu_corecount` | `vmmo`, `vm`, `hostmo`, `vcenter` | 配置的 vCPU 数。 |
| `vmware_vm_mem_capacity_bytes` | `vmmo`, `vm`, `hostmo`, `vcenter` | 配置的内存（字节）。 |
| `vmware_vm_datastore_capacity_used_bytes` | `vmmo`, `vm`, `vcenter`, `dsmo` | 该 VM 在给定数据存储上占用的存储——磁盘、日志、快照和配置文件。每个 VM/数据存储对一条序列，所以一个 VM 在三个数据存储上有磁盘就会产生三条。 |
| `vmware_vm_snapshot_info` | `vmmo`, `vm`, `vcenter`, `name` | 每个快照一条序列；`name` 是快照名。**值是创建时间的 Unix 时间戳**，不是 `1`——所以 `time() - vmware_vm_snapshot_info` 就是快照存活时间，这是通常要设告警的内容。 |

```promql
# 快照超过 7 天
(time() - vmware_vm_snapshot_info) > 7 * 86400
```

## 数据存储

采集器：`datastore`（默认启用）。

来自 `Summary` 的静态清单指标：

| 指标 | 标签 | 含义 |
|------|------|------|
| `vmware_datastore_accessible` | `dsmo`, `ds`, `vcenter` | `1` 表示数据存储可达，`0` 表示不可达。 |
| `vmware_datastore_capacity_bytes` | `dsmo`, `ds`, `vcenter` | 数据存储总容量（字节）。 |
| `vmware_datastore_free_bytes` | `dsmo`, `ds`, `vcenter` | 可用空间（字节）。 |
| `vmware_datastore_info` | `dsmo`, `ds`, `type`, `pfinstance`, `foldermo`, `vcenter` | 数据存储元数据：`type`（VMFS、NFS、vSAN 等）。`pfinstance` 是数据存储 URL 去掉 `ds://`、`/vmfs/volumes/` 等前缀后的结果——性能计数器用它作为实例名来做 join。 |

性能计数器（`disk.provisioned.latest`、`disk.used.latest`）：

| 指标 | 标签 | 含义 |
|------|------|------|
| `vmware_datastore_disk_provisioned_bytes` | `vcenter`, `ds`, `dsmo` | 数据存储上已置备（已分配）的空间（字节）。从 `disk.provisioned.latest` 映射而来——原始的 `.latest` rollup 后缀已被剥离。 |
| `vmware_datastore_disk_used_bytes` | `vcenter`, `ds`, `dsmo` | 数据存储上实际已使用的空间（字节）。 |

两者都是来自 `kiloBytes` 源的计数器，所以值为 `(原始 KiB) × 1024`。它们**不分实例**
（没有 `pfinstance` 标签）。

## 资源池

采集器：`resourcepool`（默认启用）。

每个资源池一条序列。CPU 指标以赫兹计，内存以字节计。

| 指标 | 标签 | 含义 |
|------|------|------|
| `vmware_resourcepool_info` | `rpmo`, `rp`, `parentmo`, `ownermo`, `vcenter`, (`synthetic`) | 资源池的身份标识，带父池（`parentmo`）和所属集群/计算资源（`ownermo`）。两个 Desc 共享同一个指标名——`synthetic="true"` 变体用于 ESXi 暴露的伪资源池。它们必须是独立的 Desc，因为标签集是 Desc 的一部分；同一个 Desc 不能有时带 `synthetic` 有时不带，否则 `client_golang` 会 panic。 |
| `vmware_resourcepool_cpu_limit_hertz` | `rpmo`, `rp`, `vcenter` | 配置的 CPU 上限。不设上限时不发出；请检查 `cpu_limited`。 |
| `vmware_resourcepool_cpu_limited` | `rpmo`, `rp`, `vcenter` | `1` 表示设置了 CPU 上限，`0` 表示无上限。 |
| `vmware_resourcepool_cpu_max_usage_hertz` | `rpmo`, `rp`, `vcenter` | 资源池能达到的最大 CPU 用量（"天花板"）。 |
| `vmware_resourcepool_cpu_reservation_hertz` | `rpmo`, `rp`, `vcenter` | 配置的 CPU 预留。 |
| `vmware_resourcepool_cpu_reservation_used_hertz` | `rpmo`, `rp`, `vcenter` | 所有后代已消耗的 CPU 预留。 |
| `vmware_resourcepool_cpu_shares` | `rpmo`, `rp`, `level`, `vcenter` | CPU 份额。`level` 为 `low`、`normal`、`high` 或 `custom`。 |
| `vmware_resourcepool_cpu_unreserved_hertz` | `rpmo`, `rp`, `vcenter` | 尚可供 VM 预留的 CPU。 |
| `vmware_resourcepool_cpu_usage_hertz` | `rpmo`, `rp`, `vcenter` | 资源池及其后代的当前 CPU 用量。 |
| `vmware_resourcepool_mem_limit_bytes` | `rpmo`, `rp`, `vcenter` | 配置的内存上限。不设上限时不发出；请检查 `mem_limited`。 |
| `vmware_resourcepool_mem_limited` | `rpmo`, `rp`, `vcenter` | `1` 表示设置了内存上限，`0` 表示无上限。 |
| `vmware_resourcepool_mem_max_usage_bytes` | `rpmo`, `rp`, `vcenter` | 资源池能达到的最大内存用量。 |
| `vmware_resourcepool_mem_reservation_bytes` | `rpmo`, `rp`, `vcenter` | 配置的内存预留。 |
| `vmware_resourcepool_mem_reservation_used_bytes` | `rpmo`, `rp`, `vcenter` | 所有后代已消耗的内存预留。 |
| `vmware_resourcepool_mem_shares` | `rpmo`, `rp`, `level`, `vcenter` | 内存份额。`level` 为 `low`、`normal`、`high` 或 `custom`。 |
| `vmware_resourcepool_mem_unreserved_bytes` | `rpmo`, `rp`, `vcenter` | 尚可供 VM 预留的内存。 |
| `vmware_resourcepool_mem_usage_bytes` | `rpmo`, `rp`, `vcenter` | 资源池及其后代的当前内存用量。 |
| `vmware_resourcepool_overall_status` | `rpmo`, `rp`, `status`, `vcenter` | vSphere 报告的整体状态。`status` 为 `gray`、`green`、`yellow` 或 `red`。 |
| `vmware_resourcepool_vm` | `rpmo`, `rp`, `vmmo`, `vcenter` | 资源池到 VM 的映射，每个 VM 一条序列。 |

## vSAN

采集器：`vsan`（默认禁用）。需要 `--collector.vsan`。

来自 vSAN API 的集群级容量、磁盘、健康状态和同步指标。`vmwcluster` 标签与
`vmware_cluster_info` 一致，可按此 join。

| 指标 | 标签 | 含义 |
|------|------|------|
| `vmware_vsan_enabled` | `cmo`, `vmwcluster`, `vcenter` | `1` 表示集群上启用了 vSAN，`0` 表示未启用。每个集群都会发出，所以 `0` 能区分"vSAN 关闭"和"vsan 采集器未运行"。 |
| `vmware_vsan_capacity_bytes` | `cmo`, `vmwcluster`, `vcenter` | vSAN 数据存储总容量（字节）。 |
| `vmware_vsan_capacity_free_bytes` | `cmo`, `vmwcluster`, `vcenter` | vSAN 可用容量。来自 API 原始值。 |
| `vmware_vsan_capacity_used_bytes` | `cmo`, `vmwcluster`, `vcenter` | vSAN 已用容量。由 `capacity - free` 推导得出——API 不直接报告已用值。 |
| `vmware_vsan_dedup_enabled` | `cmo`, `vmwcluster`, `vcenter` | `1` 表示启用了去重和压缩，`0` 表示未启用。 |
| `vmware_vsan_disk_capacity_bytes` | `cmo`, `vmwcluster`, `host`, `device`, `vcenter` | vSAN 物理磁盘容量。 |
| `vmware_vsan_disk_capacity_used_bytes` | `cmo`, `vmwcluster`, `host`, `device`, `vcenter` | vSAN 物理磁盘已用容量。 |
| `vmware_vsan_disk_health` | `cmo`, `vmwcluster`, `host`, `device`, `uuid`, `state`, `vcenter` | vSAN 磁盘健康状态以标签形式呈现。`state` 是概要健康值。 |
| `vmware_vsan_health_status` | `cmo`, `vmwcluster`, `status`, `vcenter` | vSAN 集群健康状态。`status` 为 `green`、`yellow`、`red` 或 `unknown`。 |
| `vmware_vsan_resync_bytes` | `cmo`, `vmwcluster`, `vcenter` | 尚待同步的数据量（字节）。零表示没有正在进行的同步。需要 vSphere API 6.7+。 |
| `vmware_vsan_resync_objects` | `cmo`, `vmwcluster`, `vcenter` | 当前正在同步的对象数。零表示没有正在进行的同步。 |
| `vmware_vsan_resync_recovery_seconds` | `cmo`, `vmwcluster`, `vcenter` | 预估的同步完成时间。零表示没有正在进行的同步。 |

### vSAN 性能

采集器：`vsan.perf`（默认禁用）。需要 `--collector.vsan.perf`，且集群上必须启用
vSAN 性能服务（vSphere 中默认关闭，关闭时 vCenter 返回空数据而非错误）。

指标命名为 `vmware_vsan_perf_<label>`，其中 `<label>` 来自一个固定的白名单——
exporter 不会导出 vSAN 提供的所有计数器。实体类型是**标签值，不是指标名的一部分**
，所以 `sum by (entity) (vmware_vsan_perf_iops_read)` 一行就能完成，不需要跨指标
名做 join。

| 指标 | 含义 |
|------|------|
| `vmware_vsan_perf_iops_read` | 读 IOPS。 |
| `vmware_vsan_perf_iops_write` | 写 IOPS。 |
| `vmware_vsan_perf_throughput_read` | 读吞吐量。 |
| `vmware_vsan_perf_throughput_write` | 写吞吐量。 |
| `vmware_vsan_perf_latency_avg_read` | 平均读延迟（domclient 侧命名）。 |
| `vmware_vsan_perf_latency_avg_write` | 平均写延迟（domclient 侧命名）。 |
| `vmware_vsan_perf_latency_read` | 读延迟（磁盘级命名）。 |
| `vmware_vsan_perf_latency_write` | 写延迟（磁盘级命名）。 |
| `vmware_vsan_perf_congestion` | vSAN 拥堵——一个在 vSphere 侧没有等效指标的瓶颈信号。 |
| `vmware_vsan_perf_oio` | 未完成 IO（Outstanding IO）。 |
| `vmware_vsan_perf_capacity` | 磁盘组级容量。不同于上面集群级的 `vmware_vsan_capacity_bytes`，那个是整集群的。 |
| `vmware_vsan_perf_capacity_used` | 磁盘组级已用容量。 |
| `vmware_vsan_perf_capacity_reserved` | 磁盘组级预留容量。 |
| `vmware_vsan_perf_rc_hit_rate` | 读缓存命中率。 |
| `vmware_vsan_perf_wb_free_pct` | 写缓冲区空闲百分比。 |

同一个指标名在不同实体类型下携带略有不同的语义——对 `cluster-domclient` 它是集群
总和，对 `capacity-disk` 它是单个磁盘的值。`entity` 标签用于区分。

| 标签 | 说明 |
|------|------|
| `cmo` | 集群 Managed Object ID——join 到 `vmware_cluster_info`。 |
| `vmwcluster` | 集群显示名。 |
| `entity` | vSAN 性能实体类型（如 `cluster-domclient`、`host-domclient`、`capacity-disk`）。 |
| `entityid` | 实体 UUID。vSAN 的 API 只返回 UUID，没有友好名。 |
| `vcenter` | 抓取目标。 |

两个 flag 可调节此采集器：

- `-vmware.vsan.interval`（默认 `300`）——查询窗口（秒）。vSAN 统计数据以 5 分钟
  粒度落盘，所以低于 300 的值只会反复获取同一个数据点，而不是更精细的细节。
- `-collector.vsan.perf.skip-verify`（默认 `false`）——直接按白名单查询，不向
  vCenter 询问它支持哪些实体类型。原因是 `VsanPerfGetSupportedEntityTypes` 不会报告
  所有可查询的实体类型，与之取交集会静默丢掉实际上能用的实体。

## ESXi CLI

这些采集器向每个 ESXi 主机单独发出 SOAP 调用，**默认禁用**。分别用
`--collector.esxcli.host.nic` 和 `--collector.esxcli.storage` 启用。

### `esxcli.host.nic`

| 指标 | 标签 | 含义 |
|------|------|------|
| `vmware_esxcli_host_nic_driver` | `mo`, `host`, `descr`, `driver`, `version`, `firmware` | 主机的 NIC 驱动信息。每个 NIC 一条序列。值恒为 `1`。 |

### `esxcli.storage`

| 指标 | 标签 | 含义 |
|------|------|------|
| `vmware_esxcli_storage_driver` | `mo`, `host`, `vendor`, `model`, `revision` | 存储设备驱动信息。每个设备一条序列。值恒为 `1`。 |

## 性能计数器

`host`、`vm` 和 `datastore` 采集器会发出从 vCenter Performance Manager（`PerfMgr`）
获取的动态性能计数器。这些计数器**不是**在 `descs.go` 中静态注册的——它们是在抓取
时由 vCenter 返回的计数器元数据动态生成的。

### 命名规则

计数器名由 `translatePerfCounter()`（perfnames.go）规范化：

| 原始名 | 规范化后 | 说明 |
|--------|----------|------|
| `cpu.usagemhz.average` | `vmware_host_cpu_usage_hertz` | `.average` 被剥离；`usagemhz` 改写为 `cpu_usage`；单位 `megaHertz` → `_hertz`（×1e6）。 |
| `cpu.ready.summation` | `vmware_host_cpu_ready_seconds_total` | `.summation` 被剥离；delta 转为 counter 加 `_total`；单位 `millisecond` → `_seconds`（×1e-3）。 |
| `net.bytesRx.average` | `vmware_host_net_receive_bytes_per_second` | `bytesRx` 改写为 `receive`；单位 `kiloBytesPerSecond` → `_bytes_per_second`（×1024）。 |
| `datastore.read.average` | `vmware_host_datastore_read_bytes_per_second` | 单位 `kiloBytesPerSecond` → `_bytes_per_second`（×1024）。 |
| `sys.uptime.latest` | `vmware_host_sys_uptime_seconds` | `.latest` 被剥离；单位 `second` → `_seconds`（×1）。 |
| `cpu.latency.average` | `vmware_host_cpu_latency_ratio` | 单位 `percent` → `_ratio`（÷10000）。 |
| `disk.provisioned.latest` | `vmware_datastore_disk_provisioned_bytes` | 单位 `kiloBytes` → `_bytes`（×1024）。 |

命名流水线如下：

1. **剥离 rollup 后缀**：`.average`、`.latest`、`.summation`、`.maximum`、`.minimum`。
2. **应用语义改写**：从 `perfNameOverrides` 查找（如 `bytesRx` → `receive`、
   `usagemhz` → `usage`）。
3. **追加单位后缀**：从 `perfUnitRules` 查找（如 `_bytes`、`_seconds`、`_ratio`）。
4. **Delta 计数器**：如果 `StatsType = delta`，追加 `_total` 并将类型设为 counter。

### 标签

所有性能指标 Desc 至少携带以下标签：

| 标签 | 说明 |
|------|------|
| `vcenter` | 抓取目标。 |
| `host` / `hostmo` | 实体名和 MOID——用于 `HostSystem` 计数器。 |
| `vm` / `vmmo` | 实体名和 MOID——用于 `VirtualMachine` 计数器。 |
| `ds` / `dsmo` | 实体名和 MOID——用于 `Datastore` 计数器。 |

当计数器是 instanced（分实例）的（例如每个 NIC 的 `net.bytesRx.average`）时，会
额外增加一个标签：

| 标签 | 含义 |
|------|------|
| `pfinstance` | vCenter 中的计数器实例名（如 `vmnic0`、`vmhba1`）。 |

标签顺序始终是 `vcenter`、`<entity_name>`、`<entity_moid>`，可选 `pfinstance`。

### `-metrics.legacy`

使用 `--metrics.legacy=true` 时，exporter 会对每个性能计数器**同时发出**规范化名
和原始名。原始名把点直接替换为下划线（如 `vmware_host_cpu_usage_average`）。
原始指标的值是**单位换算前**的原始值（但 delta 计数器的 bug 修复——求和而非求平均
——对两个名字都生效）。

### Delta 计数器

Delta（`StatsType = delta`）计数器在采样窗口内累积，导出为 Prometheus counter 并
加 `_total` 后缀。值是该窗口内所有样本的**和**，不是平均值。这是对原始实现（全部
求平均）的有意修正。

非 delta（`absolute`、`rate`）计数器在窗口内求平均，作为降噪手段。

### 各采集器的计数器清单

**`host`**（通用，不分实例）：
`cpu.usagemhz.average`, `cpu.demand.average`, `cpu.latency.average`,
`cpu.entitlement.latest`, `cpu.ready.summation`, `cpu.readiness.average`,
`cpu.costop.summation`, `cpu.maxlimited.summation`,
`mem.entitlement.average`, `mem.active.average`, `mem.shared.average`,
`mem.vmmemctl.average`, `mem.swapped.average`, `mem.consumed.average`,
`sys.uptime.latest`

**`host`**（分实例，每个 NIC / 每个数据存储）：
`net.bytesRx.average`, `net.bytesTx.average`, `net.errorsRx.summation`,
`net.errorsTx.summation`, `net.droppedRx.summation`, `net.droppedTx.summation`,
`datastore.read.average`, `datastore.write.average`,
`datastore.numberReadAveraged.average`, `datastore.numberWriteAveraged.average`,
`datastore.totalReadLatency.average`, `datastore.totalWriteLatency.average`

**`vm`**（通用，不分实例）：
与 `host` 通用计数器相同。

**`vm`**（分实例）：
`net.bytesRx.average`, `net.bytesTx.average`,
`datastore.read.average`, `datastore.write.average`,
`datastore.numberReadAveraged.average`, `datastore.numberWriteAveraged.average`,
`datastore.totalReadLatency.average`, `datastore.totalWriteLatency.average`

**`datastore`**（不分实例）：
`disk.provisioned.latest`, `disk.used.latest`

## 已弃用指标

以下静态指标有对应的改名版本。旧名仍会发出作为软弃用——将在未来版本中移除。此处只
列出已改名的指标；性能计数器的旧名受 `--metrics.legacy` 控制，不在此重复。

| 旧名 | 替代名 | 变更内容 |
|------|--------|----------|
| `vmware_host_cpu_capacity` | `vmware_host_cpu_capacity_hertz` | 单位写进名（MHz → hertz）。 |
| `vmware_host_cpu_capacity_mhz` | `vmware_host_cpu_capacity_hertz` | 单位写进名（MHz → hertz）。 |
| `vmware_host_mem_capacity` | `vmware_host_mem_capacity_bytes` | 单位写进名（MB → bytes）。 |
| `vmware_vm_mem_capacity` | `vmware_vm_mem_capacity_bytes` | 单位写进名（MB → bytes）。 |
| `vmware_vm_datastore_capacity_used` | `vmware_vm_datastore_capacity_used_bytes` | 单位写进名（bytes——旧名缺少后缀）。 |
| `vmware_datastore_capacity` | `vmware_datastore_capacity_bytes` | 单位写进名（bytes——旧名缺少后缀）。 |
| `vmware_datastore_free` | `vmware_datastore_free_bytes` | 单位写进名（bytes——旧名缺少后缀）。 |

弃用指标的标签集与替代品完全相同。数值也一致（旧名已使用正确单位，只是命名有歧义）。

## 通用标签

以下标签在多个指标中反复出现。MOID 代表"Managed Object ID"——vSphere 内部标识符，
在重命名后仍保持不变。

| 标签 | 含义 | 出现在 |
|------|------|--------|
| `vcenter` | 抓取目标（主机名或 IP）。 | 所有指标。 |
| `target` | 同 `vcenter`，但仅存在于 `vmware_target_info`。 | `target_info` |
| `dcmo` | 数据中心 MOID。 | `datacenter_info`, `folder_info`, `cluster_info`, `compute_info` |
| `dc` | 数据中心显示名。 | `datacenter_info` |
| `foldermo` | 文件夹 MOID。 | `folder_info`, `cluster_info`, `compute_info`, `datastore_info` |
| `cmo` | 集群或 ComputeResource 的 MOID。 | `cluster_info`, `cluster_datastore`, `compute_info`, `compute_datastore`, `host_info`, `vsan_*`, `vsan_perf_*` |
| `vmwcluster` | 集群显示名。 | `cluster_info`, `cluster_datastore`, `vsan_*`, `vsan_perf_*` |
| `hostmo` | 主机 MOID。 | `host_*`, `vm_*` |
| `host` | 主机显示名。 | `host_*`, `compute_info`, `compute_datastore`, `esxcli_*` |
| `vmmo` | 虚拟机 MOID。 | `vm_*`, `resourcepool_vm` |
| `vm` | 虚拟机显示名。 | `vm_*` |
| `dsmo` | 数据存储 MOID。 | `datastore_*`, `cluster_datastore`, `compute_datastore`, `vm_datastore_*` |
| `ds` | 数据存储显示名。 | `datastore_*` |
| `rpmo` | 资源池 MOID。 | `resourcepool_*` |
| `rp` | 资源池显示名。 | `resourcepool_*` |
| `synthetic` | `"true"` 表示该对象是仅在独立 ESXi 上存在的伪对象（API 为了保持与 vCenter 兼容的形状而创建的占位符）：`ha-datacenter`、`ha-compute-res`、`ha-root-pool`。**不**适用于 vCenter 的根资源池，那是真实对象。 | `datacenter_info`, `compute_info`, `resourcepool_info` |
| `mo` | 通用 MOID——用于 esxcli 采集器。 | `esxcli_*` |
| `pfinstance` | 性能计数器实例名（如 `vmnic0`）。 | 性能计数器（仅分实例时）。 |

你可以用 `{synthetic!="true"}` 过滤掉这些伪对象。它们是 vSphere 自动创建的占位符，
在清单中没有真实对应物。

## Joining on `_info` 指标

数值型指标只带 MOID，不带人类可读的名字和层级关系。`*_info` 指标（值恒为 1）存在的
意义就是补上这些**标签**，供你 join。

典型场景：主机的内存容量只有 `hostmo` 和 `host`，没有集群信息。要按集群汇总，需要
从 `vmware_host_info` 取 `cmo`，再从 `vmware_cluster_info` 取集群名：

```promql
# 第一步：给主机内存容量补上 cmo（集群 MOID）
vmware_host_mem_capacity_bytes
  * on (hostmo) group_left(cmo) vmware_host_info
```

`group_left(cmo)` 表示「保留左侧的全部标签，并从右侧额外带上 `cmo`」。
`on (hostmo)` 限定只用 `hostmo` 做匹配——不加 `on` 时 Prometheus 会要求两侧标签集
完全相同，而这里不同（左侧无 `cmo`，右侧无 `host` 之外的数值标签），匹配会直接失败。

第二步再换成集群名。此时左侧已经有 `hostmo`、`host` 等 `vmware_cluster_info` 不具备
的标签，所以**必须继续用 `on (cmo)` 显式限定匹配标签**：

```promql
# 按集群汇总主机内存容量
sum by (vmwcluster) (
  vmware_host_mem_capacity_bytes
    * on (hostmo) group_left(cmo) vmware_host_info
    * on (cmo)    group_left(vmwcluster) vmware_cluster_info
)
```

两点容易踩的坑：

- **乘法只是 join 的载体。** `_info` 的值恒为 1，所以乘完不改变左侧数值。用 `*`
  而不是 `+` 是惯例——`+` 会把 1 加进去，把数值弄错。
- **不是所有关联都需要 join。** 先查一下目标指标的标签集：例如
  `vmware_vsan_disk_health` 已经自带 `vmwcluster`，直接
  `sum by (vmwcluster) (...)` 即可，多做一次 join 只会增加出错面。
