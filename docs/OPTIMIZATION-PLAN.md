# vmware-exporter 优化与能力扩展设计方案

> 状态：**已全部实施完毕**（Stage 1 ~ 10 均已合并到 `master`）。
> 本文件自此转为**历史决策记录**，保留当初的分析、取舍与理由，
> 供后续回溯"当时为什么这么定"。**不要再把它当作待办清单读。**
>
> 编写日期：2026-09-01
> 编写时的基线：`master` @ `4a1013e`，tag `v0.1.18-beta.1`
> 编写时的依赖：`govmomi v0.55.0`、`prezhdarov/prometheus-exporter v0.1.5`、Go 1.26
>
> **实施后的现状（与上面的编写时基线不同，以此为准）**：
> - 上游框架 `prezhdarov/prometheus-exporter` **已完全移除**，调度层由
>   `internal/collector` 自行实现。
> - `govmomi` 已升至 **v0.56.0**。
> - collector 从 7 个增至 **10 个**（新增 `resourcepool`、`vsan`、`vsan.perf`）。
> - flag 从 26 个增至 **31 个**。
> - 后续增量的设计见 `docs/DESIGN-resourcepool-vsan.md`，
>   能力缺口分析见 `docs/COVERAGE-GAP-ANALYSIS.md`。

---

## 0. 本方案要解决什么

两件事，分开评审、分开交付：

| 主题 | 性质 | 覆盖章节 |
| --- | --- | --- |
| 存量代码的正确性/安全/一致性缺陷修复 | 改存量 | 第 2 ~ 4 章 |
| 新增单台 vSphere(ESXi) 直连采集支持 | 加增量 | 第 5 ~ 7 章 |

采集模式从 2 种扩展为 3 种：

| # | 模式 | 端点 | 凭证来源 | 现状 |
| --- | --- | --- | --- | --- |
| 1 | 单 vCenter | `/metrics` | 全局 flag | 已有 |
| 2 | 多 vCenter（每 target 独立凭证） | `/probe` | URL 参数 / Basic Auth | 已有 |
| 3 | **单台 ESXi 直连** | `/metrics` + `/probe` 均需支持 | 同上 | **本次新增** |

---

## 1. 分支与交付策略

### 1.1 硬性约束

- **禁止在 `master` 上直接 coding**。
- 所有开发在 `dev` 分支进行。
- 每个阶段（Stage）在 `dev` 上独立成一个或多个 commit，阶段完成即可评审。
- 全部阶段验收通过后，`dev` 合并回 `master`，最后 push 到 GitHub。

### 1.2 分支流程

```
master (4a1013e, v0.1.18-beta.1)
   │
   ├─ git switch -c dev
   │
   │   Stage 1  P0 正确性修复          → commit(s)
   │   Stage 2  双路径收敛为单一调度    → commit(s)
   │   Stage 3  ESXi 直连支持（路线丙） → commit(s)
   │   Stage 4  可观测性与指标语义      → commit(s)
   │   Stage 5  安全加固               → commit(s)
   │   Stage 6  清理与 CI 加固          → commit(s)
   │
   └─ 验收通过后 merge --no-ff 回 master → tag → push origin
```

### 1.3 提交与合并规范

- Commit message 沿用仓库现有的 Conventional Commits 风格（历史中已有 `fix:` / `chore:` / `feat:`）。
- 合并用 `git merge --no-ff dev`，保留阶段边界，便于回溯与回滚。
- 建议合并后打 tag `v0.2.0`：本次含破坏性变更（指标单位修正、label 调整），按语义化版本应升 minor 而非 patch。
- **不使用 `--force` push**，不重写已推送的历史。

### 1.4 回滚预案

每个 Stage 都是独立 commit 区间，任一阶段验收不通过可单独 `git revert`，不影响其他阶段。Stage 之间的依赖关系：

```
Stage 1 ──> Stage 2 ──> Stage 3
                 └─────> Stage 4
Stage 5 / Stage 6 独立，可任意顺序
```

Stage 3（ESXi）依赖 Stage 2（路径收敛）先落地，否则要在两套调度里各写一遍适配逻辑。

---

## 2. 审计发现清单

以下每条均已核对源码，标注了文件与行号。部分结论交叉验证了依赖库源码（`govmomi@v0.55.0`、`prometheus-exporter@v0.1.5`），非经验推测。

### 2.1 P0 — 正确性缺陷，会导致线上故障

#### P0-1 vCenter 会话泄漏

