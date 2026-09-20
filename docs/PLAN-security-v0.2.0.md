# vmware-exporter 安全 / 内存溢出 / 凭证泄露排查与修复方案（v0.2.0 / ee918c2）

- 基线：`master @ ee918c2`
- 日期：2026-09-10
- 状态：**只排查与设计，待审批后编码**。本文不含已落地改动。
- 依据：全量人工走读 38 个生产 Go 文件 + 打包/前端/CI，`govulncheck`（可达性口径）= 0，`go vet` 干净，`go test ./...` 全绿。
- 与既有报告关系：`AUDIT-security-performance-v0.2.0.md` 已实证 S-01～S-09；本轮新增**内存/状态增长**维度的 M-01～M-04，并把所有项整理成可逐项审批的修复批次。问题编号与审计保持一致，不重复编号。

---

## 1. 威胁模型（排查基准）

- 部署：exporter 常在受控机房/K8s，HTTP 端口默认 `:9169` 可被同网段访问；vCenter 只读凭证仍可枚举**全部资产拓扑 + 性能数据**，属高价值。
- 攻击者画像：
  - (a) 能访问 9169 但无 vCenter 凭证的同网段者；
  - (b) 容器逃逸 / 同主机低权限本地用户；
  - (c) 诱使运维或 Prometheus 抓取其构造的 `/probe` 链接（CSRF/钓鱼式 SSRF）；
  - (d) 控制一个被加入白名单的域名 DNS（rebinding）。
- 核心信任边界：**`/probe` 由请求方提供 target 与凭证**，本质是「带凭证的 SSRF 代理」。所有防护围绕「不把凭证/请求发往非预期主机、不被未授权方放大成攻击代理、不在本机/日志/镜像里留存凭证」展开。

---

## 2. 结论总览

### 2.1 安全漏洞 / SSRF / 凭证

| 编号 | 级别 | 问题 | 位置 | 状态 |
|---|---|---|---|---|
| S-01 | **P1** | SSRF 白名单可被 userinfo 注入绕过，并把 Basic 凭证转发给攻击者主机（已用 govmomi 实证） | targetfilter.go:47 | ✅ 已修（S1） |
| S-02 | P2 | `/probe` POST 表单体无大小上限 → 未授权内存型 DoS | handlers.go:253 | ✅ 已修（S1，1MiB/413） |
| S-03 | P2 | systemd 下含密码 config.yaml 被迫 0644，本机任意用户可读 | packaging/systemd | ✅ 已修（S2：静态系统账号 + 0600；弃用 LoadCredential 以保 SIGHUP 热重载） |
| S-04 | P2 | 容器镜像以 root 运行（FROM scratch 无 USER） | Dockerfile:64 | ✅ 已修（S2：USER 65534 + compose 加固） |
| S-05 | P3 | `schema` 参数无白名单，可强制 http 明文传凭证 | handlers.go:293 | ✅ 已修（S1，http/https 白名单） |
| S-06 | P3 | 白名单只做字符串匹配不做 DNS 解析（rebinding/主机名绕过残留） | targetfilter.go | ✅ 已修（S4：`internal/safedial` 在 govmomi transport 的 DialContext **与** DialTLSContext 上按 connect 前实际 IP 复核；默认拦链路本地/未指定，`-vmware.deny-private-addresses=true` 加严到环回/私网） |
| S-07 | P3 | 仍接受 GET 查询串里的 password（代理日志/Referer 泄漏面） | handlers.go:277 | ✅ 已修（S4：opt-in `-probe.deny-query-credentials`，默认 false 保兼容；开启后查询串带 username/password 一律 400，只收 POST body/Basic Auth，判定只看查询串与方法无关，SIGHUP 热重载） |
| S-08 | P3 | 默认无认证、debug 控制台默认开、无统一安全响应头 | main.go/ui.go | ✅ 已加固（S3：安全响应头 + 非回环无认证启动 warning；debug 控制台保留默认开，可用 `-web.debug-console=false` 关） |
| S-09 | 信息 | 1 个间接依赖漏洞不可达；CI 已含 govulncheck；toolchain 钉 1.26.6 | go.mod / CI | 无需动作 |

### 2.2 内存溢出 / 无界状态增长（本轮新增）

