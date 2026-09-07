package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prezhdarov/vmware-exporter/internal/config"
	vmware "github.com/prezhdarov/vmware-exporter/vmware/api"
	vmwareCollectors "github.com/prezhdarov/vmware-exporter/vmware/collectors"
	// 本项目的 web 包与 exporter-toolkit 的 web 包同名，两者都要用，
	// 因此给自己的这个起别名。
	ui "github.com/prezhdarov/vmware-exporter/web"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	versioncollector "github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promslog"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
)

const (
	exporterName = "VMware vSphere Exporter"
	namespace    = "vmware"
)

var (
	listenAddress = flag.String("http.address", ":9169", "Address and port to listen for http connections")

	// maxConcurrency 取代了 -prom.maxRequests。
	//
	// 那个 flag 是个死参数：框架把它存进 eHandler.maxRequests 之后就再没
	// 读过（prometheus-exporter/pkg/exporter/exporter.go:20 与
	// handler.go:16），设成任何值都没有效果。
	//
	// 现在这个值真的限制并发。它同时约束两处：CollectorSet 层同时运行的
	// collector 数，以及 esxcli collector 内部按主机 fan-out 的宽度。
	// 后者是真正危险的那个 —— 改动前 500 台主机就是 500 个并发 SOAP 请求，
	// 每台主机的网卡再各起一个 goroutine，实测能到 2500 个并发请求同时打
	// 同一个 vCenter。
	//
	// 默认 8 是个保守值：足以让属性检索与性能采样重叠起来，又不至于让
	// vCenter 的连接池成为瓶颈。0 或负数表示不限制 collector 层，但
	// per-host fan-out 仍有内建下限（见 internal/collector.HostConcurrency）。
	maxConcurrency = flag.Int("collector.max-concurrency", 8,
		"Maximum number of collectors running in parallel, and the fan-out width used inside the esxcli collectors. Use 0 to leave the collector layer unlimited.")

	disableExporterTarget  = flag.Bool("disable.exporter.target", false, "Disable default target for /metrics path.")
	disableExporterMetrics = flag.Bool("disable.exporter.metrics", true, "Disable exporter metrics in /metrics path. Always enabled if /metrics target disabled")

	logLevel  = flag.String("log.level", "debug", "Log Level minimums. Available options are: debug,info,warn and error")
	logFormat = flag.String("log.format", "logfmt", "Log output format. Available options are: logfmt and json")

	// webConfigFile 交给 exporter-toolkit 处理 TLS 与 HTTP Basic Auth。
	//
	// 在此之前 WebConfigFile 被硬编码为空字符串，也就是说没有任何办法给
	// exporter 加上 TLS 或认证 —— 而 /probe 接受 URL 参数与 Basic Auth 形式的
	// vCenter 凭证，明文 HTTP 下这些凭证在网络上是裸奔的。
	//
	// 文件格式见 exporter-toolkit 的文档：
	// https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md
	webConfigFile = flag.String("web.config.file", "",
		"Path to a web configuration file enabling TLS and/or HTTP basic auth. See "+
			"https://github.com/prometheus/exporter-toolkit/blob/master/docs/web-configuration.md")

	// debugConsole 控制 /debug 是否提供。
	//
	// 默认启用，因为「先在浏览器里试一次再去写 Prometheus 配置」是这个
	// exporter 最常见的第一步：凭证是否够权限、哪些 collector 在这套环境里
	// 真的有数据，都要试过才知道。让它默认关闭等于把这一步藏起来。
	//
	// 保留关掉的开关是因为调试页会在一个没有认证的页面上摆出凭证输入框。
	// 这不会新增攻击面 —— /probe 本身就不带认证，凭证由请求方提供，页面
	// 存不存在都一样 —— 但面向公网或多租户的部署应该能把它收掉。
	debugConsole = flag.Bool("web.debug-console", true,
		"Serve the interactive debug console on /debug. Disable it for deployments where the exporter's HTTP interface is reachable by untrusted users.")
)

