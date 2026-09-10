package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	vmwareCollectors "github.com/prezhdarov/vmware-exporter/vmware/collectors"
	"github.com/vmware/govmomi/simulator"
)

func TestProbeHandlerReturnsBadRequestWithoutTarget(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	if !strings.Contains(rec.Body.String(), "target parameter is required") {
		t.Fatalf("response body = %q, want it to contain %q", rec.Body.String(), "target parameter is required")
	}
}

func TestProbeHandlerReturnsBadRequestWithoutCredentials(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	req := httptest.NewRequest(http.MethodGet, "/probe?target=vcenter.example.com", nil)
	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	if !strings.Contains(rec.Body.String(), "username and password are required") {
		t.Fatalf("response body = %q, want it to mention missing credentials", rec.Body.String())
	}
}

// TestProbeHandlerAcceptsBasicAuthCredentials 覆盖凭证回退路径：
// URL 未带 username/password 时应从 Basic Auth 取。
//
// 断言的信号换了。改动前用 401 区分「凭证解析成功但登录失败」与 400
// 「缺凭证」；现在登录失败不再返回 401（见
// TestProbeHandlerEmitsUpZeroOnLoginFailure），401 这个信号消失了。
//
// 换成 `vmware_up 0`：它只在真的尝试过登录之后才会产出。若凭证没被解析出来，
// probeHandler 会在登录之前就 400 掉，响应里根本不会有任何 vmware_ 指标。
// 所以这个断言与原来一样能分辨两种情况，而且更贴近实际行为。
func TestProbeHandlerAcceptsBasicAuthCredentials(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	req := httptest.NewRequest(http.MethodGet, "/probe?target=127.0.0.1:1&schema=http", nil)
	req.SetBasicAuth("user", "pass")
	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}

	// up=0 的存在本身就证明登录被尝试过 —— 也就证明 Basic Auth 里的凭证
	// 被解析出来了。凭证缺失会在登录之前 400，不会有任何 vmware_ 指标。
	if !strings.Contains(rec.Body.String(), "vmware_up 0") {
		t.Fatalf("response has no `vmware_up 0`, so login was never attempted; "+
			"the Basic Auth credentials were not parsed. body=%q", rec.Body.String())
	}
}

func TestProbeHandlerRejectsUnknownCollector(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	req := httptest.NewRequest(http.MethodGet,
		"/probe?target=vcenter.example.com&username=u&password=p&collect[]=vms", nil)
	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d for a misspelled collector name", rec.Code, http.StatusBadRequest)
	}

	body := rec.Body.String()

	if !strings.Contains(body, "unknown collector") {
		t.Fatalf("response body = %q, want it to report the unknown collector", body)
	}

	// 报错必须列出可用名，否则调用方无从修正。
	if !strings.Contains(body, "vm") {
		t.Fatalf("response body = %q, want it to list available collectors", body)
	}
}

// TestProbeHandlerEmitsUpZeroOnLoginFailure 锁住登录失败的新行为。
//
// **这是一次刻意的行为变更。** 改动前 probeHandler 先登录、失败就
// http.Error(401)，于是 Prometheus 收到一个 HTTP 错误、拿不到任何指标 ——
// 「vCenter 拒绝了凭证」和「exporter 自己挂了」在监控上完全无法区分，
// 两者都只表现为抓取失败。
//
// 现在登录发生在 CollectorSet.Collect 内部，失败会产出 vmware_up 0 加上
// 每个 collector 的 vmware_scrape_collector_success 0。凭证错误于是变成
// 一条可告警的时间序列，而 HTTP 状态码保持 200 —— 抓取本身是成功的，
// 失败的是目标。
//
// 只发 up=0 不够：那样 collector_success 序列会凭空消失，依赖它的告警从
// 「触发」变成「无数据」，这两种状态在 Alertmanager 里行为完全不同。
// 所以下面同时断言两者。
func TestProbeHandlerEmitsUpZeroOnLoginFailure(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	req := httptest.NewRequest(http.MethodGet,
		"/probe?target=127.0.0.1:1&username=u&password=p&schema=http", nil)
	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; the scrape succeeded, it is the target that failed. body=%q",
			rec.Code, http.StatusOK, rec.Body.String())
	}

	body := rec.Body.String()

	if !strings.Contains(body, "vmware_up 0") {
		t.Fatalf("response is missing `vmware_up 0`; without it a credential error is "+
			"indistinguishable from a dead exporter. body=%q", body)
	}

	// 每个启用的 collector 都要有 success 0，一个都不能少。
	for _, def := range vmwareCollectors.Definitions() {
		if !def.DefaultEnabled {
			continue
		}
		want := `vmware_scrape_collector_success{collector="` + def.Name + `"} 0`
		if !strings.Contains(body, want) {
			t.Errorf("response is missing %s; a vanished series turns an alert from "+
				"`firing` into `no data`, which Alertmanager treats differently", want)
		}
	}
}

