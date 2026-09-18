# vmware-exporter 性能与安全审计报告（v0.2.0 / ee918c2）

- 审计基线：`master @ ee918c2`（含 v0.2.0 Batch 0 生命周期指标、新版 dashboard、systemd 部署包）
- 审计日期：2026-09-16
- 审计方式：全量人工代码走读（cmd / internal / vmware / web / 打包 / CI），辅以可运行的概念验证、`govulncheck`、`go vet`、全量 `go test`
- 工具基线：`govulncheck v1.8.0` → **0 个可调用漏洞**（1 个间接依赖漏洞不可达）；`go vet` 干净；`go test ./...` 全绿（CGO_ENABLED=0）

---

## 第一部分：审计设计（先设计，后审计）

### 1.1 审计范围

| 层 | 文件 |
|---|---|
| HTTP 入口 | `cmd/vmware-exporter/{main,handlers,ui,gate,targetfilter,settings,reload}.go` |
| 调度/并发 | `internal/collector/{set,scrape,throttle,inventorycache,entitystats,errors}.go` |
| vCenter 会话 | `vmware/api/{vmware,targettype}.go` |
| 采集热路径 | `vmware/collectors/*.go`（含 v0.2.0 生命周期代码）、`vmware/esxcli/esxcli.go` |
| 配置/重载 | `internal/config/config.go` |
| 前端 | `web/*.{html,js}` |
| 部署/供应链 | `Dockerfile`、`docker-compose.yml`、`packaging/systemd/*`、`.github/workflows/*`、`go.mod` |

### 1.2 威胁模型

- 部署形态：exporter 运行在受控机房或 K8s 节点，HTTP 端口默认可被同网段访问；vCenter 为高价值资产（只读凭证仍可枚举全部资产拓扑与性能数据）。
- 攻击者画像：(a) 能访问 9169 端口但无 vCenter 凭证的同网段人员；(b) 容器逃逸/同主机低权限本地用户；(c) 诱使运维访问其构造的 `/probe` 链接。
- 信任边界：**`/probe` 的请求方提供 target 与凭证**，因此它本质是一个「带凭证的 SSRF 代理」——所有防护都要围绕「不让请求方把凭证/请求发往非预期主机」展开。

### 1.3 评级标准

| 级别 | 含义 |
|---|---|
| P0 | 未授权远程代码执行 / 凭证明文外泄 / 全量数据泄露，默认配置即可触发 |
| P1 | 核心安全控制被绕过，或默认配置下可造成实质危害（需特定部署前提） |
| P2 | 需要附加条件的危害、内存型 DoS、本地权限相关的凭证暴露、显著性能热点 |
| P3 | 加固建议、小性能项、文档/默认值收敛 |

### 1.4 方法

1. 画出请求路径（mux → 闸 → 白名单 → 凭证 → 登录 → 并发采集），逐点核对输入校验与机密处理；
2. 对可疑点写**可运行 PoC**（标准库 + 项目实际依赖 govmomi 双重验证），不凭阅读下结论；
3. 性能侧从「序列基数、每轮对象分配、SOAP 往返数、日志量、缓存边界」五个口径过每个 collector；
4. 用工具交叉验证：govulncheck（可达性分析）、go vet、全量测试。

---

## 第二部分：安全审计结论

### 总览

| 编号 | 级别 | 问题 | 位置 |
|---|---|---|---|
| S-01 | **P1** | `/probe` SSRF 白名单可被 userinfo 注入绕过，并把 Basic 凭证转发给攻击者主机 | `cmd/vmware-exporter/targetfilter.go:47` |
| S-02 | **P2** | POST 表单体无大小上限，未授权可内存型 DoS | `cmd/vmware-exporter/handlers.go:253` |
| S-03 | **P2** | systemd 模式下含密码的 config.yaml 必须 0644，本机任意用户可读 | `packaging/systemd/{config.yaml,vmware-exporter.service}` |
| S-04 | **P2** | 容器镜像以 root 运行（Dockerfile 无非特权用户） | `Dockerfile:64-70` |
| S-05 | P3 | `/probe` 的 `schema` 参数无白名单，可强制 `http` 明文传输凭证 | `cmd/vmware-exporter/handlers.go:293` |
| S-06 | P3 | 白名单只做字符串匹配、不做 DNS 解析，存在 DNS rebinding/主机名绕过残留面 | `targetfilter.go` |
| S-07 | P3 | `/probe` 仍接受 GET 查询串里的 password（代理日志/Referer 泄漏面，文档已警示） | `handlers.go:277`、`web/index.html` |
| S-08 | P3 | 默认无认证、debug 控制台默认开启；无统一安全响应头 | `main.go`、`ui.go` |
| S-09 | 信息 | 1 个间接依赖存在漏洞但调用路径不可达（govulncheck 不报）；CI 已含 govulncheck，toolchain 已固定到 1.26.6 | `go.mod`、`.github/workflows/test.yml` |

