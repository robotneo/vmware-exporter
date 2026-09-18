# vmware-exporter 采集性能优化方案（CPU / 内存视角，v0.2.0 / ee918c2）

- 基线：`master @ ee918c2`
- 日期：2026-09-10
- 状态：**只设计，待审批后编码**。本文不含已落地改动。
- 范围：从「采集端 CPU 消耗」与「内存分配 / GC 压力 / 常驻内存」两个口径重审整条抓取管线（登录 → 属性检索 → perf 分块 → esxcli/vsan → 编码输出）。
- 与既有审计关系：承接 `AUDIT-security-performance-v0.2.0.md` 的 P-01/P-03/P-04/P-05，新增本轮全量重扫发现的 P-07/P-08/P-09/P-10，并给出每项的**改法、影响面、验证方法、风险**，供逐项审批。

---

## 0. 度量方法（先立基线，再谈收益）

所有优化都应以可对比的基准证明收益，避免「凭感觉重构」：

1. **基准场景**：用 govmomi 自带 `vcsim`（模拟器）+ 大 inventory 参数（如 `-vm 2000 -host 200 -cluster 8`），跑固定 collector 组合（默认全开 / esxcli 全开 / vsan.perf 全开三档）。
2. **进程内口径**：`GODEBUG=gctrace=1`（GC 次数/暂停/堆目标）、`runtime.MemStats`（HeapAlloc/HeapInuse/NumGC/Mallocs）、pprof（见 P-10，仅本地/独立端口）。
3. **业务口径**：单轮 wall-clock（已有 `vmware_scrape_collector_duration_seconds`）、SOAP 往返数（建议 P-09 加计数器）、单轮输出字节数与序列数。
4. **门禁**：每项改动前后对比 `go test ./...` + 基准数字；序列与指标名逐字一致（除有意的 desc 复用外不得改变输出）。

> 原则：**默认关闭的 collector（esxcli/vsan）上的分配优化收益低于默认路径**；优先级按「默认路径 × 实体规模」排序。

---

## 1. 问题总览（按 收益/风险 排序）

| 编号 | 级别 | 类型 | 问题 | 默认路径 | 位置 |
|---|---|---|---|---|---|
| P-01 | P2 | CPU+内存 | esxcli 两个 collector 实体热循环内逐个 `NewDesc` + 每次新建 label map | 否（默认关） | esxclihostnic.go:183、esxclistoragelist.go:135 |
| P-07 | P2 | CPU | datastore 每轮调用一次 `regexp.MustCompile` 重新编译正则 | 是 | datastore.go:60 |
| P-08 | P2 | CPU+内存 | perf 管线每轮对全量实体做**未裁剪**的 QueryPerf：不可用计数器已过滤，但「对关停/无数据实体仍发查询」与大结果集中间切片分配可优化 | 是 | vmware/collectors/scrape.go:222-435 |
| P-03 | P3 | CPU+网络 | 每轮重新 `CounterInfoByName`（数千 PerfCounterInfo）并重建 by-name map | 是 | vmware/api/vmware.go:288 |
| P-04 | P3 | CPU+IO | `-log.level` 默认 debug，大环境每轮刷大量 chunk/skip/counter 日志（序列化也是 CPU） | 裸二进制/容器是；systemd 模板已 info | main.go:50 |
| P-09 | P3 | 可观测 | 无 SOAP 往返/在飞请求计数，无法量化优化收益、也无法给 vCenter 压力配告警 | 是 | throttle.go |
| P-05 | P3 | 网络+CPU | esxcli N+1 固有往返（MME+list+每网卡 get），全局闸只控并发不降总量 | 否 | esxclihostnic.go |
| P-02 | P3 | 内存 | datacenter/cluster 实体循环内 NewDesc（实体少，个位~几十） | 是 | datacenter.go、cluster.go |
| P-06 | 观察 | — | 无 pprof，线上无法定位 CPU/堆 | — | — |
| P-10 | P3 | 内存 | 每请求新建 `prometheus.Registry` + Go/Process collector（/metrics 默认已关自身指标）；vsanperf CSV 解析为整串读入（govmomi 限制） | 是/否 | handlers.go、vsanperf.go |

「已经做对、不要动」的项见第 4 节。

---

## 2. 逐项方案

### P-01 esxcli 热循环 NewDesc 消除（建议第一批做）