- **位置**：`vmware/api/vmware.go:233-237`（登录）、`vmware/api/vmware.go:132-145`（登出）
- **现象**：登录时设置 `cache.Session{Passthrough: true}`，但 `VMware.Logout()` 只调用 `cancel()`，从未发起 SOAP Logout。
- **依据**：`govmomi/session/cache/session.go:337-348` 显示，**仅当** `Passthrough == true` 时 `cache.Session.Logout()` 才会真正执行 `session.NewManager(client).Logout(ctx)`。当前代码持有了 Passthrough 语义却没有履行登出义务。
- **后果**：每次抓取在 vCenter 侧残留一个会话，直到服务端会话超时（默认 30 分钟）才回收。按 20s 抓取间隔计算，单 target 稳态会堆积约 90 个会话；多 target 场景会更快触达 vCenter 会话上限，最终登录被拒、采集全面中断。
- **修复要点**：把 `*cache.Session` 存入 `loginData`，`Logout()` 中**先**发 SOAP Logout **再** `cancel()`。顺序不可颠倒——ctx 取消后请求无法发出。

#### P0-2 ctx 超时被误用作会话生命周期

- **位置**：`vmware/api/vmware.go:231`
- **现象**：`context.WithTimeout(context.Background(), time.Duration(*vmwInterval-2)*time.Second)`，该 ctx 被存入 `loginData["ctx"]` 并供后续**全部**采集操作使用。
- **问题**：`-vmware.interval` 的语义是「采样频率」（喂给 PerfManager 的 `IntervalId`），却被用来推导「单次抓取的超时时间」。默认 interval=20 → ctx 仅存活 18 秒。
- **后果**：
  - 大规模环境（数千 VM）单轮采集耗时超过 18s 时，中途 `context deadline exceeded`，指标残缺且错误信息指向性差。
  - `interval < 2` 时超时为**负值**，请求立即失败。
  - 想延长超时就必须调大采样间隔，两个本应独立的参数被强行绑定。
- **修复要点**：新增独立的 `-vmware.timeout`（默认 60s）表达单次抓取超时；`-vmware.interval` 回归其原本语义，仅用于 PerfManager。

#### P0-3 除零 panic 与静默采空

- **位置**：`vmware/api/vmware.go:264`
- **现象**：`loginData["samples"] = int32(*vmwInterval / *vmGranularity)`，全程无参数校验。
- **后果**：
  - `-vmware.granularity=0` → 整数除零，进程 panic 崩溃。
  - `granularity > interval` → `samples = 0` → `PerfQuerySpec.MaxSample=0`，性能指标全部采不到，但**不报任何错误**，属静默失败。
- **修复要点**：启动时 fail-fast 校验并打印明确原因。

### 2.2 P1 — 可靠性与安全

#### P1-1 测试假阳性，probe 逻辑零覆盖

- **位置**：`vmware-exporter_test.go:52-82`
- **现象**：两个测试命名为 `TestProbeHandlerReturnsBadRequestWithoutTarget` / `TestProbeHandlerWithTargetReturnsMetricsPayload`，但实际调用的是**依赖库**的 `exporter.CreateHandleFunc`，而非本项目的 `probeHandler`（`vmware-exporter.go:203`）。
- **后果**：项目真正的 probe 逻辑——凭证解析、Basic Auth 回退、`parseCollectors`、登录失败分支、登出 defer——**一行都未被覆盖**，但 CI 显示测试通过。这比缺少测试更危险：它提供了虚假的安全感。
- **修复要点**：改为直连 `probeHandler`，并补齐凭证与 collector 选择的分支覆盖。

#### P1-2 明文凭证入库，密码可经 URL 传输

- **位置**：`docker-compose.yml:12-14`、`vmware.conf`
- **现象**：包含疑似真实的 vCenter 账号密码与内网 IP，且已被 git 跟踪（历史提交中同样存在）。
- **附加风险**：`/probe?password=xxx` 的查询参数会被写入反向代理与 Prometheus 的 access log。
- **修复要点**：仓库内改为占位符 + `.example` 后缀；文档明确推荐 Basic Auth 而非 URL 参数；提示用户轮换已泄露凭证。**注意**：清理 git 历史属破坏性操作，需单独确认，不纳入本次自动执行范围。

#### P1-3 Exporter 自身无法启用 TLS 与鉴权

- **位置**：`vmware-exporter.go:55-65`
- **现象**：`webConfig()` 将 `WebConfigFile` 硬编码为 `""`。
- **后果**：exporter-toolkit 原生支持 web-config 文件（TLS 证书 + basic auth），此处被写死导致该能力完全不可用。而 `/probe` 恰恰是需要传输凭证的端点，只能裸 HTTP 暴露。
- **修复要点**：暴露 `-web.config.file` flag 并透传。

#### P1-4 高基数标签

- **位置**：`vmware/collectors/vm.go:118-127`、`vmware/collectors/datastore.go:67`
- **现象**：
  - `vmware_vm_snapshot_info` 将 `created`（RFC3339 时间戳字符串）作为 label——每个快照产生一条独立时间序列，且快照删除后序列变为僵尸。
  - `vmware_datastore_info` 将完整 URL 经正则处理后塞入 `pfinstance`。
- **依据**：将时间戳作为 label 是 Prometheus 官方明确列出的反模式，会导致 TSDB 基数爆炸。
- **修复要点**：时间戳信息已经存在于 metric **value** 中（`rootSnap.CreateTime.Unix()`），label 里的 `created` 完全冗余，直接移除即可，不损失任何信息。

