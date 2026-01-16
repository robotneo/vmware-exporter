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
ARGS="-vmware.username=administrator@vsphere.local -vmware.password=public@123 -vmware.vcenter=172.16.10.1:443 -vmware.insecureTLS"
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
  -vmware.password=public@123 \
  -vmware.vcenter=172.16.10.1 \
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
        - '172.16.10.1'
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: 172.16.10.100:9169
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
  - 172.16.10.10
  # - vcenter1.example.com
  labels:
    __meta_username: 'administrator@vsphere.local'
    __meta_password: 'public@12345'
    __meta_schema: 'https'
    __meta_insecure: 'true'
    __meta_env: 'prod'  # 可删除
    __meta_datacenter: 'dc01' # 可删除

- targets:
  - 192.168.10.10
  # vcenter2.example.com
  labels:
    __meta_username: 'administrator@vsphere.local'
    __meta_password: 'public@54321'
    __meta_schema: 'https'
    __meta_insecure: 'true'
    __meta_env: 'prod'  # 可删除
    __meta_datacenter: 'dc02' # 可删除
```

## 参数设置

可以通过命令行选项、环境变量、yaml 配置文件或三者的组合来配置输出程序，设置的环境变量将被配置文件的内容覆盖，然后被启动时设置的任何命令行选项覆盖，可用的选项如下：

### 1. vCenter 连接配置
| 参数 | 类型 | 说明 | 默认值 |
| :--- | :--- | :--- | :--- |
| `-vmware.vcenter` | string | vCenter 服务器地址 (格式 `host:port`)。注意：这不是管理控制台地址。 | - |
| `-vmware.username` | string | 登录 vCenter 的用户名。 | - |
| `-vmware.password` | string | 登录 vCenter 的密码。 | - |
| `-vmware.insecureTLS` | bool | 是否信任不安全的 TLS 证书（连接自签名证书的 vCenter 时需开启）。 | `false` |
| `-vmware.schema` | string | 使用 HTTP 或 HTTPS 协议。 | `https` |

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
| `-disable.default.collectors` | bool | 禁用所有默认采集器，仅运行显式开启的采集器。 | `false` |

### 3. 性能与采样设置
| 参数 | 类型 | 说明 | 默认值 |
| :--- | :--- | :--- | :--- |
| `-vmware.interval` | int | 采集频率（单位：秒）。 | `20` |
| `-vmware.granularity` | int | 采样数据的时间粒度。 | `20` |
| `-prom.maxRequests` | int | 最大并行采集请求数（设为 0 则不限制）。 | `20` |

### 4. 服务与日志配置
| 参数 | 类型 | 说明 | 默认值 |
| :--- | :--- | :--- | :--- |
| `-http.address` | string | Exporter 监听的 HTTP 地址和端口。 | `:9169` |
| `-log.level` | string | 日志级别: `debug`, `info`, `warn`, `error`。 | `debug` |
| `-log.format` | string | 日志格式: `logfmt` 或 `json`。 | `logfmt` |
| `-file` | string | 指定配置文件的路径。 | - |

### 5. 环境变量集成
| 参数 | 类型 | 说明 | 默认值 |
| :--- | :--- | :--- | :--- |
| `-envflag.enable` | bool | 是否允许从环境变量中读取配置。 | `false` |
| `-envflag.prefix` | string | 环境变量的前缀（需配合 `-envflag.enable` 使用）。 | - |

---

## 启动示例

### 基础启动 (推荐)
针对大多数带有自签名证书的 vCenter 环境，建议开启 `-vmware.insecureTLS`：

```bash
./vmware-exporter \
  -vmware.vcenter="172.16.10.1:443" \
  -vmware.username="administrator@vsphere.local" \
  -vmware.password="your_password" \
  -vmware.insecureTLS \
  -log.level="info"
```