package vmwareCollectors

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/types"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestTargetTypeDefaultsToVCenter(t *testing.T) {
	testCases := []struct {
		name      string
		loginData map[string]interface{}
		want      string
	}{
		{"esxi", map[string]interface{}{"targetType": "esxi"}, targetTypeESXi},
		{"vcenter", map[string]interface{}{"targetType": "vcenter"}, targetTypeVCenter},
		// 键缺失 / 空串 / 类型不对时都必须回落到 vCenter，而不是 panic。
		// Update() 可能被尚未适配的调用方触发，少一个键不该炸掉整次抓取。
		{"missing key", map[string]interface{}{}, targetTypeVCenter},
		{"empty string", map[string]interface{}{"targetType": ""}, targetTypeVCenter},
		{"wrong type", map[string]interface{}{"targetType": 42}, targetTypeVCenter},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := targetType(tc.loginData); got != tc.want {
				t.Fatalf("targetType() = %q, want %q", got, tc.want)
			}

			wantESXi := tc.want == targetTypeESXi
			if got := isESXi(tc.loginData); got != wantESXi {
				t.Fatalf("isESXi() = %v, want %v", got, wantESXi)
			}
		})
	}
}

func TestSyntheticLabelsPreservesExistingKeys(t *testing.T) {
	base := map[string]string{"dcmo": "ha-datacenter", "dc": "ha-datacenter"}

	got := syntheticLabels(base)

	if got["synthetic"] != "true" {
		t.Fatalf(`synthetic = %q, want "true"`, got["synthetic"])
	}

	// 原有 label 一个都不能丢：它们是 dashboard 关联查询的连接键。
	if got["dcmo"] != "ha-datacenter" || got["dc"] != "ha-datacenter" {
		t.Fatalf("existing labels were mangled: %v", got)
	}
}

// newSimClient 起一个内存服务并返回可用的 client 与 performance manager。
//
// 用 govmomi.NewClient 而不是 Model.Run：后者会在回调返回后自动 Remove
// 整个模型，无法配合 t.Cleanup 的生命周期。
func newSimClient(t *testing.T, model *simulator.Model) (*vim25.Client, *performance.Manager, context.Context) {
	t.Helper()

	if err := model.Create(); err != nil {
		t.Fatalf("failed to create simulator model: %v", err)
	}
	t.Cleanup(model.Remove)

	server := model.Service.NewServer()
	t.Cleanup(func() { server.Close() })

	ctx := context.Background()

	c, err := govmomi.NewClient(ctx, server.URL, true)
	if err != nil {
		t.Fatalf("failed to connect to the simulator: %v", err)
	}
	t.Cleanup(func() { _ = c.Logout(context.Background()) })

	return c.Client, performance.NewManager(c.Client), ctx
}

// findRef 返回指定类型的第一个实体引用。
//
// 必须用真实存在的引用：QueryPerfProviderSummary 会校验实体存在性，
// 伪造的 moid 直接被拒（simulator/performance_manager.go:85-90）。
func findRef(t *testing.T, ctx context.Context, client *vim25.Client, moType string) types.ManagedObjectReference {
	t.Helper()

	v, err := view.NewManager(client).CreateContainerView(
		ctx, client.ServiceContent.RootFolder, []string{moType}, true,
	)
	if err != nil {
		t.Fatalf("failed to create container view for %s: %v", moType, err)
	}
	defer func() { _ = v.Destroy(ctx) }()

	refs, err := v.Find(ctx, []string{moType}, nil)
	if err != nil {
		t.Fatalf("failed to find %s: %v", moType, err)
	}

	if len(refs) == 0 {
		t.Fatalf("simulator model has no %s; the test cannot negotiate an interval without a real entity", moType)
	}

	return refs[0]
}

// TestResolvePerfIntervalUsesServerRefreshRate 验证间隔来自服务端而非客户端猜测。
//
// simulator 对 HostSystem / VirtualMachine 返回 realtimeProviderSummary
// （RefreshRate=20），对其他实体返回 historicProviderSummary
// （CurrentSupported=false、RefreshRate=-1），见
// simulator/performance_manager.go:21-31。这正是真实 vSphere 的行为。
func TestResolvePerfIntervalUsesServerRefreshRate(t *testing.T) {
	client, perf, ctx := newSimClient(t, simulator.VPX())

	hostRef := findRef(t, ctx, client, "HostSystem")

	// 故意请求一个服务端没有的间隔，验证实现以服务端为准而不是照抄用户输入。
	got := resolvePerfInterval(ctx, perf, hostRef, 137, 999, testLogger())

	if got != 20 {
		t.Fatalf("resolvePerfInterval(HostSystem) = %d, want 20 (the server RefreshRate); "+
			"the client must not use its own requested value", got)
	}
}

