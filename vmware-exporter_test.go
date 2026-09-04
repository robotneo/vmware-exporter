package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	vmware "github.com/prezhdarov/vmware-exporter/vmware/api"
	vmwareCollectors "github.com/prezhdarov/vmware-exporter/vmware/collectors"
	ui "github.com/prezhdarov/vmware-exporter/web"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/exporter-toolkit/web"
	"github.com/vmware/govmomi/simulator"
	"gopkg.in/yaml.v3"
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

// setBoolFlag 设置一个已注册的 bool flag 并在测试结束时还原。
//
// 直接改 flag.Lookup 拿到的 Value 而不是包级变量指针：collector 的开关
// 定义在 vmware/collectors 包内且未导出，根包测试碰不到。
func setBoolFlag(t *testing.T, name string, v bool) {
	t.Helper()

	f := flag.Lookup(name)
	if f == nil {
		t.Fatalf("flag -%s is not registered", name)
	}

	old := f.Value.String()
	if err := f.Value.Set(strconv.FormatBool(v)); err != nil {
		t.Fatalf("could not set -%s: %v", name, err)
	}

	t.Cleanup(func() {
		if err := f.Value.Set(old); err != nil {
			t.Errorf("could not restore -%s: %v", name, err)
		}
	})
}

// restoreVMwareTargetFlags 把 -vmware.vcenter 指向一个必定连不上的地址，
// 并在测试结束后还原。
func restoreVMwareTargetFlags(t *testing.T) {
	t.Helper()

	f := flag.Lookup("vmware.vcenter")
	if f == nil {
		t.Fatal("flag -vmware.vcenter is not registered")
	}

	old := f.Value.String()
	// 127.0.0.1:1 —— 保留端口，不会有服务在听，连接立刻被拒绝而不是超时。
	if err := f.Value.Set("127.0.0.1:1"); err != nil {
		t.Fatalf("could not set -vmware.vcenter: %v", err)
	}

	t.Cleanup(func() {
		if err := f.Value.Set(old); err != nil {
			t.Errorf("could not restore -vmware.vcenter: %v", err)
		}
	})
}

// restoreExporterFlags 存取根包 flag，避免测试之间互相污染。
// 与 api 包的 restoreVMwareFlags 同一模式。
func restoreExporterFlags(t *testing.T) {
	t.Helper()

	oldMetrics := *disableExporterMetrics
	oldTarget := *disableExporterTarget
	oldConcurrency := *maxConcurrency

	t.Cleanup(func() {
		*disableExporterMetrics = oldMetrics
		*disableExporterTarget = oldTarget
		*maxConcurrency = oldConcurrency
	})
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

// TestCollectorSetDescribeExposesScrapeMetrics 验证 Describe 不是空实现。
// 空 Describe 会让 registry 把 collector 当作 unchecked，从而跳过重复注册检测。
//
// 期望数量从 2 变成 5：Stage 9 新增了 vmware_up 与
// vmware_scrape_duration_seconds，Stage 10a 又加了 vmware_scrape_errors_total。
//   - vmware_up：不是 Prometheus 自己生成的那个 up（那个只表示 HTTP 请求
//     成功）。对多 target exporter 来说 HTTP 成功而 vCenter 登录失败是常态，
//     没有这个指标就写不出「目标不可达」的告警。
//   - vmware_scrape_duration_seconds：通用 exporter 告警规则查的是这个无标签
//     版本，框架只有带 collector="all_collectors" 标签的那个。
//   - vmware_scrape_errors_total：全库唯一的 counter。collector_success 只能
//     回答「最近一轮成不成」，答不出「过去一小时失败几次」—— 间歇性故障在
//     gauge 上会被采样间隔漏掉。
func TestCollectorSetDescribeExposesScrapeMetrics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cs, err := collector.NewCollectorSet(context.Background(), vmwareCollectors.Definitions(), collector.Options{
		Namespace: namespace,
		Login:     vmware.NewAPI(),
		Logger:    logger,
		// Enabled 为 nil 时按各 collector 的默认开关走，这里只关心 Describe，
		// 不需要真的启用任何 collector。
		Enabled: map[string]bool{},
		Errors:  collector.NewScrapeErrors(),
	})
	if err != nil {
		t.Fatalf("NewCollectorSet() returned error: %v", err)
	}

	ch := make(chan *prometheus.Desc, 16)
	cs.Describe(ch)
	close(ch)

	var descs []string
	for d := range ch {
		descs = append(descs, d.String())
	}

	want := []string{
		"vmware_up",
		"vmware_scrape_duration_seconds",
		"vmware_scrape_collector_duration_seconds",
		"vmware_scrape_collector_success",
		"vmware_scrape_errors_total",
	}

	if len(descs) != len(want) {
		t.Fatalf("Describe() emitted %d descriptors, want %d:\n%s",
			len(descs), len(want), strings.Join(descs, "\n"))
	}

	joined := strings.Join(descs, "\n")
	for _, w := range want {
		if !strings.Contains(joined, `fqName: "`+w+`"`) {
			t.Fatalf("Describe() output is missing %q, got:\n%s", w, joined)
		}
	}
}

// TestIndexPageListsEveryCollector 保证页面文档不会与 collector 清单脱同步。
//
// 这段列表此前是手写的硬编码 HTML，新增 collector 时容易漏改。断言对象是
// 渲染后的完整页面而不是中间函数的返回值：模板本身也可能漏掉循环、或者把
// 标签写错，只测数据结构测不出来。
func TestIndexPageListsEveryCollector(t *testing.T) {
	page, err := ui.RenderIndex(pageData())
	if err != nil {
		t.Fatalf("could not render the index page: %v", err)
	}

	out := string(page)

	for _, def := range vmwareCollectors.Definitions() {
		marker := `<div class="coll-name">` + def.Name + "</div>"
		if !strings.Contains(out, marker) {
			t.Fatalf("collector %q is missing from the index page listing", def.Name)
		}

		// 每个 collector 都应有一句人类可读的说明。
		if collectorDescriptions[def.Name] == "" {
			t.Fatalf("collector %q has no entry in collectorDescriptions; the page would show it without any explanation", def.Name)
		}

		if !strings.Contains(out, collectorDescriptions[def.Name]) {
			t.Fatalf("collector %q description is missing from the index page", def.Name)
		}

		// 默认状态也要正确反映，否则文档会误导使用者。带 Cost 标注的
		// collector 展示的是开销提示而不是默认态 —— 那几个全部默认禁用，
		// 而「为什么默认关」的答案正是开销本身。
		if collectorCosts[def.Name] != "" {
			if def.DefaultEnabled {
				t.Fatalf("collector %q carries a cost note but is enabled by default; the page would not show its default state", def.Name)
			}

			if !strings.Contains(out, collectorCosts[def.Name]) {
				t.Fatalf("collector %q cost note %q is missing from the index page", def.Name, collectorCosts[def.Name])
			}

			continue
		}

		wantTag := `<span class="tag off">default off</span>`
		if def.DefaultEnabled {
			wantTag = `<span class="tag on">default on</span>`
		}

		if !strings.Contains(out, wantTag) {
			t.Fatalf("collector %q is not documented with %q", def.Name, wantTag)
		}
	}
}

