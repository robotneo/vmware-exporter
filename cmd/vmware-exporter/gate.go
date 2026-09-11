package main

import "sync"

// scrapeGate 把"同时进行的抓取数"限制在一个进程级上限内。
//
// 它与 -collector.max-concurrency 处在不同的层级：后者限制**一次**抓取内部
// 并发跑多少个 collector / 主机，而 scrapeGate 限制**同时有多少次抓取**。
// 多 vCenter 的 /probe 部署里，Prometheus 抓取间隔短于单轮耗时时请求会叠加，
// 每次抓取各自登录、各占一个 vCenter 会话并各自 fan-out，不加这道闸就可能把
// vCenter 的会话表打满。
//
// /metrics 与 /probe 共用同一个进程级实例（见 handlers.go 里的 scrapeGate）。
type scrapeGate struct {
	mu     sync.Mutex
	active int
}

// tryAcquire 尝试占用一个抓取名额。limit <= 0 表示不限制，总是成功。
//
// 刻意非阻塞：满员时返回 false 而不是排队等待。排队会让请求一直挂到客户端
// 自己超时，既占着 HTTP 连接又给不出明确信号；返回 false 让调用方回 503，
// Prometheus 把这一轮记为失败，语义与"目标此刻太忙"一致。
func (g *scrapeGate) tryAcquire(limit int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if limit > 0 && g.active >= limit {
		return false
	}

	g.active++

	return true
}

// release 归还一个名额。调用方必须在成功 acquire 之后 defer 一次，且只 defer
// 一次 —— 配对错误会让 active 变成负数，闸于是永久放行。
func (g *scrapeGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.active--
}

// current 仅供测试与可观测使用：返回当前占用的名额数。
func (g *scrapeGate) current() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.active
}
