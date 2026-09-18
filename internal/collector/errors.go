package collector

import (
	"container/list"
	"sync"

	"github.com/prezhdarov/vmware-exporter/internal/target"
)

// defaultErrorTargetLimit 是单个 ScrapeErrors 允许同时保留的不同 target 桶数。
//
// 真实部署中被持续抓取的 vCenter/ESXi 数量是个位到几十；1000 对正常使用永远
// 触不到。它的作用是安全兜底：/probe 默认无白名单且无认证时，target 来自请求，
// 登录失败也会 Add(target,"login")，若 target 桶无上限，攻击者每轮换一个 target
// 就能让进程内存随请求数无限增长。
const defaultErrorTargetLimit = 1000

// ScrapeErrors 是跨抓取累积的错误计数，按 (target, collector) 分桶。
//
// 为什么必须是独立类型而不是 CollectorSet 的字段：CollectorSet 每个 HTTP
// 请求构造一个新实例（见 set.go），实例字段的生命周期只有一轮抓取。
// 把 counter 放进去，每轮都会从 0 重新开始 —— 那不是 counter，而是一个
// 取值恒为 0 或 1 的 gauge。Prometheus 的 rate()/increase() 依赖 counter
// 单调不减，遇到反复归零会把每次归零当成一次 counter reset，算出的速率
// 完全不可用。
//
// 为什么按 target 分桶：/probe 模式下同一个进程服务多个 vCenter，
// 各自的失败次数不能混算。而每轮抓取只导出自己 target 的那一份
// （见 Snapshot），所以桶之间互不可见。
//
// target 维度必须有界（M-01）：map 键含请求方提供的 target，无界的 target
// 集合等于无界内存。这里用 LRU 把 distinct target 数钉在 targetLimit；正常
// target 每轮抓取都会被 Snapshot/Add 触碰，不会被淘汰，只有一次性伪造 target
// 才会被挤出。被淘汰桶若再次出现会表现为 counter reset，因此上限必须远大于
// 真实 target 数（默认 1000），它只在攻击/误配置下生效。
type ScrapeErrors struct {
	mu          sync.Mutex
	targetLimit int
	order       *list.List // Front 最久未使用，Back 最近使用
	buckets     map[string]*list.Element
}

// scrapeErrorBucket 是一个 target 下按 collector 名聚合的计数。
type scrapeErrorBucket struct {
	target string
	counts map[string]float64
}

// NewScrapeErrors 构造默认 target 上限的进程级错误计数器。
func NewScrapeErrors() *ScrapeErrors {
	return NewScrapeErrorsWithLimit(defaultErrorTargetLimit)
}

// NewScrapeErrorsWithLimit 构造自定义 distinct target 上限的计数器。
// limit <= 0 表示不限制（仅供测试或受控单 target 场景，生产不要用）。
func NewScrapeErrorsWithLimit(limit int) *ScrapeErrors {
	return &ScrapeErrors{
		targetLimit: limit,
		order:       list.New(),
		buckets:     make(map[string]*list.Element),
	}
}

// normalizeErrorTarget 把外部传入的 target 收敛成稳定的 map 键。
//
// /probe 在 HTTP 层已经用 internal/target.Parse 规范化过；这里再做一次是纵深
// 防御，避免其它调用路径把大小写、空白或 userinfo 形态的同一 target 记成多个
// 桶。空 target（/metrics 未显式传 Target）原样保留。无法解析时也原样保留：
// 安全拒绝发生在更外层，这里不能因为键格式而丢失一次真实的错误计数。
func normalizeErrorTarget(raw string) string {
	if raw == "" {
		return ""
	}

	if ep, err := target.Parse(raw); err == nil {
		return ep.Authority
	}

	return raw
}

// TargetCount 返回当前保留的 distinct target 桶数，主要用于测试与诊断
// M-01 的有界化是否生效。
func (e *ScrapeErrors) TargetCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()

	return len(e.buckets)
}

// Add 给 (target, collector) 的计数加一，并把该 target 标记为最近使用。
func (e *ScrapeErrors) Add(rawTarget, collectorName string) {
	tgt := normalizeErrorTarget(rawTarget)

	e.mu.Lock()
	defer e.mu.Unlock()

	el, ok := e.buckets[tgt]
	if !ok {
		// 容量兜底：新 target 到达上限时淘汰最久未被 Add/Snapshot 触碰的桶。
		if e.targetLimit > 0 && len(e.buckets) >= e.targetLimit {
			if oldest := e.order.Front(); oldest != nil {
				old := oldest.Value.(*scrapeErrorBucket)
				e.order.Remove(oldest)
				delete(e.buckets, old.target)
			}
		}

		bucket := &scrapeErrorBucket{target: tgt, counts: make(map[string]float64)}
		el = e.order.PushBack(bucket)
		e.buckets[tgt] = el
	} else {
		e.order.MoveToBack(el)
	}

	el.Value.(*scrapeErrorBucket).counts[collectorName]++
}

// Snapshot 返回 target 下各 collector 的当前累计值。
//
// seed 里的名字即使从未出错也会以 0 出现在结果里。这不是可有可无的细节：
// 一个从来没失败过的 collector 如果不导出 0，它的 errors_total 序列根本
// 不存在，写好的告警规则在「一切正常」时是「无数据」而不是「值为 0」。
// 等到它第一次失败，序列才凭空出现 —— 而 increase() 对一条刚出现的序列
// 算不出增量，第一次故障恰好是漏报的。
//
// 返回的是副本：调用方拿到后要往 channel 里写，不能持有锁。Snapshot 不创建
// target 桶（仅 seed 的 0 值不需要占内存），但已存在的 target 会被 touch：
// 健康 target 每轮抓取都调用 Snapshot，因此它在 LRU 中始终是活跃的。
func (e *ScrapeErrors) Snapshot(rawTarget string, seed []string) map[string]float64 {
	tgt := normalizeErrorTarget(rawTarget)

	e.mu.Lock()
	defer e.mu.Unlock()

	out := make(map[string]float64, len(seed))

	var counts map[string]float64
	if el, ok := e.buckets[tgt]; ok {
		e.order.MoveToBack(el)
		counts = el.Value.(*scrapeErrorBucket).counts
	}

	for _, name := range seed {
		out[name] = counts[name]
	}

	return out
}