| 编号 | 级别 | 问题 | 位置 | 说明 |
|---|---|---|---|---|
| M-01 | P2 | `ScrapeErrors.counts` 的 key 含 **target**，/probe 接受任意 target 且白名单默认为空 → 未授权方可用海量不同 target 字符串让 map 无界增长（内存慢漏） | internal/collector/errors.go:23 | 本轮新排查重点 |
| M-02 | P3 | `InventoryCache.entries` 按 target 分桶，过期项只在**再次访问同 key** 时删除；/metrics 为单一 target 实际有界，但无容量上限/兜底清扫，异常 target 形态下存在残留 | inventorycache.go:30 | ✅ 已修（S3：默认 4096 上限，put 先按各 entry 自身 TTL 清扫、超容量淘汰最旧） |
| M-03 | P3 | SOAP/XML 与 perf 结果、vsanperf CSV 为整串/整块读入；受 in-flight 闸与 chunk 约束，规模有界，但单 target 超大 inventory 时仍可能高水位 | vmware/esxcli、collectors/scrape.go、vsanperf.go | 已有四层闸+分块，记录残余面 |
| M-04 | P3 | esxcli `ConfigArguments` 把参数直接拼进 XML 字符串（`<nicname>值</nicname>`），依赖 govmomi 编码转义；值来源为内部 nic.Name，但属应显式核实的注入面 | vmware/esxcli/esxcli.go:74 | ✅ 已核实（S3：注入探针单测证明 `encoding/xml` 转义 `<>&`，无需生产改动） |

> 未发现可被未授权远程直接触发的「内存崩坏/越界」类漏洞（Go 内存安全）；真实内存风险集中在 **M-01 这种由外部输入控制 map key 的无界增长**，以及 S-02 的 body 读入。这两项构成本轮「内存溢出」维度的主要修复对象。

---

## 3. 修复方案（逐项）

### S-01（P1）白名单 userinfo 绕过 —— 第一批必做

**根因复述**：校验侧 `splitHost` 与连接侧 `soap.ParseURL` 对 target 的解析不一致。`allowed.example.com:443@169.254.169.254` 经 splitHost 得 `allowed.example.com`（过后缀规则），经 ParseURL 实际连接 `169.254.169.254`，且把 `allowed.example.com:443` 当 userinfo——请求携带的 Basic 凭证被转发到非预期主机。

**修复（统一为 URL 规范化，单一事实来源）**：
1. 在 targetfilter 增加规范化步骤：以 `url.Parse("https://" + target)`（或 `//"+target`）解析，取 `u.Hostname()`/`u.Port()` 参与规则匹配；
2. 显式拒绝：`u.User != nil`（任何 `@`/userinfo）、`u.Path`/`u.RawQuery`/`u.RawFragment` 非空（target 只允许 host 或 host:port）；对解析失败、含非法字符、端口非数字一律拒绝；
3. `targetAllowedByRules` 增补表驱动绕过用例（全部必须拒绝）：
   - `allowed.example.com:443@evil`、`allowed@evil`、`allowed.example.com#@evil`、
   - `allowed.example.com/path`、`allowed.example.com?x=`、`allowed.example.com#frag`、
   - `[::1]` 形态、大小写/空白变体、尾部点、`evil#allowed.example.com`；
4. 连接侧使用同一规范化结果（或在 api 层再次拒绝 userinfo），消除「两套解析」。

**纵深（可并入 S-06）**：在 govmomi soap client 的 `Dialer.Control`（或自定义 http.Transport.DialContext）里，对最终解析出的 IP 复核白名单 CIDR，并可选阻断链路本地/环回/保留地址（除非显式允许），从拨号层封掉 DNS rebinding 与「字符串放行但 IP 在内网」的残留面。

**验收**：新增 PoC 表驱动测试（用 net/url 断言解析后 host + 放行结果）；保留对正常 `host:port`、裸 host、IPv6、CIDR/后缀/精确三规则的既有用例全绿。

---

### S-02（P2）请求体大小上限 —— 第一批（一行级）

`ParseForm` 对 urlencoded body 整表读入内存，无 `MaxBytesReader`。
**修复**：在 `probeHandler` 调 `ParseForm` 前 `r.Body = http.MaxBytesReader(w, r.Body, 1<<20)`（凭证表单体实际几百字节，1MB 足够）；超限返回 **413**。GET 查询串不受影响。补「超 1MB body → 413」单测。

