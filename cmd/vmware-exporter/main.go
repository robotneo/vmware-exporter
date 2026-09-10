package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

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
