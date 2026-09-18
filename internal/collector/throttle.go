package collector

import (
	"context"
	"time"

	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
)

// throttledRoundTripper 给经过它的 SOAP 往返统一挂上记录层（P-09），并在
// limit > 0 时把同时在飞的往返数限制在容量以内。
//
// 这是 esxcli collector 嵌套 fan-out 的**全局**闸，与各 errgroup 上的
// SetLimit 处在不同层：SetLimit 限制"同时有多少个 goroutine 在跑"，但主机
// goroutine 内部还会再为每张网卡 fan-out 一层，旧代码两层各自 SetLimit(n)，
// 最坏仍能有 n×n 个 SOAP 请求同时打向同一个 vCenter。把闸放在 RoundTripper
// 上，令牌只包住一次同步的网络往返，于是无论调用方嵌套多少层 goroutine，
// 真正在飞的请求数都被钉死。
//
// 为什么这样不会自死锁：goroutine 在发请求前抢令牌，**并不持有令牌去等待
// 子 goroutine**。因此"外层 host 占满容量后内层 nic 永远抢不到"的环形等待
// 不成立 —— 持令牌的那个 host 自己发完 MME/list 往返后立刻释放，nic 层随即
// 能拿到。阻塞等待发生在未持令牌的栈帧上，这是普通的资源排队，不是死锁。
//
// sem 为 nil（limit <= 0）时不做并发限制，纯透传 —— 但记录层仍在，
// 因此 SOAP 往返计数与在飞峰值在任何并发配置下都可用。
type throttledRoundTripper struct {
	next soap.RoundTripper
	sem  chan struct{}
	rec  *soapRecorder
}

// RoundTrip 取得令牌（若有限流）后转发给被包装的 RoundTripper。等待可被
// ctx 取消（客户端断连 / -vmware.timeout），不会无限期挂住。
func (t *throttledRoundTripper) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if t.sem != nil {
		waitBegin := time.Now()

		select {
		case t.sem <- struct{}{}:
			if t.rec != nil {
				t.rec.observeWait(time.Since(waitBegin))
			}
			defer func() { <-t.sem }()
		case <-ctx.Done():
			// 没拿到令牌的请求不会出现在往返计数里 —— 它根本没发出去。
			return ctx.Err()
		}
	}

	if t.rec != nil {
		t.rec.begin()
	}

	err := t.next.RoundTrip(ctx, req, res)

	if t.rec != nil {
		t.rec.end(err)
	}

	return err
}

// ThrottleSOAP 在本轮抓取的 vim25 client 上安装记录层；limit > 0 时同时装一个
// 容量为 limit 的 SOAP 并发闸。
//
// 是 per-scrape 的：client 在每次登录时新建，recorder 与 sem 也随这一轮抓取
// 生灭，不跨请求共享（跨请求的累计状态在 SOAPStats，总量闸由 HTTP 层的
// scrapeGate 负责，两者分工不同）。
//
// 幂等：重复调用不在已有包装上再套一层（那会让容量被平方级压缩，并让每次
// 往返被重复计数）。
func (s *Scrape) ThrottleSOAP(limit int) {
	if s == nil || s.Client == nil {
		return
	}

	if _, already := s.Client.RoundTripper.(*throttledRoundTripper); already {
		return
	}

	rec := newSOAPRecorder()
	s.soapRec = rec

	wrapped := &throttledRoundTripper{
		next: s.Client.RoundTripper,
		rec:  rec,
	}

	// limit <= 0 时不建信号量：记录层保持纯透传，零并发限制语义与旧行为一致。
	if limit > 0 {
		wrapped.sem = make(chan struct{}, limit)
	}

	s.Client.RoundTripper = wrapped
}

// 编译期保证 vim25.Client 的 RoundTripper 字段是我们能包装的接口类型。
var _ soap.RoundTripper = (*throttledRoundTripper)(nil)
var _ soap.RoundTripper = (*vim25.Client)(nil)
