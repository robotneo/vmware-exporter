# VMware Exporter 采集指标清单

**数据来源**：`a real /metrics endpoint dump`（真实 `/metrics` 端点导出）
**指标总数**：97（gauge 87 + counter 10）
**采集目标**：vCenter 192.0.2.10:443（版本 8.0.3）

---

## 一、指标分组概览

| 前缀 | 指标数 | 说明 |
|------|--------|------|
| `vmware_host_*` | 30 | 每台 ESXi 主机的资源（CPU/内存/网络/存储/硬件） |
| `vmware_vm_*` | 28 | 每台虚拟机的资源（CPU/内存/网络/存储/快照） |
| `vmware_resourcepool_*` | 19 | 资源池的 CPU/内存配额、份额、使用率 |
| `vmware_datastore_*` | 6 | 数据存储的容量、已用空间、访问状态 |
| （其他·顶层） | 5 | vCenter/Target/DataCenter/Folder 信息和连通性 |
| `vmware_scrape_*` | 4 | 采集器自身的耗时、成功率、错误数 |
| `vmware_exporter_*` | 3 | Exporter 自身构建信息、重载状态 |
| `vmware_cluster_*` | 2 | 集群基本信息、集群到数据存储映射 |
| **合计** | **97** | |

---

## 二、各分组指标详细含义

### 2.1 顶层信息（5 指标）

| 指标 | 类型 | 含义 |
|------|------|------|
| `vmware_vcenter_info` | gauge | vCenter 本身的版本和构建号 |
| `vmware_target_info` | gauge | 采集目标的类型（vcenter/esxi）和版本 |
| `vmware_datacenter_info` | gauge | 数据中心基本信息，供父引用关联使用 |
| `vmware_folder_info` | gauge | 文件夹基本信息，供父引用关联使用 |
| `vmware_up` | gauge | 能否登录 vCenter/ESXi，0=失败，1=成功 |

### 2.2 Cluster（集群，2 指标）

| 指标 | 类型 | 含义 |
|------|------|------|
| `vmware_cluster_info` | gauge | 集群基本信息（名称、所属文件夹），供父引用关联 |
| `vmware_cluster_datastore` | gauge | 集群到数据存储的映射关系 |

### 2.3 Host（ESXi 主机，30 指标）

#### CPU（9 指标）

| 指标 | 类型 | 含义 | 单位 |
|------|------|------|------|
| `vmware_host_cpu_capacity_hertz` | gauge | 单核频率，乘以 corecount 得总容量 | Hz |
| `vmware_host_cpu_corecount` | gauge | 物理 CPU 核数 | 个 |
| `vmware_host_cpu_threadcount` | gauge | CPU 线程数（核数 × SMT 宽度） | 个 |
| `vmware_host_cpu_usage_hertz` | gauge | CPU 使用率 | MHz |
| `vmware_host_cpu_demand_hertz` | gauge | 虚拟机对 CPU 的需求量（无争用时） | MHz |
| `vmware_host_cpu_latency_ratio` | gauge | 因争用物理 CPU 而无法运行的时间百分比 | % |
| `vmware_host_cpu_readiness_ratio` | gauge | 虚拟机就绪但未调度到物理 CPU 的时间百分比 | % |
| `vmware_host_cpu_ready_seconds_total` | counter | 虚拟机就绪但不能运行的累计时间 | ms |
| `vmware_host_cpu_costop_seconds_total` | counter | 因协同调度约束不能运行的累计时间 | ms |

#### 内存（5 指标）

| 指标 | 类型 | 含义 | 单位 |
|------|------|------|------|
| `vmware_host_mem_capacity_bytes` | gauge | 物理内存总量 | bytes |
| `vmware_host_mem_consumed_bytes` | gauge | 被虚拟机消耗的物理内存 | KB |
| `vmware_host_mem_active_bytes` | gauge | 被活跃读写的内存 | KB |
| `vmware_host_mem_balloon_bytes` | gauge | 通过 balloon 驱动回收的内存 | KB |
| `vmware_host_mem_shared_bytes` | gauge | 虚拟机间共享的内存 | KB |

