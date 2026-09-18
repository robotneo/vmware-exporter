# vmware-exporter 技术栈与模块清单（v0.2.0 / ee918c2）

- 基线：`master @ ee918c2`（v0.2.0 生命周期指标 + 新版 dashboard 已合并）
- 扫描日期：2026-09-10
- 扫描方式：全量人工走读 38 个非测试 Go 文件（8,311 行生产代码）+ 28 个测试文件（10,863 行 / 197 个 Test），辅以 `go vet`、`go test ./...`、`govulncheck`
- 上游：`github.com/prezhdarov/vmware-exporter`（fork，采集调度层已自研替换上游框架）

---

## 1. 技术栈

| 类别 | 选型 | 版本 | 备注 |
|---|---|---|---|
| 语言 | Go | go 1.26.0 / toolchain go1.26.6 | toolchain 钉死 1.26.6：1.26.0–1.26.5 标准库有 7 个可达公告（net/http、crypto/tls、encoding/*），CI 用 go.mod 装版本会直接挂漏洞扫描 |
| vSphere SDK | govmomi | v0.56.0 | SOAP/vim25、view.ContainerView、performance.Manager、cache.Session、soap.ParseURL |
| 指标框架 | prometheus/client_golang | v1.23.2 | 自实现 prometheus.Collector（每请求一个 Registry），非远程写 |
| Web/TLS/认证 | prometheus/exporter-toolkit | v0.16.0 | 仅用其 web.ListenAndServe + web.config（TLS / HTTP Basic Auth） |
| 结构化日志 | prometheus/common（promslog） | v0.69.0 | 封装 log/slog，支持 logfmt/json、SIGHUP 热改级别 |
| 并发原语 | golang.org/x/sync | v0.22.0 | errgroup.SetLimit（三层并发闸）、singleflight（缓存击穿合并） |
| 配置文件 | gopkg.in/yaml.v3 | v3.0.1 | `-file` 读 flag 格式 YAML；envflag 支持环境变量 |
| 前端 | 原生 HTML/CSS/JS（无框架） | — | go:embed 打包进二进制（/、/debug、/config、静态资产） |
| 容器 | `FROM scratch` | — | 仅 COPY 二进制 + ca-certs，当前无非特权 USER（见安全方案 S-04） |
| 部署 | systemd（DynamicUser 硬ening）、docker-compose | — | `packaging/systemd/` 为唯一 systemd 事实来源 |
| CI | GitHub Actions + govulncheck + go vet/test | — | `.github/workflows/test.yml`，漏洞按可达性口径 |
| 构建产物 | 静态二进制（CGO_ENABLED=0） | — | 本机 darwin 无 C 工具链，race/cgo lint 靠 CI Linux |

可观测性输出：Prometheus 文本 exposition，端点 `/metrics`（单 vCenter 服务级凭证）、`/probe`（多 target、每请求独立凭证）。

---

## 2. 目录与包职责

```
cmd/vmware-exporter/      package main —— 进程装配层（flag、HTTP 路由、信号、UI 挂载）
internal/
  collector/              自研采集调度框架（替换上游 prometheus-exporter/pkg/collector）
  config/                 flag 注册中心 + 配置文件/envflag 解析 + SIGHUP 安全快照
vmware/
  api/        package vmware           —— vCenter 登录/会话/flag 校验/目标类型探测
  collectors/ package vmwareCollectors —— 10 个 collector + 性能管线 + Desc 工厂
  esxcli/     package esxcli           —— ManagedMethodExecuter SOAP 低层封装
web/          package web（go:embed）  —— 首页/调试控制台/配置生成器静态资产
packaging/    systemd unit + config.yaml + install/uninstall 脚本
scripts/      构建/打包/一致性校验（check_config.py）
dashboards/   Grafana 面板（v0.2.0 已迁 VictoriaMetrics 概览/详情 + 生命周期行）
docs/         设计/指标/审计文档
```

### 2.1 cmd/vmware-exporter（377+… 共 8 个生产文件）

| 文件 | 行数 | 职责 |
|---|---|---|
| `main.go` | 278 | 全部 flag 定义（约 30 个）、usage、HTTP server 超时（ReadHeader 5s/Idle 60s，刻意无 WriteTimeout）、mux 装配、SIGHUP 协程启动顺序 |
| `handlers.go` | 377 | `/metrics` 与 `/probe` 两个 handler、`parseCollectors`（collect[]/nocollect[]/all + 未知名 400）、每请求 Registry 组装、进程级 scrapeErrors 与 in-flight 闸 |
| `settings.go` | 74 | `exporterSettings`：请求路径上一次快照多个热改 flag（target 开关/并发/TTL/inflight），消除半新半旧撕裂 |
| `reload.go` | 106 | SIGHUP 处理：影子 FlagSet 重解析 + 原子提交 + 失败回滚 + reload 成功/时间指标 |
| `targetfilter.go` | 96 | `/probe` SSRF 白名单纯内核（后缀/CIDR/精确三规则）+ splitHost；**S-01 绕过点在此** |
| `gate.go` | 52 | `scrapeGate`：进程级同时抓取数信号量，满员 503 不排队 |
| `ui.go` | 256 | go:embed 资产挂到 mux，/、/debug、/config、静态文件；debug 控制台受 `-web.debug-console` 开关控制 |

### 2.2 internal/collector（自研调度层，8 个生产文件）

| 文件 | 行数 | 职责 |
|---|---|---|
| `collector.go` | 101 | `Collector` 接口（`Update(ctx, ch, *Scrape) error`，**带 ctx**）、`Definition{Name,Creator,DefaultEnabled}`、启用常量、SkipReason 常量、RegisterFlag/Registered 开关登记 |
| `set.go` | 497 | `CollectorSet`：实现 prometheus.Collector。登录失败产 up=0 + 每 collector success=0；errgroup.SetLimit 并发调度（刻意非 WithContext，单 collector 失败不连坐）；`safeUpdate` 把子协程 panic 收成 error（panic 不跨协程，不拦会崩整个进程）；自监控指标全套（up/duration/success/errors/entities found/emitted/skipped） |
| `scrape.go` | 213 | `Scrape` 结构体（一轮抓取的全部会话状态，编译期契约取代旧 13-key map）；HostSystem 请求内 `sync.Once` 共享（三 collector 的三次 ContainerView 合一）；`HostConcurrency()`、`APIVersion()` 空值安全 |
| `throttle.go` | 65 | `throttledRoundTripper`：装在 vim25 client 上，**令牌只包单次 SOAP 往返**，把嵌套 fan-out（collector×host×NIC）真正同时在飞数钉死，防止 n×n |
| `inventorycache.go` | 142 | `InventoryCache`：进程级、按 target 分桶、TTL + singleflight 合并未命中、过期项删除防 map 膨胀、失败不缓存、泛型 `FetchInventory[T]`。**/probe 不注入（越权读边界）** |
| `entitystats.go` | 133 | `EntityStats`：本轮 found/emitted/skipped 并发汇聚，reason 预填 0 值保序列连续 |
| `errors.go` | 71 | `ScrapeErrors`：进程级跨请求 counter（每请求新建实例会让 counter 每轮归零），按 target 分桶，Snapshot seed 0 值防首报漏报 |

