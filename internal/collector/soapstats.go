package collector

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"
)

// defaultSOAPTargetLimit 与 ScrapeErrors 的 target 上限同义：SOAP 自监控按
// target 分桶，/probe 下 target 来自请求，无界的 distinct target 集合等于无界
// 内存（M-01 同类问题）。真实环境被持续抓取的 target 是个位到几十，1000 只在
// 攻击/误配置下生效。
const defaultSOAPTargetLimit = 1000

// soapWaitBuckets 是「等 SOAP 并发闸令牌」耗时（秒）的直方图桶。
//
// 正常拿到令牌是微秒级，因此从 1ms 起；撞上 -collector.max-concurrency
// 上限时等待会涨到百毫秒甚至秒级（受 -vmware.timeout 约束），故上界到 10s。
var soapWaitBuckets = []float64{1e-3, 5e-3, 1e-2, 5e-2, 1e-1, 5e-1, 1, 5, 10}

// soapRecorder 是**单轮抓取**内的 SOAP 往返记录器，挂在该轮的
// throttledRoundTripper 上，被嵌套 fan-out 的全部 goroutine 并发调用。
//
// 计数器在轮末由 CollectorSet 一次性冲入进程级 SOAPStats；它自己不跨轮存活。
type soapRecorder struct {
	// inflight/peak 走原子：begin/end 在热路径上，不该为一把互斥锁排队。
	inflight atomic.Int32
	peak     atomic.Int32

	mu          sync.Mutex
	ok          int64
	failed      int64
	waits       uint64
	waitSumSecs float64
	// waitBuckets 按 Prometheus 约定存**累计**计数（每个 le 桶包含所有 <= le
	// 的观测），这样轮末并入 SOAPStats 时直接逐桶相加即可。
	waitBuckets map[float64]uint64
}

func newSOAPRecorder() *soapRecorder {
	return &soapRecorder{waitBuckets: make(map[float64]uint64, len(soapWaitBuckets))}
}

// begin 记录一次实际发出的 SOAP 往返（拿令牌之后、调用下层 RoundTripper 之前）。
func (r *soapRecorder) begin() {
	cur := r.inflight.Add(1)

	// 并发下多个 goroutine 可能同时读到高于已记录峰值的值。CAS 重试把 peak
	// 抬到真实最大值；忽略 CAS 失败 —— 成功者记录的只会是同一个或更高的值。
	for {
		top := r.peak.Load()
		if cur <= top {
			return
		}

		if r.peak.CompareAndSwap(top, cur) {
			return
		}
	}
}

// end 与 begin 配对，按 RoundTrip 是否出错分桶计数。
func (r *soapRecorder) end(err error) {
	r.inflight.Add(-1)

	r.mu.Lock()
	defer r.mu.Unlock()

	if err != nil {
		r.failed++
	} else {
		r.ok++
	}
}

// observeWait 记录一次等令牌的耗时（仅在真正经过并发闸时调用）。
func (r *soapRecorder) observeWait(d time.Duration) {
	secs := d.Seconds()

	r.mu.Lock()
	defer r.mu.Unlock()

	r.waits++
	r.waitSumSecs += secs

	for _, le := range soapWaitBuckets {
		if secs <= le {
			r.waitBuckets[le]++
		}
	}
}

// soapScrapeSnapshot 是一轮抓取结束时从 recorder 取出的不可变副本。
type soapScrapeSnapshot struct {
	ok, failed  int64
	peak        int32
	waits       uint64
	waitSumSecs float64
	waitBuckets map[float64]uint64
}

func (r *soapRecorder) snapshot() soapScrapeSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()

	buckets := make(map[float64]uint64, len(r.waitBuckets))
	for le, n := range r.waitBuckets {
		buckets[le] = n
	}

	return soapScrapeSnapshot{
		ok:          r.ok,
		failed:      r.failed,
		peak:        r.peak.Load(),
		waits:       r.waits,
		waitSumSecs: r.waitSumSecs,
		waitBuckets: buckets,
	}
}

// SOAPView 是某个 target 当前导出的 SOAP 自监控值。
type SOAPView struct {
	RequestsOK   float64
	RequestsFail float64
	InflightPeak float64
	WaitCount    uint64
	WaitSum      float64
	WaitBuckets  map[float64]uint64
}