// exporterSettings 是一次请求用到的根包 flag 快照。
//
// 为什么需要快照而不是就地解引用：SIGHUP 重载通过 flag.FlagSet.Set 改写
// 这些 flag 指针，而 flag 包的 setter 是**裸写** —— 标准库 flag.go 里
// intValue.Set 的最后一行就是 `*i = intValue(v)`，没有任何同步原语。
// 与此同时这些 flag 全部在 HTTP 请求路径上被读（这正是「改 flag 值即可
// 热重载」成立的前提）。于是重载协程写、抓取协程读，构成数据竞争。
//
// 一次读完整组而不是逐处加锁，是为了让一次请求看到的是配置的**一致切片**。
// 分两次读的话，一次恰好落在中间的重载能让同一个 /metrics 请求既走
// 「target 未禁用」的分支，又用上重载后的并发上限 —— 那是两份配置的混合，
// 复现和排查都无从下手。
//
// 注意 -http.address / -web.config.file / -log.format 不在此列：它们在
// config.reloadExempt 里，重载永远不会写它们，读它们没有竞争。
type exporterSettings struct {
	maxConcurrency  int
	targetDisabled  bool
	metricsDisabled bool
	debugConsole    bool
}

func currentExporterSettings() exporterSettings {
	var s exporterSettings

	config.Snapshot(func() {
		s = exporterSettings{
			maxConcurrency:  *maxConcurrency,
			targetDisabled:  *disableExporterTarget,
			metricsDisabled: *disableExporterMetrics,
			debugConsole:    *debugConsole,
		}
	})

	return s
}

// currentMaxConcurrency 是只需要并发上限一个值时的窄口径快照。
//
// 单独留一个而不是让调用方去取整组，是因为 probeHandler 只用得上这一个：
// 它的 target、凭证与 collector 选择全部来自请求参数，不读 flag。
func currentMaxConcurrency() int {
	var v int

	config.Snapshot(func() {
		v = *maxConcurrency
	})

	return v
}

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

func usage() {
	s := fmt.Sprintf(`%s collects metrics data from VMware vCenter.

Two modes of operation:

1. Default Mode (single vCenter with global credentials):
   Set the following flags to scrape a single vCenter on /metrics endpoint:
   -vmware.vcenter, -vmware.username, -vmware.password

2. Probe Mode (multiple vCenters with per-target credentials):
   Use /probe endpoint with URL parameters or Basic Auth.
   Each probe request uses independent credentials.

Timing flags:
   -vmware.timeout      overall timeout for one scrape (login, properties, performance)
   -vmware.interval     PerfManager sampling window; independent from the timeout
   -vmware.granularity  sampling frequency; must be > 0 and <= interval

Available collectors: %s

`, exporterName, strings.Join(vmwareCollectors.Names(), ", "))

	// 改动前这里调框架的 config.Usage，它只在带 -h/-help 时才打印
	// flag 默认值，其他情况打一行「跑 -help 看说明」。既然自己实现，
	// 就直接把 flag 列表打全 —— usage 被调用时用户就是想看它。
	out := flag.CommandLine.Output()
	// 错误显式丢弃，理由同 internal/config.Usage：写 usage 输出失败时
	// 无处可报，报错得往同一个已经坏掉的流里写。
	_, _ = fmt.Fprintf(out, "%s\n", s)
	flag.PrintDefaults()
}

func webConfig(listenAddress *string) *web.FlagConfig {
	listenAddresses := []string{*listenAddress}
	systemSocket := false

	return &web.FlagConfig{
		WebListenAddresses: &listenAddresses,
		WebSystemdSocket:   &systemSocket,
		WebConfigFile:      webConfigFile,
	}
}

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

// collectorDescriptions 给页面提供人类可读的说明。
// 缺失的条目会退化为空说明，但 collector 本身仍会被列出 ——
// 保证新增 collector 时页面不会漏项，最差也只是少一句描述。
var collectorDescriptions = map[string]string{
	"datacenter":      "vCenter and datacenter info",
	"cluster":         "Cluster information",
	"datastore":       "Datastore metrics",
	"host":            "ESXi host metrics",
	"vm":              "Virtual machine metrics",
	"resourcepool":    "Resource pool limits, reservations and usage",
	"esxcli.host.nic": "ESXi NIC driver info",
	"esxcli.storage":  "ESXi storage info",
	"vsan":            "vSAN cluster health, capacity and resync",
	"vsan.perf":       "vSAN performance statistics (requires the vSAN performance service)",
}

