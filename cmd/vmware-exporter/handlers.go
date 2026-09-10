package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	vmware "github.com/prezhdarov/vmware-exporter/vmware/api"
	vmwareCollectors "github.com/prezhdarov/vmware-exporter/vmware/collectors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	versioncollector "github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// scrapeErrors 是 vmware_scrape_errors_total 的进程级累加状态。
//
// 必须在 handler 之外、进程生命周期内只有一份：CollectorSet 每请求构造一个
// 新实例，把计数放在实例里会让 counter 每轮归零（详见
// internal/collector/errors.go 顶部）。/metrics 与 /probe 共用同一份，
// 按 target 分桶互不干扰。
var scrapeErrors = collector.NewScrapeErrors()

// 配置重载的自监控指标。
//
// 有它们才能给「reload 失败」配告警。没有的话失败只留一行日志：systemctl
// reload 是成功的（信号发出去了），进程还活着，指标照常输出 —— 从外部完全
// 看不出配置没生效。运维会以为改动已经上线。
//
// 命名跟随 Prometheus 生态的既有惯例（prometheus 自身用
// prometheus_config_last_reload_successful），前缀换成本 exporter 的
// vmware_exporter_，与 vmware_exporter_build_info 一致。
//
// 这两个指标注册在默认 registry 上，而不是 newRegistry 里每请求构造的那个：
// 重载状态是进程级的，且必须在 /metrics 与 /probe 两个端点上都可见。
// 这两个指标不能用 promauto 注册到默认 registry：newRegistry 每次请求都构造
// 一个全新的 prometheus.Registry，默认 registry 的内容在 /metrics 与 /probe
// 上都看不到。所以这里只构造 Gauge，由 newRegistry 显式注册。
var (
	configLastReloadSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vmware_exporter_config_last_reload_successful",
		Help: "Whether the last configuration reload attempt was successful (1) or failed (0). Starts at 1 because a failed startup exits instead of serving.",
	})

	configLastReloadTime = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vmware_exporter_config_last_reload_success_timestamp_seconds",
		Help: "Unix timestamp of the last successful configuration reload, or of process start if no reload has happened yet.",
	})
)

// parseCollectors 解析 collectors 参数
// 支持格式：
//
//	collect[]=datacenter&collect[]=host  (启用指定的)
//	collect[]=all                         (启用所有)
//	collect[]=all&nocollect[]=esxcli.host.nic  (启用所有，但禁用指定的)
//
// 返回的 map 只包含被显式指定的 collector；未出现的沿用各自的默认状态，
// 由 vmwareCollector.isEnabled 兜底。
//
// 第二个返回值是无法识别的名字，供调用方记日志。拼错的名字此前会被静默
// 忽略 —— 比如 collect[]=vms（多个 s）会导致所有默认 collector 都不跑却
// 毫无提示，返回一份空指标集，排查起来非常费时。
func parseCollectors(params map[string][]string, logger *slog.Logger) (map[string]bool, []string) {
	enabledCollectors := make(map[string]bool)

	collectParams := params["collect[]"]
	noCollectParams := params["nocollect[]"]

	// 未指定任何参数：返回空 map，全部走默认值。
	if len(collectParams) == 0 && len(noCollectParams) == 0 {
		return enabledCollectors, nil
	}

	// 已知的 collector 名，来自 vmware/collectors 的单一清单。
	known := make(map[string]bool)
	for _, name := range vmwareCollectors.Names() {
		known[name] = true
	}

	var unknown []string

	hasAll := false
	for _, c := range collectParams {
		if c == "all" {
			hasAll = true
			break
		}
	}

	if hasAll {
		// 启用全部，清单从 vmware/collectors 取，不再硬编码。
		for _, name := range vmwareCollectors.Names() {
			enabledCollectors[name] = true
		}

		logger.Debug("enabled all collectors", "count", len(enabledCollectors))
	} else if len(collectParams) > 0 {
		// collect[] 一旦显式给出，就意味着「只跑这些」：先把所有 collector
		// 置为 false，再逐个打开。否则默认启用的 collector 会一起跑，
		// collect[]=vm 的语义会变成「vm 加上全部默认项」。
		for _, name := range vmwareCollectors.Names() {
			enabledCollectors[name] = false
		}

		for _, c := range collectParams {
			if !known[c] {
				unknown = append(unknown, c)
				continue
			}

			enabledCollectors[c] = true
			logger.Debug("enabled collector", "name", c)
		}
	}

	// nocollect[] 在 collect[] 之后处理，因此可以从 all 里剔除。
	for _, c := range noCollectParams {
		if !known[c] {
			unknown = append(unknown, c)
			continue
		}

		enabledCollectors[c] = false
		logger.Debug("disabled collector", "name", c)
	}

	return enabledCollectors, unknown
}