### 2.3 internal/config（1 个生产文件，452 行）

- `config.go`：自定义 flag 解析（返回 error 而非 log.Fatal）、`-file` YAML 装载、envflag（`-envflag.enable/prefix`）、`Snapshot(fn)`（RWMutex 保护请求路径读 flag 指针）、`Reload()`（影子集重放 → 校验 → 原子切换，失败回滚）、SetLogger、ValidateFlags 联动。
- 解决的核心问题：SIGHUP 写 `*flag` 指针与请求协程读指针之间的数据竞争。

### 2.4 vmware/api（package vmware，2 个生产文件）

| 文件 | 行数 | 职责 |
|---|---|---|
| `vmware.go` | 324 | vmware.* flag（user/pass/vcenter/schema/insecureTLS/interval/granularity/timeout/perf.chunk-size）、`Login`/`LoginWithCredentials`：soap.ParseURL → cache.Session（Passthrough=true）→ Login → `CounterInfoByName`（每轮一次，见性能方案 P-03）→ 构造 Scrape；cleanup 顺序关键（先独立短超时 Logout 再 cancel，防服务端会话挂 30 分钟）；ValidateFlags 防除零/空 interval |
| `targettype.go` | 44 | `detectTargetType`：依 ServiceContent.About 区分 vcenter/esxi（影响历史采样语义，ESXi 不聚合历史统计） |

