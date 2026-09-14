// 无弹窗复制 helper，供配置生成页（config.js）与调试页（app.js）共用。
//
// 设计约束（来自实际部署反馈）：
//
//  1. 绝不弹窗。旧实现在 navigator.clipboard 不可用时退回到 window.prompt，
//     让用户再从一个浏览器原生输入框里手动选中复制 —— 既多一步，那个框还会
//     折行/滚动，长 YAML 手工复制极易漏掉首尾或缩进。这里在任何情况下都不
//     调 prompt/alert，成功与否只通过按钮自身的文案与颜色反馈。
//
//  2. 原格式逐字节复制。入参 text 就是生成器拼出的源文本（.yml 是 YAML、
//     .sh 是 shell、探针 URL 是纯文本）。两种复制路径写入剪贴板的都是
//     text/plain 原文，不做 HTML 转义、不改缩进、不吞末尾换行，贴回同名
//     文件即可直接使用。
//
//  3. HTTP（非安全上下文）也要能用。navigator.clipboard 只在 secure context
//     （HTTPS 或 localhost）下存在；exporter 常被直接用 http://<IP>:9169
//     访问，此时走隐藏 textarea + document.execCommand("copy") 兜底。它在
//     用户点击手势内同步执行，保留多行文本与缩进，且不需要任何权限授权。
//
// 暴露为全局 vmeCopy 而不是 ES module：三个页面都用裸 <script> 加载、没有
// 构建链，引入 type=module 会连带改变 i18n.js/config.js 的加载方式，没必要。
"use strict";

(function () {
  const FEEDBACK_MS = 1400;

  // legacyCopy 用隐藏 textarea + execCommand 复制，是 navigator.clipboard
  // 缺失（HTTP）时的兜底。返回是否成功。
  //
  // 关键点都为"逐字节保留原文"服务：readonly 避免在 iOS 上弹起软键盘；
  // 元素定位到视口外而不是 display:none —— 后者在部分浏览器里 select()
  // 选不中任何内容；setSelectionRange 覆盖全文，iOS Safari 只认这个。
  function legacyCopy(text) {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.top = "0";
    ta.style.left = "0";
    ta.style.width = "1px";
    ta.style.height = "1px";
    ta.style.padding = "0";
    ta.style.border = "none";
    ta.style.opacity = "0";

    document.body.appendChild(ta);
    ta.focus();
    ta.select();
    ta.setSelectionRange(0, text.length);

    let ok = false;
    try {
      ok = document.execCommand("copy");
    } catch (_e) {
      ok = false;
    }

    document.body.removeChild(ta);
    return ok;
  }

  // feedback 在按钮上就地显示成功/失败，FEEDBACK_MS 后恢复原文案与样式。
  //
  // 不弹任何对话框：复制是成功还是失败，信息密度只需要一个变色的词。失败时
  // 按钮变红提示"手动选择文本"，但下方 <pre> 里的内容本来就在页面上可直接
  // 选中，这比一个模态框诚实。
  function feedback(btn, ok, labels) {
    const original = btn.textContent;

    btn.classList.remove("copied", "copy-failed");
    // 强制重排以重启动画类，避免连续点击时第二次的视觉反馈不出现。
    void btn.offsetWidth;
    btn.classList.add(ok ? "copied" : "copy-failed");
    btn.textContent = ok ? labels.ok : labels.fail;

    setTimeout(() => {
      btn.classList.remove("copied", "copy-failed");
      btn.textContent = original;
    }, FEEDBACK_MS);
  }

  // vmeCopy 把 text 原样写入剪贴板，并在 btn 上就地反馈。
  //
  // labels = { ok, fail }。返回 Promise<boolean>，调用方一般无需使用。
  // secure context 下走异步 Clipboard API；它不存在（HTTP）时在同一个用户
  // 手势栈里同步走 execCommand 兜底，不等待一个注定 reject 的 promise。
  window.vmeCopy = function (btn, text, labels) {
    labels = labels || {};
    labels.ok = labels.ok || "Copied";
    labels.fail = labels.fail || "Copy failed";

    if (window.isSecureContext && navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard
        .writeText(text)
        .then(() => {
          feedback(btn, true, labels);
          return true;
        })
        .catch(() => {
          // API 存在但被拒（权限策略/失焦等），仍尝试 legacy 兜底而不是弹窗。
          const ok = legacyCopy(text);
          feedback(btn, ok, labels);
          return ok;
        });
    }

    const ok = legacyCopy(text);
    feedback(btn, ok, labels);
    return Promise.resolve(ok);
  };
})();
