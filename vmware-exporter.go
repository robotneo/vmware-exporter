package main

import (
	"flag"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prezhdarov/prometheus-exporter/pkg/collector"
	"github.com/prezhdarov/prometheus-exporter/pkg/config"
	"github.com/prezhdarov/prometheus-exporter/pkg/exporter"
	vmware "github.com/prezhdarov/vmware-exporter/vmware/api"
	vmwareCollectors "github.com/prezhdarov/vmware-exporter/vmware/collectors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promslog"
	"github.com/prometheus/exporter-toolkit/web"
)

const (
	exporterName = "VMware vSphere Exporter"
	namespace    = "vmware"
)

var (
	listenAddress          = flag.String("http.address", ":9169", "Address and port to listen for http connections")
	maxRequests            = flag.Int("prom.maxRequests", 20, "Maximum number of parallel scrape requests. Use 0 to disable.")
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
	config.Usage(s)
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

// scrapeMetrics 是 /probe 路径的自监控指标描述符。
//
// 名称与标签必须与框架 /metrics 路径产出的完全一致
// （见 prometheus-exporter/pkg/collector/collector.go:84-96），
// 否则同一套 Prometheus 查询无法同时覆盖两个端点。
type scrapeMetrics struct {
	duration *prometheus.Desc
	success  *prometheus.Desc
}

func newScrapeMetrics(namespace string) scrapeMetrics {
	return scrapeMetrics{
		duration: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "scrape", "collector_duration_seconds"),
			"Duration of a collector scrape.",
			[]string{"collector"},
			nil,
		),
		success: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, "scrape", "collector_success"),
			"Whether a collector succeeded.",
			[]string{"collector"},
			nil,
		),
	}
}

// vmwareCollector 包装 VMware collectors 以符合 prometheus.Collector 接口
type vmwareCollector struct {
	loginData         map[string]interface{}
	namespace         string
	logger            *slog.Logger
	clientAPI         collector.ClientAPI
	enabledCollectors map[string]bool
	scrapeMetrics     scrapeMetrics
}

func newVMwareCollector(loginData map[string]interface{}, namespace string, logger *slog.Logger, enabledCollectors map[string]bool) (*vmwareCollector, error) {
	return &vmwareCollector{
		loginData:         loginData,
		namespace:         namespace,
		logger:            logger,
		clientAPI:         vmware.NewAPI(),
		enabledCollectors: enabledCollectors,
		scrapeMetrics:     newScrapeMetrics(namespace),
	}, nil
}

// Describe 实现 prometheus.Collector 接口。
//
// 业务指标的 Desc 在各 collector 内部按采集结果动态构造，无法预先枚举，
// 因此这里只描述两个固定的自监控指标。空实现会让 registry 把本 collector
// 视为「unchecked collector」，从而跳过重复注册检测。
func (c *vmwareCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.scrapeMetrics.duration
	ch <- c.scrapeMetrics.success
}

// isEnabled 判断某个 collector 本次是否应该运行。
// 未显式指定时回退到该 collector 的默认状态。
func (c *vmwareCollector) isEnabled(def vmwareCollectors.Definition) bool {
	if enabled, exists := c.enabledCollectors[def.Name]; exists {
		return enabled
	}

	return def.DefaultEnabled
}

