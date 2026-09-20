// Package safedial 在真正建立 TCP 连接的那一刻复核对端 IP，堵住 SSRF 纵深里
// 最后一个残留面：DNS rebinding（校验白名单时主机名解析到一个地址、真正拨号
// 时却解析到另一个地址）。
//
// 为什么必须在拨号器而不是预先 net.LookupIP 里判定：任何"先解析校验、再用主机
// 名发起请求"的两步法之间都有 TOCTOU 窗口，攻击者的权威 DNS 可以对两次查询
// 返回不同结果。net.Dialer.Control 在 Go 完成解析、选定一个具体地址、调用
// connect(2) 之前回调，address 就是本次实际要拨的 IP —— 校验与连接共用同一
// 次解析结果，窗口不存在。
//
// 默认阻断范围刻意收窄，避免误伤：vCenter/ESXi 几乎总是部署在 RFC1918 内网，
// 所以默认只拦"管理端点绝不可能出现、且是 SSRF 经典跳板"的地址 —— 链路本地
// （含云元数据端点 169.254.169.254）与未指定地址（0.0.0.0/::）。需要更强
// 收敛的部署可用 Strict 策略一并拦环回与私网。
package safedial

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// ErrBlockedAddress 表示一次连接因对端 IP 落入策略禁止区间而被拒绝。它是
// sentinel，调用方/测试可用 errors.Is 精确匹配，而不必解析错误文本。
var ErrBlockedAddress = errors.New("safedial: dial target resolves to a blocked IP address")

// Policy 决定哪些地址区间被禁止。
type Policy int

const (
	// PolicyDefault（零值）只阻断链路本地与未指定地址。这两类地址上不可能
	// 跑着一个需要被监控的 vCenter，却分别是云元数据 SSRF（169.254.169.254）
	// 与"绑定本机任意服务"的经典落点，默认拦掉零误伤。
	PolicyDefault Policy = iota

	// PolicyStrict 在默认基础上再阻断环回与私网（RFC1918、IPv6 唯一本地地址）。
	// 适用于 exporter 与 vCenter 之间明确走可路由地址、或要求强隔离的部署；
	// 不适用于 vCenter 在内网（最常见形态）或经 127.0.0.1 sidecar 代理的场景。
	PolicyStrict
)

// GuardIP 判定一个已解析的 IP 是否允许连接。ip 为 nil（无法解析为具体地址）
// 时拒绝 —— 放行一个我们看不懂的目标违背纵深防御的初衷。
func GuardIP(ip net.IP, policy Policy) error {
	if ip == nil {
		return fmt.Errorf("%w: <nil>", ErrBlockedAddress)
	}

	switch {
	// 未指定地址：0.0.0.0 / ::。连接它在多数系统上等价于连本机，且没有正常
	// 监控目标会用它，任何策略下都拦。
	case ip.IsUnspecified():
		return blocked(ip, "unspecified address")

	// 链路本地：IPv4 169.254.0.0/16（含 169.254.169.254 云元数据）、
	// IPv6 fe80::/10。任何策略下都拦。
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return blocked(ip, "link-local address")
	}

	if policy == PolicyStrict {
		switch {
		case ip.IsLoopback():
			return blocked(ip, "loopback address")
		// RFC1918：10/8、172.16/12、192.168/16；IPv6 ULA：fc00::/7。
		case isPrivate(ip):
			return blocked(ip, "private address")
		}
	}

	return nil
}

// isPrivate 复刻 Go 1.17+ net.IP.IsPrivate 的判定（RFC1918 + fc00::/7），
// 单独写一份而不是直接依赖方法，是为了让"Strict 到底拦了哪些段"在本包内
// 显式可读、可被测试钉住。
func isPrivate(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 10,
			v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31,
			v4[0] == 192 && v4[1] == 168:
			return true
		}
		return false
	}
	// IPv6 唯一本地地址 fc00::/7（fc 与 fd 开头）。
	return len(ip) == net.IPv6len && ip[0]&0xfe == 0xfc
}

func blocked(ip net.IP, kind string) error {
	return fmt.Errorf("%w: %s is a %s", ErrBlockedAddress, ip, kind)
}

