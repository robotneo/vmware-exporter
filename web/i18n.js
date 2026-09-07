// i18n.js —— 中英文切换引擎。
//
// 没有框架也没有构建步骤是刻意的（同 app.js 的注释）。翻译数据直接内联在
// 这个文件里，浏览器加载后一次性生效。
//
// 使用方式：
//   页面元素加 data-i18n="key" → 自动替换文本内容。
//   动态 JS 字符串用 __("key") 函数调用。
//   语言切换按钮调用 i18n.setLang("zh"|"en")。

"use strict";

const i18n = (() => {
  const STORAGE_KEY = "vmware_exporter_lang";

  const zh = {
    // ── 顶栏 ──────────────────────────────────────────────────────────
    "topbar.overview": "概览",

    // ── 概览页 ────────────────────────────────────────────────────────
    "index.heading": "接口端点",
    "index.desc":
      "该导出器从 VMware vCenter Server 和独立 ESXi 主机收集指标。登录时自动检测目标类型，同一端点即可服务两种场景。",

    "ep.metrics.title": "单台 vCenter 或 ESXi 主机",
    "ep.metrics.kind": "单目标",
    "ep.metrics.desc_disabled":
      "当前仅提供导出器自身指标：已设置 <code>-disable.exporter.target</code>，因此不会连接 vCenter。",
    "ep.metrics.desc_enabled":
      "凭证来自全局 <code>-vmware.*</code> 参数。直接让 Prometheus 抓取即可。",

    "ep.probe.title": "多台 vCenter，一台导出器",
    "ep.probe.kind": "多目标",
    "ep.probe.desc":
      "目标和凭证每次请求单独提供，一台导出器即可服务多个 vCenter，并可分别启用不同的采集器。",

    "ep.debug.title": "在浏览器中试抓取",
    "ep.debug.kind": "交互式",
    "ep.debug.desc":
      "填写目标地址、选择采集器，预览返回的指标。在配置 Prometheus 之前验证凭证和权限，非常方便。",

    "ep.config.title": "生成 Prometheus 配置",
    "ep.config.kind": "生成器",
    "ep.config.desc":
      "将 vCenter 列表转换为 <code>scrape_configs</code> 任务及文件服务发现目标文件，支持两种端点模式。所有处理均在浏览器中完成，凭证不会发送至服务端。",

    "section.collectors": "采集器",
    "section.reference": "参考文档",

    // ── 采集器标注 ────────────────────────────────────────────────────
    "default on": "默认开启",
    "default off": "默认关闭",
    "per-host serial": "逐主机串行",
    "needs vSAN": "需 vSAN",
    "needs perf service": "需性能服务",

    // ── 采集器说明 ────────────────────────────────────────────────────
    "vCenter and datacenter info": "vCenter 与数据中心信息",
    "Cluster information": "集群信息",
    "Datastore metrics": "数据存储指标",
    "ESXi host metrics": "ESXi 主机指标",
    "Virtual machine metrics": "虚拟机指标",
    "Resource pool limits, reservations and usage": "资源池限制、预留与使用量",
    "ESXi NIC driver info": "ESXi 网卡驱动信息",
    "ESXi storage info": "ESXi 存储信息",
    "vSAN cluster health, capacity and resync": "vSAN 集群健康、容量与重同步",
    "vSAN performance statistics (requires the vSAN performance service)":
      "vSAN 性能统计（需 vSAN 性能服务）",

    // ── 参考文档：单目标模式 ──────────────────────────────────────────
    "ref.single.summary": "单目标模式",
    "ref.single.p1":
      "启动导出器时指定目标及凭证，然后让 Prometheus 抓取 <code>/metrics</code>：",
    "ref.single.p2":
      "同样的参数也适用于独立 ESXi 主机。无需选择模式：导出器在登录时读取 <code>ServiceContent.About.ApiType</code> 自动适配，因为独立主机没有实际的数据中心或集群对象可遍历。",
    "ref.single.note":
      "<strong>命令行上的密码是可见的</strong>，任何能运行 <code>ps</code> 或读取 <code>/proc</code> 的人都能看到。建议使用 <code>-envflag.enable</code> 配合环境变量文件。",

    // ── 参考文档：时间参数 ────────────────────────────────────────────
    "ref.timing.summary": "时间参数",
    "ref.timing.th_flag": "参数",
    "ref.timing.th_default": "默认值",
    "ref.timing.th_effect": "作用",
    "ref.timing.timeout": "一次抓取的总超时（秒），涵盖登录、属性检索和性能采样。清单较大时可适当调高。",
    "ref.timing.interval": "PerfManager 采样窗口（秒），与抓取超时相互独立。",
    "ref.timing.granularity": "采样频率（秒），必须大于 0 且不超过 interval。",

    // ── 参考文档：多目标模式 ──────────────────────────────────────────
    "ref.multi.summary": "多目标模式",
    "ref.multi.required":
      "必填参数：<code>target</code>、<code>username</code>、<code>password</code>。可选：<code>schema</code>（默认 https）、<code>insecure=true</code>（跳过 TLS 验证）。",
    "ref.multi.credentials": "凭证也可以通过 HTTP Basic Auth 传递，而非 URL 参数。",
    "ref.multi.basic_auth":
      "<strong>Basic Auth 与 <code>-web.config.file</code> 冲突。</strong>当导出器自身监听器配置了 <code>basic_auth_users</code> 时，<code>Authorization</code> 头被其自身占用，vCenter 凭证无法通过此方式传递。请使用 URL 参数，或调试控制台（通过表单体提交）。",
    "ref.multi.collectors":
      "采集器通过可重复的 <code>collect[]</code> 和 <code>nocollect[]</code> 参数选择：",
    "ref.multi.misspelled":
      "拼写错误的采集器名称将被拒绝并返回 HTTP 400 及有效名称列表，不会静默产生空指标集。",

    // ── 参考文档：Prometheus 配置 ────────────────────────────────────
    "ref.prom.summary": "Prometheus 配置",
    "ref.prom.generator":
      "<a href=\"/config\">配置生成器</a>可根据 vCenter 列表构建上述文件（含目标文件）。以下示例展示的是两个任务使用不同采集器时的输出。",
    "ref.prom.intro":
      "不同 vCenter 使用不同采集器，凭证通过服务发现文件而非 scrape config 携带：",
    // ── 页脚 ──────────────────────────────────────────────────────────
    "foot.source": "源码",
    "foot.metrics_ref": "指标参考文档见 docs/METRICS.md",

    // ── 调试页 ────────────────────────────────────────────────────────
    "debug.title": "调试控制台",
    "debug.desc":
      "对任意可达目标执行一次抓取，精确返回 Prometheus 会收到的内容。凭证通过表单体提交，不会出现在 URL、浏览器历史记录或记录查询字符串的访问日志中。",

    "debug.target": "目标",
    "debug.target_hint": "vCenter 或 ESXi",
    "debug.address": "地址",
    "debug.address_help": "vCenter Server 或独立 ESXi 主机，登录时自动检测类型。",
    "debug.username": "用户名",
    "debug.password": "密码",
    "debug.scheme": "协议",
    "debug.tls": "TLS",
    "debug.tls_txt": "跳过验证",
    "debug.tls_em": "不安全",

    "debug.collectors": "采集器",
    "debug.sel_count": "已选 {n} / {total} 个",
    "debug.sel_all": "全选",
    "debug.sel_default": "默认",
    "debug.sel_clear": "清空",
    "debug.run": "执行抓取",
    "debug.running": "抓取中…",
    "debug.copy_url": "复制探针 URL",
    "debug.copied": "已复制",

    "debug.result": "结果",
    "debug.not_run": "尚未运行",
    "debug.stat_http": "HTTP 状态",
    "debug.stat_elapsed": "耗时",
    "debug.stat_series": "时间序列",
    "debug.stat_up": "vmware_up",

    "debug.empty": "在左侧填写目标，然后执行抓取以查看返回的指标。",
    "debug.filter_placeholder": "过滤行…",
    "debug.req_failed": "请求失败",
    "debug.req_note": "请求未完成：",
    "debug.series_fmt": "个时间序列",
    "debug.series_note": "个序列，共 {n} 行",
    "debug.rejected": "导出器拒绝了请求，响应内容如下。",

    // ── 配置页 ────────────────────────────────────────────────────────
    "config.title": "Prometheus 配置",
    "config.desc":
      "生成 <code>scrape_configs</code> 任务及其文件服务发现目标文件。目标文件独立存放，Prometheus 在文件变更时自动重新读取，新增 vCenter 无需重载 Prometheus。",
    "config.how": "工作原理",
    "config.cred_in_output": "输出中的凭证",
    "config.foot.ref": "file_sd_config 参考文档",
    "config.foot.validate": "使用 promtool check config 校验",
    "config.targets_count": "共 {n} 台 vCenter",

    "config.mode": "模式",
    "config.mode_hint": "任务的抓取方式",
    "config.mode_probe": "/probe — 多台 vCenter，一台导出器",
    "config.mode_metrics": "/metrics — 每台导出器一个目标",
    "config.mode_help_probe": "Prometheus 对每台 vCenter 抓取本导出器一次，将目标及凭证作为请求参数传递。",
    "config.mode_help_metrics": "Prometheus 直接抓取每个导出器。每台 vCenter 一个导出器进程，各占独立端口，凭证在启动参数中。",

    "config.style": "目标文件风格",
    "config.style_help_relabel": "目标文件仅包含 vCenter 地址。文件更简洁，导出器地址只需修改一处。",
    "config.style_help_inline": "每条记录携带自身参数，不同 vCenter 可在同一任务中使用不同设置，但导出器地址在每行重复。",
    "config.style_relabel": "仅地址，通过 relabel_configs 映射",
    "config.style_inline": "参数内联为标签",

    "config.job_name": "任务名称",
    "config.job_help": "成为该任务产生的每条时间序列上的 <code>job</code> 标签。",

    "config.interval": "抓取间隔",
    "config.timeout": "抓取超时",
    "config.exporter_addr": "导出器地址",
    "config.exporter_help_probe": "Prometheus 访问本导出器的地址。每个目标都通过此地址抓取。",
    "config.exporter_help_metrics": "第一个导出器的监听地址。其余从它的端口开始递增，每台 vCenter 一个进程。",
    "config.sd_path": "目标文件路径",
    "config.sd_help": "必须以 <code>.yml</code>、<code>.yaml</code> 或 <code>.json</code> 结尾，否则 Prometheus 会忽略。",

    "config.vcenter_scheme": "vCenter 协议",
    "config.vcenter_tls": "vCenter TLS",
    "config.vcenter_tls_txt": "跳过验证",
    "config.vcenter_tls_em": "不安全",

    "config.collectors_hint": "已选 {n} / {total} 个",
    "config.coll_help_probe": "所选采集器成为 collect[] 参数。全选时输出 collect[]=all。",
    "config.coll_help_metrics": "单目标模式下采集器是启动参数；生成的命令行仅列出与默认值不同的项。",

    "config.targets": "目标",
    "config.targets_hint": "台 vCenter",
    "config.targets_label": "每行一个：address，或 address,username,password",
    "config.targets_placeholder":
      "vcenter-prod-01.example.com,svc-prom@vsphere.local,secret\nvcenter-dev.example.com",
    "config.targets_help": "凭证可选。无凭证的行将回退到任务级用户名和密码。",
    "config.job_username": "任务级用户名",
    "config.cred_redact": "脱敏",
    "config.cred_redact_em": "写入占位符",
    "config.cred_note_none": "<strong>暂无目标。</strong>下方输出显示文件格式，添加地址即可填充。",
    "config.cred_note_metrics":
      " 个目标缺少用户名。请填写任务级用户名，或在这些行中添加凭证：生成的命令行会回退到无法登录的占位符。",
    "config.cred_note_probe":
      " 个目标缺少用户名。请填写任务级用户名，或在这些行中添加凭证：<code>/probe</code> 会拒绝缺少凭证的请求并返回 HTTP 400。",

    "config.output": "输出",
    "config.tab_sd": "目标文件",
    "config.tab_flags": "导出器参数",
    "config.copy": "复制",
    "config.copied": "已复制",
    "config.download": "下载",
    "config.out_hint": "行有效内容",

    // ── 配置页参考文档 ────────────────────────────────────────────────
    "config.ref.fsd.summary": "为什么使用文件服务发现",
    "config.ref.fsd.p1":
      "static_configs 块位于 prometheus.yml 内部，每新增一个 vCenter 都需要编辑 Prometheus 配置并重新加载。使用 file_sd_configs 后，目标列表是独立的文件，Prometheus 会监听其变化：写入新条目后，下一次抓取即生效，无需重载和重启。",
    "config.ref.fsd.p2":
      "监听基于文件系统，refresh_interval（默认 5 分钟）仅在漏掉变更通知时作为后备。文件必须以 .json、.yml 或 .yaml 结尾。",

    "config.ref.style.summary": "目标文件风格",
    "config.ref.style.relabel":
      "仅地址模式。文件只包含真实 vCenter 地址；relabel_configs 将每个地址改写为 __param_target，复制到 instance，然后将 __address__ 替换为导出器地址。导出器地址仅出现一次，文件保持简洁。",
    "config.ref.style.inline":
      "参数内联模式。每条记录携带自己的 __param_* 标签，不同 vCenter 可在同一任务中使用不同协议、TLS 设置或采集器。scrape config 缩减到极致，代价是每行重复导出器地址。",
    "config.ref.style.note":
      "此模式下必须显式设置 instance。Prometheus 仅从 __address__ 推导 instance，而此处 __address__ 是导出器地址——不设置的话所有 vCenter 将报告相同的 instance，指标互相覆盖。",

    "config.ref.cred.summary": "凭证存放位置",
    "config.ref.cred.p1":
      "对于 /probe，凭证以请求参数形式传递。放在目标文件而非 prometheus.yml 中，意味着可以对文件单独设置权限（chmod 600，由 Prometheus 用户所有），且每个 vCenter 可使用不同账号。",
    "config.ref.cred.p2":
      "对于 /metrics，凭证是导出器启动参数，不出现在任何 Prometheus 文件中——代价是每个 vCenter 需要一个独立的导出器进程。",
    "config.ref.cred.note":
      "脱敏默认开启。生成的输出中使用占位符代替输入的口令，可安全粘贴到工单中。仅在实际写入文件时关闭。",

    "config.ref.single.summary": "单目标模式：一个进程、一个端口、每个 vCenter",
    "config.ref.single.p1":
      "两种模式都输入 vCenter——区别在于输出。对于 /metrics，每个 vCenter 成为独立的导出器进程，生成的端口从表单值开始递增，抓取目标为这些监听地址而非 vCenter。两个进程不能共享端口：第二个会以 address already in use 退出。",
    "config.ref.single.p2":
      "按需重编端口以匹配配置管理系统的分配——只需保持目标文件与命令行同步即可，因为下游不会察觉漂移。",
    "config.ref.single.note":
      "vcenter 标签正是每条记录独立成组的原因。此导出器不输出自身的 vcenter 标签，因此若不在此处添加，区分两个 vCenter 的唯一依据是形如 localhost:9170 的 instance——技术上唯一，但在仪表盘上不可读。",

    // ── 超时告警 ──────────────────────────────────────────────────────
    "timeout.warn_exceed":
      "scrape_timeout 超过 scrape_interval。Prometheus 会拒绝加载此类配置。",
    "timeout.warn_heavy":
      " 会逐主机遍历清单，通常需要超过一分钟。请考虑更长的间隔和超时，或为它们单独创建一个任务。",
  };

  const en = {
    // ── 顶栏 ──────────────────────────────────────────────────────────
    "topbar.overview": "Overview",

    // ── 概览页 ────────────────────────────────────────────────────────
    "index.heading": "Endpoints",
    "index.desc":
      "This exporter collects metrics from VMware vCenter Server and from standalone ESXi hosts. The target type is detected at login, so the same endpoint serves both.",

    "ep.metrics.title": "One vCenter, or one ESXi host",
    "ep.metrics.kind": "single target",
    "ep.metrics.desc_disabled":
      "Currently serving only the exporter\u2019s own metrics: <code>-disable.exporter.target</code> is set, so no vCenter is contacted.",
    "ep.metrics.desc_enabled":
      "Credentials come from the global <code>-vmware.*</code> flags. Point Prometheus straight at it.",

    "ep.probe.title": "Many vCenters, one exporter",
    "ep.probe.kind": "many targets",
    "ep.probe.desc":
      "Target and credentials are supplied per request, so a single exporter can serve an estate of vCenters with different collectors enabled for each.",

    "ep.debug.title": "Try a scrape from the browser",
    "ep.debug.kind": "interactive",
    "ep.debug.desc":
      "Fill in a target, pick collectors, and read back the metrics a scrape would return. Useful for checking credentials and permissions before wiring up Prometheus.",

    "ep.config.title": "Build the Prometheus configuration",
    "ep.config.kind": "generator",
    "ep.config.desc":
      "Turn a list of vCenters into a <code>scrape_configs</code> job and its file service discovery targets, for either endpoint. Nothing is sent to the exporter: the files are built in the browser.",

    "section.collectors": "Collectors",
    "section.reference": "Reference",

    // ── 采集器标注 ────────────────────────────────────────────────────
    "default on": "default on",
    "default off": "default off",
    "per-host serial": "per-host serial",
    "needs vSAN": "needs vSAN",
    "needs perf service": "needs perf service",

    // ── 采集器说明 ────────────────────────────────────────────────────
    "vCenter and datacenter info": "vCenter and datacenter info",
    "Cluster information": "Cluster information",
    "Datastore metrics": "Datastore metrics",
    "ESXi host metrics": "ESXi host metrics",
    "Virtual machine metrics": "Virtual machine metrics",
    "Resource pool limits, reservations and usage":
      "Resource pool limits, reservations and usage",
    "ESXi NIC driver info": "ESXi NIC driver info",
    "ESXi storage info": "ESXi storage info",
    "vSAN cluster health, capacity and resync":
      "vSAN cluster health, capacity and resync",
    "vSAN performance statistics (requires the vSAN performance service)":
      "vSAN performance statistics (requires the vSAN performance service)",

    // ── 参考文档：单目标模式 ──────────────────────────────────────────
    "ref.single.summary": "Single target mode",
    "ref.single.p1":
      "Start the exporter with the target and its credentials, then let Prometheus scrape <code>/metrics</code>:",
    "ref.single.p2":
      "The same flags work against a standalone ESXi host. There is no separate mode to select: the exporter reads <code>ServiceContent.About.ApiType</code> at login and adapts, because a lone host has no real datacenter or cluster objects to walk.",
    "ref.single.note":
      "<strong>Passwords on the command line are visible</strong> to anyone who can run <code>ps</code> or read <code>/proc</code>. Prefer <code>-envflag.enable</code> with an environment file.",

    // ── 参考文档：时间参数 ────────────────────────────────────────────
    "ref.timing.summary": "Timing flags",
    "ref.timing.th_flag": "Flag",
    "ref.timing.th_default": "Default",
    "ref.timing.th_effect": "Effect",
    "ref.timing.timeout":
      "Overall budget in seconds for one scrape, covering login, property retrieval and performance sampling. Raise it for large inventories.",
    "ref.timing.interval":
      "PerfManager sampling window in seconds. Independent of the scrape timeout.",
    "ref.timing.granularity":
      "Sampling frequency in seconds. Must be greater than 0 and no larger than the interval.",

    // ── 参考文档：多目标模式 ──────────────────────────────────────────
    "ref.multi.summary": "Multi target mode",
    "ref.multi.required":
      "Required parameters: <code>target</code>, <code>username</code>, <code>password</code>. Optional: <code>schema</code> (defaults to https), <code>insecure=true</code> to skip TLS verification.",
    "ref.multi.credentials":
      "Credentials may also arrive as HTTP Basic Auth instead of URL parameters.",
    "ref.multi.basic_auth":
      "<strong>Basic Auth collides with <code>-web.config.file</code>.</strong> When the exporter\u2019s own listener has <code>basic_auth_users</code> configured, it consumes the <code>Authorization</code> header itself, so vCenter credentials cannot travel that way. Use URL parameters, or the debug console, which posts them as a form body.",
    "ref.multi.collectors":
      "Collectors are selected with repeatable <code>collect[]</code> and <code>nocollect[]</code> parameters:",
    "ref.multi.misspelled":
      "A misspelled collector name is rejected with HTTP 400 and the list of valid names, rather than silently producing an empty metric set.",

    // ── 参考文档：Prometheus 配置 ────────────────────────────────────
    "ref.prom.summary": "Prometheus configuration",
    "ref.prom.generator":
      "The <a href=\"/config\">configuration generator</a> builds these files from a list of vCenters, including the target file. The example below is what it produces for two jobs with different collectors.",
    "ref.prom.intro":
      "Different collectors for different vCenters, with credentials carried in the service discovery file rather than the scrape config:",
    // ── 页脚 ──────────────────────────────────────────────────────────
    "foot.source": "Source",
    "foot.metrics_ref": "Metric reference ships in docs/METRICS.md",

    // ── 调试页 ────────────────────────────────────────────────────────
    "debug.title": "Debug console",
    "debug.desc":
      "Run a single scrape against any reachable target and read back exactly what Prometheus would receive. Credentials are posted as a form body, so they stay out of the URL, the browser history and any access log that records query strings.",

    "debug.target": "Target",
    "debug.target_hint": "vCenter or ESXi",
    "debug.address": "Address",
    "debug.address_help": "A vCenter Server or a standalone ESXi host. The type is detected at login.",
    "debug.username": "Username",
    "debug.password": "Password",
    "debug.scheme": "Scheme",
    "debug.tls": "TLS",
    "debug.tls_txt": "Skip verify",
    "debug.tls_em": "insecure",

    "debug.collectors": "Collectors",
    "debug.sel_count": "{n} of {total} selected",
    "debug.sel_all": "Select all",
    "debug.sel_default": "Defaults",
    "debug.sel_clear": "Clear",
    "debug.run": "Run scrape",
    "debug.running": "Running",
    "debug.copy_url": "Copy probe URL",
    "debug.copied": "Copied",

    "debug.result": "Result",
    "debug.not_run": "Not run yet",
    "debug.stat_http": "HTTP status",
    "debug.stat_elapsed": "Elapsed",
    "debug.stat_series": "Time series",
    "debug.stat_up": "vmware_up",

    "debug.empty": "Fill in a target on the left, then run a scrape to see the metrics it returns.",
    "debug.filter_placeholder": "filter lines\u2026",
    "debug.req_failed": "Request failed",
    "debug.req_note": "The request never completed: ",
    "debug.series_fmt": " time series",
    "debug.series_note": " series across {n} lines",
    "debug.rejected": "The exporter rejected the request; its response is shown below.",

    // ── 配置页 ────────────────────────────────────────────────────────
    "config.title": "Prometheus configuration",
    "config.desc":
      "Generate a <code>scrape_configs</code> job and its file service discovery targets. Targets live in a separate file that Prometheus re-reads on change, so adding a vCenter never requires a Prometheus reload.",
    "config.how": "How this works",
    "config.cred_in_output": "Credentials in output",
    "config.foot.ref": "file_sd_config reference",
    "config.foot.validate": "Validate with promtool check config",
    "config.targets_count": "{n} vCenters",

    "config.mode": "Mode",
    "config.mode_hint": "what the job scrapes",
    "config.mode_probe": "/probe \u2014 many vCenters, one exporter",
    "config.mode_metrics": "/metrics \u2014 one target per exporter",
    "config.mode_help_probe":
      "Prometheus scrapes this exporter once per vCenter, passing the target and its credentials as request parameters.",
    "config.mode_help_metrics":
      "Prometheus scrapes each exporter directly. One exporter process per vCenter, each on its own port, with credentials in its start-up flags.",

    "config.style": "Target file style",
    "config.style_help_relabel":
      "The target file holds vCenter addresses only. Cleaner file, one place to change the exporter address.",
    "config.style_help_inline":
      "Every entry carries its own parameters. Lets vCenters differ within one job, but repeats the exporter address.",
    "config.style_relabel": "Addresses only, mapped by relabel_configs",
    "config.style_inline": "Parameters inlined as labels",

    "config.job_name": "Job name",
    "config.job_help": "Becomes the job label on every series this job produces.",

    "config.interval": "Scrape interval",
    "config.timeout": "Scrape timeout",
    "config.exporter_addr": "Exporter address",
    "config.exporter_help_probe":
      "Where Prometheus reaches this exporter. Every target is scraped through it.",
    "config.exporter_help_metrics":
      "The listen address of the first exporter. The rest count up from its port, one process per vCenter.",
    "config.sd_path": "Target file path",
    "config.sd_help":
      "Must end in .yml, .yaml or .json, otherwise Prometheus ignores it.",

    "config.vcenter_scheme": "vCenter scheme",
    "config.vcenter_tls": "vCenter TLS",
    "config.vcenter_tls_txt": "Skip verify",
    "config.vcenter_tls_em": "insecure",

    "config.collectors_hint": "{n} of {total} selected",
    "config.coll_help_probe":
      "Selected collectors become collect[] parameters. Selecting all of them emits collect[]=all.",
    "config.coll_help_metrics":
      "In single target mode collectors are start-up flags; the generated command line only lists the ones that differ from the defaults.",

    "config.targets": "Targets",
    "config.targets_hint": " vCenters",
    "config.targets_label":
      "One per line: address, or address,username,password",
    "config.targets_placeholder":
      "vcenter-prod-01.example.com,svc-prom@vsphere.local,secret\nvcenter-dev.example.com",
    "config.targets_help":
      "Credentials are optional here. Lines without them fall back to the job-wide username and password below.",
    "config.job_username": "Job-wide username",
    "config.cred_redact": "Redact",
    "config.cred_redact_em": "write placeholders",
    "config.cred_note_none":
      "<strong>No targets yet.</strong> The output below shows the shape of the files; add addresses to fill them in.",
    "config.cred_note_metrics":
      " target(s) have no username. Fill in the job-wide username, or add credentials on those lines: the generated command lines fall back to a placeholder that will not log in.",
    "config.cred_note_probe":
      " target(s) have no username. Fill in the job-wide username, or add credentials on those lines: <code>/probe</code> rejects a request without them with HTTP 400.",

    "config.output": "Output",
    "config.tab_sd": "Target file",
    "config.tab_flags": "Exporter flags",
    "config.copy": "Copy",
    "config.copied": "Copied",
    "config.download": "Download",
    "config.out_hint": " significant lines",

    // ── 配置页参考文档 ────────────────────────────────────────────────
    "config.ref.fsd.summary": "Why file service discovery",
    "config.ref.fsd.p1":
      "A static_configs block lives inside prometheus.yml, so every new vCenter means editing the Prometheus configuration and reloading it. With file_sd_configs the target list is a separate file that Prometheus watches: writing a new entry takes effect on the next scrape, with no reload and no restart.",
    "config.ref.fsd.p2":
      "The watch is filesystem based, and refresh_interval (5 minutes by default) is only a fallback for when change notifications are missed. The file must end in .json, .yml or .yaml.",

    "config.ref.style.summary": "Target file styles",
    "config.ref.style.relabel":
      "Addresses only. The file holds nothing but real vCenter addresses; relabel_configs rewrites each one into __param_target, copies it into instance, and then replaces __address__ with the exporter. The exporter address appears exactly once, and the file stays readable.",
    "config.ref.style.inline":
      "Parameters inlined. Each entry carries its own __param_* labels, so different vCenters can use different schemes, TLS settings or collectors within a single job. The scrape config shrinks to almost nothing, at the cost of repeating the exporter address on every line.",
    "config.ref.style.note":
      "instance must be set explicitly in this style. Prometheus only derives instance from __address__, which here is the exporter\u2014so without it every vCenter reports under the same instance and their series overwrite each other.",

    "config.ref.cred.summary": "Where credentials live",
    "config.ref.cred.p1":
      "For /probe, credentials travel as request parameters. Keeping them in the target file rather than in prometheus.yml means the file can be permissioned separately (chmod 600, owned by the Prometheus user) and per-vCenter accounts stay possible.",
    "config.ref.cred.p2":
      "For /metrics, credentials are exporter start-up flags and appear in no Prometheus file at all\u2014the trade-off being one exporter process per vCenter.",
    "config.ref.cred.note":
      "Redaction is on by default. Generated output carries placeholders instead of the passwords you typed, so it is safe to paste into a ticket. Turn it off only when writing the real file.",

    "config.ref.single.summary": "Single target mode: one process, one port, per vCenter",
    "config.ref.single.p1":
      "Enter vCenters in both modes\u2014the difference is what comes out. For /metrics each vCenter becomes its own exporter process, so the generated ports count up from the one in the form and the scrape targets are those listen addresses, not the vCenters. Two processes cannot share a port: the second exits with address already in use.",
    "config.ref.single.p2":
      "Renumber the ports to match whatever your configuration management allocates\u2014just keep the target file and the command lines in step, because nothing downstream will notice if they drift.",
    "config.ref.single.note":
      "The vcenter label is why each entry is its own group. This exporter emits no vcenter label of its own, so without one added here the only thing distinguishing two vCenters is an instance like localhost:9170\u2014technically unique, unreadable on a dashboard.",

    // ── 超时告警 ──────────────────────────────────────────────────────
    "timeout.warn_exceed":
      "scrape_timeout exceeds scrape_interval. Prometheus refuses to load a configuration like this.",
    "timeout.warn_heavy":
      " walk the inventory host by host and regularly need more than a minute. Consider a longer interval and timeout, or a separate job for them.",
  };

  // ── 当前语言 ────────────────────────────────────────────────────────
  let currentLang = localStorage.getItem(STORAGE_KEY) || "zh";

  // ── 翻译函数 ────────────────────────────────────────────────────────
  function __(key) {
    const dict = currentLang === "zh" ? zh : en;
    return dict[key] !== undefined ? dict[key] : key;
  }

  // ── 模板替换：{n} 替换为传入的值 ────────────────────────────────────
  function __fmt(key, replacements) {
    let s = __(key);
    if (replacements) {
      for (const [k, v] of Object.entries(replacements)) {
        s = s.replace(new RegExp("\\{" + k + "\\}", "g"), v);
      }
    }
    return s;
  }

  // ── 设置语言 ────────────────────────────────────────────────────────
  function setLang(lang) {
    if (lang !== "zh" && lang !== "en") return;
    currentLang = lang;
    localStorage.setItem(STORAGE_KEY, lang);
    translatePage();
    document.documentElement.lang = lang === "zh" ? "zh-CN" : "en";
  }

  // ── 获取当前语言 ────────────────────────────────────────────────────
  function getLang() {
    return currentLang;
  }

  // ── 翻译整个页面 ────────────────────────────────────────────────────
  function translatePage() {
    // 1. 翻译 data-i18n 元素
    document.querySelectorAll("[data-i18n]").forEach((el) => {
      const key = el.dataset.i18n;
      const translated = __(key);
      if (translated !== key) {
        el.textContent = translated;
      }
    });

    // 2. 翻译 data-i18n-placeholder
    document.querySelectorAll("[data-i18n-placeholder]").forEach((el) => {
      const key = el.dataset.i18nPlaceholder;
      el.placeholder = __(key);
    });

    // 3. 翻译 data-i18n-title
    document.querySelectorAll("[data-i18n-title]").forEach((el) => {
      const key = el.dataset.i18nTitle;
      el.title = __(key);
    });

    // 4. 翻译 data-i18n-html（innerHTML 替换，注意 XSS 风险——此处均为可控翻译文本）
    document.querySelectorAll("[data-i18n-html]").forEach((el) => {
      const key = el.dataset.i18nHtml;
      const translated = __(key);
      if (translated !== key) {
        el.innerHTML = translated;
      }
    });

    // 5. 翻译采集器说明（.coll-desc）
    document.querySelectorAll(".coll-desc").forEach((el) => {
      const text = el.textContent.trim();
      const translated = __(text);
      if (translated !== text) {
        el.textContent = translated;
      }
    });

    // 6. 翻译采集器标签（.tag）
    document.querySelectorAll(".tag").forEach((el) => {
      const text = el.textContent.trim();
      const translated = __(text);
      if (translated !== text) {
        el.textContent = translated;
      }
    });

    // 7. 更新语言切换按钮
    document.querySelectorAll(".lang-btn").forEach((el) => {
      el.textContent = currentLang === "zh" ? "EN" : "中文";
    });

    // 8. 更新 html lang 属性
    document.documentElement.lang = currentLang === "zh" ? "zh-CN" : "en";

    // 9. 触发自定义事件，让 JS 驱动的页面（调试页、配置页）更新动态文本
    document.dispatchEvent(
      new CustomEvent("langchange", { detail: { lang: currentLang } }),
    );
  }

  // ── 初始化 ──────────────────────────────────────────────────────────
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", translatePage);
  } else {
    translatePage();
  }

  // ── 公开 API ────────────────────────────────────────────────────────
  return {
    __: __,
    __fmt: __fmt,
    setLang: setLang,
    getLang: getLang,
    translatePage: translatePage,
  };
})();