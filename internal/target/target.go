// Package target 是 /probe target 的规范化与安全校验层。
//
// 它存在的唯一理由是消除「白名单按一种方式解析 target、SOAP 客户端按另一种
// 方式解析 target」的双解析问题。例如：
//
//	allowed.example.com:443@169.254.169.254
//
// net.SplitHostPort 会把 @ 前的 allowed.example.com 当 host，而 URL 解析会把
// @ 后地址当真正连接主机。只要校验和连接不是同一个事实来源，白名单就可能被
// userinfo 注入绕过。因此 HTTP 层与 vmware/api 连接层都必须调用这里。
package target

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Endpoint 表示一个通过安全校验的 target。
//
// Host 是不带端口、小写化后的主机名或 IP，用于白名单匹配；Authority 是
// 规范化后的 host 或 host:port（IPv6 保留方括号），供连接层使用，也是错误
// 计数等进程级 map 的稳定分桶键。二者都不包含 userinfo、path、query、fragment。
type Endpoint struct {
	Host      string
	Authority string
}

// Parse 把 /probe 接受的 target 解析成受限的 host/host:port 形式。
//
// target 是用户输入，刻意不接受完整 URL：/probe 的参数契约一直是 host 或
// host:port。允许 URL 形态会引入路径、查询串、fragment、userinfo 等多个混淆
// 面。这里统一拒绝这些形态，而不是尝试猜测调用方意图。
func Parse(raw string) (Endpoint, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return Endpoint{}, fmt.Errorf("target is required")
	}

	// 给裸 authority 补一个 scheme，使 net/url 按 URL authority 规则解析。
	// schema 用什么无关安全结论：无论 http 还是 https，userinfo/path/query
	// 等混淆形态都必须先被拒绝。
	u, err := url.Parse("https://" + value)
	if err != nil {
		return Endpoint{}, fmt.Errorf("invalid target %q: %w", raw, err)
	}

	// 最关键的防护：target 里的任何 @ 都会被 URL 语法解释为 userinfo。
	// 白名单可能匹配 @ 前的主机，而客户端实际连接 @ 后的主机。
	if u.User != nil {
		return Endpoint{}, fmt.Errorf("target %q must not contain userinfo", raw)
	}

	// /probe target 只允许 authority，不允许携带路径、查询串或 fragment。
	// 这些字符既可能用于绕过字符串规则，也可能污染下游客户端解析。
	if u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" || u.RawFragment != "" {
		return Endpoint{}, fmt.Errorf("target %q must be a host or host:port without path, query or fragment", raw)
	}

	// 主机名/IP 小写化做稳定分桶；Hostname() 同时正确剥离 IPv6 方括号。
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return Endpoint{}, fmt.Errorf("target %q has an empty host", raw)
	}

	// Authority 用解析结果重组而不是回写原文：这样大小写、空白差异不会让
	// 错误计数与 vcenter label 产生多个桶；IPv6 由 JoinHostPort 补回方括号。
	// net/url 在解析阶段已拒绝非数字端口（u.Port 非空即合法）。
	authority := host
	if port := u.Port(); port != "" {
		authority = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}

	return Endpoint{Host: host, Authority: authority}, nil
}
