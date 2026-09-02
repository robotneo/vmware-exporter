## vmware-exporter
这是一个简单的 Prometheus 导出器，可从 vCenter 收集各种指标。

## 如何使用

当使用 /metcis 路径时，Exporter 会采集单个 vCenter 主机。 使用 /probe 可以抓取多个 vCenter 主机，但这些 vCenter 主机必须共享凭据（用户名和密码必须相同）。

### 二进制运行

下载二进制包，解压包，把二进制文件 vmware-exporter 放入到 `/usr/bin` 目录下，然后新建目录 `/etc/vmware-exporter/`

```bash
wget https://github.com/robotneo/vmware-exporter/releases/download/v0.1.12/vmware-exporter-v0.1.12-linux-amd64.tar.gz

mkdir -pv /opt/vmware
mkdir -pv /etc/vmware-exporter/

tar -zxvf vmware-exporter-v0.1.12-linux-amd64.tar.gz -C /opt/vmware
cd /opt/vmware
mv vmware-exporter /usr/bin

# 把 vmware.conf 文件放入 /etc/vmware-exporter/ 目录中，vmware.conf 通过命令行选项加载参数
ARGS="-vmware.username=administrator@vsphere.local -vmware.password=<VCENTER_PASSWORD> -vmware.vcenter=<VCENTER_HOST>:443 -vmware.insecureTLS"
# 更多参数 可通过空格进行添加 

# 复制项目中 system 目录下的 vmware-exporter.service 文件到 /etc/systemd/system/ 目录中，实现 systemd 管理 vmware-exporter 服务。
```

system 目录下的 vmware-exporter.service 文件需要放入 `/etc/systemd/system/` 目录中，并可通过 systemctl 命令进行管理。

### Docker 运行

- 前提：安装 Docker
- Docker 运行

```bash
docker run -d \
  --name vmware-exporter \
  --hostname vmware-exporter \
  -p 9169:9169 \
  meisite/vmware-exporter:latest \
  -vmware.username=administrator@vsphere.local \
  -vmware.password=<VCENTER_PASSWORD> \
  -vmware.vcenter=<VCENTER_HOST> \
  -vmware.granularity=20 \
  -vmware.interval=20 \
  -vmware.insecureTLS
```

说明下：-vmware.granularity 和 -vmware.interval 不能低于 20s ，这是由于vCenter的限制。

任务抓取配置：

```yaml
scrape_configs:
  - job_name: "vmware-exporter"
    scrape_interval: 20s
    scrape_timeout: 15s
    metrics_path: /metrics
    # metrics_path: /probe
    static_configs:
      - targets:
        - 'vcenter.example.com'
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: exporter.example.com:9169
```

多凭证支持：

- 可以在 `/probe` 路径上使用 `target` 参数指定要抓取的 vCenter 主机。
- 所有指定的 vCenter 主机可不共享相同的用户名和密码。

```yaml
scrape_configs:
  - job_name: 'vmware-vcenter'
    scrape_interval: 60s
    scrape_timeout: 55s
    metrics_path: /probe
    file_sd_configs:
      - files:
        - /etc/victoriametrics/vmagent/vmware_targets.yml
        # refresh_interval: 5m
    # 默认参数（可选）
    params:
      schema: ['https']
      insecure: ['true']
      collect[]: ['all'] # 启用所有 collectors
      # collect[]: ['datacenter', 'host', 'vm']  # 只监控主要指标
    relabel_configs:
      # 从标签中提取 username 并设置为 URL 参数
      - source_labels: [__meta_username]
        target_label: __param_username
      # 从标签中提取 password 并设置为 URL 参数
      - source_labels: [__meta_password]
        target_label: __param_password
      # 可选：从标签中提取 schema
      - source_labels: [__meta_schema]
        target_label: __param_schema
      # 可选：从标签中提取 insecure
      - source_labels: [__meta_insecure]
        target_label: __param_insecure
      # 将 target 设置为 vCenter 地址
      - source_labels: [__address__]
        target_label: __param_target
      # 设置 instance 标签
      - source_labels: [__param_target]
        target_label: instance
      # 将实际抓取地址设置为 exporter 地址
      - target_label: __address__
        replacement: 172.17.40.25:9169
      # 保留其他有用的标签（移除 __meta_ 前缀）
      - source_labels: [__meta_env]
        target_label: env
      - source_labels: [__meta_datacenter]
        target_label: datacenter
```