#### P1-5 probe 路径串行执行且无自监控

- **位置**：`vmware-exporter.go:99-145`
- **现象**：`vmwareCollector.Collect()` 用 `for` 循环**串行**遍历 collector。
- **对比**：`/metrics` 走框架的 `CollectorSet.Collect`（`prometheus-exporter@v0.1.5/pkg/collector/collect.go:28-57`），是 `sync.WaitGroup` **并发**执行，且额外产出 `vmware_scrape_collector_duration_seconds` 与 `vmware_scrape_collector_success` 两个自监控指标。
- **后果**：
  - probe 模式采集耗时约为 metrics 模式的 3~5 倍（7 个 collector 串行累加）。
  - probe 模式下**没有** `_success` 指标，单个 collector 失败在监控层面完全不可见，只能翻日志排查，无法配置告警。
- **修复要点**：见第 3 章 Stage 2。

#### P1-6 CI 缺失 lint 与竞态检测

- **位置**：`.github/workflows/test.yml`
- **现象**：
  - 仓库存在 `.golangci.yml`（启用 govet/staticcheck/ineffassign/errcheck/misspell/unused 六个 linter），但 CI **从不调用** golangci-lint。
  - `go test ./...` 未加 `-race`，而 `host.go`、`vm.go`、`esxclihostnic.go` 中存在多个 goroutine 向同一 metric channel 并发写入。
- **修复要点**：CI 增加 lint job 与 `-race` 测试。

### 2.3 P2 — 质量与维护性

| # | 问题 | 位置 | 说明 |
| --- | --- | --- | --- |
| P2-1 | 指标单位标注错误 | `host.go:135` | help 写 "Amount of RAM in MB"，但 govmomi `Summary.Hardware.MemorySize` 返回**字节**。指标名 `mem_capacity` 亦未带单位后缀 |
| P2-2 | help 文案错抄 | `vm.go:107` | `vm_datastore_capacity_used` 的 help 是 "Virtual memory configured in MB"，与实际含义（datastore 已用容量）无关 |
| P2-3 | 采样窗口不自洽 | `datastore.go:110` | 硬编码 `IntervalId=300`（5 分钟历史间隔），但仍传入按 20s 频率算出的 `samples`，两者语义冲突。代码注释自称 "A dirty workaround" |
| P2-4 | Desc 在热路径重复构造 | 全部 collector | 每个 VM/Host 每轮采集都重新 `prometheus.NewDesc(...)`，产生大量可避免的内存分配 |
| P2-5 | 死代码 | `vmware/api/` | `clusters.go`、`datastores.go`、`host.go`、`vm.go`、`inventory.go` 五个文件内容整体被块注释包裹，是早期 REST 方案遗留 |
| P2-6 | 互斥锁无效 | `esxclihostnic.go:160-200` | 读检查在锁外、写在锁内，非原子操作；且并发调用已被注释改回串行（`:129-141`），锁实际无任何作用 |
| P2-7 | Dockerfile 构建脆弱 | `Dockerfile:15` | `go build ... vmware-exporter.go` 仅编译单个文件，main 包新增文件会被静默漏掉 |
| P2-8 | `Describe()` 空实现 | `vmware-exporter.go:94-96` | 导致 registry 无法执行重复注册校验 |

---

## 3. Stage 1 — P0 正确性修复

**目标**：消除会导致进程崩溃、会话耗尽、采集中断的三类缺陷。
**风险**：低。仅新增 flag，不改变既有 flag 语义，向后兼容。
**依赖**：无，可立即开始。

### 3.1 修复会话泄漏（P0-1）

改动 `vmware/api/vmware.go`：

- `govmomiLoginWithCreds()` 中把构造出的 `*cache.Session` 与 `*vim25.Client` 一并存入 `loginData`（键名 `session`）。
- `Logout()` 改为：

  1. 从 `loginData` 取出 `*cache.Session` 与 `*vim25.Client`；
  2. 调用 `session.Logout(ctx, client)` 发起 SOAP 登出，失败仅记录 error 不中断；
  3. **随后**执行 `cancel()`。

- 顺序约束写入代码注释，避免后续维护者调换。

**边界处理**：`Logout()` 需容忍 `loginData` 中键缺失（登录中途失败的场景），用带 ok 的类型断言，不能裸断言。

### 3.2 解耦 ctx 超时与采样频率（P0-2）

- 新增 flag：`-vmware.timeout`，默认 `60`（秒），含义为「单次抓取的整体超时」。
- `context.WithTimeout` 改用 `*vmwTimeout`。
- `-vmware.interval` 语义收窄为「PerfManager 采样间隔」，不再参与超时计算。
- README / README-zh 同步更新两个参数的说明，明确区分。