#### 网络（6 指标）

| 指标 | 类型 | 含义 | 单位 |
|------|------|------|------|
| `vmware_host_net_receive_bytes_per_second` | gauge | 每秒接收字节数 | KBps |
| `vmware_host_net_receive_dropped_total` | counter | 接收丢弃累计数 | 个 |
| `vmware_host_net_receive_errors_total` | counter | 接收错误累计数 | 个 |
| `vmware_host_net_transmit_bytes_per_second` | gauge | 每秒发送字节数 | KBps |
| `vmware_host_net_transmit_dropped_total` | counter | 发送丢弃累计数 | 个 |
| `vmware_host_net_transmit_errors_total` | counter | 发送错误累计数 | 个 |

#### 数据存储（6 指标）

| 指标 | 类型 | 含义 | 单位 |
|------|------|------|------|
| `vmware_host_datastore_read_bytes_per_second` | gauge | 从数据存储读取速率 | KBps |
| `vmware_host_datastore_write_bytes_per_second` | gauge | 写入数据存储速率 | KBps |
| `vmware_host_datastore_read_latency_seconds` | gauge | 读取平均延迟 | ms |
| `vmware_host_datastore_write_latency_seconds` | gauge | 写入平均延迟 | ms |
| `vmware_host_datastore_read_operations` | gauge | 每秒读命令数 | 个 |
| `vmware_host_datastore_write_operations` | gauge | 每秒写命令数 | 个 |

#### 信息类（3 指标）

| 指标 | 类型 | 含义 |
|------|------|------|
| `vmware_host_info` | gauge | 主机基本归属信息（所属集群、IP） |
| `vmware_host_hardware_info` | gauge | 硬件信息（厂商、型号、CPU 型号） |
| `vmware_host_software_info` | gauge | 软件版本（ESXi 版本、构建号） |

#### 系统（1 指标）

| 指标 | 类型 | 含义 | 单位 |
|------|------|------|------|
| `vmware_host_sys_uptime_seconds` | gauge | 主机已运行时间 | s |

### 2.4 VM（虚拟机，28 指标）

#### CPU（9 指标）

| 指标 | 类型 | 含义 | 单位 |
|------|------|------|------|
| `vmware_vm_cpu_corecount` | gauge | 配置的 vCPU 数量 | 个 |
| `vmware_vm_cpu_usage_hertz` | gauge | CPU 使用率 | MHz |
| `vmware_vm_cpu_demand_hertz` | gauge | CPU 需求量 | MHz |
| `vmware_vm_cpu_entitlement_hertz` | gauge | ESXi 调度器分配的 CPU 资源 | MHz |
| `vmware_vm_cpu_latency_ratio` | gauge | 因争用无法运行的时间百分比 | % |
| `vmware_vm_cpu_readiness_ratio` | gauge | 就绪但未调度的时间百分比 | % |
| `vmware_vm_cpu_ready_seconds_total` | counter | 就绪但不能运行的累计时间 | ms |
| `vmware_vm_cpu_costop_seconds_total` | counter | 协同调度约束的累计时间 | ms |
| `vmware_vm_cpu_maxlimited_seconds_total` | counter | 因达到 CPU 上限限制不能运行的累计时间 | ms |

#### 内存（7 指标）