### 2.5 vmware/collectors（package vmwareCollectors，17 个生产文件）

注册表 `registry.go`（81 行）是唯一 collector 清单来源，/metrics 与 /probe 共用：

| collector | 默认 | 文件 | 数据面 |
|---|---|---|---|
| datacenter | 启用 | datacenter.go (122) | target/vcenter info + datacenter/folder 拓扑（走缓存） |
| cluster | 启用 | cluster.go (181) | 集群 info、有效主机数、*_capacity_*_hertz/bytes、overall_status（走缓存） |
| datastore | 启用 | datastore.go (145) | summary 容量/可访问、host/vm 关联（走缓存）；`regexp.MustCompile` 每轮重编译（新发现微热点 P-07） |
| host | 启用 | host.go (258) | HostSystem 全量：info/生命周期状态（power/connection/maintenance/overall_status）/容量/硬件/perf 实时 |
| vm | 启用 | vm.go (238) | VM info（含 uuid 稳定关联）、power_state、guest、perf 实时；迁移/重命名仍可追踪 |
| resourcepool | 启用 | resourcepool.go (286) | RP 层级与 limit/reservation（一次 ContainerView，成本同 cluster） |
| esxcli.host.nic | **禁用** | esxclihostnic.go (190) | 逐主机 MME+list、逐网卡 get（N+1 SOAP）；热循环内 NewDesc（P-01） |
| esxcli.storage | **禁用** | esxclistoragelist.go (142) | 逐主机 MME+list 存储设备；同上 NewDesc 热点 |
| vsan | **禁用** | vsan.go (686) | 组 A：容量/健康/resync 三次轻量查询；未启用 vSAN 优雅降级只产 enabled 0 |
| vsan.perf | **禁用** | vsanperf.go (571) | 逐实体类型 CSV 查询解析；**请求侧 label 白名单**（79 裁到十几个）控基数；含 `vsanPerfDesc` Desc 缓存范式 |

辅助文件：

| 文件 | 行数 | 职责 |
|---|---|---|
| `descs.go` | 829 | 全项目最大文件。Desc 工厂 + descsFor/descsCache（每轮缓存）；host/vm/perf 的「循环外 Desc + variableLabels」改造（P2-4 范式）；perfDesc 含单位/改名映射 |
| `scrape.go` | 435 | 性能管线：属性检索（fetchInventoryCached/fetchProperties）、QueryPerf **分块**（chunk-size 默认 64）+ 有界并发 + 按实体顺序合并；计数器 id 直构避免每块 SampleByName 往返；delta 按 StatsType 求和；历史样本尾部截断 |
| `perfnames.go` | 206 | 计数器名 → metric 名/单位映射表（translatePerfCounter）、legacy 改名 |
| `targettype.go` | 180 | collector 侧 isESXi/synthetic datacenter 判定 |
| `host_eligibility.go` | 62 | 数据面资格统一判定（poweredOn+connected+!maintenance）+ 跳过原因 map，三处复用防漂移 |
| `versionset.go` | 46 | driver→版本并发去重（原子 test-and-set） |

### 2.6 vmware/esxcli（package esxcli，3 个生产文件，267 行）