vmware_targets.yml 示例：

```yaml
- targets:
  - vcenter1.example.com
  labels:
    __meta_username: 'administrator@vsphere.local'
    __meta_password: '<VCENTER1_PASSWORD>'
    __meta_schema: 'https'
    __meta_insecure: 'true'
    __meta_env: 'prod'  # 可删除
    __meta_datacenter: 'dc01' # 可删除

- targets:
  - vcenter2.example.com
  labels:
    __meta_username: 'administrator@vsphere.local'
    __meta_password: '<VCENTER2_PASSWORD>'
    __meta_schema: 'https'
    __meta_insecure: 'true'
    __meta_env: 'prod'  # 可删除
    __meta_datacenter: 'dc02' # 可删除
```

## 三种采集模式

| 模式 | 端点 | 凭证来源 | 适用场景 |
| :--- | :--- | :--- | :--- |
| **单 vCenter** | `/metrics` | 启动参数（全局） | 只有一套 vCenter，最简部署 |
| **多 vCenter** | `/probe?target=...` | 请求参数或 Basic Auth（每 target 独立） | 多套 vCenter，凭证各不相同 |
| **单台 ESXi 直连** | 两者皆可 | 同上 | 无 vCenter 的独立主机，或需绕过 vCenter 直采 |

**目标类型自动识别**：登录后读取 `ServiceContent.About.ApiType`
（`VirtualCenter` / `HostAgent`）判定，无需任何额外配置，也**不需要单独的端点**。
Prometheus 侧不必为 ESXi 单写一份 `scrape_config`。

识别结果通过指标暴露，可用于 dashboard 条件渲染与告警分流：

```
vmware_target_info{target="10.0.0.5:443", type="esxi", version="7.0.3", build="21930508"} 1
```

### ESXi 直连的启动示例

```bash
./vmware-exporter \
  -vmware.vcenter="10.0.0.5:443" \
  -vmware.username="root" \
  -vmware.password="your_password" \
  -vmware.insecureTLS \
  -http.address=":9169"
```

Prometheus 侧混合采集 vCenter 与 ESXi 的配置：

```yaml
scrape_configs:
  - job_name: vmware
    metrics_path: /probe
    static_configs:
      - targets:
          - 10.0.0.1:443   # vCenter
          - 10.0.0.5:443   # 独立 ESXi，无需特殊处理
    params:
      insecure: ["true"]
    basic_auth:
      username: readonly@vsphere.local
      password: your_password
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: exporter-host:9169
```

> 若 vCenter 与 ESXi 的凭证不同，拆成两个 job 分别配置 `basic_auth`。

### ESXi 模式的能力边界

这些差异源自 vSphere 的对象模型，不是 exporter 的限制：

| 能力 | vCenter | ESXi 直连 | 说明 |
| :--- | :--- | :--- | :--- |
| Datacenter / Cluster | 真实数据 | **伪对象** | ESXi 只有隐式的 `ha-datacenter` / `ha-compute-res`，相关指标带 `synthetic="true"` |
| Host / VM 指标 | 完整 | 完整 | 无差异 |
| Datastore 容量 | 完整 | 完整 | 无差异 |
| Datastore 性能计数器 | 完整 | 受限 | `disk.provisioned.latest` 等计数器依赖 vCenter 的历史汇总，ESXi 不做汇总 |
| 采样间隔 | 可选实时或 5 分钟汇总 | **仅实时** | ESXi 不聚合历史统计，`-vmware.interval` 的期望值会被服务端 `RefreshRate` 覆盖 |
| esxcli 采集 | 经 vCenter 转发 | 直连 | 走 SOAP 的 `vim.EsxCLI.*` 接口，**不是 SSH** |

**关于 `synthetic` label**：ESXi 上仍然输出 `vmware_datacenter_info` 与
`vmware_compute_info`，是为了让依赖 `dcmo` / `cmo` 关联的 dashboard 查询不断链；
同时打上 `synthetic="true"`，让「这不是真实数据中心」在指标层面可见。
只想统计真实数据中心时按 `synthetic!="true"` 过滤即可。

---

## 参数设置