// soapBucket 是一个 target 的进程级累计状态。
type soapBucket struct {
	target string

	// 请求数与等待直方图是**跨轮累计**的 counter（rate() 依赖单调不减）。
	requestsOK   float64
	requestsFail float64
	waits        uint64
	waitSumSecs  float64
	waitBuckets  map[float64]uint64

	// 在飞峰值是**最近一轮**的快照（gauge），不跨轮取 max：取 max 会让一次
	// 历史尖峰永远挂在输出上，失去「现在压力多大」的含义。
	peakInflight float64
}

// SOAPStats 是按 target 分桶、distinct target 数有界的进程级 SOAP 统计。
//
// 为什么不做成 per-CollectorSet 字段：与 ScrapeErrors 同理，CollectorSet
// 每请求新建，请求计数会每轮归零，rate() 会把每次归零当成 counter reset。
// /metrics 与 /probe 共用一份，每轮只导出自己 target 的桶。
type SOAPStats struct {
	mu          sync.Mutex
	targetLimit int
	order       *list.List // Front 最久未使用，Back 最近使用
	buckets     map[string]*list.Element
}

// NewSOAPStats 构造默认 target 上限的进程级 SOAP 统计器。
func NewSOAPStats() *SOAPStats {
	return newSOAPStatsWithLimit(defaultSOAPTargetLimit)
}

// newSOAPStatsWithLimit 构造自定义 distinct target 上限的统计器。
// limit <= 0 表示不限制（仅供测试或受控单 target 场景，生产不要用）。
func newSOAPStatsWithLimit(limit int) *SOAPStats {
	return &SOAPStats{
		targetLimit: limit,
		order:       list.New(),
		buckets:     make(map[string]*list.Element),
	}
}

// TargetCount 返回当前保留的 distinct target 桶数（测试/诊断用）。
func (s *SOAPStats) TargetCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.buckets)
}

// Observe 把一轮抓取的记录并入 target 桶，返回该 target 合并后的视图。
func (s *SOAPStats) Observe(rawTarget string, snap soapScrapeSnapshot) SOAPView {
	tgt := normalizeErrorTarget(rawTarget)

	s.mu.Lock()
	defer s.mu.Unlock()

	el, ok := s.buckets[tgt]
	if !ok {
		if s.targetLimit > 0 && len(s.buckets) >= s.targetLimit {
			if oldest := s.order.Front(); oldest != nil {
				old := oldest.Value.(*soapBucket)
				s.order.Remove(oldest)
				delete(s.buckets, old.target)
			}
		}

		bucket := &soapBucket{target: tgt, waitBuckets: make(map[float64]uint64, len(soapWaitBuckets))}
		el = s.order.PushBack(bucket)
		s.buckets[tgt] = el
	} else {
		s.order.MoveToBack(el)
	}

	b := el.Value.(*soapBucket)
	b.requestsOK += float64(snap.ok)
	b.requestsFail += float64(snap.failed)
	b.waits += snap.waits
	b.waitSumSecs += snap.waitSumSecs
	b.peakInflight = float64(snap.peak)

	for le, n := range snap.waitBuckets {
		b.waitBuckets[le] += n
	}

	return viewFromBucket(b)
}

// Snapshot 返回 target 的当前视图；桶不存在（例如登录失败、一轮往返都没发出）
// 时返回零值视图。零值也要导出：序列缺失会让告警在「一切正常」时无数据，
// 第一次故障恰好漏报（与 errors_total 的 seed 同理）。
func (s *SOAPStats) Snapshot(rawTarget string) SOAPView {
	tgt := normalizeErrorTarget(rawTarget)

	s.mu.Lock()
	defer s.mu.Unlock()

	if el, ok := s.buckets[tgt]; ok {
		s.order.MoveToBack(el)
		return viewFromBucket(el.Value.(*soapBucket))
	}

	return SOAPView{WaitBuckets: map[float64]uint64{}}
}

func viewFromBucket(b *soapBucket) SOAPView {
	buckets := make(map[float64]uint64, len(b.waitBuckets))
	for le, n := range b.waitBuckets {
		buckets[le] = n
	}

	return SOAPView{
		RequestsOK:   b.requestsOK,
		RequestsFail: b.requestsFail,
		InflightPeak: b.peakInflight,
		WaitCount:    b.waits,
		WaitSum:      b.waitSumSecs,
		WaitBuckets:  buckets,
	}
}
