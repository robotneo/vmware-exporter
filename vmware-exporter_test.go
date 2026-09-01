package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prezhdarov/prometheus-exporter/pkg/exporter"
	vmwareCollectors "github.com/prezhdarov/vmware-exporter/vmware/collectors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/simulator"
)

func TestWebConfigUsesListenAddress(t *testing.T) {
	addr := ":9999"

	cfg := webConfig(&addr)

	if cfg == nil {
		t.Fatal("webConfig() returned nil")
	}

	if cfg.WebListenAddresses == nil {
		t.Fatal("WebListenAddresses is nil")
	}

	if len(*cfg.WebListenAddresses) != 1 {
		t.Fatalf("expected exactly 1 listen address, got %d", len(*cfg.WebListenAddresses))
	}

	if got := (*cfg.WebListenAddresses)[0]; got != addr {
		t.Fatalf("listen address = %q, want %q", got, addr)
	}

	if cfg.WebSystemdSocket == nil {
		t.Fatal("WebSystemdSocket is nil")
	}

	if *cfg.WebSystemdSocket {
		t.Fatal("WebSystemdSocket = true, want false")
	}

	if cfg.WebConfigFile == nil {
		t.Fatal("WebConfigFile is nil")
	}

	if *cfg.WebConfigFile != "" {
		t.Fatalf("WebConfigFile = %q, want empty string", *cfg.WebConfigFile)
	}
}

// 以下 probe 测试直接调用项目自己的 probeHandler。
//
// 此前它们调的是依赖库的 exporter.CreateHandleFunc，那是框架为 /metrics
// 路径提供的函数，与本项目的 probeHandler 毫无关系。测试名让人以为覆盖了
// probe 逻辑，实际上凭证解析、parseCollectors、登录失败分支全部零覆盖 ——
// 典型的假阳性。

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
// target 指向一个必然连不通的地址，因此预期是 401（登录失败）而不是 400
// （缺凭证）—— 能走到登录说明凭证已被正确解析。
func TestProbeHandlerAcceptsBasicAuthCredentials(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	req := httptest.NewRequest(http.MethodGet, "/probe?target=127.0.0.1:1&schema=http", nil)
	req.SetBasicAuth("user", "pass")
	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status code = %d, want %d (credentials parsed but login must fail against an unreachable target); body=%q",
			rec.Code, http.StatusUnauthorized, rec.Body.String())
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

// TestProbeHandlerReturnsUnauthorizedOnLoginFailure 覆盖登录失败分支。
// 127.0.0.1:1 上不会有服务监听，登录必然失败。
func TestProbeHandlerReturnsUnauthorizedOnLoginFailure(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	req := httptest.NewRequest(http.MethodGet,
		"/probe?target=127.0.0.1:1&username=u&password=p&schema=http", nil)
	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), "Login failed") {
		t.Fatalf("response body = %q, want it to contain %q", rec.Body.String(), "Login failed")
	}
}

// TestExporterMetricsHandlerServesBuildInfo 保留对框架 handler 的冒烟检查。
// 原测试名为 TestProbeHandlerWithTargetReturnsMetricsPayload，但它调的是
// exporter.CreateHandleFunc（框架实现），与本项目的 probeHandler 无关，
// 名字属于误导。真正的 probe 覆盖见上面几个直连 probeHandler 的测试
// 以及 TestProbeHandlerAgainstSimulator。
//
// 框架 handler 要求 target 参数，127.0.0.1:1 连不通，但 build_info 属于
// exporter 自身指标，即便目标登录失败也会照常产出。
func TestExporterMetricsHandlerServesBuildInfo(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	req := httptest.NewRequest(http.MethodGet, "/metrics?target=127.0.0.1:1", nil)
	rec := httptest.NewRecorder()

	exporter.CreateHandleFunc(rec, req, namespace, "", logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), "vmware_exporter_build_info") {
		t.Fatalf("response body = %q, want it to contain %q", rec.Body.String(), "vmware_exporter_build_info")
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

// TestVMwareCollectorDescribeExposesScrapeMetrics 验证 Describe 不再是空实现。
// 空 Describe 会让 registry 把 collector 当作 unchecked，从而跳过重复注册检测。
func TestVMwareCollectorDescribeExposesScrapeMetrics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	col, err := newVMwareCollector(map[string]interface{}{}, namespace, logger, map[string]bool{})
	if err != nil {
		t.Fatalf("newVMwareCollector() returned error: %v", err)
	}

	ch := make(chan *prometheus.Desc, 8)
	col.Describe(ch)
	close(ch)

	var descs []string
	for d := range ch {
		descs = append(descs, d.String())
	}

	if len(descs) != 2 {
		t.Fatalf("Describe() emitted %d descriptors, want 2 (duration and success)", len(descs))
	}

	joined := strings.Join(descs, "\n")

	for _, want := range []string{"vmware_scrape_collector_duration_seconds", "vmware_scrape_collector_success"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Describe() output is missing %q, got:\n%s", want, joined)
		}
	}
}

// TestCollectorDefinitionsMatchRegisteredFlags 保证 collector 清单与命令行
// 开关一一对应。清单是 /probe 的来源，命令行开关是 /metrics 的来源；
// 两者不同步就会让同一个 collector 在两个端点上表现不一致。
// TestCollectorListHTMLCoversEveryCollector 保证首页文档不会与 collector
// 清单脱同步。这段 HTML 此前是手写的硬编码列表，新增 collector 时容易漏改。
func TestCollectorListHTMLCoversEveryCollector(t *testing.T) {
	out := collectorListHTML()

	for _, def := range vmwareCollectors.Definitions() {
		if !strings.Contains(out, "<code>"+def.Name+"</code>") {
			t.Fatalf("collector %q is missing from the index page listing:\n%s", def.Name, out)
		}

		// 默认状态也要正确反映，否则文档会误导使用者。
		wantState := "disabled"
		if def.DefaultEnabled {
			wantState = "enabled"
		}

		marker := "<code>" + def.Name + "</code>"
		idx := strings.Index(out, marker)
		rest := out[idx:]

		end := strings.Index(rest, "</li>")
		if end < 0 {
			t.Fatalf("malformed list item for collector %q:\n%s", def.Name, rest)
		}

		item := rest[:end]

		if !strings.Contains(item, "default: "+wantState) {
			t.Fatalf("collector %q is documented as %q, want %q; item=%q", def.Name, item, wantState, item)
		}
	}

	// 每个 collector 都应有一句人类可读的说明。
	for _, def := range vmwareCollectors.Definitions() {
		if collectorDescriptions[def.Name] == "" {
			t.Fatalf("collector %q has no entry in collectorDescriptions; the index page would show it without any explanation", def.Name)
		}
	}
}

func TestCollectorDefinitionsMatchRegisteredFlags(t *testing.T) {
	for _, name := range vmwareCollectors.Names() {
		flagName := "collector." + name

		if flag.Lookup(flagName) == nil {
			t.Fatalf("collector %q is listed in Definitions() but has no -%s flag registered; /metrics and /probe would diverge",
				name, flagName)
		}
	}
}