// TestMetricsHandlerServesBuildInfo 是 /metrics 的冒烟检查。
//
// 改动前这个测试调的是框架的 exporter.CreateHandleFunc，测的是依赖库而不是
// 本仓库的代码。现在它走 metricsHandler —— 也就是生产环境真正挂在
// /metrics 上的那个 handler。
//
// 127.0.0.1:1 连不通，但 build_info 属于 exporter 自身指标，即便目标登录
// 失败也会照常产出。这一点本身就是个断言：自监控指标不能因为目标故障而消失。
func TestMetricsHandlerServesBuildInfo(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	restoreExporterFlags(t)
	*disableExporterMetrics = false
	*disableExporterTarget = false

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()

	metricsHandler(logger)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), "vmware_exporter_build_info") {
		t.Fatalf("response body = %q, want it to contain %q", rec.Body.String(), "vmware_exporter_build_info")
	}
}

// TestMetricsHandlerHonoursCollectorFlags 断言 /metrics 尊重 -collector.<name>。
//
// 回归测试。metricsHandler 构造 Options 时漏了 Enabled 字段，于是
// NewCollectorSet 对每个 collector 都退回 def.DefaultEnabled
// （set.go:178），命令行开关在这条路径上完全失效：
//
//   - -collector.vsan=true 打不开默认禁用的 collector，用户看不到任何
//     vsan 指标，日志里只有一行 "collector disabled"，与命令行矛盾；
//   - -collector.vm=false 也关不掉默认启用的 collector，本该被排除的
//     采集照跑不误。
//
// 后者是更隐蔽的一半：症状是「关不掉」而不是「没数据」，在大规模环境里
// 表现为无法通过关闭 collector 来降低 vCenter 压力。
//
// /probe 一直是对的（它从 URL 参数构造 Enabled），所以两条路径的行为
// 在这个 bug 下是分叉的 —— 同一份 flag，/probe 生效、/metrics 不生效。
func TestMetricsHandlerHonoursCollectorFlags(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	restoreExporterFlags(t)
	*disableExporterMetrics = false
	*disableExporterTarget = false

	// 不连 vCenter：登录必然失败，但那正好够用 —— 登录失败时
	// CollectorSet 会为**每个启用的 collector** 产出 success=0
	// （set.go:291），这份名单就是「哪些 collector 被启用了」的
	// 可观测投影，不需要一个能连上的 target 就能断言。
	restoreVMwareTargetFlags(t)

	// 一开一关，覆盖 bug 的两个方向。
	setBoolFlag(t, "collector.vsan", true) // 默认禁用，要能打开
	setBoolFlag(t, "collector.vm", false)  // 默认启用，要能关掉

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()

	metricsHandler(logger)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}

	body := rec.Body.String()

	if want := `vmware_scrape_collector_success{collector="vsan"}`; !strings.Contains(body, want) {
		t.Errorf("-collector.vsan=true did not enable the collector: %s is absent.\n"+
			"metricsHandler must pass Enabled: collector.Registered() to NewCollectorSet.\nbody:\n%s",
			want, body)
	}

	if notWant := `vmware_scrape_collector_success{collector="vm"}`; strings.Contains(body, notWant) {
		t.Errorf("-collector.vm=false did not disable the collector: %s is present.\n"+
			"metricsHandler must pass Enabled: collector.Registered() to NewCollectorSet.\nbody:\n%s",
			notWant, body)
	}
}