### S-01（P1）SSRF 白名单 userinfo 注入绕过 —— 已用项目依赖实证

**位置**：`cmd/vmware-exporter/targetfilter.go` 的 `splitHost()`。

**根因**：白名单校验与实际连接对 target 的解析方式不一致。

- 校验侧：`splitHost` 用 `net.SplitHostPort("allowed.example.com:443@169.254.169.254")`，Go 把它解析成 host=`allowed.example.com`、port=`443@169.254.169.254`？不是——实测 `SplitHostPort` 以最后一个 `:` 切分，得到 host 段 `allowed.example.com`（`@` 前的部分被当作普通 host 字符，校验函数随后用它匹配后缀规则并**放行**）。
- 连接侧：`vmware/api/vmware.go:234` 用 `soap.ParseURL("https://"+target+"/sdk")` 解析。URL 语法里 `@` 是 userinfo 分隔符，于是 `allowed.example.com:443` 变成**用户名:端口形式的 userinfo**，真正连接的 host 是 `@` 之后的地址。

**PoC（用项目实际依赖 govmomi 运行，非推测）**：

```
target="allowed.example.com:443@169.254.169.254"
  → splitHost 得到 "allowed.example.com"，命中 ".example.com" 后缀规则：放行 ✅
  → soap.ParseURL 得到实际 connectHost="169.254.169.254"，userinfo="allowed.example.com:443" ❌
```

**影响**：运维一旦配置了 `-probe.allowed-targets`（这是官方提供的 SSRF 深度防御手段，配置它的人恰恰是安全敏感用户），攻击者可用该构造让 exporter 向任意内网地址（云元数据 `169.254.169.254`、内网管理口、攻击者主机）**发起带 Authorization: Basic 头的 HTTPS 连接，头里就是提交给 /probe 的 vCenter 凭证**。即：白名单被绕过 + 凭证被转发到非预期主机。默认（白名单为空）不触发，但该控制存在的意义被完全抵消。

**修复建议**（先做 URL 规范化，再做规则匹配，不要让校验与连接用两套解析）：

1. 在 `targetfilter.go` 中先用 `url.Parse` 规范化 target（如 `url.Parse("https://" + target)` 或对 `//"+target` 解析），取 `u.Hostname()`/`u.Port()` 做匹配；
2. 显式拒绝 `u.User != nil`（任何 `@`、`:` userinfo），并拒绝 `u.Path/RawQuery/RawFragment` 非空（target 只允许是 `host` 或 `host:port`）；
3. `targetAllowedByRules` 增加针对该绕过的表驱动用例：`allowed.example.com:443@evil`、`allowed@evil`、`allowed.example.com#@evil`、`allowed.example.com/path` 等，全部应拒绝；
4. 纵深：在拨号侧用 `net.Dialer.Control` 解析后复核 IP 是否落在规则 CIDR（顺带解决 S-06 的 DNS rebinding）。

### S-02（P2）POST body 无上限 → 未授权内存 DoS

**位置**：`cmd/vmware-exporter/handlers.go:253` `r.ParseForm()`。

Go 对 `application/x-www-form-urlencoded` 的 body 用 `io.ReadAll` **整表读入内存**（只有 multipart 才受 32MB 与磁盘溢出保护）。exporter 没有 `http.MaxBytesReader`，`ReadHeaderTimeout` 也只管请求头。未认证攻击者向 `/probe` 慢速 POST 一个超大 urlencoded body，即可让进程内存随连接数线性增长。

