// 配置生成页的前端逻辑。
//
// 与 app.js 一样不引任何库，包括 YAML 序列化库：这个页面产出的 YAML 结构是
// 固定的几种模板，手写字符串拼接足以覆盖，而引入 js-yaml 就要么加一条前端
// 构建链、要么在离线管理网段里加载一个取不到的 CDN 脚本。
//
// 生成的配置应当能通过 promtool check config。Go 侧的测试用 yaml.v3 的严格
// 模式解析生成结果 —— 与 Prometheus 自己用的解析器同一个，重复键与未知字段
// 都会被拒 —— 再断言各字段指向正确的东西。改动这个文件里任何一处缩进或键名
// 之后，都应该重跑那些测试。
//
// 采集器清单同样来自 Go 侧注册表渲染的复选框，这里只读 DOM。

"use strict";

const $ = (id) => document.getElementById(id);

// PLACEHOLDER 是脱敏开关打开时写进输出的口令占位符。
//
// 用尖括号包起来而不是写 "changeme"：前者一眼能看出是待替换的槽位，后者
// 会被当成一个真实的弱口令直接留在配置里。
const PLACEHOLDER = "<password>";

const list = $("collList");
const boxes = () => Array.from(list.querySelectorAll("input[type=checkbox]"));
const selected = () => boxes().filter((b) => b.checked).map((b) => b.value);

// mode 返回当前端点模式：probe（多目标）或 metrics（单目标）。
const mode = () => $("mode").value;

// style 返回目标文件风格：relabel（只放地址）或 inline（标签内联）。
//
// 单目标模式下这个选择不适用 —— /metrics 没有任何请求参数要映射。
const style = () => (mode() === "metrics" ? "relabel" : $("style").value);