// TestPagesRenderCompleteHTML 守住一个具体的历史缺陷：改动前的落地页没有
// <!DOCTYPE>、没有 <html> 与 <body> 的开标签（只有闭标签），全靠浏览器容错
// 才显示得出来。模板化之后这类结构缺失同样可能悄悄发生，尤其是在拆分或
// 追加片段的时候。
func TestPagesRenderCompleteHTML(t *testing.T) {
	pages := map[string]func(ui.Data) ([]byte, error){
		"index":  ui.RenderIndex,
		"debug":  ui.RenderDebug,
		"config": ui.RenderConfig,
	}

	for name, render := range pages {
		t.Run(name, func(t *testing.T) {
			page, err := render(pageData())
			if err != nil {
				t.Fatalf("could not render: %v", err)
			}

			out := string(page)

			for _, want := range []string{
				"<!DOCTYPE html>", "<html", "</html>", "<body>", "</body>",
				`<link rel="stylesheet" href="app.css">`,
			} {
				if !strings.Contains(out, want) {
					t.Fatalf("rendered page is missing %q", want)
				}
			}

			// 未替换的模板动作说明数据结构与模板脱节了。html/template 对
			// 未定义字段会直接报错，但拼错的 range 变量可能留下字面量。
			if strings.Contains(out, "{{") {
				t.Fatalf("rendered page still contains a template action")
			}
		})
	}
}

// TestCollectorDefinitionsMatchRegisteredFlags 保证 collector 清单与命令行
// 开关一一对应。清单是 /probe 的来源，命令行开关是 /metrics 的来源；
// 两者不同步就会让同一个 collector 在两个端点上表现不一致。
func TestCollectorDefinitionsMatchRegisteredFlags(t *testing.T) {
	for _, name := range vmwareCollectors.Names() {
		flagName := "collector." + name

		if flag.Lookup(flagName) == nil {
			t.Fatalf("collector %q is listed in Definitions() but has no -%s flag registered; /metrics and /probe would diverge",
				name, flagName)
		}
	}
}

// TestScrapeAgainstESXiSimulator 是 Stage 3 的核心验收：直连一台 ESXi 时
// 全部 7 个 collector 都要能跑完且不 panic。
//
// simulator.ESX() 的对象模型与真实 ESXi 一致 —— Datacenter / Cluster /
// ClusterHost 全为零值（simulator/model.go:141），只有隐式的 ha-* 伪对象，
// ApiType 是 "HostAgent"（simulator/esx/service_content.go:27）。
// 因此这个测试不需要真实的 ESXi 主机。
//
// 之所以要跑全部 collector 而不是挑几个：ESXi 缺层的影响面很难靠推理穷举，
// 缺 Datacenter 时哪个 collector 会在解引用 Parent 时 panic，只有真跑一遍才知道。
func TestScrapeAgainstESXiSimulator(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	model := simulator.ESX()
	if err := model.Create(); err != nil {
		t.Fatalf("failed to create ESX simulator model: %v", err)
	}
	defer model.Remove()

	server := model.Service.NewServer()
	defer server.Close()

	url := fmt.Sprintf("/probe?target=%s&username=user&password=pass&schema=%s&insecure=true&collect[]=all",
		server.URL.Host, server.URL.Scheme)

	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}

	body := rec.Body.String()

	// 每个 collector 都要有成功标记。success=0 说明 Update() 返回了错误，
	// 在 ESXi 上通常意味着某个 vCenter 专属假设没有被正确分支掉。
	for _, name := range vmwareCollectors.Names() {
		want := fmt.Sprintf(`vmware_scrape_collector_success{collector="%s"} 1`, name)
		if !strings.Contains(body, want) {
			t.Errorf("collector %q did not succeed against ESXi\nlooking for: %s", name, want)
		}
	}

	// 类型标识指标是全部 dashboard 条件渲染的依据。
	if !strings.Contains(body, `type="esxi"`) {
		t.Errorf(`vmware_target_info is missing type="esxi"`)
	}

	if !strings.Contains(body, "vmware_target_info{") {
		t.Errorf("vmware_target_info was not emitted at all")
	}
}

// TestESXiEmitsSyntheticDatacenterAndCompute 验证伪对象被正确标注。
//
// 路线丙的核心取舍就在这里：ESXi 上仍然输出 datacenter / compute 指标（否则
// dashboard 里依赖 dcmo / cmo 的关联查询会断链），但打上 synthetic="true"，
// 让「这不是真实数据中心」这件事在指标层面可见。
func TestESXiEmitsSyntheticDatacenterAndCompute(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	model := simulator.ESX()
	if err := model.Create(); err != nil {
		t.Fatalf("failed to create ESX simulator model: %v", err)
	}
	defer model.Remove()

	server := model.Service.NewServer()
	defer server.Close()

	url := fmt.Sprintf("/probe?target=%s&username=user&password=pass&schema=%s&insecure=true&collect[]=datacenter&collect[]=cluster",
		server.URL.Host, server.URL.Scheme)

	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}

	body := rec.Body.String()

	// datacenter 指标必须存在（不断链）且被标记（不含糊）。
	if !strings.Contains(body, "vmware_datacenter_info{") {
		t.Fatalf("vmware_datacenter_info missing on ESXi; dashboards relying on dcmo would break\nbody:\n%s", body)
	}

	if !strings.Contains(body, `dcmo="ha-datacenter"`) {
		t.Errorf("expected the implicit ha-datacenter pseudo object\nbody:\n%s", body)
	}

	// 断言必须精确到具体的指标行。只检查 body 里出现过 synthetic="true"
	// 是不够的 —— cluster 的 ha-compute-res 也带这个 label，会掩盖
	// datacenter 分支根本没生效的情况。这是反向验证时发现的：注掉
	// datacenter 的标注逻辑后，宽泛断言依然通过。
	if !metricHasLabels(body, "vmware_datacenter_info", `dcmo="ha-datacenter"`, `synthetic="true"`) {
		t.Errorf("vmware_datacenter_info for ha-datacenter is not marked synthetic; "+
			"the pseudo object would be indistinguishable from a real datacenter\nbody:\n%s", body)
	}

	if !metricHasLabels(body, "vmware_compute_info", `cmo="ha-compute-res"`, `synthetic="true"`) {
		t.Errorf("vmware_compute_info for ha-compute-res is not marked synthetic\nbody:\n%s", body)
	}

	// cluster collector 在 ESXi 上应回退到 ComputeResource。
	if !strings.Contains(body, "vmware_compute_info{") {
		t.Errorf("vmware_compute_info missing; the ComputeResource fallback did not run\nbody:\n%s", body)
	}
}