// newRegistry 组装一次抓取用的 registry。
//
// /metrics 与 /probe 走的是同一个函数，这是本次重构的要点之一：改动前
// /metrics 用框架的 exporter.CreateHandler，/probe 用根包手写的
// vmwareCollector，两份实现的并发行为与自监控指标各不相同，同一份告警规则
// 在两个端点上表现不一样。
func newRegistry(cs *collector.CollectorSet, includeExporterMetrics bool) (*prometheus.Registry, error) {
	registry := prometheus.NewRegistry()

	// build_info 指标。exporter 的版本信息本身就是运维要查的东西
	// （「这台还没升级？」），两个端点都应该有。
	registry.MustRegister(versioncollector.NewCollector(fmt.Sprintf("%s_exporter", namespace)))

	// 重载状态两个端点都要有：配置重载是进程级事件，用 /probe 的部署同样
	// 需要给它配告警。
	registry.MustRegister(configLastReloadSuccess, configLastReloadTime)

	if includeExporterMetrics {
		registry.MustRegister(
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
			collectors.NewGoCollector(),
		)
	}

	if err := registry.Register(cs); err != nil {
		return nil, fmt.Errorf("could not register the %s collector: %w", namespace, err)
	}

	return registry, nil
}

// serveScrape 跑一轮抓取并把结果写进响应。
func serveScrape(w http.ResponseWriter, r *http.Request, cs *collector.CollectorSet,
	includeExporterMetrics bool, logger *slog.Logger) {

	registry, err := newRegistry(cs, includeExporterMetrics)
	if err != nil {
		logger.Error("could not build the metrics registry", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		ErrorLog:      slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ErrorHandling: promhttp.ContinueOnError,
	}).ServeHTTP(w, r)
}

