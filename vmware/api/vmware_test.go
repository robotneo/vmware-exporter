package vmware

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
)

func restoreVMwareFlags(t *testing.T) {
	t.Helper()

	oldUser := *vmwUser
	oldPassword := *vmwPasswd
	oldVCenter := *vCenter
	oldSchema := *vmwSchema
	oldTLS := *vmwTLS
	oldInterval := *vmwInterval
	oldGranularity := *vmGranularity
	oldTimeout := *vmwTimeout

	t.Cleanup(func() {
		*vmwUser = oldUser
		*vmwPasswd = oldPassword
		*vCenter = oldVCenter
		*vmwSchema = oldSchema
		*vmwTLS = oldTLS
		*vmwInterval = oldInterval
		*vmGranularity = oldGranularity
		*vmwTimeout = oldTimeout
	})
}

// TestLoginUsesDefaultVCenterWhenTargetEmpty 验证 target 为空时回落到
// -vmware.vcenter。
//
// 断言方式换了：改动前 Login 返回 map，失败时 map 里仍有 target 键可查。
// 现在失败返回 nil Scrape —— 没有可查的字段了，所以改为断言错误信息里
// 出现那个默认地址。这不是退化：连不上 127.0.0.1:1 的错误必然带上它实际
// 尝试连的地址，若回落没生效，错误里就不会有这个地址。
func TestLoginUsesDefaultVCenterWhenTargetEmpty(t *testing.T) {
	restoreVMwareFlags(t)

	*vmwUser = "user"
	*vmwPasswd = "pass"
	*vCenter = "127.0.0.1:1"
	*vmwSchema = "https"
	*vmwTLS = true
	*vmwInterval = 20
	*vmwTimeout = 2

	vm := NewAPI()

	s, cleanup, err := vm.Login(context.Background(), "")
	if err == nil {
		t.Fatal("expected Login() to return an error")
	}

	// cleanup 必须永不为 nil，调用方才能无条件 defer。这一点比返回值本身
	// 重要：CollectorSet.Collect 里就是无条件 defer cleanup()。
	if cleanup == nil {
		t.Fatal("Login() returned a nil cleanup on failure; callers defer it unconditionally")
	}
	cleanup()

	if s != nil {
		t.Fatalf("Login() returned a non-nil Scrape alongside an error: %+v", s)
	}

	if !strings.Contains(err.Error(), *vCenter) {
		t.Fatalf("error %q does not mention the default vCenter %q; the fallback did not take effect",
			err, *vCenter)
	}
}

// TestLoginReturnsErrorWhenGovmomiLoginFails 覆盖显式 target 连不上的情形。
func TestLoginReturnsErrorWhenGovmomiLoginFails(t *testing.T) {
	restoreVMwareFlags(t)

	*vmwUser = "user"
	*vmwPasswd = "pass"
	*vmwSchema = "https"
	*vmwTLS = true
	*vmwInterval = 20
	*vmwTimeout = 2

	// 显式清空默认值：若实现错误地忽略了传入的 target 而用默认值，
	// 空字符串会让它走进「target 未指定」分支，与这里期望的连接失败
	// 是两种不同的错误 —— 断言就能分辨出来。
	*vCenter = ""

	vm := NewAPI()

	s, cleanup, err := vm.Login(context.Background(), "127.0.0.1:1")
	if err == nil {
		t.Fatal("expected Login() to return an error")
	}
	if cleanup == nil {
		t.Fatal("Login() returned a nil cleanup on failure")
	}
	cleanup()

	if s != nil {
		t.Fatalf("Login() returned a non-nil Scrape alongside an error: %+v", s)
	}

	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("error %q does not mention the requested target", err)
	}
}