**修复**：`ParseForm` 前包一层 `r.Body = http.MaxBytesReader(w, r.Body, 1<<20)`（凭证表单体实际只有几百字节，1MB 绰绰有余），超限返回 413。顺带对 GET 查询串无影响。

### S-03（P2）systemd 下含密码配置被迫 0644，本机任意用户可读

**位置**：`packaging/systemd/config.yaml:31`（`vmware.password`）+ unit 的 `DynamicUser=yes`。

现状注释已解释了反直觉约束：DynamicUser 的临时 uid 读不了 root:root 0600，所以安装脚本强制 `chmod 0644`。代价是：**同主机任何本地用户都能 `cat /etc/vmware-exporter/config.yaml` 拿到 vCenter 只读账号密码**。在多租户/共享主机上属于本地凭证暴露。

**修复（推荐 systemd 原生凭证机制）**：

- unit 用 `LoadCredential=vmware.yaml:/etc/vmware-exporter/config.yaml`（或 `LoadCredentialEncrypted=`），systemd 会把它挂到 `$CREDENTIALS_DIRECTORY/vmware.yaml`，属主为本服务动态 uid、0600，进程仍用 `-file=${CREDENTIALS_DIRECTORY}/vmware.yaml` 读取；
- 磁盘原文件可收紧为 root:root 0600；
- 同步更新 `install.sh` 权限处理与 `DEPLOY-zh.md`。这样既保留 DynamicUser 又消除世界可读。
- 次优：为 exporter 建固定系统账号（unit 注释已给 useradd 方案），配置 chown 该账号 0600。

### S-04（P2）容器镜像默认以 root 运行

**位置**：`Dockerfile:64-70`（`FROM scratch` 后无 `USER`）。

scratch 基础镜像本身最小化（好），但没有非特权用户，容器内进程 uid=0。docker-compose 又把端口直接映射到 `0.0.0.0:9169` 且无认证。建议：

- builder 阶段生成 `/etc/passwd`（`nobody` 65534）并 COPY 进 scratch，运行行加 `USER 65534:65534`（数字 uid 无需 passwd 也可，但带 passwd 后日志可读）；
- compose 增加 `read_only: true`、`cap_drop: [ALL]`、`security_opt: [no-new-privileges:true]`，并把端口绑定到 `127.0.0.1:9169` 或前置反代（systemd 包的加固水平应同步到容器）。

### S-05（P3）`schema` 参数可强制明文 http

`probeHandler` 直接取 `params.Get("schema")`，默认 https 但接受任意值。攻击者构造 `schema=http` 可让凭证以明文发往 target（配合 S-01 可发往攻击者主机）。建议白名单收敛为 `{http, https}`，其余 400；更严格的默认应拒绝 http（提供显式 `-probe.allow-insecure-schema` 再放开）。

### S-06（P3）白名单不做 DNS 解析

后缀/精确规则对主机名只做字符串比较：配置 `.corp` 时，能解析到内网 IP 的外部主机名（或 rebinding）不受 CIDR 规则约束。建议在拨号控制函数里对最终 IP 复核 CIDR（与 S-01 的修复合并实现）。

### S-07（P3）GET 查询串密码仍被接受

调试页已改 POST、页面与 README 已强烈警示，但服务端为兼容 Prometheus `__param_password` 仍接受 URL 中的 password（会进反代访问日志/Referer）。建议保留能力但在响应头或文档持续强调；可增加一个开关 `-probe.deny-query-credentials`，生产部署强制只允许 Basic Auth/POST。

### S-08（P3）默认无认证 / debug 控制台默认开 / 无安全头

均为「有意的易用性默认」，且都有开关（`web.config.file` 上 basic auth/TLS、`-web.debug-console=false`）。建议：(a) 当探测到监听 `0.0.0.0` 且未配 `web.config.file` 时，启动日志打一条显著 warning；(b) 统一加 `X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer`（对带凭证的 /debug、/config 页面有实际意义）。