// collectorCosts 标注哪些 collector 会显著拉长一次抓取，或有额外前置条件。
//
// 这一栏存在的理由是它决定了勾选的后果，而默认开关状态并不足以表达：
// esxcli 两个 collector 逐主机串行发 SOAP 调用，在几百台主机的环境里会把
// 一次抓取从几秒拖到几分钟；vsan 与 vsan.perf 在没有启用 vSAN（或没有开启
// 性能服务）的集群上只会白跑一轮往返。运维在页面上勾选之前就该看到这些，
// 而不是抓取超时之后再去翻源码里的注释。
//
// 文字与 vmware/collectors/registry.go 的注释同源。没有条目表示没有特别的
// 开销提示，页面会退回展示默认开关状态。
var collectorCosts = map[string]string{
	"esxcli.host.nic": "per-host serial",
	"esxcli.storage":  "per-host serial",
	"vsan":            "needs vSAN",
	"vsan.perf":       "needs perf service",
}

// collectorDocs 把 collector 清单转成页面需要的形状。
//
// 此前这个函数叫 collectorListHTML，直接拼一段 <li> 字符串。改成返回结构体
// 是因为现在有两个页面要用同一份数据、而且形式不同：概览页要一个只读列表，
// 调试页要一组复选框（还需要 DefaultEnabled 来决定预勾选）。让模板决定标签
// 长什么样，Go 这边只负责数据。
//
// 关键点不变：清单来自 vmwareCollectors.Definitions()，不是页面自己维护的
// 第二份副本。新增 collector 时页面自动跟上，由
// TestIndexPageListsEveryCollector 兜底。
func collectorDocs() []ui.Collector {
	defs := vmwareCollectors.Definitions()
	docs := make([]ui.Collector, 0, len(defs))

	for _, def := range defs {
		docs = append(docs, ui.Collector{
			Name:           def.Name,
			Description:    collectorDescriptions[def.Name],
			DefaultEnabled: def.DefaultEnabled,
			Cost:           collectorCosts[def.Name],
		})
	}

	return docs
}

// defaultScrapeAddr 把 -http.address 的值变成一个可以放进 Prometheus 配置的
// 地址。
//
// -http.address 的惯例写法是省略主机的 ":9169"，表示监听所有网卡。但
// relabel_configs 里的 replacement 要的是一个 host:port —— 直接把 ":9169"
// 写进去，Prometheus 会拿到一个空主机名的 target，抓取全部失败，而配置本身
// 通过校验，所以症状是「target 全 down」而不是一条错误。这里补上 localhost，
// 让生成的配置在单机部署下开箱可用；跨机部署的人本来就必须改成真实主机名，
// 页面上那一栏可编辑。
func defaultScrapeAddr(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "localhost" + listen
	}

	return listen
}

// pageData 组装三个页面共用的模板数据。
//
// 每次请求都会调用（落地页刻意不缓存，见 registerUI），所以它也在重载的
// 读侧上：-disable.exporter.target 与 -web.debug-console 都是可热改的
// flag。走 currentExporterSettings 一次性快照，理由同 metricsHandler。
//
// -http.address 例外：它在 config.reloadExempt 里，重载不会写它，
// 而且它在这里只是拿来渲染一段示例配置。
func pageData() ui.Data {
	// version.Info() 在没有 ldflags 的构建里返回带空字段的字符串，页面上
	// 显示成一串括号很难看。这种情况直接标 dev —— 本地 go build 出来的
	// 二进制正是这个状态。
	v := version.Version
	if v == "" {
		v = "dev"
	}

	cfg := currentExporterSettings()

	return ui.Data{
		ExporterName:          exporterName,
		Version:               v,
		Collectors:            collectorDocs(),
		DebugConsole:          cfg.debugConsole,
		MetricsTargetDisabled: cfg.targetDisabled,
		DefaultListenAddr:     defaultScrapeAddr(*listenAddress),
	}
}