| 指标 | 类型 | 含义 | 单位 |
|------|------|------|------|
| `vmware_vm_mem_capacity_bytes` | gauge | 配置的虚拟内存总量 | bytes |
| `vmware_vm_mem_consumed_bytes` | gauge | 消耗的宿主机物理内存 | KB |
| `vmware_vm_mem_active_bytes` | gauge | 被活跃读写的内存 | KB |
| `vmware_vm_mem_balloon_bytes` | gauge | 通过 balloon 驱动回收的内存 | KB |
| `vmware_vm_mem_entitlement_bytes` | gauge | 虚拟机应得的内存量（由 ESXi 决定） | KB |
| `vmware_vm_mem_shared_bytes` | gauge | 跨虚拟机共享的内存 | KB |
| `vmware_vm_mem_swapped_bytes` | gauge | 被交换到磁盘的内存 | KB |

#### 网络（2 指标）

| 指标 | 类型 | 含义 | 单位 |
|------|------|------|------|
| `vmware_vm_net_receive_bytes_per_second` | gauge | 每秒接收字节数 | KBps |
| `vmware_vm_net_transmit_bytes_per_second` | gauge | 每秒发送字节数 | KBps |

#### 数据存储（7 指标）

| 指标 | 类型 | 含义 | 单位 |
|------|------|------|------|
| `vmware_vm_datastore_capacity_used_bytes` | gauge | 虚拟机在数据存储上占用的空间（含磁盘、日志、快照、配置文件） | bytes |
| `vmware_vm_datastore_read_bytes_per_second` | gauge | 读取速率 | KBps |
| `vmware_vm_datastore_write_bytes_per_second` | gauge | 写入速率 | KBps |
| `vmware_vm_datastore_read_latency_seconds` | gauge | 读取延迟 | ms |
| `vmware_vm_datastore_write_latency_seconds` | gauge | 写入延迟 | ms |
| `vmware_vm_datastore_read_operations` | gauge | 每秒读命令数 | 个 |
| `vmware_vm_datastore_write_operations` | gauge | 每秒写命令数 | 个 |

#### 信息类（2 指标）

| 指标 | 类型 | 含义 |
|------|------|------|
| `vmware_vm_info` | gauge | 虚拟机基本归属信息（名称、所在宿主机） |
| `vmware_vm_snapshot_info` | gauge | 快照创建时间（Unix 时间戳），标签含快照名称 |

#### 系统（1 指标）

| 指标 | 类型 | 含义 | 单位 |
|------|------|------|------|
| `vmware_vm_sys_uptime_seconds` | gauge | 虚拟机运行时间 | s |

### 2.5 ResourcePool（资源池，19 指标）

#### CPU（8 指标）

| 指标 | 类型 | 含义 |
|------|------|------|
| `vmware_resourcepool_cpu_limit_hertz` | gauge | 配置的 CPU 上限（无线限时不出此指标） |
| `vmware_resourcepool_cpu_limited` | gauge | 是否有 CPU 上限（1=有，0=无限） |
| `vmware_resourcepool_cpu_max_usage_hertz` | gauge | 最大可用 CPU |
| `vmware_resourcepool_cpu_reservation_hertz` | gauge | 配置的 CPU 预留 |
| `vmware_resourcepool_cpu_reservation_used_hertz` | gauge | 已消耗的 CPU 预留 |
| `vmware_resourcepool_cpu_shares` | gauge | 配置的 CPU 份额（带 level 标签） |
| `vmware_resourcepool_cpu_unreserved_hertz` | gauge | 仍可预留的 CPU |
| `vmware_resourcepool_cpu_usage_hertz` | gauge | 当前 CPU 使用量 |

#### 内存（8 指标）

| 指标 | 类型 | 含义 |
|------|------|------|
| `vmware_resourcepool_mem_limit_bytes` | gauge | 配置的内存上限 |
| `vmware_resourcepool_mem_limited` | gauge | 是否有内存上限 |
| `vmware_resourcepool_mem_max_usage_bytes` | gauge | 最大可用内存 |
| `vmware_resourcepool_mem_reservation_bytes` | gauge | 配置的内存预留 |
| `vmware_resourcepool_mem_reservation_used_bytes` | gauge | 已消耗的内存预留 |
| `vmware_resourcepool_mem_shares` | gauge | 配置的内存份额（带 level 标签） |
| `vmware_resourcepool_mem_unreserved_bytes` | gauge | 仍可预留的内存 |
| `vmware_resourcepool_mem_usage_bytes` | gauge | 当前内存使用量 |