**兼容性**：未显式设置 `-vmware.timeout` 的现有部署，超时从 18s 放宽到 60s，属行为改善而非破坏。需在 CHANGELOG 中说明。

### 3.3 参数校验 fail-fast（P0-3）

在 `main()` 中 `config.Parse()` 之后、启动 HTTP 服务之前插入校验函数：

| 参数 | 约束 | 不满足时 |
| --- | --- | --- |
| `-vmware.granularity` | `> 0` | 退出并提示不可为 0 |
| `-vmware.interval` | `>= granularity` | 退出并提示 samples 会为 0 |
| `-vmware.interval` | `>= 20` | 退出并提示 vCenter 硬限制（README 已载明） |
| `-vmware.timeout` | `> 0` | 退出并提示 |

校验失败时输出**具体的参数名、当前值、期望范围**，而非笼统的 "invalid config"。

> ESXi 模式下 `interval >= 20` 这条约束需要放宽——ESXi 的 `RefreshRate` 由服务端返回，通常为 20s 但不保证。此处的处理见第 5 章 5.3 节。

### 3.4 Stage 1 验收标准

- 新增测试：用 `simulator.VPX()` 起内存 vCenter，验证 `Logout()` 调用后服务端会话数归零。
- 新增测试：`granularity=0` 时校验函数返回错误而非 panic。
- 新增测试：`timeout` 与 `interval` 相互独立，改动其一不影响其二。
- `go test ./... -race` 通过。

---

## 4. Stage 2 — 双路径收敛为单一调度

**目标**：消除 `/metrics` 与 `/probe` 两套实现的行为差异。
**风险**：中。涉及调度逻辑重写，需完整回归。
**依赖**：Stage 1。

### 4.1 问题本质

同一份 collector 代码，当前存在两套调度器、两套开关体系、两套可观测性：

| 维度 | `/metrics` | `/probe` |
| --- | --- | --- |
| 调度实现 | 框架 `CollectorSet.Collect` | 项目自写 `vmwareCollector.Collect` |
| 执行方式 | 并发（WaitGroup） | 串行（for 循环） |
| 自监控指标 | 有 `_duration` / `_success` | **无** |
| collector 开关 | `-collector.xxx` flag，启动时固定 | `collect[]` / `nocollect[]` URL 参数 |
| 凭证来源 | 全局 flag | 每请求独立 |

根因：框架的 `ClientAPI.Login(target string, ...)` 签名只接受 target，天然容纳不了「每请求独立凭证」，因此 probe 场景当初被迫另写一套。

### 4.2 路线选择

评审需拍板，三个选项：

| 路线 | 做法 | 优点 | 代价 |
| --- | --- | --- | --- |
| **甲** | 只改本仓库：把 probe 路径补齐成与框架等价（并发 + 自监控），不碰上游 | 改动最小、最安全、不受上游进度制约 | 与框架逻辑有一定重复 |
| 乙 | 向上游 `prezhdarov/prometheus-exporter` 提 PR 扩展 `ClientAPI` 支持按请求传凭证 | 根治重复 | 需等上游合并，周期不可控 |
| 丙 | 将框架 519 行代码内联进本仓库自行维护 | 完全自主 | 接管维护成本，偏离上游后难同步 |

**本方案推荐路线甲**，理由：ESXi 支持（Stage 3）是本次的核心增量，不宜被上游协作周期阻塞；且路线甲保留了未来切换到乙的空间。

### 4.3 具体改动（按路线甲）

在项目侧抽出统一的采集调度函数，`/metrics` 与 `/probe` 共用：

- 输入：`loginData`、`namespace`、启用的 collector 集合、logger。
- 行为：
  - **并发**执行各 collector（WaitGroup，与框架行为一致）；
  - 每个 collector 产出 `vmware_scrape_collector_duration_seconds{collector="..."}`；
  - 每个 collector 产出 `vmware_scrape_collector_success{collector="..."}`；
  - 单个 collector 失败不影响其他 collector（保持现有容错语义）。
- `vmwareCollector.Describe()` 补上两个自监控 Desc，修掉 P2-8。

**并发安全前置检查**：现有 collector 已在内部使用 goroutine 向 `ch` 写入（`host.go:151`、`vm.go:140`）。改为外层并发后并发度提升，必须以 `-race` 验证。`prometheus.MustNewConstMetric` 本身是纯函数、channel 写入天然安全，理论上无共享状态，但 `esxclihostnic.go` 的 `driverMap`/`firmwareMap` 存在跨 goroutine 共享（P2-6），需在本阶段一并修正。

### 4.4 collector 开关体系统一

现状两套开关互不相通。收敛为：

- flag（`-collector.xxx`）决定**默认值**；
- URL 参数（`collect[]` / `nocollect[]`）在单次请求内**覆盖**默认值；
- 两个端点都遵循同一套解析逻辑（现有 `parseCollectors` 提取为公共函数并补测试）。

这样 `/metrics` 也能通过 URL 参数临时调整采集范围，行为一致且更灵活。

