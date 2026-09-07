# vmware-exporter 部署指南（systemd + YAML 配置）

面向 Ubuntu / Debian / RHEL 系的 systemd 主机。x86_64 架构。

## 包内文件

| 文件 | 说明 |
|---|---|
| `vmware-exporter` | 静态编译的二进制，无运行时依赖 |
| `vmware-exporter.service` | systemd unit |
| `config.yaml` | 配置模板，装到 `/etc/vmware-exporter/config.yaml` |
| `install.sh` | 安装 / 升级 |
| `uninstall.sh` | 卸载 |

## 安装

```bash
tar xzf vmware-exporter-*-linux-amd64-systemd.tar.gz
cd vmware-exporter-*-linux-amd64-systemd
sudo ./install.sh
```

脚本会装好二进制、unit 和配置模板，设置开机自启，但**不会启动服务**——
此时配置里还是占位值，起来也只会刷登录失败的日志。

接着填配置：

```bash
sudo vi /etc/vmware-exporter/config.yaml
```

至少要改这三项：

```yaml
vmware.vcenter: vcenter.your-domain.com
vmware.username: readonly@vsphere.local
vmware.password: "你的密码"
```

然后启动并验证：

```bash
sudo systemctl start vmware-exporter
systemctl status vmware-exporter
curl -s localhost:9169/metrics | head
```

浏览器打开 `http://<主机>:9169/` 可以看到内置的概览页、调试台和配置生成器
（界面默认中文，右上角可切英文）。

## 升级

同样跑 `sudo ./install.sh`。脚本检测到已安装会走升级路径：

- **保留** `/etc/vmware-exporter/config.yaml`
- 新版本的配置模板放在 `config.yaml.example` 供对照（新增项从这里抄）
- 原本在运行的服务会自动重启；原本停着的保持停止

## 卸载

```bash
sudo ./uninstall.sh              # 保留配置
sudo ./uninstall.sh --purge      # 连配置一起删（会二次确认）
sudo ./uninstall.sh --purge -y   # 免确认
```

可重复执行，不会因为"已经卸载过了"而报错。

## 改配置

改完执行：

```bash
sudo systemctl reload vmware-exporter
```

不用重启，指标不断点。重载失败会保留旧配置，并把
`vmware_exporter_config_last_reload_successful` 置 0 ——
**建议对这个指标告警**，因为 `systemctl reload` 即使重载失败也退出 0
（信号确实送到了）。

只有这三项改了必须 `restart`：

- `http.address` —— 端口已经绑定
- `web.config.file` —— TLS 和 basic auth 在 listen 时加载
- `log.format` —— 日志 handler 类型在构造时固定

改这些用 reload 会在日志里出 warning，而不是静默生效。

## 配置文件格式

扁平的 `flag-name: value`，键名就是命令行 flag 去掉前导 `-`。

几条硬约束，踩到都是启动失败：

- **不能嵌套**。写成 `vmware:` 下面缩进 `vcenter:` 会解析失败
- **未知键直接失败**：`config file X sets unknown flag "Y"`
- 类型不对会报 `cannot set flag X to "Y" from Z`
- 重复的键在 yaml 层就报 `mapping key "X" already defined`
- **密码含 `#` 必须加引号**，否则被当成行内注释截断

优先级：命令行 > 配置文件 > 环境变量。

### 两个反直觉的默认值

- `log.level` 内置默认是 **debug**（不是 info）。模板里已经显式写了 `info`
- `disable.exporter.metrics` 默认 **true**，即 exporter 自身的进程指标默认不暴露

## 权限：config.yaml 必须是 0644

这一条值得单独说，因为改错了服务直接起不来。

配置文件是 **exporter 进程自己读**的，而此时 `DynamicUser=yes` 已经生效，
进程用的是 systemd 分配的临时 uid。`0600 root:root` 它读不了，会得到：

```
cannot read config file: permission denied
```

所以必须 `0644 root:root`。`install.sh` 每次都会把权限修正回来。

（对比：老版本用 `EnvironmentFile` 的包可以 0600，因为那是 systemd 以 root
读完再传给进程的。换成 `-file` 之后这个前提就不成立了。）

## 老版本 systemd

unit 里用了 `DynamicUser=yes`，需要 systemd **232+**（2016 年）。
安装脚本检测到更老的版本会告警。这种情况下手工改：

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin vmware-exporter
sudo vi /etc/systemd/system/vmware-exporter.service
```

删掉 `DynamicUser=yes`，加上：

```ini
User=vmware-exporter
Group=vmware-exporter
```

然后 `sudo systemctl daemon-reload && sudo systemctl restart vmware-exporter`。

## Prometheus 抓取配置

单个 vCenter：

```yaml
scrape_configs:
  - job_name: vmware
    scrape_interval: 60s
    scrape_timeout: 55s
    static_configs:
      - targets: ['exporter-host:9169']
```

`scrape_timeout` 必须大于配置里的 `vmware.timeout`，否则 Prometheus 会先放弃。

多个 vCenter 用 `/probe` 模式。exporter 内置的配置生成器
（`http://<主机>:9169/config`）可以直接生成 scrape config 和 file_sd 文件，
比手写省事。

## 排障

```bash
# 服务状态
systemctl status vmware-exporter

# 实时日志
journalctl -u vmware-exporter -f

# 只看错误
journalctl -u vmware-exporter -p err

# 确认在听
ss -lntp | grep 9169

# 构建信息（版本、commit、构建时间）
curl -s localhost:9169/metrics | grep vmware_exporter_build_info
```

常见问题：

- **启动就退出，日志说 permission denied** —— config.yaml 权限不是 0644，见上文
- **unknown flag** —— 配置里有拼错的键名，报错信息里有具体是哪个
- **登录失败** —— 检查 `vmware.vcenter` 填的是不是 vCenter 本身
  （不是 5480 端口的 Management Console）；自签证书需要 `vmware.insecureTLS: true`
- **抓取超时** —— 大集群调大 `vmware.timeout`，同时相应调大 Prometheus 的
  `scrape_timeout`；esxcli 系列采集器会逐台主机遍历，开之前先确认超时给够了
