package safedial

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"
)

func mustParseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("net.ParseIP(%q) returned nil", s)
	}
	return ip
}

func TestGuardIP(t *testing.T) {
	// 每个策略下的期望：true=必须拦截。
	cases := []struct {
		name   string
		ip     string
		nilIP  bool
		def    bool // PolicyDefault 应拦截
		strict bool // PolicyStrict 应拦截
	}{
		{"nil", "", true, true, true},

		// 未指定地址：任何策略都拦。
		{"ipv4 unspecified", "0.0.0.0", false, true, true},
		{"ipv6 unspecified", "::", false, true, true},

		// 链路本地（含云元数据）：任何策略都拦。
		{"cloud metadata", "169.254.169.254", false, true, true},
		{"link-local 169.254/16", "169.254.42.42", false, true, true},
		{"ipv6 link-local", "fe80::1", false, true, true},
		{"link-local multicast", "224.0.0.1", false, true, true},

		// 环回：默认放行（vcsim/sidecar 场景），Strict 拦。
		{"loopback v4", "127.0.0.1", false, false, true},
		{"loopback v6", "::1", false, false, true},

		// RFC1918：默认放行（vCenter 最常见部署位），Strict 拦。
		{"private 10/8", "10.0.0.5", false, false, true},
		{"private 172.16/12 low", "172.16.0.1", false, false, true},
		{"private 172.16/12 high", "172.31.255.255", false, false, true},
		{"private 192.168/16", "192.168.1.1", false, false, true},
		{"ipv6 ULA fd00::/8", "fd00::1", false, false, true},
		{"ipv6 ULA fc00::/8", "fc00::1", false, false, true},

		// 公网与 RFC1918 边界外侧：任何策略都放行。
		{"public 8.8.8.8", "8.8.8.8", false, false, false},
		{"just below 172.16", "172.15.255.255", false, false, false},
		{"just above 172.31", "172.32.0.0", false, false, false},
		{"11/8 is public", "11.0.0.1", false, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ip net.IP
			if !tc.nilIP {
				ip = mustParseIP(t, tc.ip)
			}

			if got := errors.Is(GuardIP(ip, PolicyDefault), ErrBlockedAddress); got != tc.def {
				t.Errorf("GuardIP(%s, Default) blocked=%v, want %v", tc.ip, got, tc.def)
			}
			if got := errors.Is(GuardIP(ip, PolicyStrict), ErrBlockedAddress); got != tc.strict {
				t.Errorf("GuardIP(%s, Strict) blocked=%v, want %v", tc.ip, got, tc.strict)
			}
		})
	}
}

func TestIsPrivateBoundaries(t *testing.T) {
	private := []string{"10.0.0.0", "10.255.255.255", "172.16.0.0", "172.31.255.255",
		"192.168.0.0", "192.168.255.255", "fc00::1", "fdff::1"}
	for _, s := range private {
		if !isPrivate(mustParseIP(t, s)) {
			t.Errorf("isPrivate(%s) = false, want true", s)
		}
	}
	public := []string{"9.255.255.255", "11.0.0.0", "172.15.255.255", "172.32.0.0",
		"192.169.0.0", "8.8.8.8", "2001:4860:4860::8888", "fe80::1"}
	for _, s := range public {
		if isPrivate(mustParseIP(t, s)) {
			t.Errorf("isPrivate(%s) = true, want false", s)
		}
	}
}

func TestControlCallback(t *testing.T) {
	cb := Control(PolicyDefault)
	var _ func(network, address string, c syscall.RawConn) error = cb

	// 实际地址形如 ip:port，命中链路本地即拦截，且错误可被 errors.Is 识别。
	if err := cb("tcp", "169.254.169.254:80", nil); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("Control default: metadata err=%v, want ErrBlockedAddress", err)
	}

	// RFC1918 在默认策略下放行（Control 不真正建连，放行时返回 nil 即可）。
	if err := cb("tcp", "10.0.0.5:443", nil); err != nil {
		t.Errorf("Control default: private addr err=%v, want nil", err)
	}

	// Strict 下私网被拦。
	if err := Control(PolicyStrict)("tcp", "10.0.0.5:443", nil); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("Control strict: private addr err=%v, want ErrBlockedAddress", err)
	}

	// IPv6 字面量带括号。
	if err := cb("tcp", "[fe80::1]:443", nil); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("Control default: ipv6 link-local err=%v, want ErrBlockedAddress", err)
	}

	// 非字面量（主机名）不应在 connect 前一刻出现；出现即拒，不放行。
	if err := cb("tcp", "internal.example.com:443", nil); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("Control: non-literal addr err=%v, want ErrBlockedAddress", err)
	}
}

func TestPolicyForFlag(t *testing.T) {
	if PolicyForFlag(false) != PolicyDefault {
		t.Error("PolicyForFlag(false) != PolicyDefault")
	}
	if PolicyForFlag(true) != PolicyStrict {
		t.Error("PolicyForFlag(true) != PolicyStrict")
	}
}

// 起一个本地 TCP 监听，返回其 host:port，测试结束自动关闭。
func localListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func TestGuardTransportDialContext(t *testing.T) {
	ln := localListener(t)
	tr := GuardTransport(&http.Transport{}, PolicyDefault)

	// 默认策略放行环回：受控 dialer 能真正连上本地监听。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := tr.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("DialContext to loopback under default policy: %v", err)
	}
	_ = conn.Close()

	// 链路本地在 Control 阶段即被拒，不会发出任何报文 —— 必须立即返回
	// sentinel，而不是超时。
	start := time.Now()
	_, err = tr.DialContext(ctx, "tcp", "169.254.169.254:80")
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("DialContext to metadata err=%v, want ErrBlockedAddress", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("blocked dial took %v, Control should reject before connecting", time.Since(start))
	}
}

func TestGuardTransportDialTLSBlocked(t *testing.T) {
	ln := localListener(t)
	// Strict 连环回都拦：自定义 DialTLSContext 必须在建 TCP 前就被 Control 挡住。
	tr := GuardTransport(&http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
	}}, PolicyStrict)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := tr.DialTLSContext(ctx, "tcp", ln.Addr().String())
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("DialTLSContext to loopback under strict err=%v, want ErrBlockedAddress", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("blocked TLS dial took %v, should be rejected at Control", time.Since(start))
	}
}

// 端到端验证自定义 TLS 拨号：经 GuardTransport 的 https 请求既要过 IP 复核，
// 又要在「已校验的那条 TCP」上成功完成握手并取回响应 —— 证明我们替换
// govmomi 的 DialTLSContext 后没有破坏正常 https 通信。
func TestGuardTransportHTTPSHappyPath(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	tr := GuardTransport(&http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true,
	}}, PolicyDefault)
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET through guarded TLS transport: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
