// 调试台的前端逻辑。
//
// 没有框架也没有构建步骤是刻意的：exporter 常部署在没有外网出口的管理网段，
// 任何 CDN 引用在那里都是一个加载不出来的空白页面，而把 React 打包进来又要
// 求 CI 里多一条前端构建链。这个文件只依赖浏览器自身。
//
// 采集器清单不在这里维护：复选框由 debug.html 从 Go 侧的注册表渲染，这段
// 脚本只读 DOM。新增采集器时这个文件不需要改。

"use strict";

const $ = (id) => document.getElementById(id);

// lastBody 保存最近一次响应的完整文本，供过滤框反复取用。
let lastBody = "";

const list = $("collList");
const boxes = () => Array.from(list.querySelectorAll("input[type=checkbox]"));
const selected = () => boxes().filter((b) => b.checked).map((b) => b.value);

function updateCount() {
  $("selCount").textContent = selected().length;
}

list.addEventListener("change", updateCount);

document.querySelectorAll(".chip").forEach((chip) => {
  chip.addEventListener("click", () => {
    const act = chip.dataset.act;

    boxes().forEach((b) => {
      if (act === "all") {
        b.checked = true;
      } else if (act === "none") {
        b.checked = false;
      } else if (act === "default") {
        // data-default 由模板写入，值是 Go 的 true/false 字面量。
        b.checked = b.dataset.default === "true";
      }
    });

    updateCount();
  });
});

updateCount();

// buildParams 组装 /probe 的参数。
//
// 全选时发 collect[]=all 而不是逐个列出：两者对 exporter 等价，但 all 是
// 文档与 Prometheus 配置里通用的写法，生成出来的 URL 更接近运维实际会写的。
//
// 一个都不选时不发任何 collect[] 参数，让服务端回退到各采集器的默认状态 ——
// 这与 parseCollectors 的语义一致：显式给出 collect[] 意味着「只跑这些」。
function buildParams(passwordOverride) {
  const params = new URLSearchParams();

  params.set("target", $("target").value.trim());
  params.set("username", $("username").value.trim());
  params.set(
    "password",
    passwordOverride === undefined ? $("password").value : passwordOverride,
  );
  params.set("schema", $("schema").value);

  if ($("insecure").checked) {
    params.set("insecure", "true");
  }

  const sel = selected();

  if (sel.length === boxes().length) {
    params.set("collect[]", "all");
  } else {
    sel.forEach((name) => params.append("collect[]", name));
  }

  return params;
}

function setStat(id, value, cls) {
  const el = $(id);
  el.textContent = value;
  el.className = cls ? "v " + cls : "v";
}

async function run() {
  const target = $("target").value.trim();

  if (target === "") {
    $("target").focus();
    return;
  }

  const btn = $("run");

  btn.disabled = true;
  btn.classList.add("busy");
  $("runTxt").textContent = "Running";
  $("empty").hidden = true;

  const started = performance.now();
  let status = 0;
  let body = "";
  let failed = "";

  try {
    // Accept 必须显式声明。promhttp 会做内容协商：Accept 里出现 protobuf
    // 时它返回二进制，页面上就是一堆乱码。浏览器默认的 Accept 目前协商到
    // 文本，但那不是可以依赖的行为。
    const resp = await fetch("/probe", {
      method: "POST",
      headers: {
        "Accept": "text/plain",
        // 显式声明表单编码。缺了这个头，服务端的 ParseForm 会静默忽略
        // 整个请求体 —— 实测行为，不是推测。
        "Content-Type": "application/x-www-form-urlencoded",
      },
      body: buildParams().toString(),
    });

    status = resp.status;
    body = await resp.text();
  } catch (err) {
    failed = err instanceof Error ? err.message : String(err);
  }

  const elapsed = Math.round(performance.now() - started);

  btn.disabled = false;
  btn.classList.remove("busy");
  $("runTxt").textContent = "Run scrape";
  $("stats").hidden = false;
  $("result").hidden = false;

  if (failed !== "") {
    setStat("sCode", "\u2014", "err");
    setStat("sTime", elapsed + " ms");
    setStat("sSeries", "0");
    setStat("sUp", "\u2014");
    $("resHint").textContent = "Request failed";
    $("resNote").textContent = "The request never completed: " + failed;
    lastBody = "";
    $("out").textContent = "";
    return;
  }

  const lines = body.split("\n");
  const series = lines.filter((l) => l !== "" && !l.startsWith("#"));

  setStat("sCode", String(status), status === 200 ? "ok" : "err");
  setStat("sTime", elapsed + " ms");
  setStat("sSeries", String(series.length));

  // vmware_up 是判断「凭证对不对、目标通不通」的那一条。登录失败时
  // /probe 仍然返回 200 加上 vmware_up 0，而不是一个 HTTP 错误，所以
  // 光看状态码是不够的。
  const up = body.match(/^vmware_up(?:\{[^}]*\})?\s+(\S+)/m);
  setStat("sUp", up ? up[1] : "\u2014", up && up[1] === "1" ? "ok" : up ? "err" : "");

  if (status === 200) {
    $("resHint").textContent = series.length + " time series";
    $("resNote").textContent =
      series.length + " series across " + lines.length + " lines";
  } else {
    $("resHint").textContent = "HTTP " + status;
    $("resNote").textContent =
      "The exporter rejected the request; its response is shown below.";
  }

  lastBody = body;
  $("out").textContent = body;
  $("filter").value = "";
  $("filterCount").textContent = "";
}

// copyProbeURL 生成一条可以直接贴进 Prometheus 配置的 GET URL。
//
// 密码默认替换成占位符。带明文密码的 URL 一旦生成就会被贴进工单、聊天和
// wiki，而这个按钮的用途是「把刚调通的参数组合抄下来」，不是分发凭证。
async function copyProbeURL() {
  const url =
    location.origin + "/probe?" + buildParams("<password>").toString();

  const btn = $("genUrl");
  const restore = btn.textContent;

  try {
    await navigator.clipboard.writeText(url);
    btn.textContent = "Copied";
  } catch {
    // 非 HTTPS 下 clipboard API 不可用，退回到让用户自己复制。
    window.prompt("Copy this URL", url);
    return;
  }

  setTimeout(() => {
    btn.textContent = restore;
  }, 1400);
}

// filterLines 在本地过滤已经拿到的输出，不重发请求。
//
// 过滤必须以 lastBody 为源，不能读 <pre> 的当前内容：这个函数自己就在改写
// 那段内容，拿它当输入的话每次按键都会在上一次的过滤结果上再过滤一遍，
// 删掉一个字符也回不去。
function filterLines() {
  const q = $("filter").value.trim().toLowerCase();

  if (q === "") {
    $("out").textContent = lastBody;
    $("filterCount").textContent = "";
    return;
  }

  const lines = lastBody.split("\n");
  const kept = lines.filter((l) => l.toLowerCase().includes(q));

  $("out").textContent = kept.join("\n");
  $("filterCount").textContent = kept.length + " / " + lines.length;
}

$("run").addEventListener("click", run);
$("genUrl").addEventListener("click", copyProbeURL);
$("filter").addEventListener("input", filterLines);