// TestVCenterDoesNotEmitSyntheticLabel 是 vCenter 侧的回归防线。
//
// synthetic 只应出现在 ESXi 的伪对象上。若它泄漏到 vCenter，用户用
// {synthetic!="true"} 过滤时会误伤真实数据中心 —— 这类错误在图上表现为
// 数据莫名消失，很难定位到 exporter。
func TestVCenterDoesNotEmitSyntheticLabel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	model := simulator.VPX()
	if err := model.Create(); err != nil {
		t.Fatalf("failed to create VPX simulator model: %v", err)
	}
	defer model.Remove()

	server := model.Service.NewServer()
	defer server.Close()

	url := fmt.Sprintf("/probe?target=%s&username=user&password=pass&schema=%s&insecure=true&collect[]=datacenter&collect[]=cluster",
		server.URL.Host, server.URL.Scheme)

	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()

	probeHandler(rec, req, logger)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
	}

	body := rec.Body.String()

	if strings.Contains(body, `synthetic="true"`) {
		t.Fatalf("synthetic label leaked into vCenter output\nbody:\n%s", body)
	}

	if !strings.Contains(body, `type="vcenter"`) {
		t.Errorf(`vmware_target_info is missing type="vcenter"`)
	}

	// 既有指标必须原样保留 —— 用户的 dashboard 引用的是它，不是新指标。
	if !strings.Contains(body, "vmware_vcenter_info{") {
		t.Errorf("vmware_vcenter_info disappeared; existing dashboards would break")
	}
}

// metricHasLabels 检查指定指标族里是否存在同时带有全部给定 label 的序列。
//
// 为什么需要它：strings.Contains(body, `synthetic="true"`) 这种宽泛断言
// 只能证明整个响应里某处出现过该 label，无法定位到哪个指标。当多个 collector
// 都可能产出同一个 label 时，宽泛断言会掩盖其中一个分支失效的情况 ——
// Stage 3 的反向验证就撞上了这个坑。
func metricHasLabels(body, metricName string, labels ...string) bool {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metricName+"{") {
			continue
		}

		matched := true
		for _, label := range labels {
			if !strings.Contains(line, label) {
				matched = false
				break
			}
		}

		if matched {
			return true
		}
	}

	return false
}

// TestTargetInfoTargetLabelMatchesVCenterLabel 锁死 dashboard 依赖的一条不变量。
//
// 背景：Stage 3 的 dashboard 改造把 $vcenter 模板变量的取值来源从
// vmware_vcenter_info{...}的 vcenter label 换成了
// vmware_target_info{...}的 target label —— 因为只有后者带 type label，
// 换过去才能按目标类型过滤。
//
// 而所有 panel 的查询依然写着 vcenter=~"$vcenter"。也就是说：
//
//	target_info 的 target 值  ->  填进 $vcenter  ->  用来匹配业务指标的 vcenter 值
//
// 这条链要成立，两个 label 的取值必须逐字符相等。一旦哪天有人给其中一个
// 加了端口归一化、去掉了 :443、或改成小写，dashboard 会**静默**变空 ——
// 查询语法合法、指标存在、只是匹配不上，没有任何报错。
//
// 这种失败模式在 Grafana 里极难排查，所以在这里用测试钉住。
func TestTargetInfoTargetLabelMatchesVCenterLabel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, tc := range []struct {
		name  string
		model *simulator.Model
	}{
		{"vcenter", simulator.VPX()},
		{"esxi", simulator.ESX()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := tc.model
			if err := model.Create(); err != nil {
				t.Fatalf("failed to create simulator model: %v", err)
			}
			defer model.Remove()

			server := model.Service.NewServer()
			defer server.Close()

			url := fmt.Sprintf("/probe?target=%s&username=user&password=pass&schema=%s&insecure=true&collect[]=datacenter&collect[]=host",
				server.URL.Host, server.URL.Scheme)

			req := httptest.NewRequest(http.MethodGet, url, nil)
			rec := httptest.NewRecorder()

			probeHandler(rec, req, logger)

			if rec.Code != http.StatusOK {
				t.Fatalf("status code = %d, want %d; body=%q", rec.Code, http.StatusOK, rec.Body.String())
			}

			body := rec.Body.String()

			target := labelValue(body, "vmware_target_info", "target")
			if target == "" {
				t.Fatalf("could not extract target label from vmware_target_info\nbody:\n%s", body)
			}

			// vmware_vcenter_info 是既有指标，$vcenter 原来就取自它。
			vcenter := labelValue(body, "vmware_vcenter_info", "vcenter")
			if vcenter == "" {
				t.Fatalf("could not extract vcenter label from vmware_vcenter_info\nbody:\n%s", body)
			}

			if target != vcenter {
				t.Errorf("vmware_target_info{target=%q} != vmware_vcenter_info{vcenter=%q};\n"+
					"the bundled dashboards feed $vcenter from target_info's target label but filter\n"+
					"panels on vcenter=~\"$vcenter\", so a mismatch silently blanks every panel",
					target, vcenter)
			}

			// 业务指标侧同样必须匹配，否则 panel 过滤照样落空。
			hostVCenter := labelValue(body, "vmware_host_info", "vcenter")
			if hostVCenter == "" {
				t.Fatalf("could not extract vcenter label from vmware_host_info\nbody:\n%s", body)
			}

			if hostVCenter != target {
				t.Errorf("vmware_host_info{vcenter=%q} != vmware_target_info{target=%q}; panel filters would not match",
					hostVCenter, target)
			}
		})
	}
}

// labelValue 从 Prometheus 文本格式里取出指定指标第一条序列上某个 label 的值。
//
// 只做够用的解析：label 值本身不含逗号或引号（这里全是主机名与 moid），
// 所以按 `name="` 定位再截到下一个引号即可，不必引入完整的 expfmt 解析器。
func labelValue(body, metricName, label string) string {
	needle := label + `="`

	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metricName+"{") {
			continue
		}

		idx := strings.Index(line, needle)
		if idx < 0 {
			continue
		}

		rest := line[idx+len(needle):]
		end := strings.Index(rest, `"`)
		if end < 0 {
			continue
		}

		return rest[:end]
	}

	return ""
}

// TestWebConfigPassesThroughConfigFile 覆盖 -web.config.file 是否真的接到了
// exporter-toolkit。
//
// 上面那个 TestWebConfigUsesListenAddress 只断言默认值为空 —— 在 webConfig()
// 里硬编码 `configFile := ""` 的旧实现下它同样能通过，所以它对这条链路是
// 没有约束力的。这里改设 flag 再读回，才能区分「接上了」与「返回了一个恰好
// 也是空字符串的局部变量」。
//
// 这条链路断掉是静默的：exporter 会正常启动、正常服务，只是明文 HTTP，
// 而 /probe 的 URL 参数与 Basic Auth 里带着 vCenter 凭证。
func TestWebConfigPassesThroughConfigFile(t *testing.T) {
	original := *webConfigFile
	t.Cleanup(func() { *webConfigFile = original })

	const want = "/etc/vmware-exporter/web-config.yml"
	*webConfigFile = want

	addr := ":9169"
	cfg := webConfig(&addr)

	if cfg.WebConfigFile == nil {
		t.Fatal("WebConfigFile is nil")
	}

	if got := *cfg.WebConfigFile; got != want {
		t.Fatalf("WebConfigFile = %q, want %q\n"+
			"  the flag is not reaching web.FlagConfig, so TLS and basic auth "+
			"cannot be enabled at all", got, want)
	}
}