### 已经做对、无需重复投入的安全项（审计确认）

- 凭证绝不进日志：`LoginWithCredentials` 刻意不记 username；GET/POST 两路径日志只记 target/schema/insecure/collector；前端配置生成有默认口令脱敏且有测试兜底。
- 命令行不摆密码：systemd 走 `-file`、compose 走 `-envflag.*`，两处注释都点明 /proc 与 docker inspect 泄漏面。
- 配置热重载的 flag 数据竞争已用 RWMutex + 一次性快照消除；reload 影子集提交 + 失败回滚，坏配置不污染运行态；`ValidateFlags` 重载后二次校验。
- 进程级 in-flight 闸（满员 503）、scrape ctx 贯通到 SOAP（断连即取消）、SOAP Logout 独立超时避免会话泄漏、子协程 panic 被 `safeUpdate` 收成 error 而非崩全进程。
- HTTP server 有 `ReadHeaderTimeout`/`IdleTimeout`（Slowloris 已防），并有测试断言字段值。
- systemd unit 加固非常完整（DynamicUser、ProtectSystem=strict、NoNewPrivileges、SystemCallFilter、CapabilityBoundingSet= 空、LimitNOFILE 收敛）。
- CI 含 govulncheck（可达性口径）、go.mod 固定 `toolchain go1.26.6` 规避标准库 7 个公告。
- /probe 每请求独立凭证，**不注入**进程级清单缓存，杜绝跨凭证越权读（代码注释与实现一致，已核对）。

---

## 第三部分：性能审计结论

### 总览

| 编号 | 级别 | 问题 | 位置 |
|---|---|---|---|
| P-01 | **P2** | esxcli 两个 collector 在实体热循环里逐个 `prometheus.NewDesc`（host/vm 早已修掉的 P2-4 热点在此残留） | `esxclihostnic.go:183`、`esxclistoragelist.go:135` |
| P-02 | P3 | datacenter/cluster/compute 仍在每轮每实体循环里 NewDesc（实体数小，收益低） | `datacenter.go:42-111`、`cluster.go:56-167` |
| P-03 | P3 | 每次抓取都新建会话 + TLS 握手 + `CounterInfoByName` 拉计数器元数据（可跨轮缓存） | `vmware/api/vmware.go:280-293` |
| P-04 | P3 | 默认 `log.level=debug`，大规模环境每轮刷大量 chunk/counter 日志（systemd 模板已收敛为 info，但 flag 默认值仍是 debug） | `main.go:50` |
| P-05 | P3 | esxcli N+1 SOAP 固有：每主机 MME+list、每网卡一次 get，实体多时往返数高（已有全局 SOAP 闸兜并发，但总往返数没降） | `esxclihostnic.go:117-170` |
| P-06 | 观察 | 无 pprof 端点，线上定位抓取耗时/内存只能靠日志（既是安全优点也是可观测性缺口） | — |

### P-01（P2）esxcli 热路径逐个 NewDesc

**位置**：`vmware/collectors/esxclihostnic.go:182-187`（每块网卡）、`esxclistoragelist.go:134-139`（每个存储设备）。

`descs.go` 顶部注释明确记载：host/vm/perf 当年把「实体循环内 NewDesc + constLabels 塞实体值」改造成了「Desc 复用 + variableLabels」（P2-4），以 1000 VM 计单轮少造约 4000 个 Desc。**但 esxcli 两个 collector 没纳入那次改造**，仍是老写法：

```go
ch <- prometheus.MustNewConstMetric(
    prometheus.NewDesc(name, "NIC Info", nil,
        map[string]string{"mo":..., "host":..., "descr": nic.Description,
            "driver":..., "version":..., "firmware":...}), // 每网卡一次
    ...)
```

每次 `NewDesc` 都要做 fqName 拼接、label 名校验/排序、const label 拷贝。esxcli.host.nic 默认关闭，但一旦在几百台主机、每机多网卡的环境启用，单轮就是数千次 Desc 构造 + 数千个小 map 分配，属于纯浪费的 GC 压力。