### 4.5 Stage 2 验收标准

- `/metrics` 与 `/probe` 对同一 target 采集，产出的指标集合**完全一致**（除凭证来源不同）。
- 两个端点均输出 `_duration` 与 `_success` 自监控指标。
- 单个 collector 人为失败时，`_success{collector="x"}` 为 0，其余 collector 仍为 1。
- probe 模式采集耗时显著下降（并发化收益），记录改造前后实测数据。
- `go test ./... -race` 通过。
- 新增测试直连项目自己的 `probeHandler`，修掉 P1-1 的假阳性。

---

## 5. Stage 3 — 新增单台 vSphere(ESXi) 直连采集

**目标**：支持直连单台 ESXi 主机采集，无需经由 vCenter。
**风险**：中高。涉及新增运行模式与 collector 行为分支。
**依赖**：Stage 2（路径收敛）必须先完成，否则要在两套调度里各写一遍适配。
**评审决策**：采用**路线丙** —— 新增 target 类型指标，collector 按类型自适应，同步更新 dashboard。

### 5.1 关键技术约束（已核实 govmomi v0.55.0 源码）

ESXi 直连与 vCenter 存在三处硬性差异，是本阶段全部设计的出发点：

| # | 差异 | 依据 | 影响 |
| --- | --- | --- | --- |
| C-1 | **对象模型缺层**：ESXi 无真实 Datacenter / Cluster，仅有隐式伪对象 `ha-datacenter`、`ha-folder-host`、`ha-folder-vm`、`ha-folder-datastore`、`ha-compute-res` | `simulator/esx/datacenter.go:18,37-40`、`simulator/esx/root_folder.go:19`；`simulator/model.go:141` 的 `ESX()` 模型中 `Datacenter`/`Cluster`/`ClusterHost` 全为零值，而 `VPX()`（`:157`）才有完整层级 | `datacenter`、`cluster` 两个 collector 在 ESXi 上会返回空或无意义占位值 |
| C-2 | **性能数据仅支持实时间隔**：`PerfProviderSummary` 中 `CurrentSupported` 与 `SummarySupported` 是两个独立布尔字段，ESXi 仅支持前者 | `vim25/types/types.go:62185-62206`。字段注释明确：支持实时统计时应将 `PerfQuerySpec.intervalId` 设为 `PerfProviderSummary.refreshRate`；历史间隔需 `SummarySupported` 为真 | `datastore.go:110` 硬编码的 `IntervalId=300`（5 分钟历史间隔）在 ESXi 上**必然查不到数据** |
| C-3 | **esxcli 调用路径实际不变** | `vmware/esxcli/esxcli.go:26-31` 构造 `ExecuteSoapRequest`，走 SOAP 的 `vim.EsxCLI.*` 方法，**并非 SSH** | 两个 esxcli collector 在 ESXi 直连下同样可用，且语义上更自然（省去 vCenter 中转） |

> 附带修正：原架构图将 `vmware/esxcli` 标注为「SSH 远程执行」，与源码不符，应为「经 SOAP 调用 EsxCLI 接口」。文档与图需一并更正。

**利好**：`simulator.ESX()` 可启动内存版 ESXi，因此本阶段全部回归测试**无需真实 ESXi 主机**，与现有 collector 测试使用 `simulator.VPX()` 的思路一致。

### 5.2 模式识别：不新增端点，靠探测分流

**设计原则**：ESXi 支持**不引入新的 HTTP 端点**，复用现有 `/metrics` 与 `/probe`。理由：

- 用户视角下「采集一个 target」的心智模型不应因目标类型而分裂；
- Prometheus 侧的 `scrape_config` 无需为 ESXi 单独配置一份；
- 避免第三套调度实现，与 Stage 2 的收敛目标一致。

**探测机制**：登录成功后读取 `client.ServiceContent.About.ApiType`：

| `About.ApiType` 取值 | 判定 | 说明 |
| --- | --- | --- |
| `VirtualCenter` | vCenter | 现有行为不变 |
| `HostAgent` | ESXi 直连 | 新增分支 |

判定结果写入 `loginData["targetType"]`（字符串 `vcenter` / `esxi`），供全部下游 collector 读取。

**为何用 `ApiType` 而非其他手段**：该字段由服务端在 `ServiceContent` 中直接返回，登录后即可获得，**无需额外 API 调用**，也不依赖对象树探测（后者在权限受限时可能误判）。

### 5.3 采样间隔自适应（修复 C-2 与 P2-3）

现有 `scrapePerformance()` 的 `IntervalId` 来源混乱：host/vm 传 `loginData["interval"]`，datastore 硬编码 `300`。改为统一策略：

1. 登录阶段对代表性实体调用 `perfManager.ProviderSummary(ctx, entity)`，取回 `RefreshRate` 与 `CurrentSupported` / `SummarySupported`，存入 `loginData`。
2. 采样间隔按下表决策：