// TestWebConfigFileFlagIsRegistered 断言 flag 以预期的名字注册。
//
// 名字是对外契约：systemd unit、compose 文件和文档都写死了它。而且
// exporter-toolkit 上游用的就是 web.config.file，跟着它走能让用户在不同
// exporter 之间复用同一套部署脚本。
func TestWebConfigFileFlagIsRegistered(t *testing.T) {
	f := flag.Lookup("web.config.file")
	if f == nil {
		t.Fatal("flag -web.config.file is not registered")
	}

	if f.DefValue != "" {
		t.Errorf("default = %q, want empty (TLS off unless explicitly configured)", f.DefValue)
	}

	// 说明文字里必须留下能查到格式的线索 —— 这个文件的 schema 不是自解释的，
	// 用户没有指引就只能猜。
	if !strings.Contains(f.Usage, "exporter-toolkit") {
		t.Errorf("usage text does not point at the exporter-toolkit docs: %q", f.Usage)
	}
}

// TestWebConfigFileAcceptsGeneratedTLSConfig 用真实的证书与配置文件走一遍
// exporter-toolkit 的加载路径。
//
// 前两个测试只证明字符串传到了结构体里，不能证明这个值最终真的被用于建立
// TLS。这里生成一张自签证书、写一份最小 web config、启一个 server，然后用
// HTTPS 客户端去访问 —— 如果配置没生效，服务端会是明文 HTTP，TLS 握手就会
// 失败。
func TestWebConfigFileAcceptsGeneratedTLSConfig(t *testing.T) {
	dir := t.TempDir()

	certPath, keyPath := writeSelfSignedCert(t, dir)

	configPath := filepath.Join(dir, "web-config.yml")
	configBody := fmt.Sprintf("tls_server_config:\n  cert_file: %s\n  key_file: %s\n",
		certPath, keyPath)
	if err := os.WriteFile(configPath, []byte(configBody), 0o600); err != nil {
		t.Fatalf("write web config: %v", err)
	}

	original := *webConfigFile
	t.Cleanup(func() { *webConfigFile = original })
	*webConfigFile = configPath

	// 监听 127.0.0.1:0 拿一个空闲端口，避免与并行测试抢固定端口。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprint(w, "pong"); err != nil {
			t.Errorf("write response body: %v", err)
		}
	})
	srv := &http.Server{Handler: mux}
	t.Cleanup(func() { _ = srv.Close() })

	listenAddr := addr
	cfg := webConfig(&listenAddr)

	errCh := make(chan error, 1)
	go func() {
		// ServeMultiple 会读取 WebConfigFile 并据此包上 TLS。
		errCh <- web.ServeMultiple([]net.Listener{ln}, srv, cfg,
			slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	client := &http.Client{
		Transport: &http.Transport{
			// 自签证书，跳过校验；这里要验证的是「有没有 TLS」，
			// 不是「证书链是否可信」。
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 5 * time.Second,
	}

	var resp *http.Response
	var lastErr error
	// server 启动是异步的，短暂重试几次。
	for i := 0; i < 20; i++ {
		resp, lastErr = client.Get("https://" + addr + "/ping")
		if lastErr == nil {
			break
		}
		select {
		case err := <-errCh:
			t.Fatalf("server exited early: %v", err)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("HTTPS request failed after retries: %v\n"+
			"  the web config file was not applied, so the listener is plain HTTP", lastErr)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "pong" {
		t.Fatalf("body = %q, want %q", body, "pong")
	}
	if resp.TLS == nil {
		t.Fatal("resp.TLS is nil: the connection was not encrypted")
	}
}

// writeSelfSignedCert 生成一张仅用于测试的自签证书，返回证书与私钥的路径。
func writeSelfSignedCert(t *testing.T, dir string) (string, string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "vmware-exporter-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPath := filepath.Join(dir, "cert.pem")
	certOut, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open cert file: %v", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}
	if err := certOut.Close(); err != nil {
		t.Fatalf("close cert file: %v", err)
	}

	keyPath := filepath.Join(dir, "key.pem")
	keyOut, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("open key file: %v", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}); err != nil {
		t.Fatalf("encode key: %v", err)
	}
	if err := keyOut.Close(); err != nil {
		t.Fatalf("close key file: %v", err)
	}

	return certPath, keyPath
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

// TestDebugConsoleDisabledReturns404 锁住 -web.debug-console=false 的实际效果。
//
// 这个 flag 的存在理由是暴露面：调试页会把一个「输入任意 target 与凭证、
// 由 exporter 代为发起连接」的表单挂在一个通常不做认证的端口上。在共享
// 网段里这等于给了任何能访问该端口的人一个凭证探测器，所以必须能关掉。
// 配置生成页同样是面向人的交互页面，共用这一个开关。
//
// 断言 404 而不是断言「页面里没有表单」：关掉的语义是路由根本不存在，
// 而不是渲染一张空页面。差别在于前者不会给扫描器留下「这里有个被禁用的
// 调试端点」的痕迹。
//
// 顺带断言 / 与 /app.css 仍然可用 —— 这个 flag 只该影响那两条交互路由。
func TestDebugConsoleDisabledReturns404(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, tc := range []struct {
		name     string
		enabled  bool
		wantCode int
	}{
		{name: "enabled", enabled: true, wantCode: http.StatusOK},
		{name: "disabled", enabled: false, wantCode: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 每个子测试自己的 mux。用 http.DefaultServeMux 的话第二次
			// 注册同一路径会 panic，而且两个子测试会互相污染。
			mux := http.NewServeMux()

			if err := registerUI(mux, logger, tc.enabled); err != nil {
				t.Fatalf("registerUI: %v", err)
			}

			// /config 与 /debug 共用同一个开关，两条路由必须一起开关。
			// 只关一个会留下一个「关不掉的交互页面」，而运维设这个 flag
			// 的意图正是把 exporter 收拢成纯指标端点。
			for _, path := range []string{"/debug", "/config"} {
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

				if rec.Code != tc.wantCode {
					t.Fatalf("GET %s with debug console enabled=%v: status = %d, want %d",
						path, tc.enabled, rec.Code, tc.wantCode)
				}
			}

			// 关掉交互页面不应该顺手关掉落地页或它引用的样式表。
			for _, path := range []string{"/", "/app.css"} {
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

				if rec.Code != http.StatusOK {
					t.Fatalf("GET %s with debug console enabled=%v: status = %d, want %d; "+
						"the -web.debug-console flag must only affect /debug",
						path, tc.enabled, rec.Code, http.StatusOK)
				}
			}
		})
	}
}

// TestStaticAssetsServed 覆盖三个静态资源的注册与响应头。
//
// Content-Type 是断言的重点，不是顺带检查的细节：浏览器对样式表和脚本做
// MIME 类型检查，text/plain 的 app.css 会被直接忽略（页面变成没有样式的
// 裸 HTML），text/plain 的 app.js 在启用了 X-Content-Type-Options: nosniff
// 的部署里会被拒绝执行 —— 两种情况在服务端看都是 200，只有浏览器控制台
// 里才有报错，单测不覆盖就只能靠人工打开页面才能发现。
//
// 同时断言响应体非空且内容对得上：go:embed 的模式写错（比如漏了某个文件）
// 在编译期就会失败，但 registerUI 的路径拼接写错（"/"+path 少了斜杠之类）
// 只会表现为 404，而路由注册顺序变化也可能让 "/" 的通配前缀吃掉它们。
func TestStaticAssetsServed(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	mux := http.NewServeMux()
	if err := registerUI(mux, logger, true); err != nil {
		t.Fatalf("registerUI: %v", err)
	}

	for _, tc := range []struct {
		path        string
		contentType string
		// 一段只可能出现在正确文件里的内容，用来确认路由没有串到别的资源上。
		wantBody string
	}{
		{path: "/app.css", contentType: "text/css; charset=utf-8", wantBody: "--bg"},
		{path: "/app.js", contentType: "text/javascript; charset=utf-8", wantBody: "/probe"},
		{path: "/config.js", contentType: "text/javascript; charset=utf-8", wantBody: "file_sd"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s: status = %d, want %d", tc.path, rec.Code, http.StatusOK)
			}

			if got := rec.Header().Get("Content-Type"); got != tc.contentType {
				t.Fatalf("GET %s: Content-Type = %q, want %q; a wrong MIME type makes the "+
					"browser silently ignore the asset while the server still reports 200",
					tc.path, got, tc.contentType)
			}

			// 升级之后浏览器必须立刻拿到新版资源。max-age 那类缓存会让
			// 旧副本在升级后继续命中，页面与二进制版本对不上。
			if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
				t.Fatalf("GET %s: Cache-Control = %q, want %q", tc.path, got, "no-cache")
			}

			body := rec.Body.String()

			if body == "" {
				t.Fatalf("GET %s: empty body", tc.path)
			}

			if !strings.Contains(body, tc.wantBody) {
				t.Fatalf("GET %s: body does not contain %q, so the route is not serving "+
					"the expected embedded file", tc.path, tc.wantBody)
			}
		})
	}
}

