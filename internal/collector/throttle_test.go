package collector

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
)

// fakeRT 是一个可阻塞的 soap.RoundTripper 桩：inflight 记录当前同时在飞的
// 调用数，release 控制调用何时返回，借此撑开"判断容量→占用"的窗口。
type fakeRT struct {
	entered  chan struct{}
	release  chan struct{}
	inflight atomic.Int32
	peak     atomic.Int32
	calls    atomic.Int32
	err      error
}

func newFakeRT() *fakeRT {
	return &fakeRT{entered: make(chan struct{}, 64), release: make(chan struct{})}
}

func (f *fakeRT) RoundTrip(ctx context.Context, _ soap.HasFault, _ soap.HasFault) error {
	return f.roundTrip(ctx)
}

func (f *fakeRT) roundTrip(ctx context.Context) error {
	now := f.inflight.Add(1)
	f.calls.Add(1)
	for {
		old := f.peak.Load()
		if now <= old || f.peak.CompareAndSwap(old, now) {
			break
		}
	}
	f.entered <- struct{}{}

	select {
	case <-f.release:
	case <-ctx.Done():
		f.inflight.Add(-1)
		return ctx.Err()
	}
	f.inflight.Add(-1)
	return f.err
}

func TestThrottledRoundTripperCapsInFlight(t *testing.T) {
	const limit = 3
	rt := newFakeRT()
	tr := &throttledRoundTripper{next: rt, sem: make(chan struct{}, limit)}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < limit*4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = tr.RoundTrip(context.Background(), nil, nil)
		}()
	}

	close(start)

	// 等到底层恰好有 limit 个在飞；再给一点窗口确认不会超过。
	deadline := time.After(2 * time.Second)
	for rt.peak.Load() < limit {
		select {
		case <-deadline:
			t.Fatalf("only %d calls entered, want %d to exercise the limit", rt.peak.Load(), limit)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	// 此刻闸应把第 limit+1 个挡在外面：再多等一会，peak 仍必须等于 limit。
	time.Sleep(30 * time.Millisecond)
	if got := rt.peak.Load(); got != limit {
		t.Fatalf("peak in-flight SOAP calls = %d, want %d", got, limit)
	}

	// 放行全部。
	close(rt.release)
	wg.Wait()

	if got := rt.calls.Load(); got != int32(limit*4) {
		t.Fatalf("all calls must eventually run, got %d", got)
	}
}

// TestThrottledRoundTripperHonorsContext 确认在拿不到令牌时，等待可被 ctx
// 取消 —— 否则一个卡住的 vCenter 能让堆积的请求挂到天荒地老。
func TestThrottledRoundTripperHonorsContext(t *testing.T) {
	tr := &throttledRoundTripper{
		next: newFakeRT(),
		sem:  make(chan struct{}, 1),
	}
	tr.sem <- struct{}{} // 唯一名额已占满，且永不释放。

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := tr.RoundTrip(ctx, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded while queued", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("queued request was not promptly cancelled")
	}
}

func TestThrottleSOAP(t *testing.T) {
	t.Run("wraps when limit positive", func(t *testing.T) {
		s := &Scrape{Client: &vim25.Client{RoundTripper: newFakeRT()}}
		s.ThrottleSOAP(8)
		if _, ok := s.Client.RoundTripper.(*throttledRoundTripper); !ok {
			t.Fatalf("RoundTripper = %T, want *throttledRoundTripper", s.Client.RoundTripper)
		}
	})

	t.Run("no wrap when limit zero", func(t *testing.T) {
		rt := newFakeRT()
		s := &Scrape{Client: &vim25.Client{RoundTripper: rt}}
		s.ThrottleSOAP(0)
		if s.Client.RoundTripper != soap.RoundTripper(rt) {
			t.Fatal("limit 0 must leave the original RoundTripper untouched")
		}
	})

	// 幂等：重复安装不得在闸上再套一层（那会把容量平方级压缩）。
	t.Run("idempotent", func(t *testing.T) {
		s := &Scrape{Client: &vim25.Client{RoundTripper: newFakeRT()}}
		s.ThrottleSOAP(8)
		first := s.Client.RoundTripper
		s.ThrottleSOAP(8)
		if s.Client.RoundTripper != first {
			t.Fatal("second ThrottleSOAP must not double-wrap the RoundTripper")
		}
	})

	t.Run("nil safe", func(t *testing.T) {
		var s *Scrape
		s.ThrottleSOAP(8) // 不应 panic
		s2 := &Scrape{}
		s2.ThrottleSOAP(8) // Client 为 nil 也不应 panic
	})
}
