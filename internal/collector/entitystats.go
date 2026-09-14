package collector

import "sync"

// 实体被跳过的原因标签值。
//
// 这些字符串直接成为 vmware_scrape_entities_skipped 的 reason label，
// 一旦发布就被告警规则引用，不要重命名。一台实体可以同时命中多个原因
// （例如维护中断连），每个原因各自 +1，所以各原因之和可能大于被跳过的
// 实体数 —— 这是刻意的：「有多少台在维护」与「有多少台断连」本就是两个
// 独立的运维问题。
const (
	// SkipReasonPoweredOff：VM/主机已关机，vCenter 没有实时 perf 数据。
	SkipReasonPoweredOff = "powered_off"
	// SkipReasonSuspended：VM 被挂起，同样没有实时 perf 数据。
	SkipReasonSuspended = "suspended"
	// SkipReasonDisconnected：主机连接态为 disconnected。
	SkipReasonDisconnected = "disconnected"
	// SkipReasonNotResponding：主机连接态为 notResponding。
	SkipReasonNotResponding = "not_responding"
	// SkipReasonMaintenance：主机处于维护模式。
	SkipReasonMaintenance = "maintenance"
	// SkipReasonUnsupported：目标版本/许可证不支持该数据面。
	SkipReasonUnsupported = "unsupported"
	// SkipReasonError：取数报错导致实体被跳过（错误本身另计入 errors_total）。
	SkipReasonError = "error"
)

// entityCounters 是一个 (collector, kind) 维度的一轮抓取计数。
type entityCounters struct {
	// found 是本轮在清单里发现的实体总数。
	found int
	// emitted 是本轮实际拿到数据面（perf/esxcli）输出的实体数。
	// 静态指标（_info/容量/状态）对全部 found 个实体输出，不在此列。
	emitted int
	// skipped 按原因汇总被排除出数据面的实体数。允许一个实体命中多个原因。
	skipped map[string]int
}

// EntityStats 在一轮抓取内汇聚各 collector 上报的实体计数。
//
// 它必须是并发安全的：collector 在 CollectorSet 的 errgroup 里并发执行，
// 会同时调用 Record*。生命周期与 Scrape 相同（一次抓取），所以计数是
// 「本轮快照」语义，由 CollectorSet 在所有 collector 结束后作为 gauge
// 输出 —— 它不是跨请求累积的 counter，名字因此不带 _total 后缀。
type EntityStats struct {
	mu sync.Mutex

	// key 为 collector 名 + "\x00" + kind。
	reports map[string]*entityCounters
}

// NewEntityStats 构造一轮抓取的实体统计。
func NewEntityStats() *EntityStats {
	return &EntityStats{reports: make(map[string]*entityCounters)}
}

func key(collectorName, kind string) string {
	return collectorName + "\x00" + kind
}

// RecordEntities 由 collector 在遍历完实体清单后上报一次。
//
// collectorName 是注册名（vm/host/datastore/...），kind 是实体种类
// （vm/host/datastore/cluster）—— 两者不同，因为 esxcli.host.nic 这样的
// collector 统计的也是 host 实体。skipped 可为 nil；调用后不要再修改
// 传入的 map。
func (e *EntityStats) RecordEntities(collectorName, kind string, found, emitted int, skipped map[string]int) {
	if e == nil {
		// 裸 Scrape 驱动的单元测试没有统计实例，上报静默丢弃。
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	k := key(collectorName, kind)
	c := e.reports[k]
	if c == nil {
		c = &entityCounters{skipped: make(map[string]int)}
		e.reports[k] = c
	}

	c.found += found
	c.emitted += emitted
	for reason, n := range skipped {
		c.skipped[reason] += n
	}
}

// snapshot 返回确定顺序的计数切片，供 CollectorSet 输出。
//
// 返回值是值拷贝：调用方在 g.Wait() 之后读取，理论上已无并发写入，
// 但拷贝一层可以让「将来有人把输出挪进并发路径」时不产生数据竞争。
type entitySnapshot struct {
	collectorName string
	kind          string
	found         int
	emitted       int
	skipped       map[string]int
}

func (e *EntityStats) snapshot() []entitySnapshot {
	if e == nil {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	out := make([]entitySnapshot, 0, len(e.reports))
	for k, c := range e.reports {
		skipped := make(map[string]int, len(c.skipped))
		for reason, n := range c.skipped {
			skipped[reason] = n
		}
		// key 用 \x00 分隔，实体名/collector 名里不可能出现该字节。
		for i := 0; i < len(k); i++ {
			if k[i] == 0 {
				out = append(out, entitySnapshot{
					collectorName: k[:i],
					kind:          k[i+1:],
					found:         c.found,
					emitted:       c.emitted,
					skipped:       skipped,
				})
				break
			}
		}
	}

	return out
}