---

### M-01（P2）ScrapeErrors 按任意 target 无界增长 —— 第一批（本轮重点）

**问题**：`ScrapeErrors.Add(target, collector)` 以 target 入 map key。/probe 无白名单（默认）+ 无认证时，攻击者每轮用不同 target（`t1`、`t2`…，哪怕登录失败也会走 `errors.Add(target,"login")`）发请求，map key 数量随请求数无限增长，进程常驻内存缓慢上升，最终 OOM 被杀死——且每次只增不清（Snapshot 只 seed 已知 collector，不淘汰旧 target 桶）。

> 触发路径已核对：`set.go` 登录失败分支确实调用 `cs.errors.Add(cs.target, loginBucket)`，target 来自请求，无任何归一化或上限。问题成立。

**修复（任选其一，推荐 A+B）**：
- **A. target 归一化 + 有界化（首选）**：入 key 前对 target 做规范化（host 小写、去默认端口、去 userinfo——与 S-01 共用规范化函数），杜绝 `t:443` 与 `t` 各占一桶；这只压缩，不封顶。
- **B. 给计数器加 target 维度上限 / LRU**：维护一个有最大桶数（如默认 1000，可用 flag 调）的结构，超出后新 target 记入固定 `target="other"` 聚合桶或丢弃计数并打一条 warn（绝不因计数器 OOM）。进程级计数器本用于「同一批长期 target 的错误趋势」，为上千个一次性伪造 target 精确分桶没有业务价值。
- 可选 C：/probe 路径的 errors 在「未通过白名单」时根本到不了 Add（白名单 403 已提前返回），因此**配置了 S-01 白名单的部署天然免疫**；但默认空白名单必须靠 B 兜底。

**验收**：单测注入 10k 随机 target，断言桶数 ≤ 上限、内存不随次数增长、合法 target 的计数仍单调正确。

---

### S-03（P2）systemd 含密码配置世界可读 —— 第二批

**现状**：DynamicUser 临时 uid 读不了 root:root 0600，install.sh 被迫 `chmod 0644`，同机任意用户可 `cat config.yaml` 取 vCenter 密码。

**修复（systemd 原生凭证，推荐）**：
- unit 改 `LoadCredential=vmware.yaml:/etc/vmware-exporter/config.yaml`（敏感部署可文档化 `LoadCredentialEncrypted=`）；systemd 把文件挂到 `$CREDENTIALS_DIRECTORY/vmware.yaml`，属主为动态 uid、0600；
- 启动行用 `-file=${CREDENTIALS_DIRECTORY}/vmware.yaml`；磁盘原文件收紧 root:root **0600**；
- 同步改 install.sh/uninstall.sh 权限处理与部署文档（README/DEPLOY-zh）。
- 次优：固定系统账号（unit 注释已给 useradd 方案）+ chown 该账号 0600。
- 注意项目约定：systemd 事实来源只在 `packaging/systemd/`，改完跑 `scripts/check_config.py` 防文档/脚本分叉。

---

### S-04（P2）容器非特权 + compose 加固 —— 第二批

**修复**：
- Dockerfile builder 阶段生成 `/etc/passwd`（nobody 65534）并 COPY 进 scratch，运行阶段加 `USER 65534:65534`；确认不依赖写任何路径（scratch + 只读运行，无状态，应可直接切）；
- docker-compose：`read_only: true`、`cap_drop: ["ALL"]`、`security_opt: ["no-new-privileges:true"]`，端口绑定回环或前置反代（`127.0.0.1:9169`），与 systemd 的加固水平对齐。
- 验收：构建镜像并以非 root 起 `/metrics`（disable target 模式）自测；`docker inspect` 确认 uid。

---

### S-05（P3）schema 白名单 —— 第二批（小改）

`schema` 仅允许 `http`/`https`，其余 **400**；更严格默认拒绝 http（如需明文另设显式开关）。至少做到白名单 + 拒绝未知值，补单测。

### S-06（P3）拨号侧 IP 复核 —— ✅ 已实现（S4）

