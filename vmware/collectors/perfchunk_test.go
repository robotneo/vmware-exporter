package vmwareCollectors

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

func TestChunkRefs(t *testing.T) {
	refs := make([]types.ManagedObjectReference, 10)
	for i := range refs {
		refs[i] = types.ManagedObjectReference{Type: "HostSystem", Value: "host-" + string(rune('a'+i))}
	}

	tests := []struct {
		name string
		size int
		want int // number of chunks
	}{
		{"disabled is a single chunk", 0, 1},
		{"negative is a single chunk", -1, 1},
		{"one per entity", 1, 10},
		{"even division", 5, 2},
		{"uneven division keeps a smaller tail", 3, 4},
		{"larger than input is a single chunk", 100, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkRefs(refs, tc.size)
			if len(got) != tc.want {
				t.Fatalf("chunkRefs(size=%d) returned %d chunks, want %d", tc.size, len(got), tc.want)
			}

			// 重组后必须仍是全部实体，顺序不变、不重不漏。
			var total int
			for _, c := range got {
				total += len(c)
			}

			if total != len(refs) {
				t.Fatalf("chunks cover %d entities, want %d", total, len(refs))
			}

			// 除最后一片外，每片都应被填满到 size（单片模式除外）。
			if tc.size > 0 && tc.size < len(refs) {
				for i, c := range got[:len(got)-1] {
					if len(c) != tc.size {
						t.Fatalf("chunk %d has %d entities, want a full chunk of %d", i, len(c), tc.size)
					}
				}
			}

			// 第一片必须从原始切片开头开始（连续切分，不得重排）。
			if len(got[0]) > 0 && got[0][0].Value != refs[0].Value {
				t.Fatal("chunks do not preserve the original order")
			}
		})
	}
}

// countingRT 包裹 simulator 的 RoundTripper，统计 QueryPerf SOAP 调用数与
// 单次请求携带的最大 spec 数。它是分块行为的观察点：分块与否、每块多大，
// 最终都体现在「发了几次 QueryPerf、每次装几条 spec」上。
type countingRT struct {
	next       soap.RoundTripper
	queryCalls atomic.Int32
	maxSpecs   atomic.Int32
}

func (rt *countingRT) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	if body, ok := req.(*methods.QueryPerfBody); ok && body.Req != nil {
		n := int32(len(body.Req.QuerySpec))
		rt.queryCalls.Add(1)

		for {
			old := rt.maxSpecs.Load()
			if n <= old || rt.maxSpecs.CompareAndSwap(old, n) {
				break
			}
		}
	}

	return rt.next.RoundTrip(ctx, req, res)
}

var _ soap.RoundTripper = (*countingRT)(nil)

// runChunkedHostPerf 在一个新的 simulator 抓取上下文上，用给定 chunkSize 跑
// 一次主机 perf 抓取，返回 QueryPerf 调用数、单次最大 spec 数与产出指标条数。
func runChunkedHostPerf(t *testing.T, chunkSize int) (queryCalls, maxSpecs, emitted int) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, s, cleanup := setupCollectorScrape(t)
	t.Cleanup(cleanup)

	refs, names := getHostRefsAndNames(t, ctx, s, logger)
	if len(refs) < 2 {
		t.Fatalf("test needs >= 2 hosts to prove chunking, got %d", len(refs))
	}

	rt := &countingRT{next: s.Client.RoundTripper}
	s.Client.RoundTripper = rt

	ch := make(chan prometheus.Metric, 20000)
	scrapePerformance(ctx, ch, logger, 1, 20, s.Perf,
		s.Target, "HostSystem", "vmware", "host", "",
		[]string{"cpu.usage.average"}, s.Counters, refs, names, chunkSize, 4)

	return int(rt.queryCalls.Load()), int(rt.maxSpecs.Load()), len(drainMetrics(ch))
}

// TestScrapePerformanceChunksQueryPerf 是分块的反向验证：
//
//	chunkSize=1 必须让每个实体各发一次 QueryPerf；chunkSize=0（不分块）必须
//	合并成一次。两者产出的指标条数相同（合并结果与请求如何切分无关）。
//
// 注入的「真实回归形状」是有人在 scrapePerformance 里忽略 chunkSize、始终
// 单片查询 —— 那时 chunkSize=1 的 queryCalls 会退化成 1，本测试报红。
func TestScrapePerformanceChunksQueryPerf(t *testing.T) {
	callsChunked, maxSpecsChunked, emittedChunked := runChunkedHostPerf(t, 1)
	callsSingle, maxSpecsSingle, emittedSingle := runChunkedHostPerf(t, 0)

	if callsChunked == callsSingle {
		t.Fatalf("chunkSize=1 produced %d QueryPerf calls, same as unchunked %d; chunking was not applied",
			callsChunked, callsSingle)
	}

	if maxSpecsChunked != 1 {
		t.Fatalf("chunkSize=1 sent up to %d specs in one QueryPerf, want 1", maxSpecsChunked)
	}

	if maxSpecsSingle <= 1 {
		t.Fatalf("unchunked QueryPerf carried only %d spec, want all entities in one request", maxSpecsSingle)
	}

	if emittedChunked != emittedSingle || emittedChunked == 0 {
		t.Fatalf("chunked emitted %d metrics, unchunked %d; merging must not drop or duplicate series",
			emittedChunked, emittedSingle)
	}
}