**现状（CPU+内存）**：`esxcliGetNicInfo` 对每张网卡构造一个 `prometheus.NewDesc`（fqName 拼接、label 名校验/排序、const label map 拷贝）并分配一个 7 元素 map；storage collector 同理（每存储设备一个）。几百主机 × 多网卡 = 单轮数千次 Desc 构造 + 数千个小对象，纯 GC 压力。

**关键约束（不能做错）**：这些 label 是「描述性高基数」（driver/version/firmware/descr/model/revision）。直接平移成 variableLabels 每实体输出会**改变序列基数**（从「按驱动版本去重」变成「每网卡一条」），破坏 dashboard。现有 `versionSet` 已经在做去重——只有首次出现的 (driver,version)/(driver,firmware) 组合才发指标。

**方案（按 label 组合缓存 Desc，复用现成范式）**：
- 项目里已有正确范式 `vsanperf.go` 的 `vsanPerfDesc(namespace, entityType, label)`（sync.Mutex + map、复合 key）。
- 新增一个 esxcli 专用 descCache：key = 全部 label 值的有序拼接（或 struct），value = `*prometheus.Desc`。仅当 `versionSet.Add` 判定为新组合时查缓存发指标。
- 缓存挂在**每次 Update 的 collector 实例内新建**（跟随一轮抓取生命周期）即可，还是做成跨轮进程级？建议**跟随 Scrape（每轮新建）**：label 值集合随硬件/驱动升级而变，进程级缓存会随罕见组合长期堆积 key（map 泄漏面）；每轮缓存的唯一组合数 = 实际不同驱动/固件数，远小于网卡数，且天然无界增长问题。
- storage collector 的 model/revision 同样处理。

**验证**：vcsim 大 inventory 下对比 gctrace/AllocsPerScrape；`go test` 序列 golden 不变；新增「相同 label 组合复用同一 Desc」的单测。
**风险**：低。输出序列不变（仍按组合去重），仅 Desc 对象复用。

---

### P-07 datastore 正则每轮重编译（一行级，建议第一批）

**现状**：`datastore.go:60 re := regexp.MustCompile(...)` 在 `Update` 内，每次抓取重新编译同一正则。

**方案**：提升为包级 `var datastorePathRE = regexp.MustCompile(...)`（或放 descs.go 同文件的 var 块），只读并发安全。
**收益**：每轮省一次正则编译（小而确定，默认路径必走）。
**风险**：近乎零；补一个「包级变量非 nil、行为逐字一致」的断言即可。
**备注**：全仓 grep 确认这是生产代码里唯一热路径 MustCompile（其余只在测试与包初始化）。

---

### P-08 perf 查询的实体裁剪与结果集内存收敛（收益最大，需谨慎设计）

这是默认路径在大环境（2000 VM）下 CPU/内存的主要来源，分两个子项：

**P-08a：跳过明确无实时数据的实体**
- 现状：host collector 已用 `hostDataPlaneEligible`（开机+connected+非维护）裁剪主机；但 **VM 侧** perf 查询是否对「 poweredOff / 不存在于实时统计的 VM」也做了等价裁剪需要核对（vm.go）。若仍向关机/模板/孤立 VM 发 PerfMetricId，vCenter 对这些实体返回空，浪费请求体积与合并期分配。
- 方案：对齐 host 的做法，构造 perf refs 前按 runtime.powerState（必要时加 connection/template 判定）过滤 VM，被过滤实体计入 `RecordEntities(skipped, powered_off)`（reason 常量已有），保证「为什么没指标」可观测。
- 风险：中。必须用 vcsim + 真实语义验证「不支持实时」的判定，避免把「开机但短暂无采样」的 VM 误裁（那种情况应保留查询，只是本轮无样本）。**这一项需要先在 vcsim 建对照实验确认收益与正确性，再决定是否动。**

**P-08b：perf 结果中间切片的容量与生命周期**
- 现状：`parts := make([][]BasePerfEntityMetricBase, len(chunks))` 保留全部分块结果到 `g.Wait()` 后统一合并输出；2000 VM × 多计数器 × 多样本时，parts 与其内联的 value 数组在合并完成前全部常驻。
- 可选优化（二选一或都做）：
  1. **块内即解析即发指标**：每个 chunk goroutine 拿到 series 后直接转换成 metric 并发到 ch（ch 由 promhttp 消费，天然背压），不再保留 parts 总集。代价：输出顺序与「按实体顺序合并」的既有契约变化——需确认是否有测试/用户依赖输出顺序（Prometheus 文本协议不依赖顺序，registry 排序在编码层，理论安全，但要用 golden 测试证明序列集合逐字一致）。
  2. 保守版：保留 parts，但给每个内层 slice 预分配容量、合并后及时 `parts[i]=nil` 帮助 GC，不改变顺序。