// Collect 实现 prometheus.Collector 接口。
//
// 并发调度所有启用的 collector，并为每个 collector 产出
// _duration_seconds 与 _success 两个自监控指标。
//
// 这里刻意复刻框架 CollectorSet.Collect 的行为
// （prometheus-exporter/pkg/collector/collect.go:28-73），使 /probe 与
// /metrics 两条路径在并发性与可观测性上完全对齐。此前 /probe 是串行执行
// 且完全没有自监控指标，同一份告警规则在两个端点上表现不同。
//
// 注意：登录/登出由调用方 probeHandler 负责，不在此处，所以本函数只产出
// 各 collector 的耗时与 all_collectors 汇总，不产出 login/logout 计时。
func (c *vmwareCollector) Collect(ch chan<- prometheus.Metric) {
	begin := time.Now()

	params := make(map[string]string)

	wg := sync.WaitGroup{}

	for _, def := range vmwareCollectors.Definitions() {
		if !c.isEnabled(def) {
			c.logger.Debug("skipping disabled collector", "name", def.Name)
			continue
		}

		instance, err := def.Creator(c.logger.With("collector", def.Name))
		if err != nil {
			// 构造失败也要产出 success=0，否则这个 collector 在监控上
			// 表现为「静默消失」而非「失败」，无法告警。
			c.logger.Error("failed to create collector", "collector", def.Name, "error", err)
			ch <- prometheus.MustNewConstMetric(c.scrapeMetrics.success, prometheus.GaugeValue, 0, def.Name)
			continue
		}

		wg.Add(1)

		go func(name string, instance collector.Collector) {
			defer wg.Done()

			collectorBegin := time.Now()

			err := instance.Update(ch, c.namespace, c.clientAPI, c.loginData, params)

			duration := time.Since(collectorBegin)

			success := float64(1)
			if err != nil {
				success = 0
				c.logger.Error("collector failed", "collector", name, "duration_seconds", duration.Seconds(), "error", err)
			} else {
				c.logger.Debug("collector scraped successfully", "collector", name, "duration_seconds", duration.Seconds())
			}

			ch <- prometheus.MustNewConstMetric(c.scrapeMetrics.duration, prometheus.GaugeValue, duration.Seconds(), name)
			ch <- prometheus.MustNewConstMetric(c.scrapeMetrics.success, prometheus.GaugeValue, success, name)
		}(def.Name, instance)
	}

	wg.Wait()

	// 与框架保持一致的汇总计时，标签值同样用 "all_collectors"。
	ch <- prometheus.MustNewConstMetric(c.scrapeMetrics.duration, prometheus.GaugeValue, time.Since(begin).Seconds(), "all_collectors")
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

// probeHandler 处理 probe 请求，支持多 target 和独立凭证
func probeHandler(w http.ResponseWriter, r *http.Request, logger *slog.Logger) {
	params := r.URL.Query()

	// 获取 target 参数
	target := params.Get("target")
	if target == "" {
		http.Error(w, "target parameter is required", http.StatusBadRequest)
		logger.Error("probe request missing target parameter")
		return
	}

	// 获取认证参数（从 URL 参数或 HTTP headers）
	username := params.Get("username")
	password := params.Get("password")

	// 也支持从 Basic Auth 获取凭证
	if username == "" || password == "" {
		if user, pass, ok := r.BasicAuth(); ok {
			username = user
			password = pass
		}
	}

	// 验证凭证
	if username == "" || password == "" {
		http.Error(w, "username and password are required (via URL params or Basic Auth)", http.StatusBadRequest)
		logger.Error("probe request missing credentials", "target", target)
		return
	}

	// 获取可选参数
	schema := params.Get("schema")
	if schema == "" {
		schema = "https"
	}

	insecure := params.Get("insecure") == "true"

	// 解析 collectors 配置
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

	collectorList := []string{}
	for name, enabled := range enabledCollectors {
		if enabled {
			collectorList = append(collectorList, name)
		}
	}

	sort.Strings(collectorList)

	logger.Debug("probe request received",
		"target", target,
		"username", username,
		"schema", schema,
		"insecure", insecure,
		"collectors", strings.Join(collectorList, ","))

	// 创建凭证对象
	creds := vmware.Credentials{
		Target:   target,
		Username: username,
		Password: password,
		Schema:   schema,
		Insecure: insecure,
	}

	// 创建临时 registry
	registry := prometheus.NewRegistry()

	// 创建 VMware API 实例并登录
	vm := vmware.NewAPI()
	loginData, err := vm.LoginWithCredentials(creds, logger)
	if err != nil {
		logger.Error("failed to login to vCenter", "target", target, "error", err)
		http.Error(w, fmt.Sprintf("Login failed: %v", err), http.StatusUnauthorized)
		return
	}

	// 确保在函数结束时登出
	defer func() {
		if err := vm.Logout(loginData, logger); err != nil {
			logger.Error("failed to logout from vCenter", "target", target, "error", err)
		}
	}()

	logger.Info("successfully logged in to vCenter", "target", target)

	// 创建 VMware collector（带 collectors 配置）
	vmwareCol, err := newVMwareCollector(loginData, namespace, logger, enabledCollectors)
	if err != nil {
		logger.Error("failed to create vmware collector", "target", target, "error", err)
		http.Error(w, fmt.Sprintf("Failed to create collector: %v", err), http.StatusInternalServerError)
		return
	}

	// 注册 collector
	if err := registry.Register(vmwareCol); err != nil {
		logger.Error("failed to register vmware collector", "target", target, "error", err)
		http.Error(w, fmt.Sprintf("Failed to register collector: %v", err), http.StatusInternalServerError)
		return
	}

	logger.Debug("collector registered, starting metric collection", "target", target)

	// 收集并返回指标
	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	})

	h.ServeHTTP(w, r)

	logger.Info("probe request completed successfully", "target", target)
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
	config.Parse()

	logger := promslog.New(config.SetLogger(logFormat, logLevel))

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
	http.Handle("/metrics", exporter.CreateHandler(!*disableExporterMetrics, *disableExporterTarget, *maxRequests, namespace, logger))

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