| 目标类型 | 请求的间隔 | 实际使用 |
| --- | --- | --- |
| vCenter，且 `SummarySupported` | 300（datastore 等低频指标） | 300，保持现有行为 |
| vCenter，实时指标 | `-vmware.interval` | 校验后使用 |
| **ESXi** | 任意 | **强制使用 `RefreshRate`**（服务端权威值，通常 20） |

3. `samples` 随之重算：`samples = max(1, timeoutWindow / actualInterval)`，并保证**不小于 1**（顺带消除 P0-3 的静默采空）。

**参数校验放宽**：Stage 1 中 `interval >= 20` 的硬校验仅对 vCenter 生效。ESXi 模式下该值由服务端 `RefreshRate` 决定，用户传入值仅作为期望；若与服务端不符，记录 warn 并采用服务端值，不 fail-fast。

### 5.4 新增类型标识指标

新增一个信息型指标，作为 dashboard 条件渲染与告警分流的依据：

```
vmware_target_info{target="<host:port>", type="vcenter|esxi", version="...", build="..."} 1
```

**设计说明**：

- 由 `datacenter` collector 输出（它已在输出 `vmware_vcenter_info`，位置最自然）。
- 保留现有 `vmware_vcenter_info` 不删除，避免破坏既有 dashboard；新指标与其并存。
- `type` 是本阶段所有 dashboard 改造的唯一依赖点。

### 5.5 collector 逐个适配策略

| collector | ESXi 下行为 | 实现要点 |
| --- | --- | --- |
| `datacenter` | **输出 `ha-datacenter` 伪对象**，并额外打 `synthetic="true"` label | 保持指标存在性，使 dashboard 关联查询不断链；`synthetic` label 诚实标注数据来源，语义不含糊 |
| `cluster` | **输出 `ha-compute-res`**，同样打 `synthetic="true"` | 现有 `cluster.go:68` 已有 `len(clusters)==0` 时回退查 `ComputeResource` 的逻辑，本阶段将其从「意外兜底」升级为「显式分支」 |
| `datastore` | 正常采集；采样间隔改用 `RefreshRate` | 修复 C-2，同时消除 P2-3 的硬编码 300 |
| `host` | 正常采集 | ESXi 下 `hosts` 切片恒为 1 个元素；`host.Parent` 指向 `ha-compute-res` |
| `vm` | 正常采集 | 无差异 |
| `esxcli.host.nic` | 正常可用 | 走 SOAP，非 SSH（见 C-3） |
| `esxcli.storage` | 正常可用 | 同上 |

**`synthetic` label 的取舍说明**：给 `ha-*` 伪对象打标记，是为了让「ESXi 上不存在真实 Datacenter」这一事实在指标层面可见。用户若只想统计真实数据中心，可用 `vmware_datacenter_info{synthetic!="true"}` 过滤。这是路线丙相对路线乙（静默填充伪值）的核心价值。

### 5.6 Dashboard 改造（5 个 JSON）

**约束**：`dashboards/` 下 5 个文件共约 296 KB，均为 Grafana 导出的机器生成格式，嵌套深、单行长。

**方法**：**脚本化结构化注入，禁止手工编辑**。

1. 用 Python 脚本加载 JSON（保持 key 顺序），定位 `templating.list` 注入 `target_type` 变量，定位受影响 panel 的 `targets[].expr` 追加 label 过滤。
2. 改写后立即 `json.load()` 回读校验，确保结构合法。
3. 脚本纳入版本控制（`scripts/patch_dashboards.py`），改造过程可复现、可审查、可回滚。
4. **改动前先备份**原始 JSON，diff 逐个人工确认后再提交。

**改造范围（预估）**：

| Dashboard | 预期改动 |
| --- | --- |
| `vmware-vcenter-view.json` | 新增 `target_type` 模板变量；vCenter 专属 panel 增加 type 过滤 |
| `vmware-cluster-view.json` | 集群相关 panel 在 ESXi 下应提示「不适用」而非显示空图 |
| `vmware-datacenter` 相关 panel | 同上 |
| `vmware-host-view.json` | 基本无需改动（host 指标两模式一致） |
| `vmware-vm-view.json` | 基本无需改动 |
| `vmware-datastore-view.json` | 基本无需改动 |

> 若审计认为 dashboard 改动可延后，Stage 3 的代码部分可独立交付——新增的 `vmware_target_info` 指标向后兼容，不会破坏现有面板。

### 5.7 文档与配置样例

- README / README-zh 新增「三种采集模式」章节，含 ESXi 直连的启动示例与 Prometheus `scrape_config` 样例。
- 明确说明 ESXi 模式的能力边界：无 Datacenter/Cluster 真实数据、性能间隔由服务端决定。
- `docker-compose.yml` 增加 ESXi 直连的注释样例（凭证用占位符，见 Stage 5）。

### 5.8 Stage 3 验收标准

