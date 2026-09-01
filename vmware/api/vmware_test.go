package vmware

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/session/cache"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

func TestRequestGETWithHeaders(t *testing.T) {
	restoreVMwareFlags(t)

	*vmwTLS = false
	*vmwInterval = 20

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %q, want %q", r.Method, http.MethodGet)
		}

		if got := r.Header.Get("X-Test-Header"); got != "test-value" {
			t.Fatalf("X-Test-Header = %q, want %q", got, "test-value")
		}

		w.Header().Set("Cookie", "vmware-session=fake-session")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	statusCode, cookie, body, err := requestWithCreds(
		http.MethodGet,
		server.URL,
		map[string]string{"X-Test-Header": "test-value"},
		testCredentials(),
		false,
	)
	if err != nil {
		t.Fatalf("request() returned error: %v", err)
	}

	if statusCode != http.StatusOK {
		t.Fatalf("statusCode = %d, want %d", statusCode, http.StatusOK)
	}

	if cookie != "vmware-session=fake-session" {
		t.Fatalf("cookie = %q, want %q", cookie, "vmware-session=fake-session")
	}

	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", string(body), "ok")
	}
}

func TestRequestPOSTWithBasicAuthWhenLoginIsTrue(t *testing.T) {
	restoreVMwareFlags(t)

	*vmwUser = "test-user"
	*vmwPasswd = "test-password"
	*vmwTLS = false
	*vmwInterval = 20

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %q, want %q", r.Method, http.MethodPost)
		}

		username, password, ok := r.BasicAuth()
		if !ok {
			t.Fatal("expected BasicAuth to be set")
		}

		if username != "test-user" {
			t.Fatalf("username = %q, want %q", username, "test-user")
		}

		if password != "test-password" {
			t.Fatalf("password = %q, want %q", password, "test-password")
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	}))
	defer server.Close()

	statusCode, _, body, err := requestWithCreds(
		http.MethodPost,
		server.URL,
		map[string]string{},
		testCredentials(),
		true,
	)
	if err != nil {
		t.Fatalf("request() returned error: %v", err)
	}

	if statusCode != http.StatusCreated {
		t.Fatalf("statusCode = %d, want %d", statusCode, http.StatusCreated)
	}

	if string(body) != "created" {
		t.Fatalf("body = %q, want %q", string(body), "created")
	}
}

func TestRequestReturnsErrorForInvalidURL(t *testing.T) {
	restoreVMwareFlags(t)

	*vmwTLS = false
	*vmwInterval = 20

	statusCode, cookie, body, err := requestWithCreds(
		http.MethodGet,
		":// invalid-url",
		map[string]string{},
		testCredentials(),
		false,
	)

	if err == nil {
		t.Fatal("expected request() to return an error")
	}

	if statusCode != 0 {
		t.Fatalf("statusCode = %d, want 0", statusCode)
	}

	if cookie != "" {
		t.Fatalf("cookie = %q, want empty string", cookie)
	}

	if body != nil {
		t.Fatalf("body = %q, want nil", string(body))
	}
}

func TestVMwareGetReturnsResponseBody(t *testing.T) {
	restoreVMwareFlags(t)

	*vmwSchema = "http"
	*vmwTLS = false
	*vmwInterval = 20

	logger := discardLogger()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/test" {
			t.Fatalf("path = %q, want %q", r.URL.Path, "/api/test")
		}

		if got := r.Header.Get("X-Session"); got != "fake-session" {
			t.Fatalf("X-Session = %q, want %q", got, "fake-session")
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	target := strings.TrimPrefix(server.URL, "http://")

	loginData := map[string]interface{}{
		"target": target,
		"headers": map[string]string{
			"X-Session": "fake-session",
		},
		"credentials": testCredentials(),
	}

	extraConfig := map[string]interface{}{
		"api": "/api/test",
	}

	vm := NewAPI()

	got, err := vm.Get(loginData, extraConfig, logger)
	if err != nil {
		t.Fatalf("Get() returned error: %v", err)
	}

	body, ok := got.(*[]byte)
	if !ok {
		t.Fatalf("Get() returned %T, want *[]byte", got)
	}

	if string(*body) != `{"status":"ok"}` {
		t.Fatalf("body = %q, want %q", string(*body), `{"status":"ok"}`)
	}
}