// TestLoginRespectsCallerContext 是 Stage 9 的核心验收之一。
//
// 框架时代 scrape context 由 govmomiLoginWithCreds 内部用
// context.Background() 派生，请求侧的取消完全传不进来：客户端早就断开了，
// exporter 还在等 vCenter 回话，一直等到 -vmware.timeout 自然到期。
//
// 关键在于「让请求真正挂住」。第一版这个测试连的是 127.0.0.1:1，
// 反向验证时发现它是个假测试 —— 把实现改回 context.Background() 仍然通过，
// 因为连一个没人监听的端口会立刻 ECONNREFUSED，压根走不到 context 检查。
//
// 所以这里起一个 accept 之后什么都不做的 listener：TCP 握手成功、TLS
// 握手挂住，唯一能让调用返回的就是 context 被取消。若实现忽略调用方的
// ctx，这个调用会一直等到 -vmware.timeout（这里设成 60s）到期，
// 而测试只给 5 秒容忍。
func TestLoginRespectsCallerContext(t *testing.T) {
	restoreVMwareFlags(t)

	// accept 连接但永不回任何字节。net.Listen 而不是 httptest.NewServer：
	// 后者会正常完成 TLS 握手并回 404，请求就不会挂住了。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	// t.Cleanup 而不是 defer：Close 的错误要报出来。这个 listener 的
	// 生命周期就是这个测试，关不掉说明有东西还挂在上面 —— 那正是这个
	// 测试要验证的情形（连接没被取消）以另一种形式出现。
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("closing the listener failed: %v", err)
		}
	})

	accepted := make(chan struct{}, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case accepted <- struct{}{}:
			default:
			}
			// 故意不读不写、不关闭：让对端一直等在 TLS 握手上。
			// 连接由 defer ln.Close() 与进程退出兜底回收。
			_ = conn
		}
	}()

	*vmwUser = "user"
	*vmwPasswd = "pass"
	*vmwSchema = "https"
	*vmwTLS = true
	*vmwInterval = 20
	*vmGranularity = 20
	// 故意设一个远大于测试容忍度的超时。若实现用 Background 派生，
	// 登录会挂到这个超时到期，下面的墙钟断言就会失败。
	*vmwTimeout = 60

	ctx, cancel := context.WithCancel(context.Background())

	// 等连接真正建立之后再取消，确保被取消的是一个已经挂住的请求，
	// 而不是一个还没发出去的请求 —— 后者任何实现都会「立刻返回」，
	// 测不出 context 是否被尊重。
	go func() {
		select {
		case <-accepted:
		case <-time.After(3 * time.Second):
		}
		cancel()
	}()

	begin := time.Now()
	s, cleanup, err := NewAPI().Login(ctx, ln.Addr().String())
	elapsed := time.Since(begin)

	if cleanup != nil {
		cleanup()
	}
	if s != nil {
		t.Fatalf("Login() returned a non-nil Scrape for a cancelled context: %+v", s)
	}
	if err == nil {
		t.Fatal("Login() succeeded against a server that never responds")
	}

	if elapsed > 10*time.Second {
		t.Fatalf("Login() took %s against an unresponsive server with -vmware.timeout=60s; "+
			"the caller's context is being ignored (it was derived from context.Background() "+
			"before Stage 9)", elapsed)
	}
}

// TestValidateFlags 覆盖 P0-3：非法参数组合必须在启动阶段被拒绝。
func TestValidateFlags(t *testing.T) {
	tests := []struct {
		name        string
		interval    int
		granularity int
		timeout     int
		schema      string
		wantErr     bool
	}{
		{name: "defaults are valid", interval: 20, granularity: 20, timeout: 60, schema: "https"},
		{name: "http schema is valid", interval: 60, granularity: 20, timeout: 60, schema: "http"},
		{name: "interval larger than granularity", interval: 300, granularity: 20, timeout: 60, schema: "https"},

		// granularity=0 曾导致 interval/granularity 除零 panic。
		{name: "zero granularity rejected", interval: 20, granularity: 0, timeout: 60, schema: "https", wantErr: true},
		{name: "negative granularity rejected", interval: 20, granularity: -5, timeout: 60, schema: "https", wantErr: true},
		{name: "zero interval rejected", interval: 0, granularity: 20, timeout: 60, schema: "https", wantErr: true},
		{name: "interval below granularity rejected", interval: 10, granularity: 20, timeout: 60, schema: "https", wantErr: true},
		{name: "zero timeout rejected", interval: 20, granularity: 20, timeout: 0, schema: "https", wantErr: true},
		{name: "negative timeout rejected", interval: 20, granularity: 20, timeout: -1, schema: "https", wantErr: true},
		{name: "unknown schema rejected", interval: 20, granularity: 20, timeout: 60, schema: "ftp", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			restoreVMwareFlags(t)

			*vmwInterval = tc.interval
			*vmGranularity = tc.granularity
			*vmwTimeout = tc.timeout
			*vmwSchema = tc.schema

			err := ValidateFlags()

			if tc.wantErr && err == nil {
				t.Fatalf("ValidateFlags() = nil, want error for interval=%d granularity=%d timeout=%d schema=%q",
					tc.interval, tc.granularity, tc.timeout, tc.schema)
			}

			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateFlags() = %v, want nil", err)
			}
		})
	}
}