- 建议：先做 (2)（零行为风险），把 (1) 作为经基准证明收益足够后的可选项单独审批。

**验证**：vcsim 2000-VM 场景 gctrace 对比堆峰值与单轮耗时；perfchunk 现有测试 + 新增「裁剪计数」「序列集合等价（排序后比对）」测试。
**风险**：(a) 中（误裁风险）；(b-1) 中（顺序契约）、(b-2) 低。

---

### P-03 CounterInfoByName 元数据 TTL 缓存（建议做，明确不做会话池）

**现状**：每轮登录后 `perf.CounterInfoByName()` 拉全量计数器元数据（vCenter 通常数千条），并建一次 by-name map；纯 CPU（XML 解析 + map 构建）+ 一次 SOAP 往返。

**方案（无状态、按 target+版本 缓存）**：
- 复用现有 `InventoryCache` 同类设施思路，但 key 必须含 **target + About.Version/Build**（计数器表随 vCenter 版本与补丁变化），TTL 取一个更长但有界的值（建议独立 flag，默认如 10m–30m，区别于 inventory 的 5m），失败不缓存。
- **仅 /metrics 注入；/probe 仍每请求实时**（与 inventory 缓存同一条越权读边界——元数据虽敏感度低，但保持边界一致，避免双标）。
- 数据是 `map[string]*PerfCounterInfo` 只读共享，放缓存安全。

**明确不做**：长会话 / 会话池复用。它会重新引入会话陈旧、并发归属、登出失败、热改不即时生效等问题（见审计 P-03 权衡），收益不抵风险。
**风险**：低-中（版本/build 拼进 key 后陈旧窗口可接受；要在升级 vCenter 后最多一个 TTL 生效，文档注明）。

---

### P-04 默认日志级别收敛为 info（建议做）

**现状**：`-log.level` 默认 `debug`。debug 路径每轮对每个 chunk、每个跳过实体、每个不可用计数器、每次属性检索都做日志参数装配与（json 模式下的）序列化——大环境是实打实的 CPU 与 journal/IO 成本。

**方案**：flag 默认改 `info`；systemd 模板已是 info（无需动）；调试时 `-log.level=debug` 或 SIGHUP 热开。
**联动**：`check_config.py`、README/README-zh、`/config` 生成器若展示默认值需同步（脚本是双向一致性护栏，会兜底报错）。
**风险**：低（仅默认值；排查时可临时开）。

---

### P-09 增加 SOAP 往返 / 在飞请求自监控（为所有优化提供度量，建议先做）

**现状**：`throttledRoundTripper` 有效限流，但没有计数器暴露「单轮/累计发了多少 SOAP 往返、峰值在飞数、因闸等待的时长」。没有它，P-05/P-08 的收益无法量化，vCenter 侧压力也无法告警。

**方案**（纯增量指标，不改行为）：
- `vmware_soap_requests_total{type?}` counter（RoundTrip 次数）、`vmware_soap_inflight` gauge（当前在飞）、可选 `vmware_throttle_wait_seconds`（取令牌等待直方图）。
- 计数器放在 per-Scrape 还是进程级需与现有自监控风格一致：建议进程级、按 target 分桶（同 ScrapeErrors），/metrics 与 /probe 都可见；注册到 newRegistry。
**风险**：低（观测增量）；注意指标命名过 promlint、help 规范。

---

### P-05 esxcli N+1（记录，不建议在本轮强改）

每主机至少 MME+list 两往返，每网卡再一次 get；全局闸只钉并发、不降总量，启用时单轮 wall-clock 可能到分钟级。这是 vSphere MME API 形态决定的，govmomi 无批量 esxcli 接口。
**建议**：不在本轮做协议级改造；用 P-09 的计数器把成本显性化，并在 README/`/config` 页标注「esxcli.* 默认关闭、成本 O(主机×网卡)」与 timeout/抓取间隔指引。若未来要降，只能在「list 是否已经带回足够字段、从而省掉每网卡 get」上逐字段核对（list 返回体可能已含部分 driver/firmware）——属于需要真实 ESXi 对照的探索项，单独立项。

