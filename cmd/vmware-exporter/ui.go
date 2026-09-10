package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	vmwareCollectors "github.com/prezhdarov/vmware-exporter/vmware/collectors"
	ui "github.com/prezhdarov/vmware-exporter/web"

	// 本项目的 web 包与 exporter-toolkit 的 web 包同名，main.go 里给
	// exporter-toolkit 那个保留了 web 名字；这里只用本项目自己的，起别名 ui。
	"github.com/prometheus/common/version"
)

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

	// 静态资源。三个页面都引用 app.css 与 i18n.js；app.js 只有调试页需要、
	// config.js 只有配置页需要，但都无条件提供 —— 它们不含任何配置或凭证，
	// 藏起来只会在 -web.debug-console=false 时留下一个 404 的引用。
	//
	// i18n.js 必须无条件提供且不能延后加载：三个页面的 HTML 里写的是中文
	// 字面量（默认语言），切到英文完全依赖这个脚本。取不到它的话页面不会
	// 报错，只是那个 EN 按钮点了没反应 —— 这种「静默降级」比 404 更难查。
	for _, asset := range []struct {
		path, contentType string
	}{
		{"app.css", "text/css; charset=utf-8"},
		{"app.js", "text/javascript; charset=utf-8"},
		{"config.js", "text/javascript; charset=utf-8"},
		{"i18n.js", "text/javascript; charset=utf-8"},
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
