package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	vmwareCollectors "github.com/prezhdarov/vmware-exporter/vmware/collectors"
	ui "github.com/prezhdarov/vmware-exporter/web"
	"gopkg.in/yaml.v3"
)

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
		{path: "/i18n.js", contentType: "text/javascript; charset=utf-8", wantBody: "setLang"},
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

	// 测试包在 cmd/vmware-exporter 下，而 generate_config.js 在仓库根的
	// scripts/ 里。go test 的工作目录固定是被测包目录，不能再依赖相对路径
	// "scripts/..." —— 那会被解析成 cmd/vmware-exporter/scripts/。
	// 从本测试源文件的位置反推出仓库根，让断言与包所在目录无关。
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	scriptPath := filepath.Join(repoRoot, "scripts", "generate_config.js")

	cmd := exec.Command(node, scriptPath, which, settingsPath)

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
