package main

import (
	"net"
	"net/http"
	"strings"
)

// 安全响应头（S-08）。exporter 自身不带认证，/debug、/config 又是带凭证输入框
// 的交互页，这两个头对"被浏览器直接打开"的场景有实际意义：
//
//   - X-Content-Type-Options: nosniff —— 阻止浏览器把响应当成可执行脚本/HTML
//     嗅探（/metrics、错误体都是纯文本，不应被解释成别的）。
//   - Referrer-Policy: no-referrer —— 从调试页点出站外链接时不带上
//     host:port（调试页可能指向 vCenter/文档，Referer 会泄露内网拓扑）。
//
// 不加 CSP/HSTS：本服务没有用户内容与登录态，且 HSTS 只在 HTTPS（web.config.file）
// 下才有意义、明文下反而是噪音。
const (
	headerContentTypeOptions = "X-Content-Type-Options"
	headerReferrerPolicy     = "Referrer-Policy"
)

// securityHeaders 给所有响应兜底设置安全头。在调用下游之前 Set，使 /metrics、
// /probe、UI 等完全不碰响应头的 handler 也都带上。本项目没有任何业务 handler
// 设置这两个头；Go 的 Header.Set 是后写者胜，所以这里是"默认值"而非不可覆盖
// 的强制值——若将来确有 handler 需要自定义，它仍可覆盖，不引入 ResponseWriter
// 包装的复杂度。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set(headerContentTypeOptions, "nosniff")
		h.Set(headerReferrerPolicy, "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// listenExposedWithoutAuth 判断"监听在非回环接口、且没有 web.config.file 提供
// TLS/Basic Auth"这种危险组合（S-08）。满足时启动日志要显著告警：此时
// /probe 接受的 vCenter 凭证在网络上明文传输，任何能连到该端口的人也能驱动
// exporter 去抓取 vCenter。
//
// listenAddress 是 -http.address 的原始值（host:port）。webConfig 非空表示
// 管理员已通过 exporter-toolkit 配置 TLS/认证，这种情况不告警。
func listenExposedWithoutAuth(listenAddress, webConfig string) bool {
	if strings.TrimSpace(webConfig) != "" {
		return false
	}

	host, _, err := net.SplitHostPort(listenAddress)
	if err != nil {
		// 不是 host:port（例如裸文件名形式的 unix socket）。本 exporter 固定
		// 走 TCP，走到这里多半是非常规配置，不替用户做"安全"的假设——但也
		// 无法据此断定它暴露在网络上，保守不告警。
		return false
	}

	return !isLoopbackHost(host)
}

// isLoopbackHost 判定监听 host 是否只在本机可达。
//
//   - 空 host（":9169"）表示绑定所有接口（INADDR_ANY），不是回环。
//   - "localhost" 按名字识别，避免依赖解析顺序。
//   - 其余按 IP 解析：127.0.0.0/8 与 ::1 视为回环；解析不了的主机名（可能是
//     某个对外网卡的名字）保守当作非回环，宁可多一次告警也不漏报。
func isLoopbackHost(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)
	if ip == nil {
		// 未知主机名：可能解析到回环，也可能解析到对外地址。无法证明安全时
		// 按暴露处理（告警），与"默认安全失败"一致。
		return false
	}
	return ip.IsLoopback()
}
