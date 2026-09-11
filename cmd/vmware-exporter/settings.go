package main

import (
	"github.com/prezhdarov/vmware-exporter/internal/config"
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
	scrapeInflight  int
	targetDisabled  bool
	metricsDisabled bool
	debugConsole    bool
}

func currentExporterSettings() exporterSettings {
	var s exporterSettings

	config.Snapshot(func() {
		s = exporterSettings{
			maxConcurrency:  *maxConcurrency,
			scrapeInflight:  *maxScrapeInflight,
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

// currentScrapeInflight 是只需要 in-flight 抓取上限时的窄口径快照，供
// probeHandler 使用（它不读其余 exporter flag）。
func currentScrapeInflight() int {
	var v int

	config.Snapshot(func() {
		v = *maxScrapeInflight
	})

	return v
}
