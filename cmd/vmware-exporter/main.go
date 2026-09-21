package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prezhdarov/vmware-exporter/internal/config"
	vmware "github.com/prezhdarov/vmware-exporter/vmware/api"
	vmwareCollectors "github.com/prezhdarov/vmware-exporter/vmware/collectors"

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

	logLevel  = flag.String("log.level", "info", "Log Level minimums. Available options are: debug,info,warn and error")
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

	// enablePprof 控制 /debug/pprof/ 运行时剖析端点。
	//
	// 与 -web.debug-console 的关键差别是默认值：调试页是无害的只读页面、
	// 默认开；profiling 端点会暴露全部 goroutine 栈并能驱动 CPU/trace 采样、
	// 产生真实开销，默认必须关。只在排查 CPU/内存问题时临时打开
	// （启动加该 flag），抓到 profile 后关掉重启。详见 diagnostics.go。
	enablePprof = flag.Bool("web.enable-pprof", false,
		"Expose Go runtime profiling endpoints on /debug/pprof/ (CPU, heap, goroutine, trace). Disabled by default; enable temporarily to diagnose high CPU or memory usage, then disable again.")

	// scrapeInflight 限制同时进行的抓取数。
	//
	// -collector.max-concurrency 限的是**单次抓取内部**同时跑多少个
	// collector / 主机，但它管不到「同时有多少个抓取在跑」。多 vCenter 的
	// /probe 部署里，Prometheus 抓取间隔短于单轮耗时（或有人并发戳 /probe）
	// 时，N 个抓取会叠加成 N 次登录、N 个 vCenter 会话与 N×并发宽度的 SOAP
	// 请求，足以把 vCenter 的会话表打满。这个闸把叠加的总高度钉住。
	//
	// 拿不到令牌时直接返回 503 而不是排队：排队只会让请求堆积到客户端侧
	// 自己超时，503 让 Prometheus 把这一轮明确记为失败，语义与"目标此刻
	// 太忙"一致。0 表示不限。
	maxScrapeInflight = flag.Int("web.max-scrape-inflight", 4,
		"Maximum number of scrapes (/metrics and /probe) running at the same time. Requests over the limit get HTTP 503. 0 disables the limit.")

	// probeAllowedTargets 是 /probe target 的可选白名单。
	//
	// /probe 接受任意 target，默认无鉴权时任何人都能让 exporter 向任意地址
	// 发起 HTTPS 连接（SSRF / 内网探测面）。默认留空 = 保持原有行为（放开），
	// 不破坏既有部署；需要收紧时填一个逗号分隔的匹配规则列表，逐条匹配
	// target 的 host 部分：
	//
	//   - 以 "." 开头（如 ".example.com"）：匹配该后缀，含其自身与所有子域
	//   - 含 "/"（如 "10.0.0.0/8"）：按 CIDR 网段匹配（IP 型 target）
	//   - 其余（如 "vc.corp" 或 "10.1.2.3"）：精确相等
	probeAllowedTargets = flag.String("probe.allowed-targets", "",
		"Comma-separated allowlist for the /probe target host: suffixes starting with '.', CIDRs containing '/', or exact host/IP matches. Empty (default) allows any target.")

	// probeDenyQueryCredentials 是 S-07 的凭证收敛开关（默认 false 保持兼容）。
	//
	// /probe 长期接受把 username/password 放进 URL 查询串
	// （/probe?target=...&username=u&password=p），Prometheus 的标准抓取配置
	// 也常用这种形态。代价是凭证会出现在：exporter/反代的访问日志、
	// Referer 头、浏览器历史、APM 链路追踪里。置 true 后，凡 URL 查询串带
	// username/password 一律 400，凭证只接受 POST 表单体或 HTTP Basic Auth。
	//
	// 判定针对查询串本身（r.URL.Query），与 HTTP 方法无关 —— 所以"POST 却
	// 把凭证写在 URL 里、body 放别的字段"这种绕过也一并挡掉。该 flag 在请求
	// 路径上经快照读取，随 SIGHUP 热重载。
	probeDenyQueryCredentials = flag.Bool("probe.deny-query-credentials", false,
		"Reject (400) any /probe request whose URL query string carries username/password. When enabled, credentials are accepted only from the POST form body or HTTP Basic Auth, keeping them out of access logs, Referer headers and browser history. Default false preserves the legacy GET-with-credentials behaviour.")

	// inventoryCacheTTL 控制进程级清单缓存的有效期。
	//
	// 缓存只覆盖慢变的拓扑/容量面（datacenter、folder、cluster、compute
	// resource、datastore、resourcepool、vSAN 集群名发现），host/vm 的运行态
	// 与全部 perf 计数器始终实时。默认 5m 与 telegraf inputs.vsphere 的
	// object_discovery_interval=300s 同量级。
	//
	// 安全边界：缓存实例只注入 /metrics（单一服务级凭证）。/probe 是多租户
	// 路径，每个请求一组不同凭证，共享缓存会让低权限凭证读到高权限凭证留下
	// 的对象清单（越权读），因此 /probe 永远拿到 nil 缓存、每轮实时检索。
	//
	// 设为 0 关闭缓存，回到每轮全量 ContainerView 检索（v0.1.19 及更早行为）。
	inventoryCacheTTL = flag.Duration("scrape.inventory-cache-ttl", 5*time.Minute,
		"How long inventory/topology lookups (datacenter, folder, cluster, datastore, resource pool) are reused on the /metrics path. Host/VM runtime state and all perf counters stay real-time. 0 disables the cache. The cache is never used on the multi-tenant /probe path.")
)