// Control 返回一个 net.Dialer.Control 回调。network/address 是 Go 拨号器在
// 解析主机名后、connect 前给出的实际 socket 目标，address 形如 "ip:port"
// （或 "[ip]:port"）。这里解析出的 IP 与即将连接的 IP 是同一个，杜绝 rebinding。
func Control(policy Policy) func(network, address string, _ syscall.RawConn) error {
	return func(_ /*network*/, address string, _ /*c*/ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			// 有些拨号路径给的 address 可能不带端口；容错再试整体解析。
			host = address
		}
		ip := net.ParseIP(host)
		if ip == nil {
			// 走到 Control 时 address 理应已是字面 IP；若仍是主机名说明底层
			// 拨号器行为变了，拒绝而不是放行。
			return fmt.Errorf("%w: non-literal dial address %q", ErrBlockedAddress, address)
		}
		return GuardIP(ip, policy)
	}
}

// dialTimeout 与 net/http.DefaultTransport 的拨号超时对齐；抓取本身还有
// -vmware.timeout 的 ctx 兜底，这里只给 TCP 建连一个独立上限。
const dialTimeout = 30 * time.Second

// ConfigureDialer 把策略安装到一个 net.Dialer 的 Control 上并返回它，供调用方
// 挂到 http.Transport.DialContext。d 为 nil 时使用带默认超时/keepalive 的拨号器。
func ConfigureDialer(d *net.Dialer, policy Policy) *net.Dialer {
	if d == nil {
		d = &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	}
	d.Control = Control(policy)
	return d
}

// GuardTransport 把策略装进一个 *http.Transport，返回同一个 transport 以便链式
// 调用。它同时覆盖明文（DialContext）与 TLS（DialTLSContext）两条建连路径 ——
// 这一点是本函数存在的理由：
//
// govmomi 的 soap.NewClient 会把 transport.DialTLSContext 设成它自己的
// dialTLSContext，而那个函数内部用 tls.Dial 直接建连，完全不走
// transport.DialContext。vCenter/ESXi 实际几乎全是 https，只覆盖 DialContext
// 的话，对真实目标的防护一次都不会触发。因此这里用「先经受控 dialer 建 TCP、
// 再在这条已校验的连接上手工握 TLS」替换它，两条路径最终都经过同一个
// net.Dialer.Control，IP 复核只有一处真相。
//
// 副作用上的取舍（本 exporter 均可接受）：自定义 DialTLSContext 会关闭 net/http
// 的 HTTP/2 自动协商（govmomi 默认 ForceAttemptHTTP2=false，本就不用 h2）；
// govmomi 基于证书 thumbprint 的回退校验被绕过 —— 本项目只使用 Insecure 布尔，
// 从不设置 thumbprint。
//
// 走 HTTP 代理（HTTPS_PROXY）时，拨号器拨到的是代理地址而非目标地址，Control
// 校验的自然也是代理 IP；目标主机名由代理代连，这是代理模式的固有语义。
func GuardTransport(t *http.Transport, policy Policy) *http.Transport {
	dialer := ConfigureDialer(&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}, policy)

	t.DialContext = dialer.DialContext
	t.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// 先建 TCP —— 这一步内部触发 Control，按「本次实际要拨的 IP」复核，
		// DNS rebinding 无窗口可钻。
		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		// 在这条已校验的连接上握 TLS。复刻 tls.Dial 的最小必要行为：沿用
		// transport 的 TLSClientConfig（含 InsecureSkipVerify），ServerName
		// 缺省取目标主机名。clone 一份避免改动共享配置。
		cfg := t.TLSClientConfig
		if cfg == nil {
			cfg = &tls.Config{}
		} else {
			cfg = cfg.Clone()
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		if cfg.ServerName == "" {
			cfg.ServerName = host
		}

		tlsConn := tls.Client(conn, cfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return tlsConn, nil
	}
	return t
}

// PolicyForFlag 把对外暴露的布尔开关映射为策略：denyPrivate=true 时在默认拦截
// （链路本地/未指定）之上再拦环回与私网。集中映射以便调用点与测试共用同一语义。
func PolicyForFlag(denyPrivate bool) Policy {
	if denyPrivate {
		return PolicyStrict
	}
	return PolicyDefault
}
