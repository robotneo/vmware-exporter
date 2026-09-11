package collector

import (
	"context"

	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
)

// throttledRoundTripper 把经过它的 SOAP 往返数量限制在容量以内。
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
type throttledRoundTripper struct {
	next soap.RoundTripper
	sem  chan struct{}
}

// RoundTrip 取得令牌后转发给被包装的 RoundTripper。等待可被 ctx 取消
// （客户端断连 / -vmware.timeout），不会无限期挂住。
func (t *throttledRoundTripper) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	select {
	case t.sem <- struct{}{}:
		defer func() { <-t.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}

	return t.next.RoundTrip(ctx, req, res)
}

// ThrottleSOAP 在本轮抓取的 vim25 client 上装一个容量为 limit 的 SOAP 闸。
//
// 是 per-scrape 的：client 在每次登录时新建，这个 sem 也随这一轮抓取生灭，
// 不跨请求共享（跨请求的总量由 HTTP 层的 scrapeGate 负责，两者分工不同）。
// limit <= 0 时不包装，保持 govmomi 原生行为。
//
// 幂等：重复调用不在已有闸上再套一层（那会让容量被平方级压缩）。
func (s *Scrape) ThrottleSOAP(limit int) {
	if limit <= 0 || s == nil || s.Client == nil {
		return
	}

	if _, already := s.Client.RoundTripper.(*throttledRoundTripper); already {
		return
	}

	s.Client.RoundTripper = &throttledRoundTripper{
		next: s.Client.RoundTripper,
		sem:  make(chan struct{}, limit),
	}
}

// 编译期保证 vim25.Client 的 RoundTripper 字段是我们能包装的接口类型。
var _ soap.RoundTripper = (*throttledRoundTripper)(nil)
var _ soap.RoundTripper = (*vim25.Client)(nil)
