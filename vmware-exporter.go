package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/prezhdarov/prometheus-exporter/collector"
	"github.com/prezhdarov/prometheus-exporter/config"
	"github.com/prezhdarov/prometheus-exporter/exporter"
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

`, exporterName)
	config.Usage(s)
}

func webConfig(listenAddress *string) *web.FlagConfig {
	listenAddresses := []string{*listenAddress}
	systemSocket := false
	configFile := ""

	return &web.FlagConfig{
		WebListenAddresses: &listenAddresses,
		WebSystemdSocket:   &systemSocket,
		WebConfigFile:      &configFile,
	}
}

// collectorConfig 定义 collector 配置
type collectorConfig struct {
	name           string
	creator        func(*slog.Logger) (collector.Collector, error)
	defaultEnabled bool
}

// vmwareCollector 包装 VMware collectors 以符合 prometheus.Collector 接口
type vmwareCollector struct {
	loginData         map[string]interface{}
	namespace         string
	logger            *slog.Logger
	clientAPI         collector.ClientAPI
	enabledCollectors map[string]bool
}

func newVMwareCollector(loginData map[string]interface{}, namespace string, logger *slog.Logger, enabledCollectors map[string]bool) (*vmwareCollector, error) {
	return &vmwareCollector{
		loginData:         loginData,
		namespace:         namespace,
		logger:            logger,
		clientAPI:         vmware.NewAPI(),
		enabledCollectors: enabledCollectors,
	}, nil
}

// Describe 实现 prometheus.Collector 接口
func (c *vmwareCollector) Describe(ch chan<- *prometheus.Desc) {
	// 动态 collector，不需要预先描述
}

// Collect 实现 prometheus.Collector 接口
func (c *vmwareCollector) Collect(ch chan<- prometheus.Metric) {
	params := make(map[string]string)

	// 定义所有可用的 collectors
	collectors := []collectorConfig{
		// 基础 collectors（默认启用）
		{"datacenter", vmwareCollectors.NewdatacenterCollector, true},
		{"cluster", vmwareCollectors.NewClusterCollector, true},
		{"datastore", vmwareCollectors.NewdatastoreCollector, true},
		{"host", vmwareCollectors.NewhostCollector, true},
		{"vm", vmwareCollectors.NewvmCollector, true},

		// ESXi CLI collectors（默认禁用）
		{"esxcli.host.nic", vmwareCollectors.NewesxcliHostNICCollector, false},
		{"esxcli.storage", vmwareCollectors.NewesxcliStorageListCCollector, false},
	}

	for _, col := range collectors {
		// 检查是否应该运行此 collector
		enabled, exists := c.enabledCollectors[col.name]
		if !exists {
			// 如果没有明确指定，使用默认设置
			enabled = col.defaultEnabled
		}

		if !enabled {
			c.logger.Debug("skipping disabled collector", "name", col.name)
			continue
		}

		c.logger.Debug("creating collector", "name", col.name)

		instance, err := col.creator(c.logger)
		if err != nil {
			c.logger.Error("failed to create collector", "collector", col.name, "error", err)
			continue
		}

		c.logger.Debug("updating collector", "name", col.name)
		if err := instance.Update(ch, c.namespace, c.clientAPI, c.loginData, params); err != nil {
			c.logger.Error("collector failed", "collector", col.name, "error", err)
			// 继续收集其他 collectors 的指标
		} else {
			c.logger.Debug("collector completed successfully", "name", col.name)
		}
	}
}

// parseCollectors 解析 collectors 参数
// 支持格式：
//
//	collect[]=datacenter&collect[]=host  (启用指定的)
//	collect[]=all                         (启用所有)
//	collect[]=all&nocollect[]=esxcli.host.nic  (启用所有，但禁用指定的)
func parseCollectors(params map[string][]string, logger *slog.Logger) map[string]bool {
	enabledCollectors := make(map[string]bool)

	// 获取 collect[] 参数
	collectParams := params["collect[]"]
	noCollectParams := params["nocollect[]"]

	// 如果没有指定任何参数，返回空 map（使用默认值）
	if len(collectParams) == 0 && len(noCollectParams) == 0 {
		return enabledCollectors
	}

	// 如果指定了 "all"，启用所有 collectors
	hasAll := false
	for _, c := range collectParams {
		if c == "all" {
			hasAll = true
			break
		}
	}

	if hasAll {
		// 启用所有 collectors
		enabledCollectors["datacenter"] = true
		enabledCollectors["cluster"] = true
		enabledCollectors["datastore"] = true
		enabledCollectors["host"] = true
		enabledCollectors["vm"] = true
		enabledCollectors["esxcli.host.nic"] = true
		enabledCollectors["esxcli.storage"] = true

		logger.Debug("enabled all collectors")
	} else if len(collectParams) > 0 {
		// 只启用指定的 collectors
		for _, c := range collectParams {
			enabledCollectors[c] = true
			logger.Debug("enabled collector", "name", c)
		}
	}

	// 处理 nocollect[] 参数（禁用指定的）
	for _, c := range noCollectParams {
		enabledCollectors[c] = false
		logger.Debug("disabled collector", "name", c)
	}

	return enabledCollectors
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
	enabledCollectors := parseCollectors(params, logger)

	collectorList := []string{}
	for name, enabled := range enabledCollectors {
		if enabled {
			collectorList = append(collectorList, name)
		}
	}

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

func main() {
	flag.CommandLine.SetOutput(os.Stdout)
	flag.Usage = usage
	config.Parse()

	logger := promslog.New(config.SetLogger(logFormat, logLevel))

	logger.Debug("disable exporter target is", fmt.Sprintf("%t", *disableExporterTarget), nil)

	vmware.Load(logger)
	vmwareCollectors.Load(logger)

	// /metrics 端点 - 使用全局 flag 配置的默认凭证（单 vCenter 模式）
	http.Handle("/metrics", exporter.CreateHandler(!*disableExporterMetrics, *disableExporterTarget, *maxRequests, namespace, logger))

	// /probe 端点 - 支持多 target 和独立凭证（多 vCenter 模式）
	http.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		probeHandler(w, r, logger)
	})

	// 首页
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`
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
			<ul>
				<li><code>datacenter</code> - vCenter and datacenter info (default: enabled)</li>
				<li><code>cluster</code> - Cluster information (default: enabled)</li>
				<li><code>datastore</code> - Datastore metrics (default: enabled)</li>
				<li><code>host</code> - ESXi host metrics (default: enabled)</li>
				<li><code>vm</code> - Virtual machine metrics (default: enabled)</li>
				<li><code>esxcli.host.nic</code> - ESXi NIC driver info (default: disabled)</li>
				<li><code>esxcli.storage</code> - ESXi storage info (default: disabled)</li>
			</ul>
			
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
			</html>`))
	})

	logger.Info("Starting "+exporterName, "listening_on", *listenAddress)

	server := &http.Server{}

	if err := web.ListenAndServe(server, webConfig(listenAddress), logger); err != nil {
		logger.Error(fmt.Sprintf("error: %s", err))
		os.Exit(1)
	}
}