**注意约束**：这些指标的 label 是「描述性高基数」（driver/version/firmware/descr/model/revision），不能简单平移成 variableLabels 后每实体输出——那会改变基数。正确做法是**按 label 值集合缓存 Desc**：项目里已有现成范式 `vsanperf.go:543 vsanPerfDesc(namespace, entityType, label)`（sync.Mutex + map 缓存、复合 key）。可为 esxcli driver 指标建一个以 label 组合为 key 的 Desc 缓存，相同 (driver,version,firmware,descr) 复用同一个 Desc。函数本身带 versionSet 去重，实际唯一组合数远小于网卡数，收益明确。

### P-02（P3）datacenter/cluster/compute 循环内 NewDesc

datacenter、cluster、compute、cluster_datastore 也在实体循环里 NewDesc（带实体 constLabels）。这些实体数是个位到几十，单次开销可忽略，`descs.go:27` 注释也明确把它们排除在 P2-4 外。仅作记录：若未来要统一，可与 P-01 一起做「按 label 组合缓存」，不建议为形式统一而扩大风险面。

### P-03（P3）会话与计数器元数据未跨轮复用

每轮抓取都重新 Login/Logout（一次会话建立 + TLS 握手）并调用一次 `perf.CounterInfoByName()`（计数器元数据）。计数器表在 vCenter 生命周期内几乎不变，几千个 PerfCounterInfo 的拉取与按名建 map 每轮重复。

权衡：当前「每轮独立会话 + 登出」的设计换来的是**零会话泄漏、无陈旧会话、配置热改即时生效、/probe 多租户隔离**，这些价值高于一次元数据往返。不建议贸然引入长会话池（会重新引入会话陈旧、并发归属、登出失败等复杂度）。若要优化，优先做「计数器元数据按 vCenter+版本做 TTL 缓存」这种无状态、可安全放在 InventoryCache 同类设施里的小项，而非会话复用。

### P-04（P3）debug 默认日志级别

`-log.level` 默认 `debug`（`main.go:50`）。debug 路径在每轮会输出：每个 perf chunk 的耗时、每台主机/VM 的跳过记录、每个不可用计数器、每次属性检索耗时。大规模 + 高频抓取时日志量可观，journal 与集中式日志都是成本。systemd 模板已把生产默认改为 info（`config.yaml:34`），但裸二进制/Docker 默认仍是 debug。建议把 flag 默认值收敛为 `info`（调试时临时 `-log.level=debug` 或 SIGHUP 热开），与「生产默认安静」的惯例一致。

### P-05（P3）esxcli N+1 固有往返

每台主机至少 2 次往返（RetrieveMME + list），每块网卡再 1 次 get。全局 RoundTripper 闸已把**同时在飞**的请求数钉死（避免 vCenter 被打爆），但总往返数 = O(主机×网卡) 没降，启用 esxcli 时单轮 wall-clock 仍可能到分钟级（registry.go 注释已警示 "per-host serial"）。这是 vSphere MME API 形态决定的，难以批量；建议仅在文档与 `/config` 页持续标注成本，并确保 `-vmware.timeout` 与抓取间隔的指引明确。

### 序列基数审计（重点核对 v0.2.0 新增指标）

| 指标 | label | 基数评估 |
|---|---|---|
| `vmware_vm_power_state` | vmmo, vm, state, vcenter | 每 VM 1 条（state 进 label、值恒 1，每实体仍只 1 条），**可控** |
| `vmware_host_power_state` / `connection_state` / `maintenance_mode` | hostmo, host, state, vcenter | 每主机 1 条/指标，**可控** |
| `*_overall_status` | …, status | 值域 gray/green/red/yellow 有界，**可控** |
| `vmware_*_info` 的 `uuid` | uuid | 每实体 1 个稳定唯一值，与实体数 1:1，**不放大基数**，且正是做跨 vMotion/重命名稳定关联所需 |
| `vmware_scrape_entities_skipped` | vcenter, collector, kind, **reason** | reason 为 7 个固定常量（`entitystats.go:12-27`），非自由文本，**有界可控** |
| `esxcli_*_driver`（P-01） | mo, host, descr/driver/version/firmware/model/revision | **高基数风险点**：descr/model 为自由文本，但随物理硬件型号有界；去重后尚可。P-01 修复时不要把它变成每实体每版本一条而抬高基数 |
| `vsan_perf_*` | cmo, cluster, entity, **entityid**, vcenter | entityid 是磁盘/主机 UUID，随硬件数线性增长；已有**请求侧 label 白名单**（`vsanperf.go:64`）把 79 个 label 裁到十几个，控制得当，默认关闭 |