// TestResolvePerfIntervalRejectsNegativeRefreshRate 固定 -1 的处理方式。
//
// 服务端在不支持实时统计时把 RefreshRate 设为 -1。把 -1 当作 IntervalId
// 会构造出无效的 PerfQuerySpec，所以实现必须显式检查 > 0，不能直接透传。
func TestResolvePerfIntervalRejectsNegativeRefreshRate(t *testing.T) {
	client, perf, ctx := newSimClient(t, simulator.VPX())

	dsRef := findRef(t, ctx, client, "Datastore")

	got := resolvePerfInterval(ctx, perf, dsRef, 20, 999, testLogger())

	if got <= 0 {
		t.Fatalf("resolvePerfInterval(Datastore) = %d; a non-positive IntervalId would produce an invalid PerfQuerySpec", got)
	}

	// Datastore 只支持历史汇总，应落到 300。
	if got != historicIntervalID {
		t.Fatalf("resolvePerfInterval(Datastore) = %d, want %d", got, historicIntervalID)
	}
}

// TestResolvePerfIntervalFallsBackOnError 验证协商失败不会中断采集。
//
// 用一个不存在的实体触发 InvalidArgument，服务端行为见
// simulator/performance_manager.go:85-90。
func TestResolvePerfIntervalFallsBackOnError(t *testing.T) {
	_, perf, ctx := newSimClient(t, simulator.VPX())

	bogus := types.ManagedObjectReference{Type: "HostSystem", Value: "does-not-exist"}

	got := resolvePerfInterval(ctx, perf, bogus, 20, 4242, testLogger())

	if got != 4242 {
		t.Fatalf("resolvePerfInterval() = %d, want the fallback 4242; a failed negotiation must not abort the scrape", got)
	}
}

func TestResolvePerfIntervalHandlesNilManager(t *testing.T) {
	ref := types.ManagedObjectReference{Type: "Datastore", Value: "datastore-1"}

	if got := resolvePerfInterval(context.Background(), nil, ref, 20, 300, testLogger()); got != 300 {
		t.Fatalf("resolvePerfInterval(nil manager) = %d, want the fallback 300", got)
	}
}

// TestResolvePerfIntervalForTargetRejectsHistoricOnESXi 是 C-2 修复的核心验收。
//
// ProviderSummary 在 ESXi 上仍可能报 SummarySupported=true —— API 形状与
// vCenter 一致 —— 但 ESXi 不运行统计汇总服务，请求 300 秒间隔会返回空结果集，
// 表现为 datastore 性能指标静默消失且无任何报错。
//
// 因此仅凭 ProviderSummary 不足以决策，必须叠加目标类型判断。
func TestResolvePerfIntervalForTargetRejectsHistoricOnESXi(t *testing.T) {
	client, perf, ctx := newSimClient(t, simulator.ESX())

	dsRef := findRef(t, ctx, client, "Datastore")

	// 先确认前提成立：单看 ProviderSummary，ESXi 上的 Datastore 也会得到 300。
	// 如果这条断言将来失败，说明 govmomi 改了 simulator 行为，
	// 下面那条针对性修复的前提就需要重新评估。
	plain := resolvePerfInterval(ctx, perf, dsRef, 20, 999, testLogger())
	if plain != historicIntervalID {
		t.Fatalf("precondition changed: resolvePerfInterval(ESXi Datastore) = %d, expected %d",
			plain, historicIntervalID)
	}

	got := resolvePerfIntervalForTarget(ctx, perf, dsRef, 20, historicIntervalID, targetTypeESXi, testLogger())

	if got == historicIntervalID {
		t.Fatalf("resolvePerfIntervalForTarget() returned the historic interval %d on ESXi; "+
			"ESXi does not aggregate historical statistics, so this query yields an empty result set", got)
	}

	if got != 20 {
		t.Fatalf("resolvePerfIntervalForTarget() = %d, want 20 (the realtime interval)", got)
	}
}

// TestResolvePerfIntervalForTargetKeepsHistoricOnVCenter 是 vCenter 侧的回归防线。
//
// 300 秒间隔在 vCenter 上是正确且必要的：disk.provisioned.latest 这类
// 容量计数器只在历史汇总里存在。若 ESXi 的修复误伤 vCenter，这些指标会消失。
func TestResolvePerfIntervalForTargetKeepsHistoricOnVCenter(t *testing.T) {
	client, perf, ctx := newSimClient(t, simulator.VPX())

	dsRef := findRef(t, ctx, client, "Datastore")

	got := resolvePerfIntervalForTarget(ctx, perf, dsRef, 20, historicIntervalID, targetTypeVCenter, testLogger())

	if got != historicIntervalID {
		t.Fatalf("resolvePerfIntervalForTarget(vCenter) = %d, want %d; "+
			"capacity counters only exist in the historical rollup", got, historicIntervalID)
	}
}

// TestResolvePerfIntervalForTargetGuardsAgainstZeroRequest 覆盖一个边界：
// requested 若为 0 或负数，不能被当作 IntervalId 透传出去。
func TestResolvePerfIntervalForTargetGuardsAgainstZeroRequest(t *testing.T) {
	client, perf, ctx := newSimClient(t, simulator.ESX())

	dsRef := findRef(t, ctx, client, "Datastore")

	got := resolvePerfIntervalForTarget(ctx, perf, dsRef, 0, historicIntervalID, targetTypeESXi, testLogger())

	if got != realtimeFallbackInterval {
		t.Fatalf("resolvePerfIntervalForTarget(requested=0) = %d, want the fallback %d",
			got, realtimeFallbackInterval)
	}
}