// inventoryCache 是 /metrics 路径共享的进程级清单缓存单例。
//
// 必须在 handler 之外只有一份：每请求构造的 CollectorSet 若各自持有缓存，
// 命中率永远是 0。它不持有任何连接或会话，只存上一次（已登出）会话拉回的
// 纯数据 —— MoRef 在同一 vCenter 内跨会话稳定，复用安全。
//
// 刻意不传给 probeHandler：见 inventoryCacheTTL 的安全说明。
var inventoryCache = collector.NewInventoryCache()

// counterCache 是 /metrics 路径共享的进程级 PerfCounterInfo 缓存单例（P-03）。
// TTL 不在构造时固定，而由 -scrape.counter-cache-ttl 的配置快照在每次登录
// 传入，因此 SIGHUP 改值立即生效。刻意不传给 probeHandler。
var counterCache = vmware.NewCounterCache()

// providerSummaryCache 是 /metrics 路径共享的进程级采样间隔协商缓存单例
// （QueryPerfProviderSummary 按 target+About 版本/build+entity.Type 缓存）。
// TTL 由 -scrape.perf-interval-cache-ttl 的配置快照在每次登录闭包里传入，
// SIGHUP 改值立即生效。刻意不传给 probeHandler。
var providerSummaryCache = vmware.NewProviderSummaryCache()

// HTTP server 超时。不设 WriteTimeout：一次抓取可能跑满 -vmware.timeout
// （默认 60s），WriteTimeout 会从读完请求头开始计时并掐断正常的慢响应；
// 抓取时长本就由 -vmware.timeout 与请求 context 兜住。
const (
	readHeaderTimeout = 5 * time.Second
	idleTimeout       = 60 * time.Second
)

// newHTTPServer 构造带基本抗慢连接超时的 *http.Server。
//
// exporter-toolkit 的 web.ListenAndServe 不会替我们设任何超时（v0.16 的
// 实现里 grep 不到 ReadHeaderTimeout/IdleTimeout），裸 http.Server{} 的
// 这些字段全是 0（=无限）。于是一个只连上、慢慢发 header 的客户端
// （Slowloris）就能长期占住一个连接与一个 goroutine，几乎零成本耗尽 fd。
// 抽成函数是为了让"超时确实被设置"可测。
func newHTTPServer() *http.Server {
	return &http.Server{
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
}

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

	// 专用的 ServeMux，而不是 http.DefaultServeMux。
	//
	// 为什么不能用 DefaultServeMux：import net/http/pprof 会在它的 init() 里
	// 把 /debug/pprof/ 系列以 Go 1.22+ 的「带方法」模式（GET /debug/pprof/…）
	// 注册进 DefaultServeMux；而 registerDiagnostics 又要在同一个 mux 上按
	// flag 显式注册不带方法的同名路径 —— 两者模式冲突，ServeMux 在启动时
	// 直接 panic。换一个全新的、pprof init 碰不到的 mux 后，剖析端点完全由
	// -web.enable-pprof 决定，默认保持真正关闭。
	mux := http.NewServeMux()

	// /metrics 端点 - 使用全局 flag 配置的默认凭证（单 vCenter 模式）
	//
	// 改动前这里是 exporter.CreateHandler(...)，由框架内部去查它自己的
	// collector 注册表。现在两条路径（/metrics 与 /probe）都走
	// internal/collector.CollectorSet，调度逻辑只有一份。
	mux.Handle("/metrics", metricsHandler(logger))

	// /probe 端点 - 支持多 target 和独立凭证（多 vCenter 模式）
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		probeHandler(w, r, logger)
	})
	if err := registerUI(mux, logger, *debugConsole); err != nil {
		logger.Error("could not register the web UI", "error", err)
		os.Exit(1)
	}

	// 运行时剖析端点（默认关闭）。启动期裸读 *enablePprof，与上面的
	// *debugConsole 同属「只在启动读一次」的刻意用法；reload goroutine 在
	// 这之后才启动（见下方 handleReloadSignals），故不构成竞争窗口。
	registerDiagnostics(mux, *enablePprof)

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

	// 无鉴权暴露告警（S-08）。监听非回环接口、又没配 web.config.file（TLS/Basic
	// Auth）时，/probe 接收的 vCenter 凭证在网络上是明文，且任何能连到该端口
	// 的人都能驱动 exporter 去抓 vCenter。这只是显著提示、不阻止启动 —— 内网
	// 受控环境 + 前置反代是常见合法形态。
	if listenExposedWithoutAuth(*listenAddress, *webConfigFile) {
		logger.Warn("listening on a non-loopback address without -web.config.file: " +
			"the HTTP interface has no TLS or authentication, and /probe credentials travel in clear text. " +
			"Bind to 127.0.0.1 behind a reverse proxy, or configure -web.config.file " +
			"(see exporter-toolkit web-configuration). Set -web.debug-console=false for untrusted networks.")
	}

	server := newHTTPServer()
	// 统一安全响应头（S-08），包住整个 mux，/metrics、/probe、UI 全覆盖。
	server.Handler = securityHeaders(mux)

	if err := web.ListenAndServe(server, webConfig(listenAddress), logger); err != nil {
		logger.Error("listen and serve failed", "error", err)
		os.Exit(1)
	}
}