- 用 `simulator.ESX()` 起内存 ESXi，全部 7 个 collector 均能执行且不 panic。
- `vmware_target_info{type="esxi"}` 正确输出；vCenter 模式下为 `type="vcenter"`。
- ESXi 模式下 `datacenter` / `cluster` 输出带 `synthetic="true"` 的伪对象指标。
- ESXi 模式下 datastore 性能指标**能采到数据**（验证 C-2 修复生效，对比修复前应为空）。
- 用 `simulator.VPX()` 回归：vCenter 模式全部指标与改造前**完全一致**（除新增的 `target_info`）。
- 5 个 dashboard JSON 改造后均通过 JSON 解析校验。
- `go test ./... -race` 通过。

---

## 6. Stage 4 — 可观测性与指标语义修正

**目标**：修正指标语义错误与高基数隐患。
**风险**：中。**含破坏性变更**，需在 CHANGELOG 显式声明。
**依赖**：Stage 2。

### 6.1 指标单位与文案修正（P2-1、P2-2）

| 指标 | 当前问题 | 修正方案 |
| --- | --- | --- |
| `vmware_host_mem_capacity` | help 声称 MB，实际是**字节**（govmomi `Summary.Hardware.MemorySize` 返回 int64 字节） | **新增** `vmware_host_mem_capacity_bytes`，help 修正为字节；旧指标保留一个版本周期并在 help 中标注 deprecated |
| `vmware_vm_datastore_capacity_used` | help 错抄为 "Virtual memory configured in MB" | 修正 help 为「VM 在该 datastore 上已提交的容量（字节）」；同步新增 `_bytes` 后缀版本 |
| `vmware_host_cpu_capacity` | 单位为 MHz 但名称无体现 | 新增 `_mhz` 后缀版本 |

**兼容策略**：采用「新增带单位后缀指标 + 旧指标标注 deprecated」的双写过渡，而非直接改名。直接改名会瞬间打断所有既有 dashboard 与告警规则。双写一个版本周期后再移除旧指标。

### 6.2 消除高基数标签（P1-4）

`vmware_vm_snapshot_info` 移除 `created` label。

**为何可以安全移除**：该时间戳信息**已经存在于 metric value 中**（`vm.go:126` 的 `rootSnap.CreateTime.Unix()`），label 里的 RFC3339 字符串完全冗余。移除后不损失任何信息，但消除了「每个快照一条独立时间序列 + 快照删除后残留僵尸序列」的基数膨胀。

`vmware_datastore_info` 的 `pfinstance`（URL 派生）保留——它是 datastore 性能指标关联的必要 join key，基数等于 datastore 数量，可控。

### 6.3 性能优化：Desc 提出热路径（P2-4）

将各 collector 中在 `for` 循环内反复构造的 `prometheus.NewDesc(...)` 提升为包级变量或 collector 结构体字段，仅初始化一次。

**收益量化**：以 1000 台 VM 计，`vm` collector 单轮采集当前会构造约 4000 个 Desc 对象；改造后为 4 个。

### 6.4 Stage 4 验收标准

- 新旧指标并存，旧指标 help 含 deprecated 标注。
- `vmware_vm_snapshot_info` 不再含 `created` label，但 value 仍为创建时间戳。
- Desc 构造次数与实体数量无关（可通过 benchmark 或 pprof 验证）。
- CHANGELOG 明确列出破坏性变更清单与迁移指引。

---

## 7. Stage 5 — 安全加固

**目标**：消除凭证泄露面，支持传输层加密。
**风险**：低（代码改动小），但**涉及敏感信息处置**。
**依赖**：无，可与其他阶段并行。

### 7.1 仓库内凭证清理（P1-2）

| 文件 | 处置 |
| --- | --- |
| `docker-compose.yml` | 密码/IP 替换为占位符；重命名为 `docker-compose.example.yml`，`.gitignore` 加入实际使用的 `docker-compose.yml` |
| `vmware.conf` | 同上，改为 `vmware.conf.example` |

**必须提示用户的事项**（不在自动执行范围内）：

1. `docker-compose.yml` 与 `vmware.conf` 中的凭证已进入 git 历史，仅修改当前版本**不能**清除历史记录。
2. 若为真实环境凭证，**应立即在 vCenter 侧轮换密码**——这是唯一彻底的补救。
3. 清理 git 历史需 `filter-repo` 或 `filter-branch` 重写，属破坏性操作且会改变所有 commit hash，需单独评审后手动执行。

### 7.2 支持 Exporter 自身 TLS 与鉴权（P1-3）

- 暴露 `-web.config.file` flag，透传给 `web.FlagConfig.WebConfigFile`（当前被硬编码为空字符串，见 `vmware-exporter.go:58`）。
- 同时暴露 `-web.systemd-socket`（当前也被写死为 false）。
- 文档补充 exporter-toolkit web-config 文件样例（TLS 证书 + basic_auth_users）。

### 7.3 凭证传输方式建议

`/probe` 当前支持两种凭证传递方式，安全性不同：