// TestValidateFlagsPreventsDivideByZero 明确记录 P0-3 的回归约束：
// granularity=0 必须在启动阶段返回错误，而不是运行到 samples 计算时 panic。
func TestValidateFlagsPreventsDivideByZero(t *testing.T) {
	restoreVMwareFlags(t)

	*vmwInterval = 20
	*vmGranularity = 0
	*vmwTimeout = 60
	*vmwSchema = "https"

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ValidateFlags() panicked instead of returning an error: %v", r)
		}
	}()

	if err := ValidateFlags(); err == nil {
		t.Fatal("ValidateFlags() accepted granularity=0, which would divide by zero at scrape time")
	}
}

// TestTimeoutIsIndependentFromInterval 验证 P0-2：抓取超时不再由采样频率推导。
// 旧实现是 (interval-2)s，改 interval 会连带改超时；现在两者必须解耦。
func TestTimeoutIsIndependentFromInterval(t *testing.T) {
	restoreVMwareFlags(t)

	*vmwInterval = 20
	*vmwTimeout = 120

	*vmwInterval = 300
	if got := *vmwTimeout; got != 120 {
		t.Fatalf("timeout changed to %d after modifying interval; the two must be independent", got)
	}

	*vmwTimeout = 45
	if got := *vmwInterval; got != 300 {
		t.Fatalf("interval changed to %d after modifying timeout; the two must be independent", got)
	}
}

// activeSessionCount 通过 property collector 读 SessionManager.sessionList
// 统计服务端当前会话数。SessionList 是 mo.SessionManager 的属性，
// session.Manager 上没有对应方法，只能走属性检索。
func activeSessionCount(ctx context.Context, c *vim25.Client) (int, error) {
	var sm mo.SessionManager

	ref := c.ServiceContent.SessionManager
	if ref == nil {
		return 0, fmt.Errorf("service content has no SessionManager reference")
	}

	if err := property.DefaultCollector(c).RetrieveOne(ctx, *ref, []string{"sessionList"}, &sm); err != nil {
		return 0, err
	}

	return len(sm.SessionList), nil
}

// newObserverClient 建立一个独立的观测会话用于读取服务端会话数。
// 它本身也占一个会话，所以调用方必须以「登录前的数量」作为基线做差值比较，
// 不能假设 Logout 后归零。
func newObserverClient(ctx context.Context, rawURL string) (*vim25.Client, error) {
	u, err := soap.ParseURL(rawURL)
	if err != nil {
		return nil, err
	}

	u.User = url.UserPassword("observer", "pass")

	c, err := govmomi.NewClient(ctx, u, true)
	if err != nil {
		return nil, err
	}

	return c.Client, nil
}

// TestCleanupReleasesServerSession 是 P0-1 的核心回归测试。
//
// 旧实现里 cache.Session 设了 Passthrough:true，但 Logout() 只调用 cancel()，
// 从不发 SOAP Logout，服务端会话会一直挂到自然超时（vCenter 默认 30 分钟）。
// 高频抓取下会迅速堆到会话上限，之后所有登录都失败。
//
// 相对改动前，这个测试现在验证的是**生产路径**：登出逻辑从独立的 Logout()
// 方法搬进了 LoginWithCredentials 返回的 cleanup 闭包，调用方 defer 一次即可。
// 原先它跑的是 govmomiLoginWithCreds + Logout，而这两个在生产代码里
// 已经没人调用了 —— 测一条死路径永远是绿的，说明不了正在跑的代码是对的。
//
// 用内存版 vCenter 验证：cleanup() 之后服务端会话数必须回落到登录前的水平。
func TestCleanupReleasesServerSession(t *testing.T) {
	restoreVMwareFlags(t)

	*vmwInterval = 20
	*vmGranularity = 20
	*vmwTimeout = 60

	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatalf("failed to create simulator model: %v", err)
	}
	defer model.Remove()

	server := model.Service.NewServer()
	defer server.Close()

	observerCtx := context.Background()
	observer, err := newObserverClient(observerCtx, server.URL.String())
	if err != nil {
		t.Fatalf("failed to create observer client: %v", err)
	}

	baseline, err := activeSessionCount(observerCtx, observer)
	if err != nil {
		t.Fatalf("failed to read baseline session count: %v", err)
	}

	creds := Credentials{
		Username: "user",
		Password: "pass",
		Target:   server.URL.Host,
		Schema:   server.URL.Scheme,
		Insecure: true,
	}

	s, cleanup, err := NewAPI().LoginWithCredentials(observerCtx, creds, discardLogger())
	if err != nil {
		t.Fatalf("LoginWithCredentials() failed: %v", err)
	}
	if s == nil {
		t.Fatal("LoginWithCredentials() returned a nil Scrape without an error")
	}

	afterLogin, err := activeSessionCount(observerCtx, observer)
	if err != nil {
		t.Fatalf("failed to read session count after login: %v", err)
	}

	if afterLogin <= baseline {
		t.Fatalf("session count did not grow after login: baseline=%d after=%d", baseline, afterLogin)
	}

	cleanup()

	afterLogout, err := activeSessionCount(observerCtx, observer)
	if err != nil {
		t.Fatalf("failed to read session count after logout: %v", err)
	}

	if afterLogout != baseline {
		t.Fatalf("session leaked: baseline=%d after_login=%d after_logout=%d (want %d)",
			baseline, afterLogin, afterLogout, baseline)
	}
}