// --- 配置生成页 ---------------------------------------------------------

// nodeBinary 找到可用的 node，找不到就让调用方跳过测试。
//
// 为什么允许跳过：生成逻辑是浏览器代码，测它必须有一个 JS 运行时，而 node
// 不是这个 Go 项目的构建依赖。CI 的容器里有 node，开发机上不一定有 —— 让
// 它在没有 node 的机器上跳过而不是失败，比为了跑一个前端测试给整个项目加一
// 条工具链依赖更合理。
func nodeBinary(t *testing.T) string {
	t.Helper()

	// 优先用管理目录里的版本，其次是 PATH 上的。
	for _, candidate := range []string{
		os.Getenv("VMWARE_EXPORTER_TEST_NODE"),
		"node",
	} {
		if candidate == "" {
			continue
		}

		if path, err := exec.LookPath(candidate); err == nil {
			return path
		}
	}

	t.Skip("node is not available; skipping the config generator tests")

	return ""
}

// generateConfig 用 scripts/generate_config.js 跑一次 web/config.js 的生成
// 逻辑，返回生成的文本。
//
// which 是 sd（目标文件）、job（scrape_configs）或 flags（启动参数）。
// settings 里的键是页面控件的 id。
func generateConfig(t *testing.T, which string, settings map[string]any) string {
	t.Helper()

	node := nodeBinary(t)

	// 采集器清单从注册表来，不在测试里硬编码：新增采集器时这些测试跟着变，
	// 而写死一份清单会让它们在页面已经跟上之后仍然测的是旧状态。
	type collBox struct {
		Name           string `json:"name"`
		DefaultEnabled bool   `json:"defaultEnabled"`
		Checked        bool   `json:"checked"`
	}

	if _, ok := settings["__collectors"]; !ok {
		boxes := make([]collBox, 0, len(vmwareCollectors.Definitions()))

		for _, def := range vmwareCollectors.Definitions() {
			boxes = append(boxes, collBox{
				Name:           def.Name,
				DefaultEnabled: def.DefaultEnabled,
				// 页面加载时预勾选的正是默认开启的那些。
				Checked: def.DefaultEnabled,
			})
		}

		settings["__collectors"] = boxes
	}

	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("could not marshal settings: %v", err)
	}

	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")

	if err := os.WriteFile(settingsPath, body, 0o600); err != nil {
		t.Fatalf("could not write settings: %v", err)
	}

	cmd := exec.Command(node, "scripts/generate_config.js", which, settingsPath)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generate_config.js %s failed: %v\n%s", which, err, out)
	}

	return string(out)
}

// TestGeneratedTargetFileParsesAsFileSD 保证目标文件是合法的 file_sd 输入。
//
// file_sd 读的是一个 <static_config> 列表，每项有 targets 与可选的 labels。
// 用 yaml.v3 以严格模式解成这个形状，就能同时抓住三类错误：缩进写错（解析
// 失败）、键名写错（KnownFields 拒绝未知字段）、以及重复键 —— 最后一类是这
// 个生成器最容易犯的，因为 __param_<name> 是标签，同名标签只能有一个值，而
// collect[] 恰恰需要重复出现才能表达多个采集器。yaml.v3 对重复 map 键直接
// 报错，Prometheus 也一样，所以这条断言等价于「Prometheus 会不会拒绝加载」。
func TestGeneratedTargetFileParsesAsFileSD(t *testing.T) {
	// staticConfig 对应 Prometheus 的 <static_config>。
	type staticConfig struct {
		Targets []string          `yaml:"targets"`
		Labels  map[string]string `yaml:"labels"`
	}

	for _, tc := range []struct {
		name     string
		settings map[string]any
		// wantLabels 是每条记录都必须带上的标签。
		wantLabels []string
	}{
		{
			name: "relabel style carries credentials only",
			settings: map[string]any{
				"style":   "relabel",
				"targets": "vcenter-a.example.com,svc@vsphere.local,pw\nvcenter-b.example.com,svc@vsphere.local,pw",
			},
			wantLabels: []string{"__meta_username", "__meta_password"},
		},
		{
			name: "inline style carries the probe parameters",
			settings: map[string]any{
				"style":   "inline",
				"targets": "vcenter-a.example.com,svc@vsphere.local,pw\nvcenter-b.example.com,svc@vsphere.local,pw",
			},
			// instance 是这里的重点，见下面的专项测试。
			wantLabels: []string{"__param_target", "__param_username", "__param_password", "instance"},
		},
		{
			name: "all collectors selected",
			settings: map[string]any{
				"style":   "inline",
				"targets": "vcenter-a.example.com,svc@vsphere.local,pw",
				"__collectors": []map[string]any{
					{"name": "host", "defaultEnabled": true, "checked": true},
					{"name": "vm", "defaultEnabled": true, "checked": true},
				},
			},
			wantLabels: []string{"__param_target", "instance"},
		},
		{
			// 单目标模式下 targets 里填的仍然是 vCenter —— 页面的输入契约
			// 只有一个 —— 而生成器把抓取目标换成 exporter 的监听地址，并把
			// vCenter 写进标签。vcenter 标签是必须的：exporter 自己的指标里
			// 没有这个标签，Prometheus 侧不加就只剩形如 localhost:9170 的
			// instance 能区分两台 vCenter。
			name: "single target mode labels each exporter with its vCenter",
			settings: map[string]any{
				"mode":    "metrics",
				"targets": "vcenter-a.example.com\nvcenter-b.example.com",
			},
			wantLabels: []string{"vcenter"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := generateConfig(t, "sd", tc.settings)

			var groups []staticConfig

			dec := yaml.NewDecoder(strings.NewReader(out))
			// 严格模式。关掉的话拼错的 "label:"（少了 s）会被静默忽略，
			// 生成的文件解析通过、Prometheus 也接受，但标签一个都没生效。
			dec.KnownFields(true)

			if err := dec.Decode(&groups); err != nil {
				t.Fatalf("generated target file is not a valid file_sd document: %v\n%s", err, out)
			}

			if len(groups) == 0 {
				t.Fatalf("generated target file has no target groups:\n%s", out)
			}

			for i, g := range groups {
				if len(g.Targets) == 0 {
					t.Fatalf("target group %d has no targets:\n%s", i, out)
				}

				for _, want := range tc.wantLabels {
					if _, ok := g.Labels[want]; !ok {
						t.Fatalf("target group %d is missing the %s label; Prometheus would "+
							"send an incomplete request:\n%s", i, want, out)
					}
				}
			}
		})
	}
}

