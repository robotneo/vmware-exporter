package main

import (
	"net"
	"strings"

	"github.com/prezhdarov/vmware-exporter/internal/config"
	"github.com/prezhdarov/vmware-exporter/internal/target"
)

// checkTarget 解析并校验 /probe 的 target，再按白名单判定，一步到位。
// 返回规范化后的 Endpoint 与是否放行；畸形 target（userinfo/path/query/
// 非法端口等）与未命中白名单都返回 false。
func checkTarget(raw string) (target.Endpoint, bool) {
	ep, err := target.Parse(raw)
	if err != nil {
		return target.Endpoint{}, false
	}

	if !targetAllowlisted(ep.Host) {
		return target.Endpoint{}, false
	}

	return ep, true
}

// targetAllowlisted 按 -probe.allowed-targets 判定已规范化的 host。
//
// 解析与白名单必须在同一个规范化结果上完成。此前 splitHost 用
// net.SplitHostPort 取 host，而连接侧用 url/soap 解析 authority；对
// "allowed.example.com:443@169.254.169.254" 这类输入两边结论不同，白名单
// 匹配 @ 前的主机、实际连接 @ 后的主机。调用方必须先用 internal/target.Parse
// 拒绝 userinfo/path/query/fragment/非法端口，再把 Endpoint.Host 传进来。
//
// 走 Snapshot 读 flag 而不是裸解引用：该 flag 是 SIGHUP 可热改的配置，
// 请求路径上读它必须与其它 flag 一样受 RWMutex 保护，否则就是数据竞争。
func targetAllowlisted(host string) bool {
	var rules string

	config.Snapshot(func() {
		rules = *probeAllowedTargets
	})

	return targetAllowedByRules(host, splitRules(rules))
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