#### 其他（3 指标）

| 指标 | 类型 | 含义 |
|------|------|------|
| `vmware_resourcepool_info` | gauge | 资源池基本信息（名称、父/所有者引用） |
| `vmware_resourcepool_overall_status` | gauge | 整体健康状态（status 标签：gray/green/yellow/red） |
| `vmware_resourcepool_vm` | gauge | 资源池到虚拟机的映射关系 |

### 2.6 Datastore（数据存储，6 指标）

| 指标 | 类型 | 含义 | 单位 |
|------|------|------|------|
| `vmware_datastore_info` | gauge | 数据存储基本信息（名称、类型 VMFS/vsan） |
| `vmware_datastore_accessible` | gauge | 是否可访问（1=可访问） |
| `vmware_datastore_capacity_bytes` | gauge | 总容量 | bytes |
| `vmware_datastore_free_bytes` | gauge | 可用空间 | bytes |
| `vmware_datastore_disk_provisioned_bytes` | gauge | 已预分配的空间大小 | KB |
| `vmware_datastore_disk_used_bytes` | gauge | 实际已使用的空间 | KB |

### 2.7 Scrape（采集器自监控，4 指标）

| 指标 | 类型 | 含义 |
|------|------|------|
| `vmware_scrape_collector_success` | gauge | 子采集器是否成功（1=成功，0=失败），按 collector 标签区分 |
| `vmware_scrape_collector_duration_seconds` | gauge | 子采集器耗时 |
| `vmware_scrape_duration_seconds` | gauge | 总采集耗时（含登录/登出） |
| `vmware_scrape_errors_total` | counter | 采集错误总数，按 collector 标签区分，login 表示认证失败 |

### 2.8 Exporter（Exporter 自身，3 指标）

| 指标 | 类型 | 含义 |
|------|------|------|
| `vmware_exporter_build_info` | gauge | 构建信息（版本/修订/分支/Go 版本/OS/架构），值恒为 1 |
| `vmware_exporter_config_last_reload_successful` | gauge | 最近一次配置重载是否成功（1=成功，0=失败） |
| `vmware_exporter_config_last_reload_success_timestamp_seconds` | gauge | 最近一次成功重载的时间戳 |

---

## 三、特殊标签含义

### 3.1 全局通用标签

| 标签 | 含义 | 示例值 |
|------|------|--------|
| `vcenter` | 采集目标的地址（host:port），所有 vCenter 级别指标都带 | `192.0.2.10:443` |

### 3.2 `*mo` 家族——Managed Object 引用

这些标签携带 vSphere 内部的 **Managed Object ID（MOID）**，用于在 PromQL 中通过 `*_info` 指标做 JOIN 关联。

| 标签 | 含义 | 示例值 | 说明 |
|------|------|--------|------|
| `hostmo` | 主机的 MOID | `host-12395`、`host-4091` | 唯一标识一台 ESXi 主机 |
| `vmmo` | 虚拟机的 MOID | `vm-12742`、`vm-32218` | 唯一标识一台虚拟机 |
| `rpmo` | 资源池的 MOID | `resgroup-1007` | 唯一标识一个资源池 |
| `dsmo` | 数据存储的 MOID | `datastore-12913`、`datastore-1022` | 唯一标识一个数据存储 |
| `cmo` | 集群的 MOID | `domain-c1006` | 唯一标识一个集群 |
| `dcmo` | 数据中心的 MOID | `datacenter-1001` | 唯一标识一个数据中心 |
| `foldermo` | 文件夹的 MOID | `group-s1004`、`group-h1003` | 唯一标识一个文件夹 |
| `parentmo` | 资源池的父对象 MOID | `domain-c1006` | 资源池的父容器 |
| `ownermo` | 资源池的所有者 MOID | `domain-c1006` | 资源池的拥有者 |