// TestProbeHandlerAgainstSimulator 端到端跑通一次 probe，对着内存版 vCenter。
//
// 这是 Stage 2 的核心验收：/probe 此前串行执行且不产出任何自监控指标，
// 与 /metrics 行为分叉。现在它必须产出与框架完全同名的
// vmware_scrape_collector_duration_seconds / _success。
func TestProbeHandlerAgainstSimulator(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatalf("failed to create simulator model: %v", err)
	}
	defer model.Remove()

	server := model.Service.NewServer()
	defer server.Close()

	// 只跑 datacenter，够验证调度与自监控指标，又不至于让测试太慢。
	url := fmt.Sprintf("/probe?target=%s&username=user&password=pass&schema=%s&insecure=true&collect[]=datacenter",
		server.URL.Host, server.URL.Scheme)

	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}

	body := rec.Body.String()

	// 自监控指标必须与框架 /metrics 路径同名，否则同一套查询无法覆盖两个端点。
	wantMetrics := []string{
		`vmware_scrape_collector_duration_seconds{collector="datacenter"}`,
		`vmware_scrape_collector_success{collector="datacenter"}`,
		`vmware_scrape_collector_duration_seconds{collector="all_collectors"}`,
	}

	for _, want := range wantMetrics {
		if !strings.Contains(body, want) {
			t.Fatalf("response body is missing %q\nbody:\n%s", want, body)
		}
	}

	// 被禁用的 collector 不应出现。
	if strings.Contains(body, `collector="vm"`) {
		t.Fatalf(`response contains collector="vm" although only datacenter was requested`)
	}

	if !strings.Contains(body, `vmware_scrape_collector_success{collector="datacenter"} 1`) {
		t.Fatalf("datacenter collector did not report success=1\nbody:\n%s", body)
	}
}

func TestParseCollectors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	allNames := vmwareCollectors.Names()

	tests := []struct {
		name         string
		params       map[string][]string
		wantEnabled  []string
		wantDisabled []string
		wantUnknown  []string
		// wantEmpty 表示返回空 map，即全部走各自的默认值。
		wantEmpty bool
	}{
		{
			name:      "no params falls back to defaults",
			params:    map[string][]string{},
			wantEmpty: true,
		},
		{
			name:        "all enables every collector",
			params:      map[string][]string{"collect[]": {"all"}},
			wantEnabled: allNames,
		},
		{
			name:         "all minus one",
			params:       map[string][]string{"collect[]": {"all"}, "nocollect[]": {"esxcli.host.nic"}},
			wantEnabled:  []string{"datacenter", "vm", "host"},
			wantDisabled: []string{"esxcli.host.nic"},
		},
		{
			// 显式 collect[] 意味着「只跑这些」，默认启用的其他项必须关掉。
			name:         "explicit list disables the rest",
			params:       map[string][]string{"collect[]": {"vm"}},
			wantEnabled:  []string{"vm"},
			wantDisabled: []string{"datacenter", "cluster", "datastore", "host"},
		},
		{
			name:        "misspelled name is reported",
			params:      map[string][]string{"collect[]": {"vms"}},
			wantUnknown: []string{"vms"},
		},
		{
			name:        "unknown name in nocollect is reported",
			params:      map[string][]string{"collect[]": {"all"}, "nocollect[]": {"nosuch"}},
			wantUnknown: []string{"nosuch"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enabled, unknown := parseCollectors(tc.params, logger)

			if tc.wantEmpty && len(enabled) != 0 {
				t.Fatalf("expected an empty map so defaults apply, got %v", enabled)
			}

			for _, name := range tc.wantEnabled {
				if !enabled[name] {
					t.Fatalf("collector %q should be enabled, got map %v", name, enabled)
				}
			}

			for _, name := range tc.wantDisabled {
				// 必须区分「显式设为 false」和「键不存在」：后者会回退到
				// 该 collector 的默认值，对默认启用的项来说等于没关掉。
				// 用 map 索引的零值判断无法区分这两种情况，所以查 ok。
				value, ok := enabled[name]
				if !ok {
					t.Fatalf("collector %q is absent from the map, so it would fall back to its default (enabled); it must be explicitly set to false. map=%v",
						name, enabled)
				}

				if value {
					t.Fatalf("collector %q should be disabled, got map %v", name, enabled)
				}
			}

			if len(unknown) != len(tc.wantUnknown) {
				t.Fatalf("unknown = %v, want %v", unknown, tc.wantUnknown)
			}

			for i, name := range tc.wantUnknown {
				if unknown[i] != name {
					t.Fatalf("unknown[%d] = %q, want %q", i, unknown[i], name)
				}
			}
		})
	}
}