| 方式 | 风险 | 建议 |
| --- | --- | --- |
| URL 查询参数 `?password=xxx` | 会被写入反向代理、Prometheus 的 access log | 文档标注为**不推荐**，仅作兼容保留 |
| HTTP Basic Auth | 不进 access log | **推荐**，文档置于首位 |

代码层面不移除 URL 参数支持（破坏兼容），但在检测到经 URL 传递密码时输出 warn 日志。

### 7.4 Stage 5 验收标准

- 仓库内无明文真实凭证。
- `-web.config.file` 可用，能成功启用 HTTPS 与 basic auth。
- URL 传密码时有 warn 日志。
- 已向用户书面提示凭证轮换与 git 历史清理事项。

---

## 8. Stage 6 — 清理与 CI 加固

**目标**：移除死代码，让已有的质量工具真正生效。
**风险**：低。
**依赖**：建议最后执行，避免与其他阶段产生冲突。

### 8.1 死代码清理（P2-5）

删除 `vmware/api/` 下 5 个内容整体被块注释的文件：`clusters.go`、`datastores.go`、`host.go`、`vm.go`、`inventory.go`。

**确认依据**：这些文件内所有代码均被 `/* */` 包裹，不参与编译；实现的是早期 REST API 方案，已被 `vmware.go` 的 SOAP 方案取代。删除前用 `go build ./...` 与 `go test ./...` 确认无引用。

### 8.2 修正 esxcli 无效互斥锁（P2-6）

`esxclihostnic.go:160-200` 当前「读检查在锁外、写在锁内」，非原子，且并发调用已被注释（`:129-141`）改回串行，锁实际无作用。

处置二选一（评审决定）：

- **方案 A（推荐）**：既然已是串行执行，直接移除 mutex，代码更诚实。
- **方案 B**：恢复并发调用，并把读检查一并纳入锁保护范围，或改用 `sync.Map`。

考虑到 esxcli collector 默认禁用且 README 自述为「示例性质」，倾向方案 A。

### 8.3 Dockerfile 加固（P2-7）

`Dockerfile:15` 的 `go build ... vmware-exporter.go` 改为 `go build -o vmware-exporter .`，避免 main 包新增文件被静默漏编译。

同时建议：
- 固定 builder 基础镜像 tag（当前 `golang:alpine` 为浮动 tag，构建不可复现）。
- `.dockerignore` 补充 `dashboards/`、`docs/`、`.workbuddy/`，减小构建上下文。

### 8.4 CI 加固（P1-6）

`.github/workflows/test.yml` 增加 lint job 与 race 检测：

```yaml
- name: Lint
  uses: golangci/golangci-lint-action@v6
  with:
    version: latest

- name: Run tests with race detector
  run: go test ./... -race -cover
```

**说明**：仓库已有 `.golangci.yml`（6 个 linter）但 CI 从不调用，属于「配置了却不生效」。`-race` 对本项目尤为必要——多个 collector 内部使用 goroutine 并发写 metric channel，Stage 2 收敛后并发度进一步提升。

### 8.5 Stage 6 验收标准

- `go build ./...` 与 `go test ./... -race` 均通过。
- `golangci-lint run` 零 issue（或已明确豁免并记录原因）。
- CI 三个 job（build / lint / test-race）全绿。
- Docker 镜像可正常构建并启动。

---

## 9. 执行顺序与里程碑

| 阶段 | 内容 | 前置依赖 | 破坏性变更 |
| --- | --- | --- | --- |
| Stage 1 | P0 正确性修复 | — | 无（仅超时默认值放宽） |
| Stage 2 | 双路径收敛 | Stage 1 | 无 |
| Stage 3 | **ESXi 直连支持** | Stage 2 | 无（纯增量） |
| Stage 4 | 指标语义修正 | Stage 2 | **有**（label 移除、指标改名） |
| Stage 5 | 安全加固 | — | 配置文件重命名 |
| Stage 6 | 清理与 CI | 建议最后 | 无 |

**建议交付节奏**：Stage 1 → 2 → 3 为一组（核心功能），完成后即可评审合并；Stage 4 → 5 → 6 为一组（质量收尾）。

---

## 10. 待评审确认项

| # | 事项 | 方案倾向 |
| --- | --- | --- |
| Q1 | Stage 2 采用路线甲/乙/丙（是否改动上游框架依赖） | **甲**（只改本仓库） |
| Q2 | Stage 6 的 esxcli 互斥锁采用方案 A/B | **A**（移除锁） |
| Q3 | Stage 4 的旧指标 deprecated 过渡期长度 | 一个 minor 版本 |
| Q4 | dashboard 改造是否与代码同批交付，或延后独立评审 | 可延后（代码向后兼容） |
| Q5 | git 历史中的凭证是否需要清理（破坏性操作） | 需单独确认 |
| Q6 | 合并后的版本号 | `v0.2.0`（含破坏性变更，按语义化版本升 minor） |