可以通过命令行选项、环境变量、yaml 配置文件或三者的组合来配置输出程序，设置的环境变量将被配置文件的内容覆盖，然后被启动时设置的任何命令行选项覆盖，可用的选项如下：

### 1. 目标连接配置
| 参数 | 类型 | 说明 | 默认值 |
| :--- | :--- | :--- | :--- |
| `-vmware.vcenter` | string | 采集目标地址 (格式 `host:port`)。可以是 vCenter，也可以是单台 ESXi 主机。注意：这不是管理控制台地址。 | - |
| `-vmware.username` | string | 登录用户名。 | - |
| `-vmware.password` | string | 登录密码。 | - |
| `-vmware.insecureTLS` | bool | 是否信任不安全的 TLS 证书（连接自签名证书时需开启，ESXi 默认自签名）。 | `false` |
| `-vmware.schema` | string | 使用 HTTP 或 HTTPS 协议。 | `https` |

> 参数名保留 `vcenter` 是为了向后兼容既有部署，它同时接受 ESXi 地址。
> 目标类型由 exporter 自动识别，无需额外配置，详见「三种采集模式」。

### 2. 采集器开关 (Collectors)
通过以下参数可以精确控制需要采集的数据类型，以优化性能。

| 参数 | 类型 | 说明 | 默认值 |
| :--- | :--- | :--- | :--- |
| `-collector.cluster` | bool | 开启集群 (Cluster) 数据采集。 | `true` |
| `-collector.datacenter` | bool | 开启数据中心 (Datacenter) 数据采集。 | `true` |
| `-collector.datastore` | bool | 开启存储 (Datastore) 数据采集。 | `true` |
| `-collector.host` | bool | 开启 ESXi 主机 (Host) 数据采集。 | `true` |
| `-collector.vm` | bool | 开启虚拟机 (VM) 数据采集。 | `true` |
| `-collector.esxcli.host.nic` | bool | 开启基于 esxcli 的主机网卡采集。 | `false` |
| `-collector.esxcli.storage` | bool | 开启基于 esxcli 的存储采集。 | `false` |

> **`-disable.default.collectors` 从未存在。** 本表此前列出过它，但二进制
> 从来没有注册这个 flag —— 传它会让 exporter 直接以
> `flag provided but not defined` 退出。要只跑一个子集，请逐个显式关闭默认项：
> `-collector.datacenter=false -collector.cluster=false -collector.datastore=false -collector.host=false -collector.vm=false`。
>
> `scripts/check_config.py` 现在会对「出现在参考表里但代码未注册」的 flag
> 报错，所以这类文档漂移不会再回来。

### 3. 性能与采样设置
| 参数 | 类型 | 说明 | 默认值 |
| :--- | :--- | :--- | :--- |
| `-vmware.timeout` | int | 单次抓取的整体超时（秒），覆盖登录、属性检索与性能采样全过程。 | `60` |
| `-vmware.interval` | int | PerfManager 采样窗口（秒）。**不再参与超时计算。** | `20` |
| `-vmware.granularity` | int | 采样数据的时间粒度（秒）。必须大于 0，且不大于 `-vmware.interval`。 | `20` |
| `-collector.max-concurrency` | int | 并发上限，同时约束两处：`CollectorSet` 层同时运行的 collector 数，以及 esxcli collector 内部按主机 fan-out 的宽度。设为 0 则不限制 collector 层，但 per-host fan-out 仍有内建下限。 | `8` |

> **取代了 `-prom.maxRequests`**：那个参数是死参数 —— 上游框架把它存进
> `eHandler.maxRequests` 之后就再没读过，设成任何值都没有效果。现在这个值
> 真的限制并发，其中 per-host fan-out 是真正危险的那一处：改动前 500 台主机
> 就是 500 个并发 SOAP 请求，每台主机的网卡再各起一个 goroutine，实测能到
> 2500 个并发请求同时打同一个 vCenter。

> **关于 `-vmware.interval`**：它表达的是期望值。真实采样间隔由服务端的
> `PerfProviderSummary.RefreshRate` 决定 —— 传一个服务端不支持的间隔只会
> 得到空数据，所以两者不一致时以服务端为准，并在日志中记录 warn。
>
> 参数非法（例如 `granularity=0`）时进程会在启动阶段直接退出并给出原因，
> 不会带着错误配置运行。