// metricsHandler 服务 /metrics：单 vCenter 模式，凭证来自全局 flag。
func metricsHandler(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 三个 flag 一次快照。分开读会让一次恰好落在中间的 SIGHUP 重载
		// 把同一个请求切成两半：前半用旧的 target 开关，后半用新的并发
		// 上限。详见 exporterSettings 的注释。
		cfg := currentExporterSettings()

		// -disable.exporter.target 时只输出 exporter 自身的指标，
		// 不去连 vCenter。
		if cfg.targetDisabled {
			promhttp.Handler().ServeHTTP(w, r)
			return
		}

		// ctx 派生自请求：客户端断连或 Prometheus 抓取超时会真正取消上游的
		// vCenter 调用。改动前这条路径上的 context 由 api 层用
		// context.Background() 独立派生，请求侧的取消传不进来。
		cs, err := collector.NewCollectorSet(r.Context(), vmwareCollectors.Definitions(), collector.Options{
			Namespace:      namespace,
			Target:         "", // 空表示用 -vmware.vcenter
			Login:          vmware.NewAPI(),
			Logger:         logger,
			Enabled:        collector.Registered(),
			MaxConcurrency: cfg.maxConcurrency,
			Errors:         scrapeErrors,
		})
		if err != nil {
			logger.Error("could not create the collector set", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		serveScrape(w, r, cs, !cfg.metricsDisabled, logger)
	}
}

// probeHandler 处理 probe 请求，支持多 target 和独立凭证。
func probeHandler(w http.ResponseWriter, r *http.Request, logger *slog.Logger) {
	// ParseForm 让 GET 的查询串与 POST 的表单体走同一套取值逻辑。调试页
	// 用 POST 提交，凭证因此留在请求体里，不进 URL、不进浏览器历史、也不进
	// 任何记录查询串的反向代理日志。
	//
	// 必须无条件调用，不能写成 if r.Method == http.MethodPost。实测：只在
	// POST 分支调用时，GET 请求的 r.Form 是一个空 map，现有的
	// /probe?target=... 会全部拿不到参数 —— Prometheus 那一侧会直接失效。
	//
	// error 只记日志、不中断，是为了保持与 r.URL.Query() 相同的宽容语义：
	// 那个函数遇到畸形百分号转义时静默丢弃该键，其余键照常返回；ParseForm
	// 则返回 error，但同样会把能解析的键填进 r.Form。中断请求会把「某个
	// 参数写错了」从「那个参数为空，于是报 400 缺少 target」变成「整个请求
	// 400」，对既有调用方是行为变更。
	if err := r.ParseForm(); err != nil {
		logger.Warn("could not fully parse probe request parameters; continuing with what was parsed",
			"error", err)
	}

	params := r.Form

	target := params.Get("target")
	if target == "" {
		http.Error(w, "target parameter is required", http.StatusBadRequest)
		logger.Error("probe request missing target parameter")
		return
	}

	// 认证参数可来自 URL 参数或 HTTP Basic Auth。
	username := params.Get("username")
	password := params.Get("password")

	if username == "" || password == "" {
		if user, pass, ok := r.BasicAuth(); ok {
			username = user
			password = pass
		}
	}

	if username == "" || password == "" {
		http.Error(w, "username and password are required (via URL params or Basic Auth)", http.StatusBadRequest)
		logger.Error("probe request missing credentials", "target", target)
		return
	}

	schema := params.Get("schema")
	if schema == "" {
		schema = "https"
	}

	insecure := params.Get("insecure") == "true"

	enabledCollectors, unknownCollectors := parseCollectors(params, logger)

	// 拼错的 collector 名此前被静默忽略，会得到一份空指标集且毫无提示。
	// 这里拒绝请求并把已知名列出来，让调用方立刻能改对。
	if len(unknownCollectors) > 0 {
		msg := fmt.Sprintf("unknown collector(s): %s. Available: %s",
			strings.Join(unknownCollectors, ", "),
			strings.Join(vmwareCollectors.Names(), ", "))

		http.Error(w, msg, http.StatusBadRequest)
		logger.Error("probe request specified unknown collectors",
			"target", target, "unknown", strings.Join(unknownCollectors, ","))

		return
	}

	// 凭证与 target 走 probeLogin，它把 Credentials 绑进 collector.Login 接口。
	cs, err := collector.NewCollectorSet(r.Context(), vmwareCollectors.Definitions(), collector.Options{
		Namespace: namespace,
		Target:    target,
		Login: &probeLogin{
			api: vmware.NewAPI(),
			creds: vmware.Credentials{
				Target:   target,
				Username: username,
				Password: password,
				Schema:   schema,
				Insecure: insecure,
			},
			logger: logger,
		},
		Logger:         logger,
		Enabled:        enabledCollectors,
		MaxConcurrency: currentMaxConcurrency(),
		Errors:         scrapeErrors,
	})
	if err != nil {
		logger.Error("could not create the collector set", "target", target, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	logger.Debug("probe request received",
		"target", target,
		"schema", schema,
		"insecure", insecure,
		"collectors", strings.Join(cs.Names(), ","))

	// 登录失败不再返回 401。改动前 probeHandler 先登录、失败就 http.Error，
	// 于是 Prometheus 收到一个 HTTP 错误、拿不到任何指标 —— 无法区分
	// 「vCenter 拒绝了凭证」和「exporter 自己挂了」。现在登录发生在
	// CollectorSet.Collect 内部，失败会产出 vmware_up 0 加上每个 collector 的
	// success 0，凭证错误于是变成一条可告警的时间序列。
	serveScrape(w, r, cs, false, logger)
}

// probeLogin 把一组显式凭证绑进 collector.Login 接口。
//
// 需要这个适配器是因为 Login(ctx, target) 的签名里没有凭证位置 ——
// /metrics 用的是全局 flag，凭证不必传；/probe 的凭证每个请求都不同。
// 把它们捕获在结构体里，两条路径就能共用同一个 CollectorSet。
type probeLogin struct {
	api    *vmware.VMware
	creds  vmware.Credentials
	logger *slog.Logger
}

func (p *probeLogin) Login(ctx context.Context, _ string) (*collector.Scrape, func(), error) {
	return p.api.LoginWithCredentials(ctx, p.creds, p.logger)
}