// TestGeneratedScrapeConfigParsesAndTargetsTheExporter 检查生成的
// scrape_configs 能被解析，且各字段指向正确的东西。
//
// 断言的重点是 relabel 那一串的完整性与顺序。这套规则里有一个不容易发现的
// 顺序依赖：把 __address__ 改成 exporter 的那条必须排在读 __address__ 的
// 两条之后，否则 __param_target 拿到的是 exporter 自己的地址 —— 每个 target
// 都会让 exporter 去探测自己，配置完全合法，抓取也返回 200，只是所有指标都
// 来自同一个错误目标。
func TestGeneratedScrapeConfigParsesAndTargetsTheExporter(t *testing.T) {
	// relabelConfig 只声明这个生成器会用到的字段。
	type relabelConfig struct {
		SourceLabels []string `yaml:"source_labels"`
		TargetLabel  string   `yaml:"target_label"`
		Replacement  string   `yaml:"replacement"`
	}

	type fileSDConfig struct {
		Files []string `yaml:"files"`
	}

	type scrapeConfig struct {
		JobName        string              `yaml:"job_name"`
		ScrapeInterval string              `yaml:"scrape_interval"`
		ScrapeTimeout  string              `yaml:"scrape_timeout"`
		MetricsPath    string              `yaml:"metrics_path"`
		FileSDConfigs  []fileSDConfig      `yaml:"file_sd_configs"`
		Params         map[string][]string `yaml:"params"`
		RelabelConfigs []relabelConfig     `yaml:"relabel_configs"`
	}

	type promConfig struct {
		ScrapeConfigs []scrapeConfig `yaml:"scrape_configs"`
	}

	decode := func(t *testing.T, out string) scrapeConfig {
		t.Helper()

		var cfg promConfig

		dec := yaml.NewDecoder(strings.NewReader(out))
		dec.KnownFields(true)

		if err := dec.Decode(&cfg); err != nil {
			t.Fatalf("generated scrape config does not parse: %v\n%s", err, out)
		}

		if len(cfg.ScrapeConfigs) != 1 {
			t.Fatalf("got %d scrape configs, want 1:\n%s", len(cfg.ScrapeConfigs), out)
		}

		return cfg.ScrapeConfigs[0]
	}

	t.Run("relabel style maps address to target and back to the exporter", func(t *testing.T) {
		out := generateConfig(t, "job", map[string]any{
			"style":    "relabel",
			"job":      "vmware-prod",
			"exporter": "exporter.example.com:9169",
			"sdPath":   "/etc/prometheus/targets/prod.yml",
			"targets":  "vcenter-a.example.com,svc@vsphere.local,pw",
		})

		job := decode(t, out)

		if job.JobName != "vmware-prod" {
			t.Fatalf("job_name = %q, want %q", job.JobName, "vmware-prod")
		}

		if job.MetricsPath != "/probe" {
			t.Fatalf("metrics_path = %q, want /probe; the multi target endpoint is the "+
				"only one that accepts a target parameter", job.MetricsPath)
		}

		if len(job.FileSDConfigs) != 1 || len(job.FileSDConfigs[0].Files) != 1 {
			t.Fatalf("expected exactly one file_sd file:\n%s", out)
		}

		if got := job.FileSDConfigs[0].Files[0]; got != "/etc/prometheus/targets/prod.yml" {
			t.Fatalf("file_sd file = %q, want the path from the form", got)
		}

		// Prometheus 只接受以这三个后缀结尾的目标文件，其它后缀会被静默
		// 忽略：没有报错，target 数量就是 0。
		if !strings.HasSuffix(job.FileSDConfigs[0].Files[0], ".yml") {
			t.Fatalf("file_sd file %q does not end in .yml/.yaml/.json; Prometheus "+
				"would ignore it without an error", job.FileSDConfigs[0].Files[0])
		}

		// 四条规则的顺序是这套配置能工作的前提。
		wantOrder := []struct {
			source string
			target string
		}{
			{source: "__meta_username", target: "__param_username"},
			{source: "__meta_password", target: "__param_password"},
			{source: "__address__", target: "__param_target"},
			{source: "__param_target", target: "instance"},
		}

		if len(job.RelabelConfigs) != len(wantOrder)+1 {
			t.Fatalf("got %d relabel rules, want %d (four mappings plus the address "+
				"rewrite):\n%s", len(job.RelabelConfigs), len(wantOrder)+1, out)
		}

		for i, want := range wantOrder {
			got := job.RelabelConfigs[i]

			if len(got.SourceLabels) != 1 || got.SourceLabels[0] != want.source {
				t.Fatalf("relabel rule %d: source_labels = %v, want [%s]",
					i, got.SourceLabels, want.source)
			}

			if got.TargetLabel != want.target {
				t.Fatalf("relabel rule %d: target_label = %q, want %q",
					i, got.TargetLabel, want.target)
			}
		}

		// 最后一条重写 __address__，且必须是最后一条。
		last := job.RelabelConfigs[len(job.RelabelConfigs)-1]

		if last.TargetLabel != "__address__" {
			t.Fatalf("the last relabel rule targets %q, want __address__; rewriting the "+
				"address earlier would make __param_target point at the exporter itself",
				last.TargetLabel)
		}

		if last.Replacement != "exporter.example.com:9169" {
			t.Fatalf("the address rewrite replaces with %q, want the exporter address",
				last.Replacement)
		}
	})

	t.Run("single target mode scrapes metrics without relabeling", func(t *testing.T) {
		out := generateConfig(t, "job", map[string]any{
			"mode":    "metrics",
			"targets": "exporter-a.example.com:9169",
		})

		job := decode(t, out)

		if job.MetricsPath != "/metrics" {
			t.Fatalf("metrics_path = %q, want /metrics", job.MetricsPath)
		}

		// 单目标模式没有参数要映射。多出来的 relabel 规则说明模式判断漏了
		// 一处，生成的配置会把 exporter 自身的地址当成 vCenter 传进去。
		if len(job.RelabelConfigs) != 0 {
			t.Fatalf("single target mode emitted %d relabel rules, want none:\n%s",
				len(job.RelabelConfigs), out)
		}

		if len(job.Params) != 0 {
			t.Fatalf("single target mode emitted params %v, want none; credentials and "+
				"collectors are start-up flags in this mode", job.Params)
		}
	})

	t.Run("several collectors become a params list", func(t *testing.T) {
		out := generateConfig(t, "job", map[string]any{
			"style":   "relabel",
			"targets": "vcenter-a.example.com,svc@vsphere.local,pw",
			"__collectors": []map[string]any{
				{"name": "host", "defaultEnabled": true, "checked": true},
				{"name": "vm", "defaultEnabled": true, "checked": true},
				{"name": "vsan", "defaultEnabled": false, "checked": false},
			},
		})

		job := decode(t, out)

		// 不是全选，所以应该逐个列出而不是 all。
		want := []string{"host", "vm"}

		got := job.Params["collect[]"]
		if len(got) != len(want) {
			t.Fatalf("collect[] = %v, want %v", got, want)
		}

		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("collect[] = %v, want %v", got, want)
			}
		}
	})

	t.Run("selecting every collector emits all", func(t *testing.T) {
		out := generateConfig(t, "job", map[string]any{
			"style":   "relabel",
			"targets": "vcenter-a.example.com,svc@vsphere.local,pw",
			"__collectors": []map[string]any{
				{"name": "host", "defaultEnabled": true, "checked": true},
				{"name": "vm", "defaultEnabled": true, "checked": true},
			},
		})

		job := decode(t, out)

		if got := job.Params["collect[]"]; len(got) != 1 || got[0] != "all" {
			t.Fatalf("collect[] = %v, want [all]; the exporter and the documentation "+
				"both use all for this case", got)
		}
	})
}

