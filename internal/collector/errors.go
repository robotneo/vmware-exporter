package collector

import "sync"

// ScrapeErrors 是跨抓取累积的错误计数，按 (target, collector) 分桶。
//
// 为什么必须是独立类型而不是 CollectorSet 的字段：CollectorSet 每个 HTTP
// 请求构造一个新实例（见 set.go 的说明），实例字段的生命周期只有一轮抓取。
// 把 counter 放进去，每轮都会从 0 重新开始 —— 那不是 counter，而是一个
// 取值恒为 0 或 1 的 gauge。Prometheus 的 rate()/increase() 依赖 counter
// 单调不减，遇到反复归零会把每次归零当成一次 counter reset，算出的速率
// 完全不可用。
//
// 这是全库第一个 counter，所以这条约束此前没有任何代码需要面对。
//
// 为什么按 target 分桶：/probe 模式下同一个进程服务多个 vCenter，
// 各自的失败次数不能混算。而每轮抓取只导出自己 target 的那一份
// （见 Snapshot），所以桶之间互不可见。
//
// 为什么不做成包级变量：包级可变状态会让测试互相干扰 —— 本仓库已经在
// Collector 接口的注释里写明了这条取舍（collector.go:29）。代价是调用方
// 要显式持有一个实例，NewCollectorSet 因此把它列为必填项。
type ScrapeErrors struct {
	mu     sync.Mutex
	counts map[errorKey]float64
}

// errorKey 是错误计数的分桶键。
//
// collector 取值是 collector 的规范名，或者字面量 "login" —— 登录不属于
// 任何 collector，但它是最需要计数的失败点。这个取值与
// vmware_scrape_collector_duration_seconds{collector="login"} 保持一致，
// 那个标签值在框架时代就已经这么用了。
type errorKey struct {
	target    string
	collector string
}

func NewScrapeErrors() *ScrapeErrors {
	return &ScrapeErrors{counts: make(map[errorKey]float64)}
}

// Add 给 (target, collector) 的计数加一。
func (e *ScrapeErrors) Add(target, collectorName string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.counts[errorKey{target: target, collector: collectorName}]++
}

// Snapshot 返回 target 下各 collector 的当前累计值。
//
// seed 里的名字即使从未出错也会以 0 出现在结果里。这不是可有可无的细节：
// 一个从来没失败过的 collector 如果不导出 0，它的 errors_total 序列根本
// 不存在，写好的告警规则在「一切正常」时是「无数据」而不是「值为 0」。
// 等到它第一次失败，序列才凭空出现 —— 而 increase() 对一条刚出现的序列
// 算不出增量，第一次故障恰好是漏报的。
//
// 返回的是副本：调用方拿到后要往 channel 里写，不能持有锁。
func (e *ScrapeErrors) Snapshot(target string, seed []string) map[string]float64 {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make(map[string]float64, len(seed))

	for _, name := range seed {
		out[name] = e.counts[errorKey{target: target, collector: name}]
	}

	return out
}