---

### P-02 datacenter/cluster 循环内 NewDesc（不建议改）

实体数个位到几十，单次开销可忽略，`descs.go:27` 注释已明确排除在 P2-4 外。为形式统一扩大风险面无收益。维持现状，仅在 P-01 落地时复用同一 descCache 工具（顺手、零额外设计时可以带上；否则不动）。

### P-10 Registry 与 CSV 解析（观察 + 小项）

- 每请求新建 Registry 是 client_golang collector 模式的既定形状（node_exporter 同款），不属于泄漏；Go/Process 自身 collector 在 /metrics 默认已由 `-disable.exporter.metrics=true` 关闭。无需改。
- vsanperf 经 govmomi 拿 CSV 字符串再解析，整串驻留；默认关闭且实体有界，仅在启用 vSAN 性能服务时有意义。维持现状，待 P-09/pprof 数据显示为瓶颈再议。

### P-06 pprof（按需，默认不暴露）

如要做 P-08 的精细归因，建议挂到**独立回环端口或鉴权后**，绝不进默认 mux（pprof 端点本身是信息/DoS 面）。可加一个默认关闭的 `-web.pprof-address=127.0.0.1:0` 形态开关；不需要时不做。

---

## 3. 建议的落地批次（待审批）

| 批次 | 内容 | 行为变化 | 风险 | 需要的验证 |
|---|---|---|---|---|
| A（低风险快赢） | P-07 包级正则；P-01 esxcli Desc 按组合缓存；P-04 默认 info（含 check_config/README 联动） | 无（输出逐字一致），仅默认日志级别变化 | 低 | 全量 test + gctrace 对比 + golden |
| B（度量先行） | P-09 SOAP 往返/在飞/等待指标 | 仅新增指标 | 低 | promlint + 新指标文档（METRICS.md/zh） |
| C（主收益，需实验） | P-03 计数器元数据 TTL 缓存（仅 /metrics，按 target+版本）；P-08b parts 容量/及时释放 | 无（仅内存/往返） | 低-中 | vcsim 大 inventory 基准 + 缓存边界测试 |
| D（高收益但高风险，单独审批） | P-08a VM 数据面实体裁剪；P-08b-1 块内即解析即发（去 parts 总集） | 可能影响无数据实体的查询/输出顺序 | 中 | 必须先 vcsim 对照实验证明正确且等价，再动 |
| 暂不做 | P-05 esxcli 协议级降往返、P-02、P-06(默认)、P-10 | — | — | 用 P-09 数据决定是否重启 |

每个批次独立成一个 dev 分支，按项目约定 `--no-ff` 合 master，不 push、不打 tag，除非明确要求。批次 C/D 必须有「前后基准数字」贴进 PR 描述，否则不合并。

---

## 4. 已经做对、明确不要在优化中破坏的机制

- perf 分块（chunk-size 64）+ 有界并发 + 顺序合并，规避 `vpxd.stats.maxQueryMetrics` 与单请求超时；计数器 id 直构避免每块重复 SampleByName 往返。
- 拓扑/容量清单进程级 TTL 缓存 + singleflight；host/vm 运行态刻意实时；/probe 不注入缓存（越权读边界）。
- HostSystem 三 collector 请求内 sync.Once 共享（三次 ContainerView 合一）。
- 四层有界并发（inflight 闸 / collector SetLimit / per-host SetLimit / RoundTripper 单次往返闸）。
- delta 按 StatsType 求和、历史样本尾部截断复刻 govmomi 语义、descsFor 每轮 Desc 缓存。
- 每轮独立会话 + Logout 独立超时先于 cancel（不引入会话池）。

---

## 5. 成功判据

- 默认全开 collector、vcsim 2000 VM / 200 host 场景下：单轮 P95 耗时与堆峰值（HeapAlloc 峰值、Mallocs、NumGC）相对基线有可量化下降（批次 A/C 目标：Mallocs 与 GC 次数下降，P-03 少一次 SOAP 往返）。
- 全部既有 Test 绿；指标名/标签/序列集合逐字一致（D 批次允许经论证的等价，但要排序后集合比对）。
- 不新增数据竞争：所有新增缓存/计数器路径能在 CI Linux 上通过 `go test -race`（本机 darwin 跑不了，靠 CI）。
