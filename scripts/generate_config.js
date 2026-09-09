// generate_config.js 在 Node 里跑 web/config.js 的生成函数，把结果打到 stdout。
//
// 存在理由：config.js 是浏览器代码，读的是 DOM。Go 侧的测试要断言「生成的
// YAML 能被 promtool 接受」，就必须真的执行那段生成逻辑 —— 在 Go 里重写一遍
// 拼接规则只会得到两份可以各自出错的实现，测试通过也证明不了页面是对的。
//
// 实现方式是给 config.js 造一个够用的 DOM 假体，然后 require 它。不引 jsdom：
// 那是一个上百 MB 的依赖，而这里需要的只是 getElementById 与几个事件方法。
//
// 用法：node generate_config.js <which> <settings.json>
//   which    sd | job | flags
//   settings 一个 JSON 对象，键是控件 id，值是要填进去的值
//
// 这个文件只在测试里用，不随二进制发布。

"use strict";

const fs = require("fs");
const path = require("path");
const vm = require("vm");

const which = process.argv[2];
const settings = JSON.parse(fs.readFileSync(process.argv[3], "utf8"));

// 采集器清单在真实页面里由 Go 模板渲染。这里从 settings 里取，让 Go 侧的
// 测试可以传入 vmwareCollectors.Definitions() 的真实内容 —— 硬编码一份会在
// 新增采集器时静默过期。
const collectors = settings.__collectors || [];

// 一个够用的元素假体。value / checked / textContent / hidden 是 config.js
// 实际会读写的全部属性。
function makeEl(id, value, checked) {
  return {
    id: id,
    value: value === undefined ? "" : value,
    checked: checked === true,
    textContent: "",
    innerHTML: "",
    hidden: false,
    dataset: {},
    classList: { toggle() {}, add() {}, remove() {} },
    addEventListener() {},
    querySelector() {
      return makeEl("stub");
    },
    querySelectorAll() {
      return [];
    },
  };
}

const els = {};

// 页面上的全部控件。默认值与 config.html 里的 value 属性保持一致 ——
// 不一致的话这个驱动测的就不是页面的默认行为。
const defaults = {
  mode: "probe",
  style: "relabel",
  job: "vmware",
  interval: "60s",
  timeout: "55s",
  exporter: "localhost:9169",
  sdPath: "/etc/prometheus/targets/vmware.yml",
  schema: "https",
  targets: "",
  username: "",
};

Object.keys(defaults).forEach((id) => {
  els[id] = makeEl(id, defaults[id]);
});

// 两个开关。insecure 在 config.html 里默认勾选，redact 也是。
els.insecure = makeEl("insecure", "", true);
els.redact = makeEl("redact", "", true);

// 只读的展示位。config.js 会往这些里写文字，测试不关心内容，但不能是
// undefined，否则赋值会抛。
//
// tHint / collHint 是 i18n 化之后新增的：计数提示改成整条由翻译渲染
// （中英文语序不同，"共 3 台 vCenter" 对 "3 vCenters"，数字塞不进固定
// span），原来的 tCount / selCount 两个内层 span 已不再被 config.js 写入。
[
  "tHint",
  "collHint",
  "outHint",
  "outPath",
  "out",
  "modeHelp",
  "styleHelp",
  "collHelp",
  "credNote",
  "timeoutWarn",
  "styleField",
  "exporterField",
  "targetsCard",
  "outTabs",
  "copyBtn",
  "dlBtn",
].forEach((id) => {
  els[id] = makeEl(id);
});

// 采集器复选框列表。data-default 在真实页面里是 Go 模板写的字符串字面量，
// 这里保持同样的类型（字符串 "true"/"false"），因为 config.js 是拿它跟
// "true" 比较的。
const collBoxes = collectors.map((c) => {
  const box = makeEl("coll-" + c.name, c.name, c.checked);

  box.dataset.default = c.defaultEnabled ? "true" : "false";

  return box;
});

els.collList = makeEl("collList");
els.collList.querySelectorAll = function () {
  return collBoxes;
};

// 应用测试传入的设置。
Object.keys(settings).forEach((id) => {
  if (id.startsWith("__")) {
    return;
  }

  if (els[id] === undefined) {
    throw new Error("unknown control id in settings: " + id);
  }

  const v = settings[id];

  if (typeof v === "boolean") {
    els[id].checked = v;
  } else {
    els[id].value = v;
  }
});

// langchange 事件的监听器登记表。config.js 会注册一个回调用于切换语言时
// 重跑 syncMode；CLI 模式下没人切语言，所以只要「能注册、不报错」即可。
const listeners = {};

const document = {
  // i18n.js 会读写 documentElement.lang。
  documentElement: { lang: "" },
  getElementById(id) {
    // 返回 undefined 会让 config.js 在读属性时抛一个难懂的 TypeError。
    // 显式报错，把「驱动里少造了一个控件」与「生成逻辑有 bug」区分开。
    if (els[id] === undefined) {
      throw new Error("test driver is missing element: " + id);
    }

    return els[id];
  },
  querySelectorAll() {
    return [];
  },
  createElement() {
    return makeEl("created");
  },
  addEventListener(name, fn) {
    (listeners[name] = listeners[name] || []).push(fn);
  },
  dispatchEvent(ev) {
    (listeners[ev.type] || []).forEach((fn) => fn(ev));
  },
};

const sandbox = {
  document: document,
  window: { prompt() {} },
  navigator: { clipboard: { writeText() {} } },
  Blob: function () {},
  URL: { createObjectURL() {}, revokeObjectURL() {} },
  setTimeout: setTimeout,
  Number: Number,
  console: console,
  module: { exports: {} },
  // i18n.js 的依赖。语言不落盘 —— CLI 每次都从默认（中文）起步，
  // 这样生成结果是确定的，不受任何本地状态影响。
  localStorage: {
    getItem() {
      return null;
    },
    setItem() {},
  },
  CustomEvent: function (type, opts) {
    this.type = type;
    this.detail = opts && opts.detail;
  },
};


const src = fs.readFileSync(
  path.join(__dirname, "..", "web", "config.js"),
  "utf8",
);

// config.js 里的说明文字走 i18n.__()，所以必须先把真实的 i18n.js 跑起来
// —— 而不是塞一个「原样返回 key」的假实现。用真货有两个好处：
//   1. 生成出来的注释/提示跟浏览器里看到的完全一致；
//   2. 万一哪个 key 拼错了，这里会直接暴露成 key 字面量，测试能抓到，
//      不会等到用户在页面上看见一串 config.xxx_help 才发现。
const i18nSrc = fs.readFileSync(
  path.join(__dirname, "..", "web", "i18n.js"),
  "utf8",
);

vm.createContext(sandbox);
vm.runInContext(i18nSrc, sandbox, { filename: "i18n.js" });

// 把生成函数暴露出来。config.js 自身没有导出语句 —— 它是给浏览器直接
// <script> 引的，加 export 会让浏览器报错。
const exposed =
  src +
  "\nmodule.exports = { buildTargetFile, buildScrapeConfig, buildFlags };\n";

vm.runInContext(exposed, sandbox, { filename: "config.js" });

const api = sandbox.module.exports;

const out = {
  sd: api.buildTargetFile,
  job: api.buildScrapeConfig,
  flags: api.buildFlags,
}[which];

if (out === undefined) {
  throw new Error("unknown output: " + which);
}

process.stdout.write(out());