// registerUI 把落地页、调试页、配置生成页与静态资源注册到 mux 上。
//
// 抽成函数而不是留在 main() 里，是为了让这几条路由可测。留在 main() 里时
// 它们注册在 http.DefaultServeMux 上，而 DefaultServeMux 是全局单例、
// 同一路径重复注册会 panic —— 测试无从构造一个干净的 mux 去断言
// 「-web.debug-console=false 时 /debug 返回 404」这类行为。
//
// enableDebugConsole 作为参数传入而不是在函数体里读 *debugConsole，同样是
// 为了可测：flag 的值是进程级的，测试要覆盖开与关两种情况就必须能分别传。
//
// 返回 error 而不是像原先那样在读不到嵌入资源时直接 os.Exit(1)：那一行
// 会把测试进程一起干掉。调用方（main）仍然按不可恢复处理。
func registerUI(mux *http.ServeMux, logger *slog.Logger, enableDebugConsole bool) error {
	// 落地页。
	//
	// 改动前这里是一段拼在 main() 里的 HTML 字符串常量，既没有 <!DOCTYPE>
	// 也没有 <html>/<body> 的开标签（只有闭标签）—— 浏览器靠容错解析显示。
	// 现在页面来自 web 包的模板，资源由 go:embed 编进二进制，部署方式不变。
	//
	// 每次请求重新渲染而不是启动时渲染一次并缓存：模板数据里的
	// -disable.exporter.target 是 SIGHUP 可重载的 flag，缓存会让页面在重载
	// 之后继续显示旧状态。渲染成本是几微秒的字符串拼接，落地页又不在抓取
	// 路径上，没有必要为此引入一处会过期的缓存。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// "/" 在 ServeMux 里是通配前缀，不加这一判断的话 /typo 与
		// /favicon.ico 都会拿到一整张落地页加 200。exporter-toolkit 自己的
		// LandingPageHandler 同样显式做这个判断。
		if r.URL.Path != "/" {
			http.NotFound(w, r)

			return
		}

		page, err := ui.RenderIndex(pageData())
		if err != nil {
			logger.Error("could not render the index page", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		if _, err := w.Write(page); err != nil {
			logger.Error("failed to write index response", "error", err)
		}
	})

	// /debug 用精确路径注册。/debug/pprof/ 是 Prometheus 生态约定的 profiling
	// 前缀（exporter-toolkit 的落地页默认就链向它），这个 exporter 目前没有
	// 引入 net/http/pprof，但不该把整个 /debug/ 子树占掉。
	if enableDebugConsole {
		mux.HandleFunc("/debug", func(w http.ResponseWriter, _ *http.Request) {
			page, err := ui.RenderDebug(pageData())
			if err != nil {
				logger.Error("could not render the debug page", "error", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)

				return
			}

			w.Header().Set("Content-Type", "text/html; charset=utf-8")

			if _, err := w.Write(page); err != nil {
				logger.Error("failed to write debug response", "error", err)
			}
		})

		// /config 生成 Prometheus 的 scrape_configs 与 file_sd 目标文件。
		//
		// 与 /debug 共用同一个开关，因为两者是同一类东西：面向人的交互页面，
		// 而不是抓取路径的一部分。把 exporter 收拢成一个纯粹的指标端点时，
		// 应该一起消失 —— 分成两个 flag 只会让「怎样才算全关」需要查文档。
		//
		// 生成完全在浏览器里完成，这个 handler 只发页面：服务端不需要知道
		// 用户填了哪些 vCenter，凭证也就没有任何理由离开浏览器。
		mux.HandleFunc("/config", func(w http.ResponseWriter, _ *http.Request) {
			page, err := ui.RenderConfig(pageData())
			if err != nil {
				logger.Error("could not render the config page", "error", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)

				return
			}

			w.Header().Set("Content-Type", "text/html; charset=utf-8")

			if _, err := w.Write(page); err != nil {
				logger.Error("failed to write config response", "error", err)
			}
		})
	}

	// 静态资源。三个页面都引用 app.css；app.js 只有调试页需要、config.js
	// 只有配置页需要，但都无条件提供 —— 它们不含任何配置或凭证，藏起来
	// 只会在 -web.debug-console=false 时留下一个 404 的引用。
	for _, asset := range []struct {
		path, contentType string
	}{
		{"app.css", "text/css; charset=utf-8"},
		{"app.js", "text/javascript; charset=utf-8"},
		{"config.js", "text/javascript; charset=utf-8"},
	} {
		body, err := ui.Asset(asset.path)
		if err != nil {
			// go:embed 的内容在编译期确定，读不到说明二进制自身有问题，
			// 不是运行期可恢复的状况。
			return fmt.Errorf("read embedded asset %s: %w", asset.path, err)
		}

		contentType := asset.contentType
		assetPath := asset.path

		mux.HandleFunc("/"+assetPath, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", contentType)
			// 资源随二进制走，同一个版本的内容永远一样；但版本升级后必须
			// 立刻换新，所以用 no-cache 让浏览器每次带条件请求，而不是
			// max-age 那种在升级后还会命中旧副本的做法。
			w.Header().Set("Cache-Control", "no-cache")

			if _, err := w.Write(body); err != nil {
				logger.Error("failed to write an asset response", "asset", assetPath, "error", err)
			}
		})
	}

	return nil
}