### 4. 服务与日志配置
| 参数 | 类型 | 说明 | 默认值 |
| :--- | :--- | :--- | :--- |
| `-http.address` | string | Exporter 监听的 HTTP 地址和端口。 | `:9169` |
| `-log.level` | string | 日志级别: `debug`, `info`, `warn`, `error`。 | `debug` |
| `-log.format` | string | 日志格式: `logfmt` 或 `json`。 | `logfmt` |
| `-file` | string | 指定配置文件的路径。 | - |
| `-web.config.file` | string | Web 配置文件路径，用于给 exporter 自身的监听端口启用 TLS 与 HTTP Basic Auth。详见[安全加固](#安全加固)。 | - |
| `-disable.exporter.metrics` | bool | 是否**不**在 `/metrics` 中导出 exporter 自身的运行指标（`go_*`、`process_*`）。 | `true` |
| `-disable.exporter.target` | bool | 是否禁用 `/metrics` 的默认采集目标。开启后 `/metrics` 只返回 exporter 自身指标，vCenter 数据改由 `/probe` 提供。 | `false` |
| `-metrics.legacy` | bool | 是否在导出规范化指标名的同时，一并导出改名前的旧指标名。如果你自己的 dashboard 或告警规则还在用旧名，可以打开它过渡；详见[指标命名](#指标命名)。仓库自带的 Grafana dashboard 已改用新名，不需要这个开关。 | `false` |

> **注意 `-disable.exporter.metrics` 默认就是 `true`**，也就是默认**不会**有
> `go_goroutines`、`process_resident_memory_bytes` 这类指标。实测默认配置下
> `/metrics` 里 `go_*` 与 `process_*` 均为 0 条；显式设为 `false` 后分别出现
> 35 条与 5 条。如果你在排查 exporter 自身的资源占用却找不到这些指标，就是
> 这个开关。
>
> 两者存在联动：`-disable.exporter.target=true` 时 `/metrics` 走的是
> client_golang 的默认 registry，它自带 Go 与 process collector，所以无论
> `-disable.exporter.metrics` 取什么值，exporter 自身指标都会出现 —— 但
> `vmware_*` 指标会**完全消失**（实测 0 条），vCenter 数据必须改从 `/probe`
> 获取。

### 5. 环境变量集成
| 参数 | 类型 | 说明 | 默认值 |
| :--- | :--- | :--- | :--- |
| `-envflag.enable` | bool | 是否允许从环境变量中读取配置。 | `false` |
| `-envflag.prefix` | string | 环境变量的前缀（需配合 `-envflag.enable` 使用）。 | - |

#### 环境变量名的大小写（容易踩坑）

变量名 = `-envflag.prefix` 的值 + flag 名把 `.` 换成 `_`。注意**flag 名保持原样，不会转成大写**。以 `-envflag.prefix=VMWARE_` 为例：

| flag | 环境变量名 |
| :--- | :--- |
| `-vmware.password` | `VMWARE_vmware_password` |
| `-vmware.vcenter` | `VMWARE_vmware_vcenter` |
| `-vmware.insecureTLS` | `VMWARE_vmware_insecureTLS` |
| `-http.address` | `VMWARE_http_address` |

写成 `VMWARE_VMWARE_PASSWORD` 会被**静默忽略** —— 没有告警、没有报错，exporter 直接用 flag 的默认值，唯一的表现是登录失败，而日志里看不出任何原因。

仓库里的 `scripts/check_config.py` 会拿 `docker-compose.yml` 里的变量名去比对二进制实际注册的 flag，把这类拼写问题拦在 CI，而不是留到生产环境。

---

## 自监控指标

每次抓取都会附带一组自监控指标，让「某个采集器悄悄失败了」不必翻日志才能发现：

```
vmware_up 1
vmware_scrape_duration_seconds 1.512
vmware_scrape_collector_duration_seconds{collector="host"} 1.284
vmware_scrape_collector_success{collector="host"} 1
vmware_scrape_errors_total{collector="host"} 0
vmware_scrape_errors_total{collector="login"} 0
```

| 指标 | 类型 | 回答的问题 |
| --- | --- | --- |
| `vmware_up` | gauge | 目标到底登上去了没有？`0` 表示这轮完全没拿到清单数据。它和 Prometheus 自带的 `up` 不是一回事 —— 自带的那个只反映 HTTP 请求成不成功，而对多目标 exporter 来说「HTTP 正常、vCenter 拒绝登录」是常态 |
| `vmware_scrape_duration_seconds` | gauge | 整轮抓取耗时，含登录与登出 |
| `vmware_scrape_collector_duration_seconds{collector}` | gauge | 单个采集器耗时，`collector="login"` 是登录阶段 |
| `vmware_scrape_collector_success{collector}` | gauge | **最近一轮**成不成功 |
| `vmware_scrape_errors_total{collector}` | **counter** | 一共失败了多少次，`collector="login"` 记登录失败 |
| `vmware_exporter_build_info` | gauge | 当前跑的是哪个构建（`version` / `revision` / `branch` / `goversion`） |

`success` 与 `errors_total` 回答的是两个不同的问题，两个都要看。`success` 是快照
—— 它答不出「过去一小时里失败了十二次、只是恰好在最后一次抓取前恢复了」。
偶发超时的 vCenter 在 gauge 上表现为抓取之间的抖动，而 Prometheus 按
`scrape_interval` 取样，两次采样之间的失败它完全看不见；counter 不会漏。

```promql
# 目标不可达或凭证被拒
vmware_up == 0

# 整轮抓取成功、但某个采集器一直失败
min_over_time(vmware_scrape_collector_success[5m]) == 0

# 某个采集器持续失败
increase(vmware_scrape_errors_total{collector!="login"}[15m]) > 3

# 凭证被拒
increase(vmware_scrape_errors_total{collector="login"}[15m]) > 0
```

**从未失败过的采集器会导出 `0`，而不是干脆不出现**。这不是可有可无的细节：
一条在「一切正常」时根本不存在的序列，会让上面那条告警在正常状态下是
「无数据」而不是「值为 0」；等到第一次失败序列才凭空出现，而 `increase()`
对一条刚出现的序列算不出增量 —— **第一次故障恰好就是漏报的那次**。

登录失败只记在 `collector="login"` 名下，不会摊到每个采集器头上。否则一次
凭证过期会在 `errors_total` 上表现为 N+1 次故障，把真正想看的信号
（某个采集器单独坏了）埋掉。

`/probe` 模式下计数按 target 分桶，所以 A 的凭证错误不会抬高 B 的计数。

`vmware_exporter_build_info` 只有在构建时注入了 version 相关的 linker flag
才有真实取值 —— 发布构建与 `Dockerfile` 都已注入（见 Dockerfile 的
`--build-arg`），直接 `go build` 出来的二进制则是空值。

通过 `collect[]` 传入不存在的采集器名会返回 HTTP 400，而不是被静默忽略。

---

## 安全加固

有两件需要分开看的事，很容易混淆：

1. **exporter 到 vCenter/ESXi 的连接** —— 由 `-vmware.schema` 与 `-vmware.insecureTLS` 控制，默认走 HTTPS。
2. **exporter 自身的监听端口**（Prometheus 抓取的那个）—— 由 `-web.config.file` 控制，**默认完全没有保护**。

第二项的风险比看上去大：`/probe` 接受以 URL 查询参数或 HTTP Basic Auth 形式传入的 vCenter 凭证。明文 HTTP 下这些凭证在网络上是裸奔的；而查询参数的形式还会被写进中间反向代理的 access log 以及 Prometheus 自己的日志里。**优先用 Basic Auth 而不是 `?password=`，并启用 TLS。**

`-web.config.file` 指向一个 [exporter-toolkit 格式](https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md)的文件：

```yaml
tls_server_config:
  cert_file: /etc/vmware-exporter/cert.pem
  key_file: /etc/vmware-exporter/key.pem

basic_auth_users:
  # bcrypt 哈希，可用 htpasswd -nBC 12 "" | tr -d ':\n' 生成
  prometheus: $2y$12$hK1n...
```

```bash
./vmware-exporter -web.config.file=/etc/vmware-exporter/web-config.yml ...
```

Prometheus 侧对应配置：

```yaml
scrape_configs:
  - job_name: "vmware-exporter"
    scheme: https
    tls_config:
      ca_file: /etc/prometheus/vmware-exporter-ca.pem
    basic_auth:
      username: prometheus
      password_file: /etc/prometheus/vmware-exporter-password
```

### 让密码不出现在进程列表里

用 `-vmware.password=...` 传入的密码，主机上任何能读 `/proc` 的人都能看到，容器内 `ps` 能看到，`docker inspect` 也能看到。改用环境变量传：

```bash
docker run -d --name vmware-exporter -p 9169:9169 \
  -e VMWARE_vmware_username -e VMWARE_vmware_password -e VMWARE_vmware_vcenter \
  meisite/vmware-exporter:latest \
  -envflag.enable -envflag.prefix=VMWARE_ -vmware.insecureTLS
```

systemd 部署时，把密码放在 root 所有、权限 `600` 的 `EnvironmentFile` 里，而不是写进 `vmware.conf`。

用 file_sd 做多凭证（`__meta_password`）时，凭证是明文写在 target 文件里的 —— 那个文件同样要 `chmod 600` 并限制属主。

**建议使用只读的 vCenter 服务账号。** exporter 只读取属性和性能计数器，从不写入。

---

## 启动示例

### 基础启动 (推荐)
针对大多数带有自签名证书的 vCenter 环境，建议开启 `-vmware.insecureTLS`：

```bash
./vmware-exporter \
  -vmware.vcenter="<VCENTER_HOST>:443" \
  -vmware.username="administrator@vsphere.local" \
  -vmware.password="your_password" \
  -vmware.insecureTLS \
  -log.level="info"
```
---

## 指标变更与迁移指引

这个版本把**全部**指标名规范化到 Prometheus 惯例：名字里带基础单位后缀、
counter 带 `_total`、去掉 vSphere 的 rollup 后缀。完整对照表在 `CHANGELOG.md`，
这一节讲运维上要做什么。

### 仓库自带 dashboard 已完成迁移

`dashboards/` 下的 Grafana 面板已经改用新指标名，直接对接默认配置的 exporter
即可，不需要额外开关。迁移由 `scripts/migrate_dashboards.py` 完成，脚本保留在仓库
里，这样这次改动是可复现、可 review 的，而不是一次性的手工修改。

光改名字是不够的，还有两件事必须同步改，而且漏掉都不会报错：

- **单位换算挪进了 exporter。** 面板过去要乘 `1024`、`1000 * 1000` 或 `8192`，
  把 exporter 输出的 kiloBytes 和 MHz 换算成 bytes 和 hertz。现在 exporter 直接
  输出基础单位，所以这些系数被删掉，面板单位也跟着改了（`kbytes` → `bytes`、
  `KiBs` → `Bps`、`ms` → `s`）。少删一个系数，面板上的数字就会差三个数量级，
  而且没有任何提示。
- **CPU ready/costop 面板改用了 `rate()`。** 它们原本把 summation 计数器除以硬编码
  的 `20 * 1000`，等于假定 vSphere 的采样粒度固定是 20 秒。现在这些面板用
  `rate(..._seconds_total[$__rate_interval])`，窗口由查询步长推导，不再靠假设。

同一轮里还修掉了 dashboard 原有的两个 bug，详见 `CHANGELOG.md`。

`scripts/check_config.py` 会在每次 CI 运行时校验：没有面板引用改名前的指标，也没有
面板重复施加 exporter 现在已经自己做掉的换算。

如果你有**自己的** dashboard 或告警规则还在用旧名，可以在迁移期间打开
`-metrics.legacy`，或者改用下面的 recording rules。

### 指标命名

新名字是从 vCenter 自己上报的计数器元数据推导出来的，不是手写清单。三条规则覆盖
了几乎全部情况：

| 规则 | 例子 |
| --- | --- |
| 去掉 vSphere 的 rollup 后缀（`.average` / `.summation` / `.latest`）—— 它描述的是 vCenter 怎么聚合，而不是这个值是什么 | `cpu.usagemhz.average` → `cpu_usage_hertz` |
| 单位变成名字后缀，并换算成 Prometheus 的基础单位 | `mem.consumed.average`（kiloBytes）→ `mem_consumed_bytes`，值 ×1024 |
| vCenter 声明为 `delta` 的计数器变成真正的 counter，带 `_total` | `cpu.ready.summation` → `cpu_ready_seconds_total` |

有两处换算值得单独点出来，因为**弄错之后算出的数看起来仍然很合理**：

- **`percent` 类计数器要 ÷10000，不是 ÷100。** vSphere 的 percent 以百分之一个
  百分点为单位，原始值 `100` 表示 1%。新的 `*_ratio` 指标落在 Prometheus 惯用的
  0..1 区间，所以展示它的面板单位要选 `percentunit` 而不是 `percent`。
- **`kiloBytes` 是 1024 字节，`megaBytes` 是 1048576 字节。** vSphere 文档明确按
  二进制倍数定义。按 1000 算会把内存少报 2.4%。

### 这次有些指标的数值也变了

和上一个版本的"只改名不改值"不同，这次有几对替换指标的数值不同。任何拿指标跟
硬编码阈值比较的地方，阈值都要跟着换算。

| 旧名 | 新名 | 旧值乘以 |
| --- | --- | --- |
| `vmware_host_cpu_capacity`、`vmware_host_cpu_capacity_mhz` | `vmware_host_cpu_capacity_hertz` | 1000000 |
| `vmware_host_mem_capacity` | `vmware_host_mem_capacity_bytes` | 1 |
| `vmware_vm_mem_capacity` | `vmware_vm_mem_capacity_bytes` | 1048576 |
| `vmware_vm_datastore_capacity_used` | `vmware_vm_datastore_capacity_used_bytes` | 1 |
| `vmware_datastore_capacity` | `vmware_datastore_capacity_bytes` | 1 |
| `vmware_datastore_free` | `vmware_datastore_free_bytes` | 1 |

`vmware_host_cpu_capacity_mhz` 是上个版本刚引入的替代指标，这次它本身也被弃用了：
MHz 不是 Prometheus 的基础单位。如果你上一轮已经迁到 `_mhz`，这轮再迁只是乘一个
1000000。

### delta 计数器：不要再除采样间隔了

旧的 `*_summation` 指标是 gauge，值是抓取窗口内各样本的**平均值**，而把它换成速率
的惯用写法是除以一个硬编码的间隔：

```promql
# 旧写法 —— 这个 20 是 -vmware.granularity，被硬编码进了查询
vmware_host_cpu_ready_summation / (20 * 1000)
```

只要 `-vmware.granularity` 不是 20，这个表达式就是错的；而且 exporter 那一侧也是
错的 —— 对 delta 样本求平均会丢掉除一个区间之外的全部增量。两边都已修正。新指标是
counter，用 `rate()` 让 Prometheus 自己算间隔：

```promql
# 新写法 —— 没有硬编码间隔，任何 granularity 下都正确
rate(vmware_host_cpu_ready_seconds_total[$__rate_interval])
```

适用于 `cpu_ready`、`cpu_costop`、`cpu_maxlimited`，以及四个
`net_*_errors_total` / `net_*_dropped_total`。

### 用 recording rules 让旧名继续可用

如果你暂时完全不想改 dashboard，可以用 recording rules 从新指标反推出旧名。比
`-metrics.legacy` 更值得长期采用，因为这些别名放在你自己的 Prometheus 配置里，
可以一条一条删：

```yaml
groups:
  - name: vmware-exporter-legacy-aliases
    rules:
      - record: vmware_host_cpu_capacity
        expr: vmware_host_cpu_capacity_hertz / 1000000
      - record: vmware_host_mem_capacity
        expr: vmware_host_mem_capacity_bytes
      - record: vmware_vm_mem_capacity
        expr: vmware_vm_mem_capacity_bytes / 1048576
      - record: vmware_datastore_capacity
        expr: vmware_datastore_capacity_bytes
      - record: vmware_datastore_free
        expr: vmware_datastore_free_bytes
```

注意 recording rule **无法**忠实还原旧的 `*_summation` gauge —— 只要抓取窗口里
落进了一个以上的样本，它们的旧值本身就是错的。那批指标应该直接迁到 `rate()`，
而不是做别名。

### 上一个版本的改名（仍需处理）

| 变更 | 你需要做什么 |
| --- | --- |
| `vmware_cluster_datastores` → `vmware_cluster_datastore` | 改自己的规则/面板。而且现在每个 datastore 一条序列，不再是 `dsmo` 里逗号拼接的列表 |
| `vmware_compute_datastores` → `vmware_compute_datastore` | 同上 |
| `vmware_vm_snapshot_info` 去掉了 `created` label | 从 metric value 读创建时间，它就是同一时刻的 Unix 时间戳 |