见 S-01 第 4 点：解析最终 IP 后复核 CIDR，可选阻断保留/环回/链路本地地址（含 `169.254.169.254` 云元数据），并解析一次即锁定 IP（防 DNS rebinding 的「校验时一个地址、拨号时另一个」——必须在同一 DialContext 内基于实际拨号 IP 判定，而非预先解析）。

**实现（`internal/safedial`）**：通过 `cache.Session.Login` 的 `config func(*soap.Client)` 回调，在 govmomi 建好 SOAP client 后给其内部 transport 同时安装受控的 `DialContext` 与 `DialTLSContext`——govmomi 对 HTTPS 自设 `DialTLSContext`（内部 `tls.Dial` 绕过 `DialContext`），只覆盖明文路径对真实 vCenter 不会生效，故两条路径都收敛到同一个 `net.Dialer.Control`：先建过 IP 复核的 TCP，再在该连接上握 TLS。默认策略仅拦链路本地/未指定（对 RFC1918 vCenter 零误伤），`-vmware.deny-private-addresses=true` 加拦环回/私网。vcsim 集成测试钉住「默认放行环回、Strict 拦截」。

### S-07（P3）查询串凭证开关 —— ✅ 已实现（S4）

新增 `-probe.deny-query-credentials`（默认 false 保持兼容），开启后凭证只接受 Basic Auth/POST body，URL 查询串带 username/password 一律 400，堵住 URL/Referer/代理日志/浏览器历史/链路追踪的泄漏面。

**实现要点**：判定只看 `r.URL.Query()`（查询串本身）、与 HTTP 方法无关 —— 这样既挡传统的 GET `?username=&password=`，也挡「POST 却把凭证写进 URL、body 放别的字段」的绕过；POST 表单体凭证不经过查询串，照常放行。开关经 settings 快照读取，随 SIGHUP 热重载。默认 false，旧抓取配置零改动。测试覆盖：GET/POST 查询串拒绝、半个凭证（仅 username 或仅 password）拒绝、POST body/Basic Auth 放行、默认 false 兼容。

### S-08（P3）默认收敛与安全头

- 当监听非回环且未配 `web.config.file` 时，启动打印显著 warning（无认证暴露）；
- 统一加响应头 `X-Content-Type-Options: nosniff`、`Referrer-Policy: no-referrer`（对 /debug、/config 有实际意义）；
- debug 控制台维持有开关可关（`-web.debug-console=false`），不改默认以保易用。

### M-02（P3）InventoryCache 兜底防御

/metrics 单 target 下 key 数量有界（固定 mo 类型 × 属性集组合），风险低。建议加：可选的最大 entries 上限 + 一个低频兜底清扫（或在 put 时若超容量淘汰最旧项），不改变 TTL/singleflight 语义；/probe 不注入缓存，无此面。单测覆盖超容量淘汰。

### M-03（P3）大响应高水位（记录，已有缓解）

perf 已分块（64）+ 有界并发；inflight 闸限 4。残余面是单个分块/单个 CSV 仍整串驻留。缓解已足够，暂不改动；若 P-09 观测到单块过大，可把 chunk-size 默认调小或做流式（govmomi SOAP 不支持真流式，成本高，不建议本轮做）。

### M-04（P3）esxcli XML 参数拼接 —— 核实项

核实 govmomi 的 SOAP 编码是否对 `ExecuteSoapRequest.Argument` 的 Val 做 XML 转义。当前 nic.Name 来自 vCenter 返回而非直接用户输入，风险低；修复方式是不手工拼 `<%s>%s</%s>`，改用编码器/对值做 `xml.CharData` 转义。审批后先写一个含 `<&>` 值的单测确认是否被转义，再决定是否改。

---

## 4. 凭证泄露面专项排查结论（已核对，无需重复修）

