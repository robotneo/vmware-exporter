package main

import (
	"net"
	"strings"

	"github.com/prezhdarov/vmware-exporter/internal/config"
)

// targetAllowed 判断 /probe 请求的 target 是否落在 -probe.allowed-targets
// 白名单内。白名单为空时放开一切（保持默认兼容）。
//
// 这个闸是 SSRF 的深度防御：/probe 默认无鉴权且接受任意 target，任何能访问
// 端口的人都能让 exporter 向任意地址发起 HTTPS 连接。能配 web.config 做认证
// 或网络层隔离时优先用那些；这个白名单给"必须在应用层限制可探测目标"的
// 部署一个选项。
//
// 走 Snapshot 读 flag 而不是裸解引用：该 flag 是 SIGHUP 可热改的配置，
// 请求路径上读它必须与其它 flag 一样受 RWMutex 保护，否则就是数据竞争。
func targetAllowed(target string) bool {
	var rules string

	config.Snapshot(func() {
		rules = *probeAllowedTargets
	})

	return targetAllowedByRules(splitHost(target), splitRules(rules))
}

// splitRules 把逗号分隔的配置拆成去空白、去空项、小写化后的规则切片。
func splitRules(raw string) []string {
	var out []string

	for _, part := range strings.Split(raw, ",") {
		rule := strings.ToLower(strings.TrimSpace(part))
		if rule != "" {
			out = append(out, rule)
		}
	}

	return out
}

// splitHost 从 "host:port" / "[::1]:443" 形态的 target 中取出 host 部分；
// 没有端口时原样返回（小写化）。target 直接拼进 soap URL，正常形态都带端口，
// 但裸主机名（如 vcsim 测试里的 "127.0.0.1"）也要能匹配，所以两种都收。
func splitHost(target string) string {
	host := strings.TrimSpace(target)

	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	return strings.ToLower(host)
}

// targetAllowedByRules 是白名单判定的纯内核，便于不依赖 flag 与 SIGHUP
// 直接做表驱动测试。rules 为空表示不限制。
//
// 三种规则：
//   - ".example.com"（以点开头）：后缀匹配，命中 example.com 自身与任意子域；
//   - "10.0.0.0/8"（含斜杠）：CIDR 网段，仅对 IP 型 host 生效；
//   - 其余：精确相等。
func targetAllowedByRules(host string, rules []string) bool {
	if len(rules) == 0 {
		return true
	}

	host = strings.ToLower(strings.TrimSpace(host))
	hostIP := net.ParseIP(host)

	for _, rule := range rules {
		switch {
		case strings.HasPrefix(rule, "."):
			// ".example.com" 同时匹配 "example.com" 与 "vc.example.com"。
			base := rule[1:]
			if host == base || strings.HasSuffix(host, rule) {
				return true
			}

		case strings.Contains(rule, "/"):
			if _, network, err := net.ParseCIDR(rule); err == nil && hostIP != nil {
				if network.Contains(hostIP) {
					return true
				}
			}

		default:
			if host == rule {
				return true
			}
		}
	}

	return false
}