// TestInlineStyleAlwaysSetsInstance 锁住内联风格里 instance 标签的存在。
//
// 单独一条测试而不是并进上面的标签检查，是因为这是这套配置里唯一一个「配置
// 完全合法、抓取全部成功、数据却是错的」的失误。Prometheus 只在 instance
// 缺失时用 __address__ 兜底，而内联风格里 __address__ 是 exporter 自己 ——
// 于是整个 estate 的指标都带着同一个 instance，Grafana 里看到的是几十台
// vCenter 的数据在一条曲线上互相覆盖。promtool 检查不出来，抓取也不报错。
func TestInlineStyleAlwaysSetsInstance(t *testing.T) {
	type staticConfig struct {
		Targets []string          `yaml:"targets"`
		Labels  map[string]string `yaml:"labels"`
	}

	out := generateConfig(t, "sd", map[string]any{
		"style":    "inline",
		"exporter": "exporter.example.com:9169",
		"targets": "vcenter-a.example.com,svc@vsphere.local,pw\n" +
			"vcenter-b.example.com,svc@vsphere.local,pw\n" +
			"vcenter-c.example.com,svc@vsphere.local,pw",
	})

	var groups []staticConfig

	dec := yaml.NewDecoder(strings.NewReader(out))
	dec.KnownFields(true)

	if err := dec.Decode(&groups); err != nil {
		t.Fatalf("generated target file does not parse: %v\n%s", err, out)
	}

	if len(groups) != 3 {
		t.Fatalf("got %d target groups, want 3:\n%s", len(groups), out)
	}

	seen := make(map[string]bool, len(groups))

	for i, g := range groups {
		instance := g.Labels["instance"]

		if instance == "" {
			t.Fatalf("target group %d has no instance label. Prometheus would fall back "+
				"to __address__, which here is the exporter, so every vCenter would "+
				"report under the same instance and overwrite each other:\n%s", i, out)
		}

		// instance 必须是被探测的 vCenter，不是 exporter。
		if instance == "exporter.example.com:9169" {
			t.Fatalf("target group %d labels the series with the exporter address instead "+
				"of the vCenter it probes:\n%s", i, out)
		}

		if instance != g.Labels["__param_target"] {
			t.Fatalf("target group %d: instance = %q but __param_target = %q; the label "+
				"must name the vCenter actually being scraped",
				i, instance, g.Labels["__param_target"])
		}

		if seen[instance] {
			t.Fatalf("instance %q appears twice; the series of those two targets would "+
				"collide:\n%s", instance, out)
		}

		seen[instance] = true
	}
}