- **日志零凭证**：`LoginWithCredentials` 刻意不记 username；GET/POST 路径日志仅 target/schema/insecure/collectors；登出失败日志不含凭证。
- **命令行不摆密码**：systemd 走 `-file`、compose 走 `-envflag.*`，注释均点明 /proc 与 docker inspect 泄漏面；剩余风险即 S-03（文件 0644）。
- **前端纯浏览器本地**：/debug POST 凭证不进 URL/历史；/config 生成口令占位 `<password>` + 默认脱敏，有测试兜底；无服务端回传。
- **/probe 不共享进程缓存**：杜绝低权限凭证读到高权限凭证留下的清单（越权读边界，代码与注释一致）。
- **进程内/核心转储**：Go string 密码在内存中存在直到 GC，属语言常态；不建议为此引入不安全的字节清零（string 不可变、易留副本，收益低、易误导）。可在文档建议受限主机禁用 core dump（systemd 已有 `LimitCORE=0`，需在 unit 核对；容器可加 `ulimit core=0`）。
- **传输**：默认 https + 校验证书（insecureTLS 默认 false）；S-05 关闭强制 http 的口子后，明文面进一步收敛。
- **依赖/工具链**：govulncheck 0 可达漏洞；go.mod toolchain 钉 1.26.6 规避标准库公告；CI 持续扫描。

结论：**不存在凭证明文落日志、落命令行、落前端回传的现存缺陷**；主要凭证风险是 S-01（被转发到非预期主机）与 S-03（本机文件世界可读），两者修复后凭证面闭合。

---

## 5. 建议落地批次（待审批）

| 批次 | 内容 | 兼容性 | 风险 |
|---|---|---|---|
| **S1（安全快修，强烈建议先做）** | S-01 URL 规范化+拒 userinfo/path/query + 绕过用例；S-02 MaxBytesReader 413；M-01 target 归一化 + 错误计数桶有界化；S-05 schema 白名单 | 全部向后兼容（仅拒绝此前能蒙混的畸形输入） | 低-中，均有单测 |
| **S2（部署加固）** | S-03 systemd LoadCredential + 配置 0600（install/uninstall/DEPLOY/check_config 联动）；S-04 容器非特权 + compose 加固 | systemd 调用方式微调（-file 路径改 $CREDENTIALS_DIRECTORY），需文档；容器 uid 变化 | 中（部署形态，需在脚本/文档回归） |
| **S3（纵深与收敛）** | S-06 拨号侧 IP 复核（与 S-01 同改）；S-08 无认证 warning + 安全头；M-02 缓存容量兜底；M-04 esxcli 转义核实测试 | 增量/默认兼容 | 低-中 |
| **S4（可选）** | ✅ S-07 `-probe.deny-query-credentials` 开关（默认关纯增量）；✅ core dump 收敛（unit `LimitCORE=0` + compose `ulimits core=0`，DEPLOY/README 文档化）；另注：S-06 拨号复核也已落地（见上） | 默认关/默认禁 core，纯增量 | 低 |

每批独立 dev 分支、`--no-ff` 合 master，不 push、不打 tag，除非明确要求。S1 必须先于其余代码批次，因为它修的是「唯一被实际绕过的安全控制 + 唯一未授权无界内存增长」。

---

## 6. 验证与回归清单（每批必过）

1. `CGO_ENABLED=0 go vet ./...` 干净；`go test ./...` 全绿；CI Linux 跑 `go test -race`（本机 darwin 无 C 工具链）。
2. `GOPROXY=https://goproxy.cn,direct CGO_ENABLED=0 go run golang.org/x/vuln/cmd/govulncheck@latest ./...` 维持 0 可达。
3. 新增安全回归用例：
   - S-01：全部 userinfo/path/query/fragment/大小写/IPv6 绕过形态 → 拒绝；正常形态与三规则 → 维持；
   - S-02：>1MB body → 413；
   - M-01：10k 随机 target 桶数有界、合法 target 计数正确；
   - S-05：schema=foo → 400；
   - M-04：参数含 `<&>` 时的编码行为断言。
4. `scripts/check_config.py` 通过（flags ↔ README/compose/packaging/dashboards/metrics 双向一致）。
5. 手工：systemd 包安装→以动态 uid 读取 0600 凭证成功；容器非 root 启动 `/metrics` 正常。
6. 文档：METRICS.md/zh（若新增错误计数桶语义）、README/README-zh、部署文档、CHANGELOG 同步。

---

## 7. 明确不建议做

- 不引入会话池/长会话来「减少登录」（安全与热重载语义代价更高，见性能方案 P-03）。
- 不为了 esxcli/vsan 的理论高水位改 SOAP 流式（govmomi 不支持，收益低）。
- 不把 pprof 暴露到默认 mux（如需要仅回环+鉴权，见性能方案 P-06）。
- 不对 Go 内存中的密码做不安全清零（见 §4）。