// TestLogoutToleratesIncompleteLoginData 覆盖登录中途失败的场景：
// loginData 里可能缺 session / client / cancel，也可能键上挂着错误类型，
// Logout() 必须容忍而不是 panic。
//
// 本测试取代旧的 TestLogoutDoesNothingAndReturnsNil —— 那个名字断言的正是
// P0-1 的错误行为（Logout 什么都不做），现在 Logout 会真的发 SOAP 登出。
func TestLogoutToleratesIncompleteLoginData(t *testing.T) {
	logger := discardLogger()
	vm := NewAPI()

	cases := []struct {
		name      string
		loginData map[string]interface{}
	}{
		{name: "empty map", loginData: map[string]interface{}{}},
		{name: "target only", loginData: map[string]interface{}{"target": "vcenter.example.com"}},
		{
			name: "cancel without session",
			loginData: func() map[string]interface{} {
				_, cancel := context.WithCancel(context.Background())
				return map[string]interface{}{"target": "vcenter.example.com", "cancel": cancel}
			}(),
		},
		{
			// 类型不匹配时也不能裸断言 panic。
			name: "session and client keys hold wrong types",
			loginData: map[string]interface{}{
				"target":  "vcenter.example.com",
				"session": "not-a-session",
				"client":  42,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Logout() panicked on incomplete loginData: %v", r)
				}
			}()

			if err := vm.Logout(tc.loginData, logger); err != nil {
				t.Fatalf("Logout() returned error: %v", err)
			}
		})
	}
}

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

func TestRequestReturnsErrorWhenConnectionFails(t *testing.T) {
	restoreVMwareFlags(t)

	*vmwTLS = false
	*vmwInterval = 20

	statusCode, cookie, body, err := requestWithCreds(
		http.MethodGet,
		"http://127.0.0.1:1",
		map[string]string{},
		testCredentials(),
		false,
	)

	if err == nil {
		t.Fatal("expected request() to return an error")
	}

	if statusCode != 0 {
		t.Fatalf("statusCode = %d, want 0", statusCode)
	}

	if cookie != "" {
		t.Fatalf("cookie = %q, want empty string", cookie)
	}

	if body != nil {
		t.Fatalf("body = %q, want nil", string(body))
	}
}

func TestRequestReturnsErrorOnTimeout(t *testing.T) {
	restoreVMwareFlags(t)

	*vmwTLS = false
	// HTTP 客户端超时现在由 -vmware.timeout 控制，不再从 -vmware.interval 推导。
	*vmwTimeout = 1

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	statusCode, cookie, body, err := requestWithCreds(
		http.MethodGet,
		server.URL,
		map[string]string{},
		testCredentials(),
		false,
	)

	if err == nil {
		t.Fatal("expected request() to return an error")
	}

	if statusCode != 0 {
		t.Fatalf("statusCode = %d, want 0", statusCode)
	}

	if cookie != "" {
		t.Fatalf("cookie = %q, want empty string", cookie)
	}

	if body != nil {
		t.Fatalf("body = %q, want nil", string(body))
	}
}

func TestLoginUsesDefaultVCenterWhenTargetEmpty(t *testing.T) {
	restoreVMwareFlags(t)

	*vmwUser = "user"
	*vmwPasswd = "pass"
	*vCenter = "127.0.0.1:1"
	*vmwSchema = "https"
	*vmwTLS = true
	*vmwInterval = 20

	logger := discardLogger()
	vm := NewAPI()

	loginData, err := vm.Login("", logger)
	if err == nil {
		t.Fatal("expected Login() to return an error")
	}

	if got := loginData["target"]; got != *vCenter {
		t.Fatalf("target = %v, want %q", got, *vCenter)
	}
}

func TestLoginReturnsErrorWhenGovmomiLoginFails(t *testing.T) {
	restoreVMwareFlags(t)

	*vmwUser = "user"
	*vmwPasswd = "pass"
	*vmwSchema = "https"
	*vmwTLS = true
	*vmwInterval = 20

	logger := discardLogger()
	vm := NewAPI()

	loginData, err := vm.Login("127.0.0.1:1", logger)
	if err == nil {
		t.Fatal("expected Login() to return an error")
	}

	if got := loginData["target"]; got != "127.0.0.1:1" {
		t.Fatalf("target = %v, want %q", got, "127.0.0.1:1")
	}
}