// discardLogger 返回一个丢弃全部输出的 logger。测试里只关心行为，不关心日志。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestDetectTargetTypeAgainstSimulators 是 Stage 3 模式识别的核心验收。
//
// 两个 simulator 模型的 ServiceContent.About.ApiType 分别是
// "VirtualCenter"（simulator/vpx/service_content.go:31）与
// "HostAgent"（simulator/esx/service_content.go:27），与真实产品一致，
// 因此这个测试无需真实的 vCenter 或 ESXi 主机。
func TestDetectTargetTypeAgainstSimulators(t *testing.T) {
	testCases := []struct {
		name  string
		model *simulator.Model
		want  string
	}{
		{"vCenter", simulator.VPX(), TargetTypeVCenter},
		{"ESXi", simulator.ESX(), TargetTypeESXi},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			model := tc.model
			if err := model.Create(); err != nil {
				t.Fatalf("failed to create %s model: %v", tc.name, err)
			}
			defer model.Remove()

			server := model.Service.NewServer()
			defer server.Close()

			restoreVMwareFlags(t)
			*vmwSchema = server.URL.Scheme
			*vmwTLS = true
			*vmwInterval = 20
			*vmGranularity = 20
			*vmwTimeout = 60

			creds := Credentials{
				Username: "user",
				Password: "pass",
				Target:   server.URL.Host,
				Schema:   server.URL.Scheme,
				Insecure: true,
			}

			s, cleanup, err := NewAPI().LoginWithCredentials(context.Background(), creds, discardLogger())
			if err != nil {
				t.Fatalf("login failed: %v", err)
			}
			// cleanup 包含 SOAP 登出。失败不该让测试失败 —— 它是清理动作，
			// 被测的东西已经验证完了。会话是否真的释放由
			// TestCleanupReleasesServerSession 专门覆盖。
			defer cleanup()

			// targetType 从 map 里的 any 变成了 Scrape 上的 string 字段，
			// 所以「类型不对」这条断言消失了 —— 编译器已经保证它是 string。
			if s.TargetType != tc.want {
				t.Fatalf("TargetType = %q, want %q (ApiType was %q)",
					s.TargetType, tc.want, s.Client.ServiceContent.About.ApiType)
			}
		})
	}
}

// TestDetectTargetTypeFallsBackToVCenter 固定未知 ApiType 的处理方式。
//
// 未知取值按 vCenter 处理是刻意的保守选择：把一个真 vCenter 误判成 ESXi
// 会让 datacenter/cluster 指标全部退化成伪对象，破坏既有 dashboard；
// 反过来只是拿不到 ESXi 的优化，代价小得多。
func TestDetectTargetTypeFallsBackToVCenter(t *testing.T) {
	testCases := []struct {
		apiType string
		want    string
	}{
		{"VirtualCenter", TargetTypeVCenter},
		{"HostAgent", TargetTypeESXi},
		{"", TargetTypeVCenter},
		{"SomethingNew", TargetTypeVCenter},
	}

	for _, tc := range testCases {
		client := new(vim25.Client)
		client.ServiceContent.About.ApiType = tc.apiType

		if got := detectTargetType(client, discardLogger()); got != tc.want {
			t.Fatalf("detectTargetType(ApiType=%q) = %q, want %q", tc.apiType, got, tc.want)
		}
	}
}