- `esxcli.go` (135)：`Run()` 在主机上执行 esxcli 命令；空命令防护；**三层空值判断**（x / x.Returnval / fault）把「ESXi 回合法 SOAP 但无 returnval」从 nil 解引用 panic 改为 `ErrEmptyResponse` sentinel（以前一台异常 ESXi 能崩全进程）。
- `methods.go` (49)：RetrieveMME / ExecuteSoap 三个 SOAP body 的 RoundTrip 封装。
- `types.go` (83)：请求/响应 XML 类型。

### 2.7 web（go:embed，1 个 Go 文件 105 行 + 9 个静态资产）

- 首页、`/debug` 交互调试控制台（POST 提交凭证，不进 URL/历史/代理日志）、`/config` 配置生成器（口令占位 `<password>`、默认脱敏、有测试）。
- 凭证处理纯浏览器本地，无服务端回传。i18n（中英）、clipboard、app/config 逻辑分离。

---

## 3. 端到端数据流（一次抓取）

```
Prometheus / 浏览器
   │  GET /metrics（服务级凭证） 或 POST/GET /probe（target+每请求凭证）
   ▼
http.Server（ReadHeaderTimeout=5s, IdleTimeout=60s；TLS/Basic 由 exporter-toolkit）
   ▼
handler：settings 一次快照（RWMutex）
   ▼
scrapeGate.tryAcquire(web.max-scrape-inflight=4)  满 → 503（不排队）
   ▼
/probe 专属：ParseForm → target 必填 → targetAllowed 白名单（S-01 缺陷）
             → 凭证（URL 参数或 Basic Auth）→ collector 名未知 → 400
   ▼
NewCollectorSet（每请求新 Registry + 按 Enabled 过滤的 collector 实例）
   ▼
CollectorSet.Collect：
   1) Login（/metrics 用全局 flag；/probe 用 probeLogin 绑定的请求凭证）
      soap.ParseURL → cache.Session.Login → CounterInfoByName → 构造 Scrape
      失败 → vmware_up=0 + 每个 collector success=0（可告警，而非空响应）
   2) 登录后注入 Namespace/并发/缓存/EntityStats，装 throttledRoundTripper
   3) errgroup.SetLimit(8) 并发跑启用的 collector；safeUpdate 兜 panic
      ├─ 拓扑/容量类：fetchInventoryCached → InventoryCache（TTL 5m, singleflight）
      │                /probe 为 nil → 实时检索
      ├─ HostSystem：Scrape.Hosts() sync.Once 三 collector 共享
      ├─ perf：计数器 id 直构 → QueryPerf 分块(64)+有界并发→顺序合并
      └─ esxcli/vsan：默认禁用；启用时受全局 SOAP 闸约束
   4) g.Wait() 后输出 EntityStats（found/emitted/skipped by reason）
   5) cleanup：独立超时 Logout → cancel（顺序不可换）
   ▼
promhttp.HandlerFor（ContinueOnError）文本编码输出
```

四层并发防护（全部有界）：
1. `web.max-scrape-inflight`（进程级同时抓取数，默认 4，满 503）
2. collector 层 `errgroup.SetLimit`（`-collector.max-concurrency` 默认 8）
3. per-host fan-out 层 SetLimit（复用同一预算，未配置下限 8）
4. RoundTripper 单次往返令牌闸（钉死嵌套 fan-out 的真实在飞数，杜绝 n×n）

---

## 4. 配置面（约 30 个 flag）