func TestGovmomiLoginSetsRequiredFields(t *testing.T) {
	restoreVMwareFlags(t)

	model := simulator.VPX()
	defer model.Remove()

	if err := model.Create(); err != nil {
		t.Fatalf("failed to create simulator model: %v", err)
	}

	server := model.Service.NewServer()
	defer server.Close()

	if server.URL.User == nil {
		t.Fatal("simulator URL is missing credentials")
	}

	password, ok := server.URL.User.Password()
	if !ok {
		t.Fatal("simulator URL is missing password")
	}

	*vmwUser = server.URL.User.Username()
	*vmwPasswd = password
	*vmwSchema = server.URL.Scheme
	*vmwTLS = true
	*vmwInterval = 20
	*vmGranularity = 10

	loginData := map[string]interface{}{
		"target": server.URL.Host,
	}

	if err := govmomiLoginWithCreds(loginData, Credentials{Username: *vmwUser, Password: *vmwPasswd, Target: server.URL.Host, Schema: *vmwSchema, Insecure: *vmwTLS}, discardLogger()); err != nil {
		t.Fatalf("govmomiLogin() returned error: %v", err)
	}

	if _, ok := loginData["ctx"].(context.Context); !ok {
		t.Fatalf("ctx type = %T, want context.Context", loginData["ctx"])
	}

	cancel, ok := loginData["cancel"].(context.CancelFunc)
	if !ok {
		t.Fatalf("cancel type = %T, want context.CancelFunc", loginData["cancel"])
	}
	defer cancel()

	if _, ok := loginData["client"].(*vim25.Client); !ok {
		t.Fatalf("client type = %T, want *vim25.Client", loginData["client"])
	}

	// session 必须存进 loginData，否则 Logout() 无法发起 SOAP 登出（P0-1）。
	if _, ok := loginData["session"].(*cache.Session); !ok {
		t.Fatalf("session type = %T, want *cache.Session", loginData["session"])
	}

	if _, ok := loginData["view"].(*view.Manager); !ok {
		t.Fatalf("view type = %T, want *view.Manager", loginData["view"])
	}

	if _, ok := loginData["perf"].(*performance.Manager); !ok {
		t.Fatalf("perf type = %T, want *performance.Manager", loginData["perf"])
	}

	counters, ok := loginData["counters"].(map[string]*types.PerfCounterInfo)
	if !ok {
		t.Fatalf("counters type = %T, want map[string]*types.PerfCounterInfo", loginData["counters"])
	}

	if len(counters) == 0 {
		t.Fatal("expected counters to be populated")
	}

	if got := loginData["interval"]; got != int32(20) {
		t.Fatalf("interval = %v, want %d", got, int32(20))
	}

	if got := loginData["samples"]; got != int32(2) {
		t.Fatalf("samples = %v, want %d", got, int32(2))
	}
}

func testCredentials() Credentials {
	return Credentials{
		Username: *vmwUser,
		Password: *vmwPasswd,
		Schema:   *vmwSchema,
		Insecure: *vmwTLS,
	}
}

// ---------------------------------------------------------------------------
// Stage 1 (P0) 回归测试
// ---------------------------------------------------------------------------

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

// TestLogoutReleasesServerSession 是 P0-1 的核心回归测试。
//
// 旧实现里 cache.Session 设了 Passthrough:true，但 Logout() 只调用 cancel()，
// 从不发 SOAP Logout，服务端会话会一直挂到自然超时（vCenter 默认 30 分钟）。
// 高频抓取下会迅速堆到会话上限，之后所有登录都失败。
//
// 用内存版 vCenter 验证：Logout() 之后服务端会话数必须回落到登录前的水平。
func TestLogoutReleasesServerSession(t *testing.T) {
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

	loginData := make(map[string]interface{})
	if err := govmomiLoginWithCreds(loginData, creds, discardLogger()); err != nil {
		t.Fatalf("govmomiLoginWithCreds() failed: %v", err)
	}

	// Logout 完全依赖这个键，缺了就静默跳过 SOAP 登出。
	if _, ok := loginData["session"].(*cache.Session); !ok {
		t.Fatalf(`loginData["session"] type = %T, want *cache.Session`, loginData["session"])
	}

	afterLogin, err := activeSessionCount(observerCtx, observer)
	if err != nil {
		t.Fatalf("failed to read session count after login: %v", err)
	}

	if afterLogin <= baseline {
		t.Fatalf("session count did not grow after login: baseline=%d after=%d", baseline, afterLogin)
	}

	if err := NewAPI().Logout(loginData, discardLogger()); err != nil {
		t.Fatalf("Logout() returned error: %v", err)
	}

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

			loginData := make(map[string]interface{})
			creds := Credentials{
				Username: "user",
				Password: "pass",
				Target:   server.URL.Host,
				Schema:   server.URL.Scheme,
				Insecure: true,
			}

			if err := govmomiLoginWithCreds(loginData, creds, discardLogger()); err != nil {
				t.Fatalf("login failed: %v", err)
			}
			// Logout 失败不该让测试失败 —— 它是清理动作，被测的东西已经
			// 验证完了。但也不能直接丢掉错误：会话泄漏正是 Stage 1 修的那个
			// bug，真出问题时日志里得有痕迹。
			defer func() {
				if err := NewAPI().Logout(loginData, discardLogger()); err != nil {
					t.Logf("logout failed during cleanup: %v", err)
				}
			}()

			got, ok := loginData["targetType"].(string)
			if !ok {
				t.Fatalf(`loginData["targetType"] type = %T, want string`, loginData["targetType"])
			}

			if got != tc.want {
				t.Fatalf("targetType = %q, want %q (ApiType was %q)",
					got, tc.want,
					loginData["client"].(*vim25.Client).ServiceContent.About.ApiType)
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