结论：**v0.2.0 新增的生命周期/状态指标没有引入基数失控**——state/status/reason 全部是有界枚举，uuid 与实体 1:1。真正需要警惕的仍是两个历史高基面点（esxcli 描述性 label、vsan_perf entityid），二者都默认禁用且各自有缓解。

### 已经做对、无需重复投入的性能项（审计确认）

- perf 查询分块（`-vmware.perf.chunk-size` 默认 64）+ 有界并发 + 按实体顺序合并，规避 `vpxd.stats.maxQueryMetrics` 与单请求超时；计数器 id 直构，避免每块重复 SampleByName 往返。
- 慢变拓扑/容量清单的进程级 TTL 缓存（singleflight 合并并发未命中、过期项删除防 map 膨胀、失败不缓存）；host/vm 运行态刻意不缓存；/probe 不注入缓存（安全边界）。
- HostSystem 三 collector 请求内 `sync.Once` 共享，三次 ContainerView 检索降为一次。
- 三层并发全部有界：collector 层 errgroup.SetLimit、per-host 层 SetLimit、SOAP RoundTripper 全局闸（令牌只包单次往返，杜绝 n×n 嵌套 fan-out），外加进程级 in-flight 闸。
- delta 计数器按 StatsType 求和而非错误平均；历史样本尾部截断复刻 govmomi 语义；每轮 Desc 缓存（descsFor）。
- EntityStats/ScrapeErrors 并发安全、counter 跨请求单调、0 值 seed 保证告警无首报漏报。

---

## 第四部分：优先级修复路线

### 建议立即修（P1/P2，改动小、收益大）

1. **S-01 白名单 userinfo 绕过**：先 `url.Parse` 规范化再匹配，拒绝任何 userinfo/path/query/fragment，补 4 类绕过的表驱动测试。这是唯一一个「官方安全控制被实际绕过」的问题。
2. **S-02 body 大小上限**：`MaxBytesReader(1MB)` + 413，一行级改动。
3. **P-01 esxcli Desc 缓存**：照 `vsanPerfDesc` 范式按 label 组合缓存，消除热循环分配。
4. **S-04 容器非特权 root**：Dockerfile 加 nobody + `USER`，compose 加 read_only/cap_drop/no-new-privileges/绑定回环。

### 建议排期（P2/P3，涉及部署形态/默认值）

5. **S-03** systemd `LoadCredential` 让含密码配置回归 0600（含 install.sh/DEPLOY 文档联动）。
6. **S-05** schema 白名单；**S-06** 拨号侧 IP 复核（可与 S-01 同一个改造合并）。
7. **P-04** log.level 默认改 info。
8. **S-08** 0.0.0.0 无认证启动告警 + 基础安全响应头。

### 明确不建议做

- 长会话/会话池复用（P-03）：牺牲会话安全与热重载语义，收益不抵风险。
- 为形式统一把 datacenter/cluster 也做 Desc 改造（P-02）：实体数小，扩大风险面无收益。
- 引入 pprof 前先确认端口暴露面（P-06）：要加也必须挂在独立本地端口或鉴权后，不能进默认 mux。

---

## 附：本次审计复现命令

```bash
# 漏洞（可达性口径，CI 同款）
GOPROXY=https://goproxy.cn,direct CGO_ENABLED=0 go run golang.org/x/vuln/cmd/govulncheck@latest ./...
# 静态检查 / 测试
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test ./...
# S-01 绕过验证：对 target "allowed.example.com:443@169.254.169.254"
#   splitHost() → "allowed.example.com"（命中 .example.com 后缀，放行）
#   soap.ParseURL("https://"+target+"/sdk").Host → "169.254.169.254"（实际连接地址）
```