| 分组 | 关键 flag | 默认 | 热改 |
|---|---|---|---|
| 监听/运维 | `http.address` `web.config.file` `web.debug-console` `log.level` `log.format` | :9169 / 无 / true / **debug** / logfmt | log.* SIGHUP |
| 并发/负载 | `collector.max-concurrency` `web.max-scrape-inflight` | 8 / 4 | ✅ SIGHUP |
| 抓取/缓存 | `vmware.timeout` `vmware.interval` `vmware.granularity` `vmware.perf.chunk-size` `scrape.inventory-cache-ttl` | 60s / 20s / 20s / 64 / 5m | ✅ SIGHUP |
| 目标/凭证 | `vmware.vcenter` `vmware.username` `vmware.password` `vmware.schema` `vmware.insecureTLS` | 空 / 空 / 空 / https / false | ✅ SIGHUP（/metrics） |
| 采集开关 | `collector.{datacenter,cluster,datastore,host,vm,resourcepool}` 默认 true；`collector.esxcli.host.nic` `collector.esxcli.storage` `collector.vsan` `collector.vsan.perf` 默认 false | — | ✅ |
| Probe 安全 | `probe.allowed-targets` | 空（放开） | ✅ |
| 其他 | `disable.exporter.target` `disable.exporter.metrics` `metrics.legacy` `vmware.vsan.interval` `collector.vsan.perf.skip-verify` `envflag.*` `-file` | — | 部分 |

配置来源优先级：命令行 > 环境变量（envflag）> `-file` YAML。所有运行期读取热改 flag 的路径都必须经 `config.Snapshot` 或 settings 快照。

---

## 5. 指标面概览

- 命名空间 `vmware`，约 45+ gauge + 1 个 counter 族（`vmware_scrape_errors_total`）。
- v0.2.0 生命周期/状态族：`vmware_{vm,host}_power_state`、`host_connection_state`、`host_maintenance_mode`、`*_overall_status`（有界枚举）、集群 `*_effective_hosts`、`*_capacity_*_hertz/bytes`、`*_info` 的 `uuid` label、`vmware_scrape_entities_{found,emitted,skipped}`。
- 自监控：`vmware_up`、`vmware_scrape_duration_seconds`、`vmware_scrape_collector_duration_seconds{collector}`、`vmware_scrape_collector_success{collector}`、`vmware_exporter_config_last_reload_successful/timestamp`、`vmware_exporter_build_info`。
- 基数结论：新增 state/status/reason 全为有界枚举，uuid 与实体 1:1，无基数失控；历史高基面点仅 esxcli 描述性 label 与 vsan_perf entityid，二者默认禁用且有缓解。

---

## 6. 测试与质量基线

- 28 个测试文件 / 10,866 行 / 197 个 Test；测试与被测代码同目录同包（项目约定）。
- 覆盖重点：调度/闸/节流/缓存/错误计数、targetfilter 绕过用例、perf 分块与 golden 迁移映射、lifecycle、registry 与 flag 一致性、UI 脱敏、config 重载回滚。
- 本扫描日复核：`CGO_ENABLED=0 go vet ./...` 干净；`go test ./...` 全绿；`govulncheck` = 0 个可调用漏洞（1 个间接依赖漏洞不可达）。
- 已知环境限制：本机（darwin）无 C 工具链（xcrun/CLT 损坏），`go test -race` 与 cgo lint 本地不可跑，依赖 CI Linux。

---

## 7. 架构上已经做对的关键决策（再投资时不要破坏）

1. **ctx 贯通**：从 http.Request 到每个 SOAP 调用，断连/超时真正取消上游。
2. **登录失败产 up=0**，而非空响应——凭证错误变成可告警时间序列。
3. **单一 collector 清单 + 两条路径共用 CollectorSet**，杜绝 /metrics 与 /probe 行为分叉。
4. **配置热重载无数据竞争**（RWMutex 快照 + 影子集原子提交回滚）。
5. **/probe 不注入进程缓存**——多租户越权读边界。
6. **panic 收敛在子协程边界**（safeUpdate）+ esxcli 空响应不 panic。
7. **会话 Logout 独立超时且先于 cancel**，防 vCenter 会话表堆积。
8. **凭证零日志**（刻意不记 username），命令行不摆密码（-file/envflag）。

待优化点见同批两份方案：`PLAN-performance-cpu-mem-v0.2.0.md`、`PLAN-security-v0.2.0.md`。