**典型 JOIN 用法**：
```
# 查询某台 ESXi 的硬件型号
vmware_host_hardware_info{hostmo="host-12395"}
  + JOIN on hostmo
vmware_host_cpu_usage_hertz{hostmo="host-12395"}

# 查询某台虚拟机所在的宿主机
vmware_vm_info{vmmo="vm-12742"}  → hostmo 标签指明宿主机
```

### 3.3 身份标识标签

| 标签 | 含义 | 示例值 |
|------|------|--------|
| `host` | ESXi 主机管理 IP 地址 | `192.0.2.20`、`192.0.2.21` |
| `vm` | 虚拟机显示名称（常含业务标签+人名） | `192.0.2.30-f2e-s3-db01` |
| `ds` | 数据存储名称 | `43.1-data`、`vsan-lun091` |
| `rp` | 资源池名称 | `Resources`（默认根池） |
| `dc` | 数据中心名称 | `Datacenter` |
| `vmwcluster` | 集群名称（注意不是 `cluster`，后者是 Prometheus 保留词） | `vSAN-A` |

### 3.4 性能实例标签

| 标签 | 含义 | 示例值 |
|------|------|--------|
| `pfinstance` | 性能提供者实例 UUID，标识具体网卡、磁盘、数据存储链路 | `5da56967-c9967720-510b-1866daf2a076` |

> 带 `pfinstance` 的指标会按每块网卡/磁盘/存储路径分别产生一条数据。

### 3.5 枚举/状态标签

| 标签 | 含义 | 取值 |
|------|------|--------|
| `level` | vSphere 分配级别（份额相关） | `low`、`normal`、`high`、`custom` |
| `status` | 资源池健康状态颜色 | `gray`、`green`、`yellow`、`red` |
| `type` | 数据存储类型或采集目标类型 | `VMFS`、`vsan` / `vcenter`、`esxi` |

### 3.6 采集器标签

| 标签 | 含义 | 示例值 |
|------|------|--------|
| `collector` | 子采集器名称 | `all_collectors`、`cluster`、`host`、`vm`、`datastore`、`resourcepool` |
| `target` | 采集目标标识 | `192.0.2.10:443` |

### 3.7 构建信息标签（仅 `vmware_exporter_build_info`）

| 标签 | 含义 | 示例值 |
|------|------|--------|
| `version` | Exporter 版本 | `v0.1.18-beta.1-58-gb0b084e` |
| `revision` | Git 提交哈希 | `b0b084ef506a51a54640c011529da72e853f3818` |
| `branch` | Git 分支 | `dev` |
| `goversion` | Go 编译器版本 | `go1.26.6` |
| `goos` | 目标操作系统 | `linux` |
| `goarch` | 目标架构 | `amd64` |
| `tags` | 构建标签 | `unknown` |

### 3.8 快照标签（仅 `vmware_vm_snapshot_info`）

| 标签 | 含义 | 示例值 |
|------|------|--------|
| `name` | 快照名称（中文） | `虚拟机快照 2026/3/16 19:32:43` |

---

## 四、使用建议

1. **关联查询**：通过 `*mo` 标签在 `*_info` 指标和性能指标之间做 JOIN
2. **告警阈值**：`vmware_scrape_collector_success=0` 表示子采集器失败；`vmware_up=0` 表示目标不可达
3. **资源池限值**：`cpu_limited=0` / `mem_limited=0` 时，对应的 `*_limit_*` 指标不会出现
4. **实际数据特征**：硬件为 Dell PowerEdge 系列（R530/R720/R730/R740），ESXi 版本 7.0.3 / 8.0.3，VM 命名习惯为 `IP-业务模块-人名`，快照名为中文时间戳格式，数据存储包含 VMFS 和 vsan 两种类型