// handleReloadSignals 监听 SIGHUP 并重新加载配置，直到 ctx 被取消。
//
// 为什么需要它：在此之前进程没有任何信号处理器，SIGHUP 走 Go 的默认处置 ——
// **终止进程**。也就是说 `systemctl reload` 会静默杀掉 exporter（unit 里
// 一度写着 ExecReload=/bin/kill -HUP $MAINPID，实测把服务打挂），运维改完
// 配置只能 restart，抓取因此出现一个缺口。
//
// 为什么重载只需要改 flag 的值：这个 exporter 的配置几乎全部在请求路径上
// 解引用。每次抓取重新读各 -collector.* 开关与 -collector.max-concurrency，
// 每次登录重新读 -vmware.* 那一组。所以 config.Reload 写回 flag 指针之后，
// **下一轮抓取自然用上新配置** —— 不需要重建 handler，不需要重启监听，
// 正在进行中的抓取也不受影响（它们已经拿到了自己那一份值）。
//
// promslogLevel 单独传进来是因为 -log.level 走的不是 flag 指针那条路：logger
// 在启动时已经构造好，热改级别要写它内部的 slog.LevelVar。promslog.Level
// 正是 LevelVar 的包装，并发安全。
func handleReloadSignals(ctx context.Context, logger *slog.Logger, promslogLevel *promslog.Level) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			reloadConfig(logger, promslogLevel)
		}
	}
}

// reloadConfig 执行一次重载并更新自监控指标。
//
// 失败时保持旧配置不变（由 config.Reload 保证原子性），只把指标打成 0 并记
// 一条 error。一个正在正常抓取的 exporter 不该因为配置文件里打错一个字符就
// 降级 —— 但也不能让失败无声无息，那样运维会以为改动已经生效。
func reloadConfig(logger *slog.Logger, promslogLevel *promslog.Level) {
	logger.Info("received SIGHUP, reloading configuration")

	skipped, err := config.Reload()
	if err != nil {
		configLastReloadSuccess.Set(0)
		logger.Error("configuration reload failed, keeping the previous configuration", "error", err)

		return
	}

	// -log.level 的值此时已经被 Reload 写回 flag，但 logger 内部的 LevelVar
	// 还是旧的，要显式同步过去。放在 ValidateFlags 之前：级别本身非法会被
	// Set 拒绝，那属于配置错误，应该和其他重载失败一样处理。
	//
	// 走快照而不是裸读 *logLevel：Reload 的写锁在返回时就释放了，这里已经
	// 在锁外。目前只有一个信号协程调 reloadConfig，但 Reload 是导出函数、
	// 没有任何东西保证这一点 —— 两次并发重载时这个读会撞上另一次的写。
	var level string

	config.Snapshot(func() { level = *logLevel })

	if err := promslogLevel.Set(level); err != nil {
		configLastReloadSuccess.Set(0)
		logger.Error("configuration reload failed, keeping the previous configuration", "error", err)

		return
	}

	// 重载后的 vmware.* 组合仍然要过一遍启动时的那套校验。不校验的话
	// -vmware.granularity=0 会在下一轮抓取时引发除零 —— 启动路径专门有
	// fail-fast 拦这个，重载路径漏掉就等于给它开了个后门。
	//
	// 注意这里已经无法回滚了：ValidateFlags 读的是 flag 的当前值，而 Reload
	// 已经提交。所以非法组合会带着错误日志留在进程里。这是有意的取舍 ——
	// 让 Reload 去理解 vmware 包的跨 flag 约束会把两个包耦在一起，而这种
	// 组合错误在 restart 时同样会被 fail-fast 拦住，不会悄悄长期存在。
	if err := vmware.ValidateFlags(); err != nil {
		configLastReloadSuccess.Set(0)
		logger.Error("configuration reloaded but the vmware.* combination is invalid; scrapes may fail until this is corrected", "error", err)

		return
	}

	if len(skipped) > 0 {
		logger.Warn("some settings changed but cannot be applied without a restart",
			"flags", strings.Join(skipped, ","))
	}

	configLastReloadSuccess.Set(1)
	configLastReloadTime.SetToCurrentTime()

	logger.Info("configuration reload succeeded", "log_level", level)
}