// TestProbeHandlerAcceptsPostForm 覆盖调试页走的那条路径：参数在 POST 的
// 表单体里，不在 URL 上。
//
// 这条路径存在的理由是凭证。作为查询参数发出去的密码会进浏览器地址栏、
// 进浏览器历史，并且会被任何记录查询串的反向代理写进 access log；放在请求体
// 里则不会。Basic Auth 本来是另一个选择，但 exporter 自身的 basic auth
// （-web.config.file 的 basic_auth_users）用的是同一个 Authorization 头，
// 配了之后 vCenter 凭证就传不进来了。
func TestProbeHandlerAcceptsPostForm(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	form := url.Values{
		"target":   {"127.0.0.1:1"},
		"username": {"user"},
		"password": {"pass"},
		"schema":   {"http"},
	}

	req := httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(form.Encode()))
	// 这个头是必须的。缺了它 ParseForm 会静默忽略整个请求体，参数一个都
	// 拿不到 —— 实测行为，所以前端也必须显式设置它。
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}

	// 与 Basic Auth 那个测试同样的信号：up=0 证明登录被尝试过，也就证明
	// target 与凭证都从表单体里解析出来了。任何一个缺失都会在登录之前 400。
	if !strings.Contains(rec.Body.String(), "vmware_up 0") {
		t.Fatalf("response has no `vmware_up 0`, so login was never attempted; "+
			"the form body was not parsed. body=%q", rec.Body.String())
	}
}

// TestProbeHandlerAcceptsPostFormCollectors 确认 collect[] 这种重复键在表单
// 体里也能正确解析成多个值。
//
// 单独一条测试是因为重复键与普通键的解析路径不同：url.Values 是
// map[string][]string，取错了会只拿到第一个值，于是 collect[]=vm&collect[]=host
// 会退化成「只跑 vm」。这种错误不会报错，只会让指标少一半。
func TestProbeHandlerAcceptsPostFormCollectors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	form := url.Values{
		"target":    {"127.0.0.1:1"},
		"username":  {"user"},
		"password":  {"pass"},
		"schema":    {"http"},
		"collect[]": {"vm", "host"},
	}

	req := httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}

	body := rec.Body.String()

	// collect[] 显式给出意味着「只跑这些」。被选中的两个必须有 success
	// 指标，没被选中的必须一个都没有 —— 后者才是能抓到「只取到第一个值」
	// 那类 bug 的断言。
	for _, want := range []string{
		`vmware_scrape_collector_success{collector="vm"`,
		`vmware_scrape_collector_success{collector="host"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response is missing %q; the repeated collect[] values were not all parsed. body=%q", want, body)
		}
	}

	if strings.Contains(body, `vmware_scrape_collector_success{collector="datastore"`) {
		t.Fatalf("datastore ran even though collect[] listed only vm and host; body=%q", body)
	}
}

// TestProbeHandlerStillAcceptsGetQuery 是一道回归护栏，不是新功能的测试。
//
// 加表单支持最自然的写法是 if r.Method == http.MethodPost { r.ParseForm() }，
// 那样写会静默废掉整个 GET 路径：实测确认，只在 POST 分支调用 ParseForm 时，
// GET 请求的 r.Form 是一个空 map，于是 target 取不到、每个 /probe?... 都
// 变成 400。Prometheus 那一侧会全部停摆，而单测如果只覆盖新增的 POST 路径
// 就完全看不见。
func TestProbeHandlerStillAcceptsGetQuery(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	req := httptest.NewRequest(http.MethodGet,
		"/probe?target=127.0.0.1:1&username=user&password=pass&schema=http&collect[]=vm", nil)
	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}

	body := rec.Body.String()

	if !strings.Contains(body, "vmware_up 0") {
		t.Fatalf("response has no `vmware_up 0`, so the query string was not parsed; body=%q", body)
	}

	if !strings.Contains(body, `vmware_scrape_collector_success{collector="vm"`) {
		t.Fatalf("collect[]=vm from the query string was not honoured; body=%q", body)
	}
}

// TestProbeHandlerToleratesMalformedParams 固定畸形参数的处置方式。
//
// r.URL.Query() 遇到坏的百分号转义时静默丢弃那一个键，其余键照常返回；
// r.ParseForm() 对同样的输入返回 error。换成 ParseForm 之后如果把这个 error
// 当作请求失败来处理，「某一个参数写错了」就会从「那个参数为空」升级成
// 「整个请求 400」—— 对既有的 GET 调用方是行为变更。所以 error 只记日志。
func TestProbeHandlerToleratesMalformedParams(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// %zz 不是合法的转义序列。target 之后的参数仍然必须被解析出来。
	req := httptest.NewRequest(http.MethodGet,
		"/probe?bad=%zz&target=127.0.0.1:1&username=user&password=pass&schema=http", nil)
	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; a malformed parameter must not fail the whole request. body=%q",
			rec.Code, http.StatusOK, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), "vmware_up 0") {
		t.Fatalf("response has no `vmware_up 0`; the well-formed parameters were discarded along with the bad one. body=%q",
			rec.Body.String())
	}
}