// yamlStr 把一个值渲染成 YAML 标量。
//
// 一律加单引号，因为目标里的内容全是外部输入：一个叫 "true" 的主机名、
// 一个纯数字的 IP 段、或是密码里的 # 与 : 都会让裸标量被解析成别的类型或
// 提前截断。单引号内只需要把单引号自身写成两个，是 YAML 里最简单的转义。
function yamlStr(s) {
  return "'" + String(s).replace(/'/g, "''") + "'";
}

// parseTargets 解析目标清单文本框。
//
// 每行可以是 address，也可以是 address,username,password。逗号分隔而不是
// 空格：vCenter 的用户名形如 svc@vsphere.local，密码里出现空格也是常事，
// 而地址本身不可能含逗号。
//
// 空行与 # 开头的行忽略，这样运维可以直接粘贴带注释的清单，也可以临时
// 注释掉一台正在维护的 vCenter 而不用删掉那一行。
function parseTargets() {
  const raw = $("targets").value;
  const fallbackUser = $("username").value.trim();
  const out = [];

  raw.split("\n").forEach((line) => {
    const t = line.trim();

    if (t === "" || t.startsWith("#")) {
      return;
    }

    // 只切前两个逗号：密码里含逗号时剩下的部分应当留在密码里。
    const first = t.indexOf(",");

    if (first === -1) {
      out.push({ address: t, username: fallbackUser, password: "" });

      return;
    }

    const second = t.indexOf(",", first + 1);

    if (second === -1) {
      out.push({
        address: t.slice(0, first).trim(),
        username: t.slice(first + 1).trim(),
        password: "",
      });

      return;
    }

    out.push({
      address: t.slice(0, first).trim(),
      username: t.slice(first + 1, second).trim(),
      password: t.slice(second + 1),
    });
  });

  return out;
}

// splitHostPort 把 host:port 拆开，没有端口时 port 为空。
//
// 只按最后一个冒号切，这样 IPv6 的字面量（含多个冒号）不会被切错 —— 那种
// 地址写成 [::1]:9169，最后一个冒号仍然是端口分隔符。
function splitHostPort(addr) {
  const i = addr.lastIndexOf(":");

  // 冒号在方括号里说明这是没带端口的 IPv6 字面量。
  if (i === -1 || addr.indexOf("]", i) !== -1) {
    return { host: addr, port: "" };
  }

  return { host: addr.slice(0, i), port: addr.slice(i + 1) };
}

// exporterAddrs 给单目标模式下的每台 vCenter 分配一个 exporter 监听地址。
//
// 为什么需要这个：单目标模式是「一个 exporter 进程绑一台 vCenter」，所以有
// N 台 vCenter 就有 N 个进程。它们不能共用一个端口 —— 第二个进程会以
// "address already in use" 退出 —— 也不能共用一个抓取目标，因为这个
// exporter 的指标里没有 vcenter 标签，Prometheus 只能靠 instance 区分它们，
// 而 instance 的兜底值正是 __address__。两台 vCenter 落在同一个 instance 上
// 的后果是两份指标互相覆盖：抓取成功，图也画得出来，数字是错的。
//
// 端口从表单里填的那个开始按顺序加一。这只是一个能跑起来的起点，不是主张
// 端口必须连号；真实部署里多半由配置管理分配，那时把生成的地址改掉即可。
function exporterAddrs(count) {
  const base = $("exporter").value.trim() || "localhost:9169";
  const parts = splitHostPort(base);
  const port = parseInt(parts.port, 10);
  const out = [];

  for (let i = 0; i < count; i++) {
    // 端口不是数字（比如只填了主机名）时不猜，原样重复 —— 猜一个默认端口
    // 会生成一份看起来没问题、实际指向不存在的监听地址的配置。
    if (parts.port === "" || Number.isNaN(port)) {
      out.push(base);

      continue;
    }

    out.push(parts.host + ":" + (port + i));
  }

  return out;
}

// secret 决定写进输出的口令：脱敏开关打开时一律是占位符。
function secret(value) {
  if ($("redact").checked) {
    return PLACEHOLDER;
  }

  return value === "" ? PLACEHOLDER : value;
}

// collectParams 返回要写进配置的 collect[] 值列表。
//
// 全选时用 all，与 /probe 的文档写法一致；一个都不选时返回空列表，表示
// 「不写 collect[]，让 exporter 用各采集器的默认状态」。这和 app.js 里
// buildParams 的判断保持一致，两个页面对同一语义不该有两种理解。
function collectParams() {
  const sel = selected();

  if (sel.length === 0) {
    return [];
  }

  if (sel.length === boxes().length) {
    return ["all"];
  }

  return sel;
}

// buildTargetFile 生成 file_sd 的目标文件。
//
// 输出是 YAML 而不是 JSON：两种格式 Prometheus 都接受，但 YAML 能带注释，
// 而一份会被手工追加新 vCenter 的文件里，注释就是给下一个人的说明。
function buildTargetFile() {
  const targets = parseTargets();
  const lines = [];

  lines.push("# Managed by hand or by your configuration management.");
  lines.push("# Prometheus re-reads this file when it changes; no reload needed.");

  if (mode() === "metrics") {
    // 单目标模式：目标文件里放的是 exporter 实例本身的地址，一个 exporter
    // 对应一台 vCenter。vCenter 的地址与凭证都在 exporter 的启动参数里，
    // 所以这份文件里没有任何密码。
    //
    // 每台一条 target group 而不是一个地址列表：每条都要带自己的 vcenter
    // 标签。这个 exporter 的指标里没有 vcenter 标签，Prometheus 侧不加的话
    // 就只剩 instance（形如 localhost:9170）能区分，看板上没人认得出那是哪
    // 台 vCenter。
    lines.push("#");
    lines.push("# Each entry is an exporter instance, already bound to one vCenter.");
    lines.push("# Credentials live in that exporter's start-up flags, not here.");

    if (targets.length === 0) {
      lines.push("# (no targets entered yet)");
      lines.push("# - targets: ['" + (exporterAddrs(1)[0]) + "']");
      lines.push("#   labels:");
      lines.push("#     vcenter: 'vcenter.example.com'");

      return lines.join("\n") + "\n";
    }

    const addrs = exporterAddrs(targets.length);

    targets.forEach((t, i) => {
      lines.push("");
      lines.push("- targets: [" + yamlStr(addrs[i]) + "]");
      lines.push("  labels:");
      lines.push("    vcenter: " + yamlStr(t.address));
    });

    return lines.join("\n") + "\n";
  }

  if (style() === "relabel") {
    // 只放真实 vCenter 地址加凭证。地址如何变成 __param_target、以及
    // __address__ 如何被替换成 exporter，全部交给 scrape config 里的
    // relabel_configs —— exporter 地址因此只出现一次。
    lines.push("#");
    lines.push("# Addresses are real vCenters. relabel_configs in prometheus.yml");
    lines.push("# rewrites them into /probe parameters.");

    if (targets.length === 0) {
      lines.push("# (no targets entered yet)");
      lines.push("# - targets: ['vcenter.example.com']");
      lines.push("#   labels:");
      lines.push("#     __meta_username: 'svc-prometheus@vsphere.local'");
      lines.push("#     __meta_password: '" + PLACEHOLDER + "'");

      return lines.join("\n") + "\n";
    }

    targets.forEach((t) => {
      lines.push("");
      lines.push("- targets: [" + yamlStr(t.address) + "]");
      lines.push("  labels:");
      // 用 __meta_ 前缀承载凭证，再由 relabel 搬到 __param_username /
      // __param_password。所有 __ 开头的标签在 relabel 结束后都会被丢弃，
      // 所以凭证不会出现在任何一条时间序列上。
      lines.push("    __meta_username: " + yamlStr(t.username));
      lines.push("    __meta_password: " + yamlStr(secret(t.password)));
    });

    return lines.join("\n") + "\n";
  }

  // 标签内联风格：每条记录自己带齐 /probe 需要的全部参数。
  //
  // 官方文档明确 relabel 一节提到的特殊标签（__param_*、__scheme__）可以
  // 直接写在这里，所以 scrape config 可以压缩到几行。代价是 exporter 地址
  // 在每一行重复，而且 instance 必须显式写出来 —— 见下面的注释。
  lines.push("#");
  lines.push("# Each entry carries its own /probe parameters, so different");
  lines.push("# vCenters can use different schemes, TLS settings or collectors.");

  const exporter = $("exporter").value.trim() || "localhost:9169";
  const params = collectParams();
  const schema = $("schema").value;
  const insecure = $("insecure").checked;

  if (targets.length === 0) {
    lines.push("# (no targets entered yet)");

    return lines.join("\n") + "\n";
  }

  targets.forEach((t) => {
    lines.push("");
    lines.push("- targets: [" + yamlStr(exporter) + "]");
    lines.push("  labels:");
    lines.push("    __param_target: " + yamlStr(t.address));
    lines.push("    __param_username: " + yamlStr(t.username));
    lines.push("    __param_password: " + yamlStr(secret(t.password)));
    lines.push("    __param_schema: " + yamlStr(schema));

    if (insecure) {
      lines.push("    __param_insecure: 'true'");
    }

    // 采集器只在恰好一项时能内联。
    //
    // __param_<name> 是一个标签，一条记录里同名标签只能有一个值，而
    // collect[] 需要重复出现才能表达多个采集器 —— 写成两行 YAML 是重复键
    // （Prometheus 直接拒绝加载），写成逗号拼接则会被 exporter 当作一个
    // 拼错的采集器名、返回 400。所以多项时统一退到 job 级的 params，由
    // buildScrapeConfig 负责；那里可以写成真正的列表。
    if (params.length === 1) {
      lines.push("    '__param_collect[]': " + yamlStr(params[0]));
    }

    // instance 必须显式给。Prometheus 只在 instance 缺失时用 __address__
    // 兜底，而这里的 __address__ 是 exporter 自己 —— 不写的话几十台
    // vCenter 会全部落到同一个 instance 上，指标互相覆盖。
    lines.push("    instance: " + yamlStr(t.address));
  });

  return lines.join("\n") + "\n";
}

// buildScrapeConfig 生成 prometheus.yml 里的那一段 scrape_configs。
//
// 输出包含 scrape_configs 顶层键，方便整段粘贴；已经有其它 job 的人删掉
// 第一行即可。
function buildScrapeConfig() {
  const job = $("job").value.trim() || "vmware";
  const sdPath = $("sdPath").value.trim() || "/etc/prometheus/targets/vmware.yml";
  const interval = $("interval").value.trim();
  const timeout = $("timeout").value.trim();
  const exporter = $("exporter").value.trim() || "localhost:9169";
  const lines = [];

  lines.push("scrape_configs:");
  lines.push("  - job_name: " + yamlStr(job));

  if (interval !== "") {
    lines.push("    scrape_interval: " + interval);
  }

  if (timeout !== "") {
    // scrape_timeout 必须小于 scrape_interval，Prometheus 在加载配置时就会
    // 拒绝反过来的组合。页面上有一处提示负责在输入时就把这个说清楚。
    lines.push("    scrape_timeout: " + timeout);
  }

  if (mode() === "metrics") {
    // 单目标模式：Prometheus 直接抓 exporter 的 /metrics，没有任何参数
    // 要传，也没有 relabel 要做。metrics_path 是默认值，仍然写出来 ——
    // 这份配置的读者需要一眼看出它抓的是哪个端点。
    lines.push("    metrics_path: /metrics");
    lines.push("    file_sd_configs:");
    lines.push("      - files: [" + yamlStr(sdPath) + "]");
    lines.push("");
    lines.push("    # Nothing to relabel: each exporter already serves exactly one");
    lines.push("    # vCenter, and instance defaults to the exporter's address.");

    return lines.join("\n") + "\n";
  }

  lines.push("    metrics_path: /probe");
  lines.push("    file_sd_configs:");
  lines.push("      - files: [" + yamlStr(sdPath) + "]");

  const params = collectParams();
  const schema = $("schema").value;
  const insecure = $("insecure").checked;

  if (style() === "inline") {
    // 参数基本都在目标文件的标签里，这里只在必要时补 collect[]。
    //
    // 一项采集器可以内联成 __param_collect[]，多项不行（同名标签只能有一个
    // 值），所以多项时必须落在这里 —— params 下的 collect[] 是一个真正的
    // YAML 列表，可以有多个元素。这也意味着多项采集器在内联风格下是 job 级
    // 统一的，无法逐 vCenter 区分；要逐台不同就得拆成多个 job。
    if (params.length > 1) {
      lines.push("    params:");
      lines.push("      collect[]: [" + params.map(yamlStr).join(", ") + "]");
      lines.push("");
      lines.push("    # collect[] cannot be inlined per target: a label holds one");
      lines.push("    # value, so several collectors have to be a job-wide list.");
      lines.push("    # Everything else comes from the target file's labels,");
      lines.push("    # including instance, which must be set there explicitly.");

      return lines.join("\n") + "\n";
    }

    lines.push("");
    lines.push("    # All /probe parameters come from the target file's labels,");
    lines.push("    # including instance, which must be set there explicitly.");

    return lines.join("\n") + "\n";
  }

  // relabel 风格：job 级别的公共参数写在 params，凭证与地址由 relabel 从
  // 目标文件的标签搬过来。
  lines.push("    params:");
  lines.push("      schema: [" + yamlStr(schema) + "]");

  if (insecure) {
    lines.push("      insecure: ['true']");
  }

  if (params.length > 0) {
    lines.push("      collect[]: [" + params.map(yamlStr).join(", ") + "]");
  }

  lines.push("    relabel_configs:");
  lines.push("      # Credentials: target file label -> request parameter.");
  lines.push("      - source_labels: [__meta_username]");
  lines.push("        target_label: __param_username");
  lines.push("      - source_labels: [__meta_password]");
  lines.push("        target_label: __param_password");
  lines.push("");
  lines.push("      # The discovered address is the vCenter to probe.");
  lines.push("      - source_labels: [__address__]");
  lines.push("        target_label: __param_target");
  lines.push("");
  lines.push("      # Label the series by vCenter, not by the exporter.");
  lines.push("      - source_labels: [__param_target]");
  lines.push("        target_label: instance");
  lines.push("");
  lines.push("      # Send the request to the exporter. Must come last: the rules");
  lines.push("      # above read __address__, and this one overwrites it.");
  lines.push("      - target_label: __address__");
  lines.push("        replacement: " + yamlStr(exporter));

  return lines.join("\n") + "\n";
}

// buildFlags 生成 exporter 的启动参数。
//
// 两种模式的启动方式差别很大，这一栏的作用是让人不必回落地页去对照：多目标
// 模式下 exporter 不需要任何 vCenter 参数，单目标模式下则全靠它们。
function buildFlags() {
  const targets = parseTargets();
  const exporter = $("exporter").value.trim() || "localhost:9169";
  const insecure = $("insecure").checked ? "true" : "false";
  const lines = [];

  if (mode() !== "metrics") {
    lines.push("# Multi target mode: one exporter serves the whole estate. It needs");
    lines.push("# no vCenter flags -- target and credentials arrive per request.");
    lines.push("");
    lines.push("./vmware-exporter \\");
    lines.push("  -http.address=" + exporter + " \\");
    lines.push("  -disable.exporter.target=true");
    lines.push("");
    lines.push("# -disable.exporter.target stops /metrics from reaching a vCenter of");
    lines.push("# its own, so it serves only the exporter's own metrics.");

    return lines.join("\n") + "\n";
  }

  lines.push("# One exporter per vCenter. Credentials are flags, so they appear in");
  lines.push("# no Prometheus file at all.");
  lines.push("#");
  lines.push("# Each process needs its own port: two of them on the same address");
  lines.push("# means the second exits with `address already in use`. The ports");
  lines.push("# below just count up from the one in the form -- renumber them to");
  lines.push("# match whatever your configuration management allocates, and keep");
  lines.push("# the target file in step.");
  lines.push("#");
  lines.push("# Passwords on a command line are readable by anyone who can run ps.");
  lines.push("# Prefer -envflag.enable with a systemd EnvironmentFile.");
  lines.push("");

  // 采集器开关在单目标模式下是启动参数而不是请求参数。与默认状态不同的项
  // 才需要写出来，两个方向都要处理：未勾选的默认开启项要显式关掉，勾选的
  // 默认关闭项要显式打开。只处理一边会让页面勾选与实际抓取不一致。
  const toggles = boxes()
    .filter((b) => b.checked !== (b.dataset.default === "true"))
    .map((b) => "  -collector." + b.value + "=" + (b.checked ? "true" : "false"));

  const emit = (listen, address, username, password) => {
    lines.push("./vmware-exporter \\");
    lines.push("  -http.address=" + listen + " \\");
    lines.push("  -vmware.vcenter=" + address + " \\");
    lines.push("  -vmware.username=" + username + " \\");
    lines.push("  -vmware.password=" + password + " \\");

    // insecureTLS 放在采集器开关之前，最后一行不能有续行反斜杠。
    if (toggles.length === 0) {
      lines.push("  -vmware.insecureTLS=" + insecure);

      return;
    }

    lines.push("  -vmware.insecureTLS=" + insecure + " \\");
    lines.push(toggles.join(" \\\n"));
  };

  if (targets.length === 0) {
    emit(
      exporterAddrs(1)[0],
      "vcenter.example.com",
      "svc-prometheus@vsphere.local",
      PLACEHOLDER,
    );

    return lines.join("\n") + "\n";
  }

  const addrs = exporterAddrs(targets.length);

  targets.forEach((t, i) => {
    if (i > 0) {
      lines.push("");
    }

    emit(
      addrs[i],
      t.address,
      t.username === "" ? "svc-prometheus@vsphere.local" : t.username,
      secret(t.password),
    );
  });

  return lines.join("\n") + "\n";
}

// ---------------------------------------------------------------------------
// UI 状态与渲染
// ---------------------------------------------------------------------------

// tab 记录当前展示的是哪一份输出。
let tab = "sd";

// build 按当前标签生成对应文本。
function build() {
  if (tab === "job") {
    return buildScrapeConfig();
  }

  if (tab === "flags") {
    return buildFlags();
  }

  return buildTargetFile();
}

// fileName 给下载按钮一个合适的文件名。
//
// 目标文件用输入框里的路径末段，这样下载下来的名字与要落盘的名字一致；
// 路径里含目录时 basename 才是文件名。
function fileName() {
  if (tab === "job") {
    return "prometheus-" + ($("job").value.trim() || "vmware") + ".yml";
  }

  if (tab === "flags") {
    return "vmware-exporter-flags.sh";
  }

  const path = $("sdPath").value.trim();
  const base = path.split("/").pop();

  return base === "" ? "vmware-targets.yml" : base;
}

// syncMode 让页面随模式变化调整可见性与说明文字。
//
// 单目标模式下目标文件风格这一项不适用：/metrics 没有任何请求参数要映射。
// 隐藏而不是禁用，是因为一个灰掉的下拉框仍然会让人琢磨它是什么意思。
//
// 两种模式下 Targets 框填的都是 vCenter —— 只有一个输入契约。区别在输出：
// 单目标模式把 vCenter 变成一组 exporter 进程的启动参数，抓取目标则是那些
// 进程的监听地址。
function syncMode() {
  const single = mode() === "metrics";

  $("styleField").hidden = single;
  $("targetsCard").querySelector(".card-head .hint").textContent = i18n.__("config.targets_hint");

  $("modeHelp").textContent = single
    ? i18n.__("config.mode_help_metrics")
    : i18n.__("config.mode_help_probe");

  $("styleHelp").textContent =
    style() === "relabel"
      ? i18n.__("config.style_help_relabel")
      : i18n.__("config.style_help_inline");

  $("exporterField").querySelector(".help").textContent = single
    ? i18n.__("config.exporter_help_metrics")
    : i18n.__("config.exporter_help_probe");

  $("collHelp").textContent = single
    ? i18n.__("config.coll_help_metrics")
    : i18n.__("config.coll_help_probe");

  render();
}

// syncCounts 更新两处计数与凭证提示。
function syncCounts() {
  const targets = parseTargets();

  // 计数提示整条由 i18n 渲染：中英文语序不同（「共 3 台 vCenter」对
  // "3 vCenters"），把数字塞进固定 span 再拼死语序，切语言就读不通。
  $("tHint").textContent = i18n.__fmt("config.targets_count", {
    n: targets.length,
  });

  $("collHint").textContent = i18n.__fmt("config.collectors_hint", {
    n: selected().length,
    total: boxes().length,
  });

  const missing = targets.filter((t) => t.username === "").length;

  if (missing > 0) {
    // 两种模式都要提示，只是后果不同：多目标模式下缺用户名会让 /probe 直接
    // 返回 400，单目标模式下则是把一个占位用户名写进启动参数 —— 后者更隐蔽，
    // 因为命令行看着是完整的，要等进程起来登录失败才发现。
    $("credNote").hidden = false;
    $("credNote").innerHTML =
      "<strong>" +
      missing +
      "</strong>" +
      (mode() === "metrics"
        ? i18n.__("config.cred_note_metrics")
        : i18n.__("config.cred_note_probe"));
  } else if (targets.length === 0) {
    $("credNote").hidden = false;
    $("credNote").innerHTML = i18n.__("config.cred_note_none");
  } else {
    $("credNote").hidden = true;
  }
}

// syncTimeout 在 scrape_timeout 与 scrape_interval 冲突、或勾了高开销采集器
// 时给出提示。
//
// 为什么值得一处专门的提示：scrape_timeout 大于 scrape_interval 会让
// Prometheus 直接拒绝加载整份配置，而 vsan.perf 与 esxcli 这两类采集器在
// 大清单上很容易超过默认的 10s，症状是抓取超时而不是报错。
function syncTimeout() {
  const secs = (v) => {
    const m = /^(\d+)(s|m)?$/.exec(v.trim());

    if (m === null) {
      return NaN;
    }

    return m[2] === "m" ? Number(m[1]) * 60 : Number(m[1]);
  };

  const interval = secs($("interval").value);
  const timeout = secs($("timeout").value);
  const heavy = selected().filter((n) => n === "vsan.perf" || n.startsWith("esxcli."));
  const notes = [];

  if (!Number.isNaN(interval) && !Number.isNaN(timeout) && timeout > interval) {
    notes.push(
      "<strong>" + i18n.__("timeout.warn_exceed") + "</strong>",
    );
  }

  if (heavy.length > 0 && !Number.isNaN(timeout) && timeout < 60) {
    notes.push(
      "<strong>" +
        heavy.join(", ") +
        "</strong>" +
        i18n.__("timeout.warn_heavy"),
    );
  }

  $("timeoutWarn").hidden = notes.length === 0;
  $("timeoutWarn").innerHTML = notes.join("<br><br>");
}

// render 重算输出区。
function render() {
  syncCounts();
  syncTimeout();

  const text = build();

  $("out").textContent = text;
  $("outPath").textContent = tab === "sd" ? $("sdPath").value.trim() : "";

  const lines = text.split("\n").filter((l) => l !== "" && !l.trim().startsWith("#"));

  $("outHint").textContent = lines.length + i18n.__("config.out_hint");
}

// 输入变化一律重新生成。全量重算而不是增量更新：整份输出也就几十行，重算的
// 成本可以忽略，而增量更新需要维护一份「哪个输入影响哪几行」的映射，那才是
// 真正会出错的地方。
[
  "mode",
  "style",
  "job",
  "interval",
  "timeout",
  "exporter",
  "sdPath",
  "schema",
  "insecure",
  "targets",
  "username",
  "redact",
].forEach((id) => {
  const el = $(id);

  el.addEventListener("input", id === "mode" || id === "style" ? syncMode : render);
  el.addEventListener("change", id === "mode" || id === "style" ? syncMode : render);
});

$("collList").addEventListener("change", render);

// 采集器工具栏的三个按钮。用 data-act 而不是 .coll-toolbar .chip 选择：
// 输出区的标签栏复用了同一个 .coll-toolbar 类，按类选会把它们一起绑上，
// 点「prometheus.yml」标签就会顺带清空采集器勾选。
document.querySelectorAll(".chip[data-act]").forEach((chip) => {
  chip.addEventListener("click", () => {
    const act = chip.dataset.act;

    boxes().forEach((b) => {
      if (act === "all") {
        b.checked = true;
      } else if (act === "none") {
        b.checked = false;
      } else if (act === "default") {
        b.checked = b.dataset.default === "true";
      }
    });

    render();
  });
});

$("outTabs").querySelectorAll(".chip").forEach((btn) => {
  btn.addEventListener("click", () => {
    tab = btn.dataset.tab;

    $("outTabs")
      .querySelectorAll(".chip")
      .forEach((b) => b.classList.toggle("on", b === btn));

    render();
  });
});

$("copyBtn").addEventListener("click", async () => {
  const btn = $("copyBtn");
  const restore = btn.textContent;

  try {
    await navigator.clipboard.writeText(build());
    btn.textContent = i18n.__("config.copied");
  } catch {
    // 非 HTTPS 下 clipboard API 不可用，退回到让用户自己复制。
    window.prompt("Copy this text", build());

    return;
  }

  setTimeout(() => {
    btn.textContent = restore;
  }, 1400);
});

$("dlBtn").addEventListener("click", () => {
  // 用 Blob 加一个临时 <a> 触发下载，而不是 data: URL：长内容在 data: URL
  // 里会撞上浏览器的 URL 长度限制，几十台 vCenter 的目标文件很容易超。
  const blob = new Blob([build()], { type: "text/plain;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");

  a.href = url;
  a.download = fileName();
  a.click();

  URL.revokeObjectURL(url);
});

// 语言切换时重跑 syncMode。
//
// 为什么不只调 render()：modeHelp / styleHelp / exporterField 的说明文字与
// collHelp 都是 syncMode 写进 DOM 的，render() 不碰它们。只调 render 的话
// 切语言后这四处会停在旧语言上，而它们恰好是页面上最长的几段说明。
//
// i18n.translatePage() 已经处理了带 data-i18n 的静态元素，这里补的是纯 JS
// 驱动的那部分。
document.addEventListener("langchange", () => {
  // 下拉框的 <option> 带 data-i18n，translatePage 已经改过文本，
  // 但 selectedIndex 不受影响，所以不需要在这里恢复选中项。
  syncMode();
});

syncMode();