func main() {
	flag.CommandLine.SetOutput(os.Stdout)
	flag.Usage = usage

	// Parse 现在返回 error 而不是在库里 log.Fatalf 掉进程。区别在于失败信息
	// 走的是同一个 logger、格式与其他启动错误一致，而且这条分支现在可测。
	if err := config.Parse(); err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %s\n", err)
		os.Exit(1)
	}

	// logger 还没建好，所以这里的错误只能往 stderr 写 —— 而错误本身正是
	// 「logger 参数不合法」。框架版本会静默降级成默认等级，见 config.SetLogger。
	promslogConfig, err := config.SetLogger(logFormat, logLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration error: %s\n", err)
		os.Exit(1)
	}
	logger := promslog.New(promslogConfig)

	// 重载指标的初始值。启动即成功：配置有问题的话上面几步已经 os.Exit 了，
	// 能走到这里说明当前生效的配置是好的。不初始化的话这两个指标会是 0，
	// 而「从没 reload 过」与「上次 reload 失败」是两件完全不同的事。
	configLastReloadSuccess.Set(1)
	configLastReloadTime.SetToCurrentTime()

	// fail-fast：非法的 vmware.* 参数组合会在运行期引发除零 panic 或让采样
	// 永远拿不到数据，必须在监听端口之前就拒绝启动。
	if err := vmware.ValidateFlags(); err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	logger.Debug("exporter target setting", "disabled", *disableExporterTarget)

	vmware.Load(logger)
	vmwareCollectors.Load(logger)

	// /metrics 端点 - 使用全局 flag 配置的默认凭证（单 vCenter 模式）
	//
	// 改动前这里是 exporter.CreateHandler(...)，由框架内部去查它自己的
	// collector 注册表。现在两条路径（/metrics 与 /probe）都走
	// internal/collector.CollectorSet，调度逻辑只有一份。
	http.Handle("/metrics", metricsHandler(logger))

	// /probe 端点 - 支持多 target 和独立凭证（多 vCenter 模式）
	http.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		probeHandler(w, r, logger)
	})
	if err := registerUI(http.DefaultServeMux, logger, *debugConsole); err != nil {
		logger.Error("could not register the web UI", "error", err)
		os.Exit(1)
	}

	// SIGHUP -> 重新读 -file 与环境变量。
	//
	// 位置有两个硬约束，它被夹在中间：
	//
	//  1. 必须在 ListenAndServe **之前** —— 那个调用会阻塞到进程退出，
	//     之后的代码不会被执行。
	//  2. 必须在上面那批启动期 flag 读取**之后**。这一条是本次修复补上的：
	//     启动路径上的 *disableExporterTarget 与 *debugConsole 是裸读，
	//     没有走 config.Snapshot（刻意如此 —— 它们只在启动时读一次，
	//     为此加锁是噪音）。但只要重载协程已经在跑，一个恰好在这个窗口
	//     到达的 SIGHUP 就能与它们撞上。窗口只有几微秒，正因为窄，
	//     真出问题时也永远复现不出来。把启动顺序调开是零成本的消除方式。
	//
	// ctx 的 cancel 在 main 返回时触发，让 goroutine 退出并解除信号注册。
	// 单进程的 exporter 里这在实践上无关紧要（进程紧接着就结束了），但把
	// goroutine 的生命周期与 main 绑起来是纪律问题 —— 泄漏的 signal.Notify
	// 在测试里会互相干扰。
	reloadCtx, stopReload := context.WithCancel(context.Background())
	defer stopReload()

	go handleReloadSignals(reloadCtx, logger, promslogConfig.Level)

	logger.Info("Starting "+exporterName, "listening_on", *listenAddress)

	server := &http.Server{}

	if err := web.ListenAndServe(server, webConfig(listenAddress), logger); err != nil {
		logger.Error("listen and serve failed", "error", err)
		os.Exit(1)
	}
}