// TestSingleTargetModeGivesEachExporterItsOwnPort 检查单目标模式下生成的
// 目标文件与启动参数互相对得上。
//
// 为什么值得一条专项测试：单目标模式是「一个 exporter 进程绑一台 vCenter」，
// 所以 N 台 vCenter 就是 N 个进程，而这里有两个各自独立、症状完全不同的坑：
//
//  1. 所有进程共用一个 -http.address。第一个起得来，第二个以
//     "address already in use" 退出。这个还算好查。
//  2. 目标文件里放 vCenter 的地址而不是 exporter 的。配置合法、Prometheus
//     照单全收，然后去抓 vCenter 的 80 端口 —— 全部 target down，而报错
//     指向 vCenter，不指向这份配置。
//
// 两者都要求同一件事：目标文件的每个 target 必须等于某个进程的
// -http.address，且各不相同。所以这条测试同时解析两份输出再比对，而不是
// 分别检查各自「看起来是否合理」。
func TestSingleTargetModeGivesEachExporterItsOwnPort(t *testing.T) {
	type staticConfig struct {
		Targets []string          `yaml:"targets"`
		Labels  map[string]string `yaml:"labels"`
	}

	const vcenterA = "vcenter-a.example.com"
	const vcenterB = "vcenter-b.example.com"
	const vcenterC = "vcenter-c.example.com"

	settings := map[string]any{
		"mode":     "metrics",
		"exporter": "exporter.example.com:9169",
		"targets":  vcenterA + "\n" + vcenterB + "\n" + vcenterC,
	}

	sd := generateConfig(t, "sd", settings)
	flags := generateConfig(t, "flags", settings)

	var groups []staticConfig

	dec := yaml.NewDecoder(strings.NewReader(sd))
	dec.KnownFields(true)

	if err := dec.Decode(&groups); err != nil {
		t.Fatalf("generated target file does not parse: %v\n%s", err, sd)
	}

	if len(groups) != 3 {
		t.Fatalf("got %d target groups, want one per vCenter (3):\n%s", len(groups), sd)
	}

	// 目标文件侧：抓取地址互不相同，且带着自己的 vcenter 标签。
	addrs := make(map[string]string, len(groups))

	for i, g := range groups {
		if len(g.Targets) != 1 {
			t.Fatalf("target group %d has %d targets, want exactly 1: each exporter "+
				"process serves one vCenter, so grouping them loses the vcenter "+
				"label:\n%s", i, len(g.Targets), sd)
		}

		addr := g.Targets[0]

		if prev, dup := addrs[addr]; dup {
			t.Fatalf("scrape address %q is used by both %q and %q; two processes cannot "+
				"share a port, and their series would collide on instance:\n%s",
				addr, prev, g.Labels["vcenter"], sd)
		}

		vcenter := g.Labels["vcenter"]

		if vcenter == "" {
			t.Fatalf("target group %d (%s) has no vcenter label. This exporter emits no "+
				"vcenter label of its own, so nothing downstream could tell which "+
				"vCenter the series came from:\n%s", i, addr, sd)
		}

		// 抓取目标必须是 exporter，不是 vCenter。
		if strings.HasPrefix(addr, vcenter) {
			t.Fatalf("target group %d scrapes %q, which is the vCenter itself; Prometheus "+
				"would try to scrape port 80 on the vCenter instead of the "+
				"exporter:\n%s", i, addr, sd)
		}

		addrs[addr] = vcenter
	}

	// 启动参数侧：每个 -http.address 都要在目标文件里出现，配对的
	// -vmware.vcenter 也要是同一台。
	listen := regexp.MustCompile(`-http\.address=(\S+)`).FindAllStringSubmatch(flags, -1)
	probed := regexp.MustCompile(`-vmware\.vcenter=(\S+)`).FindAllStringSubmatch(flags, -1)

	if len(listen) != 3 || len(probed) != 3 {
		t.Fatalf("got %d -http.address and %d -vmware.vcenter flags, want 3 of each:\n%s",
			len(listen), len(probed), flags)
	}

	for i := range listen {
		addr := listen[i][1]
		vcenter := probed[i][1]

		want, ok := addrs[addr]
		if !ok {
			t.Fatalf("process %d listens on %q, which appears in no target group; "+
				"Prometheus would never scrape it:\n%s\n%s", i, addr, flags, sd)
		}

		if want != vcenter {
			t.Fatalf("process %d listens on %q and probes %q, but the target file labels "+
				"that address as %q; the vcenter label would name the wrong "+
				"vCenter:\n%s\n%s", i, addr, vcenter, want, flags, sd)
		}
	}
}

// TestGeneratorRedactsPasswordsByDefault 保证生成的配置默认不含明文口令。
//
// 为什么值得一条测试：这个页面的产物会被贴进工单、聊天和 wiki —— 那是它的
// 用途。默认吐出真实口令的话，一次「帮我看看这段配置对不对」就等于把 vCenter
// 凭证发进了一个长期留存的地方，而当事人不会意识到自己做了这件事。
//
// 三份输出都要检查：目标文件、scrape_configs 与启动参数。单目标模式下口令
// 出现在启动参数里，多目标模式下出现在目标文件里，只查一份会漏掉另一半。
func TestGeneratorRedactsPasswordsByDefault(t *testing.T) {
	const password = "S3cr3t-Passw0rd-Do-Not-Leak"

	for _, tc := range []struct {
		name     string
		settings map[string]any
	}{
		{
			name: "probe relabel style",
			settings: map[string]any{
				"style":   "relabel",
				"targets": "vcenter-a.example.com,svc@vsphere.local," + password,
			},
		},
		{
			name: "probe inline style",
			settings: map[string]any{
				"style":   "inline",
				"targets": "vcenter-a.example.com,svc@vsphere.local," + password,
			},
		},
		{
			name: "single target mode",
			settings: map[string]any{
				"mode":    "metrics",
				"targets": "vcenter-a.example.com,svc@vsphere.local," + password,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, which := range []string{"sd", "job", "flags"} {
				// 每次都用一份新的 settings：generateConfig 会往里写
				// __collectors，共用同一个 map 会让后续用例拿到上一轮
				// 的采集器状态。
				settings := make(map[string]any, len(tc.settings))
				for k, v := range tc.settings {
					settings[k] = v
				}

				out := generateConfig(t, which, settings)

				if strings.Contains(out, password) {
					t.Fatalf("%s output contains the plaintext password even though "+
						"redaction is on by default:\n%s", which, out)
				}
			}
		})
	}

	// 关掉脱敏之后必须真的写出口令 —— 否则这个开关是个装饰，而运维会拿着
	// 一份填着 <password> 的文件去排查「为什么认证失败」。
	t.Run("redaction can be turned off", func(t *testing.T) {
		out := generateConfig(t, "sd", map[string]any{
			"style":   "relabel",
			"redact":  false,
			"targets": "vcenter-a.example.com,svc@vsphere.local," + password,
		})

		if !strings.Contains(out, password) {
			t.Fatalf("with redaction off the password should be written out:\n%s", out)
		}
	})
}

// TestConfigPageUsesTheConfiguredListenAddress 保证页面上的 exporter 地址栏
// 默认值来自 -http.address。
//
// 这个默认值会被写进 relabel_configs 的 replacement。写错的后果是所有 target
// 都 down，而 Prometheus 不会有任何配置错误 —— 排查要从「target 页面全红」
// 一路查到 exporter 的监听端口，是那种花半小时才发现是端口号的问题。
//
// 同时锁住 ":9169" 这种省略主机的写法会被补成 localhost:9169：裸端口作为
// replacement 会让 Prometheus 拿到一个空主机名的地址。
func TestConfigPageUsesTheConfiguredListenAddress(t *testing.T) {
	for _, tc := range []struct {
		listen string
		want   string
	}{
		{listen: ":9169", want: "localhost:9169"},
		{listen: ":19169", want: "localhost:19169"},
		{listen: "exporter.example.com:9169", want: "exporter.example.com:9169"},
		{listen: "10.0.0.5:9169", want: "10.0.0.5:9169"},
	} {
		t.Run(tc.listen, func(t *testing.T) {
			if got := defaultScrapeAddr(tc.listen); got != tc.want {
				t.Fatalf("defaultScrapeAddr(%q) = %q, want %q", tc.listen, got, tc.want)
			}
		})
	}

	// 页面上真的要出现那个值。这一段覆盖的是模板到 Data 字段的连线：
	// pageData 填了字段但 config.html 忘了用它的话，上面的单元断言仍然
	// 会通过，而页面上是一个空输入框。
	t.Run("the rendered page carries it", func(t *testing.T) {
		data := pageData()
		data.DefaultListenAddr = "exporter.example.com:19169"

		page, err := ui.RenderConfig(data)
		if err != nil {
			t.Fatalf("could not render the config page: %v", err)
		}

		if !strings.Contains(string(page), "exporter.example.com:19169") {
			t.Fatalf("the rendered config page does not contain the listen address, so " +
				"the generated relabel rule would point somewhere else")
		}
	})
}
