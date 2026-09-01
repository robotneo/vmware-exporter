package main

import (
	"context"
	"flag"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prezhdarov/vmware-exporter/internal/config"
	vmware "github.com/prezhdarov/vmware-exporter/vmware/api"
	vmwareCollectors "github.com/prezhdarov/vmware-exporter/vmware/collectors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	versioncollector "github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promslog"
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
		// -disable.exporter.target 时只输出 exporter 自身的指标，
		// 不去连 vCenter。
		if *disableExporterTarget {
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
			MaxConcurrency: *maxConcurrency,
		})
		if err != nil {
			logger.Error("could not create the collector set", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		serveScrape(w, r, cs, !*disableExporterMetrics, logger)
	}
}

// probeHandler 处理 probe 请求，支持多 target 和独立凭证。
func probeHandler(w http.ResponseWriter, r *http.Request, logger *slog.Logger) {
	params := r.URL.Query()

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
		MaxConcurrency: *maxConcurrency,
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

// collectorDescriptions 给首页文档提供人类可读的说明。
// 缺失的条目会退化为空说明，但 collector 本身仍会被列出 ——
// 保证新增 collector 时首页不会漏项，最差也只是少一句描述。
var collectorDescriptions = map[string]string{
	"datacenter":      "vCenter and datacenter info",
	"cluster":         "Cluster information",
	"datastore":       "Datastore metrics",
	"host":            "ESXi host metrics",
	"vm":              "Virtual machine metrics",
	"esxcli.host.nic": "ESXi NIC driver info",
	"esxcli.storage":  "ESXi storage info",
}

// collectorListHTML 从 collector 清单生成首页的可用 collector 列表。
//
// 此前这段 HTML 是手写的硬编码列表，是清单的第四处副本
// （另外三处：各 collector 的 init() 注册、Collect 的 slice、
// parseCollectors 里的 "all"）。新增 collector 时极易漏改文档，
// 导致首页宣称的可用项与实际不符。
func collectorListHTML() string {
	var b strings.Builder

	for _, def := range vmwareCollectors.Definitions() {
		state := "disabled"
		if def.DefaultEnabled {
			state = "enabled"
		}

		description := collectorDescriptions[def.Name]
		if description != "" {
			description = " - " + description
		}

		fmt.Fprintf(&b, "\n\t\t\t\t<li><code>%s</code>%s (default: %s)</li>",
			html.EscapeString(def.Name), html.EscapeString(description), state)
	}

	b.WriteString("\n\t\t\t")

	return b.String()
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
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte(`
			<head><title>` + exporterName + `</title></head>
			<body>
			<h1>` + exporterName + `</h1>
			<h2>Endpoints</h2>
			<ul>
				<li><a href="/metrics">Metrics</a> - Default mode with global credentials</li>
				<li><a href="/probe">Probe</a> - Multi-target mode with per-request credentials</li>
			</ul>
			
			<h2>Usage</h2>
			
			<h3>1. Default Mode (Single vCenter)</h3>
			<p>Start the exporter with flags:</p>
			<pre>
./vmware-exporter \
  -vmware.vcenter=vcenter.example.com \
  -vmware.username=admin@vsphere.local \
  -vmware.password=secret \
  -vmware.insecureTLS=true
			</pre>
			<p>Then scrape: <code>http://localhost:9169/metrics</code></p>

			<h4>Timing flags</h4>
			<ul>
				<li><b>-vmware.timeout</b> (default: 60) - overall timeout in seconds for a single
					scrape, covering login, property retrieval and performance sampling.
					Raise this for large inventories.</li>
				<li><b>-vmware.interval</b> (default: 20) - PerfManager sampling window in seconds.
					Does not affect the scrape timeout.</li>
				<li><b>-vmware.granularity</b> (default: 20) - sampling frequency in seconds.
					Must be greater than 0 and not larger than the interval.</li>
			</ul>
			
			<h3>2. Probe Mode (Multiple vCenters)</h3>
			
			<h4>Basic Parameters:</h4>
			<ul>
				<li><b>target</b> (required): vCenter server address</li>
				<li><b>username</b> (required): vCenter username</li>
				<li><b>password</b> (required): vCenter password</li>
				<li><b>schema</b> (optional): http or https (default: https)</li>
				<li><b>insecure</b> (optional): true to skip TLS verification</li>
			</ul>
			
			<h4>Collector Control Parameters:</h4>
			<ul>
				<li><b>collect[]</b>: Enable specific collectors (can be repeated)</li>
				<li><b>nocollect[]</b>: Disable specific collectors (can be repeated)</li>
			</ul>
			
			<h4>Available Collectors:</h4>
			<ul>` + collectorListHTML() + `</ul>
			
			<h4>Examples:</h4>
			<pre>
# Default collectors only
/probe?target=vcenter.example.com&username=admin&password=secret&insecure=true

# Only VM and host metrics
/probe?target=vcenter.example.com&username=admin&password=secret&insecure=true&collect[]=vm&collect[]=host

# All collectors
/probe?target=vcenter.example.com&username=admin&password=secret&insecure=true&collect[]=all

# All except ESXi CLI collectors
/probe?target=vcenter.example.com&username=admin&password=secret&insecure=true&collect[]=all&nocollect[]=esxcli.host.nic&nocollect[]=esxcli.storage

# Enable ESXi CLI collectors
/probe?target=vcenter.example.com&username=admin&password=secret&insecure=true&collect[]=esxcli.host.nic&collect[]=esxcli.storage
			</pre>
			
			<h3>Prometheus Configuration</h3>
			<h4>Different collectors for different vCenters:</h4>
			<pre>
scrape_configs:
  # Production vCenter - all collectors
  - job_name: 'vmware-prod'
    scrape_interval: 60s
    metrics_path: /probe
    file_sd_configs:
      - files: ['/etc/prometheus/targets/vmware_prod.yml']
    params:
      schema: ['https']
      insecure: ['true']
      collect[]: ['all']  # Enable all collectors
    relabel_configs:
      - source_labels: [__meta_username]
        target_label: __param_username
      - source_labels: [__meta_password]
        target_label: __param_password
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: localhost:9169

  # Dev vCenter - basic collectors only (faster)
  - job_name: 'vmware-dev'
    scrape_interval: 120s
    metrics_path: /probe
    file_sd_configs:
      - files: ['/etc/prometheus/targets/vmware_dev.yml']
    params:
      schema: ['https']
      insecure: ['true']
      collect[]: ['datacenter', 'host', 'vm']  # Only basic collectors
    relabel_configs:
      - source_labels: [__meta_username]
        target_label: __param_username
      - source_labels: [__meta_password]
        target_label: __param_password
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: localhost:9169
			</pre>
			</body>
			</html>`)); err != nil {
			logger.Error("failed to write index response", "error", err)
		}
	})

	logger.Info("Starting "+exporterName, "listening_on", *listenAddress)

	server := &http.Server{}

	if err := web.ListenAndServe(server, webConfig(listenAddress), logger); err != nil {
		logger.Error("listen and serve failed", "error", err)
		os.Exit(1)
	}
}
