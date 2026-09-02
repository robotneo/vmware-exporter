package vmwareCollectors

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	vimtypes "github.com/vmware/govmomi/vim25/types"
	vsanmethods "github.com/vmware/govmomi/vsan/methods"
	vsantypes "github.com/vmware/govmomi/vsan/types"
)

// vsanStub 是 vSAN SOAP 通道的替身。
//
// 为什么必须有它：vcsim 的 vsan/simulator.go 整个文件只有 108 行，只注册了
// 3 个方法（VsanClusterGetConfig、VsanClusterReconfig、
// VSANVcConvertToStretchedCluster）。组 A 用到的 3 个 API 里它只覆盖 1 个，
// 容量与健康完全不支持。没有替身，这个 collector 的绝大部分逻辑就无法反向
// 验证，按本项目标准是不该合入的。
//
// 成本比预想的低：注入点 soap.RoundTripper 只要求一个方法，而
// vsan/methods 的 xxxBody 类型都是导出的，所以 switch 能直接写。
// 不需要 HTTP 服务、不需要 XML、不碰网络。
type vsanStub struct {
	config *vsantypes.VsanClusterGetConfigResponse
	space  *vsantypes.VsanQuerySpaceUsageResponse
	health *vsantypes.VsanQueryVcClusterHealthSummaryResponse

	// cachedHealth 与 health 分开，用于测两阶段读取：
	// 第一次（FetchFromCache=true）返回 cachedHealth，之后返回 health。
	cachedHealth *vsantypes.VsanQueryVcClusterHealthSummaryResponse

	// 各方法的错误注入。nil 表示成功。
	configErr error
	spaceErr  error
	healthErr error

	// uncachedHealthErr 只让第二阶段失败，第一阶段照常返回 cachedHealth。
	uncachedHealthErr error

	// 调用计数，用于断言"未启用时不再查其余 API"这类行为。
	configCalls int
	spaceCalls  int
	healthCalls int

	// healthFetchFromCache 记录每次健康查询的 FetchFromCache 取值，
	// 用于断言两阶段的顺序（先 true 后 false，不能反）。
	healthFetchFromCache []bool

	// healthFields 记录健康查询请求的 Fields，用于断言我们真的请求了
	// physicalDisksHealth —— 漏了它盘级指标就会静默消失。
	healthFields []string
}

func (s *vsanStub) RoundTrip(ctx context.Context, req, res soap.HasFault) error {
	switch body := res.(type) {
	case *vsanmethods.VsanClusterGetConfigBody:
		s.configCalls++
		if s.configErr != nil {
			return s.configErr
		}
		body.Res = s.config

	case *vsanmethods.VsanQuerySpaceUsageBody:
		s.spaceCalls++
		if s.spaceErr != nil {
			return s.spaceErr
		}
		body.Res = s.space

	case *vsanmethods.VsanQueryVcClusterHealthSummaryBody:
		s.healthCalls++

		reqBody, ok := req.(*vsanmethods.VsanQueryVcClusterHealthSummaryBody)
		if !ok || reqBody.Req == nil {
			return errors.New("stub: malformed health summary request")
		}

		fromCache := reqBody.Req.FetchFromCache != nil && *reqBody.Req.FetchFromCache
		s.healthFetchFromCache = append(s.healthFetchFromCache, fromCache)
		s.healthFields = reqBody.Req.Fields

		if s.healthErr != nil {
			return s.healthErr
		}

		if fromCache {
			if s.cachedHealth != nil {
				body.Res = s.cachedHealth
			} else {
				body.Res = s.health
			}
			return nil
		}

		// 第二阶段（强制重算）。
		if s.uncachedHealthErr != nil {
			return s.uncachedHealthErr
		}
		body.Res = s.health

	default:
		return errors.New("stub: unexpected request type")
	}

	return nil
}

// collectVsanCluster 用替身跑一个集群的采集，返回按指标名分组的 label 与值。
func collectVsanCluster(t *testing.T, stub *vsanStub, cluster mo.ClusterComputeResource) (
	map[string][]map[string]string, map[string][]float64, error,
) {
	t.Helper()

	c := &vsanCollector{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := &collector.Scrape{Namespace: "vmware", Target: "vc.example.com"}

	// 不 close(ch)：drainMetrics 用 select + default，从已关闭的 channel
	// 读会让 case 永远就绪并返回零值，于是无限追加 nil。缓冲够大即可。
	ch := make(chan prometheus.Metric, 500)

	err := c.collectCluster(context.Background(), ch, s,
		descsFor(s.Namespace).vsan, stub, cluster)

	series := map[string][]map[string]string{}
	values := map[string][]float64{}

	for _, m := range drainMetrics(ch) {
		name, labels, value := labelsOf(t, m)
		series[name] = append(series[name], labels)
		values[name] = append(values[name], value)
	}

	return series, values, err
}

func testVsanCluster(moid, name string) mo.ClusterComputeResource {
	var cluster mo.ClusterComputeResource
	cluster.Self = vimtypes.ManagedObjectReference{Type: "ClusterComputeResource", Value: moid}
	cluster.Name = name

	return cluster
}

func vsanEnabledConfig(dedup bool) *vsantypes.VsanClusterGetConfigResponse {
	resp := &vsantypes.VsanClusterGetConfigResponse{}
	resp.Returnval.Enabled = vsanTrue()

	if dedup {
		resp.Returnval.DataEfficiencyConfig = &vsantypes.VsanDataEfficiencyConfig{
			DedupEnabled: true,
		}
	}

	return resp
}

func vsanHealthResponse(overall string, disks []vsantypes.VsanPhysicalDiskHealthSummary) *vsantypes.VsanQueryVcClusterHealthSummaryResponse {
	resp := &vsantypes.VsanQueryVcClusterHealthSummaryResponse{}
	resp.Returnval.OverallHealth = overall
	resp.Returnval.PhysicalDisksHealth = disks

	return resp
}

// TestVsanDisabledClusterSkipsRemainingQueries 锁死设计稿 2.4 节的降级 2：
// 集群未启用 vSAN 时输出 enabled 0，且**不再查其余 API**。
//
// 断言调用计数而不只是断言指标缺失，这是关键：只查指标的话，"查了但结果被
// 丢掉"和"没查"看起来一样，而前者每轮都在白花 SOAP 往返 —— 混合环境里
// 这正是 telegraf 需要 vsan_cluster_include 让用户手工筛集群的原因。
func TestVsanDisabledClusterSkipsRemainingQueries(t *testing.T) {
	// Enabled 为 nil 而非 false：这正是 vcsim 的行为
	// （vsan/simulator.go:72 返回空的 VsanConfigInfoEx），真实 vCenter 上
	// 属性缺失时同理。两层判空的第一层就是为它准备的。
	//
	// space 与 health 刻意给了**合法**响应，虽然本例根本不该用到它们。
	// 这不是多余的：如果留空，一旦提前返回被误删，collectCapacity 会因为
	// "empty space usage response" 先报错，测试便挂在 t.Fatalf(err) 上 ——
	// 报出来的是"返回了错误"，而这个测试真正想守的是"根本没发起查询"。
	// 反向验证时确认过这个差别：给了合法响应之后，缺陷版会精确报出
	// "expected no space usage query, got 1 call"，而不是一句笼统的错误。
	stub := &vsanStub{
		config: &vsantypes.VsanClusterGetConfigResponse{},
		space:  &vsantypes.VsanQuerySpaceUsageResponse{},
		health: vsanHealthResponse("green", nil),
	}

	series, values, err := collectVsanCluster(t, stub, testVsanCluster("domain-c7", "no-vsan"))
	if err != nil {
		t.Fatalf("collectCluster() returned error: %v", err)
	}

	if got := values["vmware_vsan_enabled"]; len(got) != 1 || got[0] != 0 {
		t.Fatalf("expected a single vmware_vsan_enabled sample with value 0, got %v", got)
	}

	if stub.spaceCalls != 0 {
		t.Errorf("expected no space usage query on a cluster without vSAN, got %d call(s)", stub.spaceCalls)
	}
	if stub.healthCalls != 0 {
		t.Errorf("expected no health query on a cluster without vSAN, got %d call(s)", stub.healthCalls)
	}

	// dedup 也不该输出：vSAN 未启用时"有没有开去重"没有意义，
	// 输出一条 0 会让用户以为 vSAN 是开着的但没开去重。
	for _, name := range []string{
		"vmware_vsan_dedup_enabled",
		"vmware_vsan_capacity_bytes",
		"vmware_vsan_health_status",
	} {
		if got, ok := series[name]; ok {
			t.Errorf("expected %s to be absent on a cluster without vSAN, got %d series", name, len(got))
		}
	}
}

// TestVsanCapacityDerivesUsedFromTotalMinusFree 锁死设计稿 2.2.2 的裁定：
// API 只给 total 与 free，used 由我们推导。
//
// 三条都断言，因为把 free 误当成 used 导出（或者反过来）不会报错，只会让
// 容量告警彻底反向 —— 磁盘快满时 used 显示很低，看起来一切正常。
func TestVsanCapacityDerivesUsedFromTotalMinusFree(t *testing.T) {
	const (
		total = int64(10 << 40) // 10 TiB
		free  = int64(3 << 40)  // 3 TiB
	)

	space := &vsantypes.VsanQuerySpaceUsageResponse{}
	space.Returnval.TotalCapacityB = total
	space.Returnval.FreeCapacityB = free

	stub := &vsanStub{
		config: vsanEnabledConfig(false),
		space:  space,
		health: vsanHealthResponse("green", nil),
	}

	_, values, err := collectVsanCluster(t, stub, testVsanCluster("domain-c7", "vsan-cluster"))
	if err != nil {
		t.Fatalf("collectCluster() returned error: %v", err)
	}

	checks := []struct {
		name string
		want float64
	}{
		{"vmware_vsan_capacity_bytes", float64(total)},
		{"vmware_vsan_capacity_free_bytes", float64(free)},
		{"vmware_vsan_capacity_used_bytes", float64(total - free)},
	}

	for _, check := range checks {
		got := values[check.name]
		if len(got) != 1 {
			t.Errorf("expected exactly one %s sample, got %d", check.name, len(got))
			continue
		}
		if got[0] != check.want {
			t.Errorf("%s = %v, want %v", check.name, got[0], check.want)
		}
	}
}

// TestVsanHealthFallsBackToUncachedQuery 锁死抄自 telegraf 的两阶段读取：
// 缓存的健康摘要为空时，关掉缓存重查一次。
//
// 这条降级路径不抄就会静默产出错误数据：vCenter 的健康缓存在刚重启、刚启用
// vSAN、健康服务刚重载时是空的，此时 OverallHealth 是空串。只读缓存的话
// 用户看到的是 unknown，而集群其实是健康的。
//
// 顺序也一并断言（先 true 后 false）。反了的话每轮都强制 vCenter 跑一遍
// 完整健康检查 —— 功能上"正确"，代价上灾难，而且没有任何测试会因此变红。
func TestVsanHealthFallsBackToUncachedQuery(t *testing.T) {
	stub := &vsanStub{
		config: vsanEnabledConfig(false),
		space:  &vsantypes.VsanQuerySpaceUsageResponse{},

		// 缓存里是空串 —— 这是真实的"缓存未就绪"表现，不是报错。
		cachedHealth: vsanHealthResponse("", nil),
		health:       vsanHealthResponse("yellow", nil),
	}

	series, _, err := collectVsanCluster(t, stub, testVsanCluster("domain-c7", "vsan-cluster"))
	if err != nil {
		t.Fatalf("collectCluster() returned error: %v", err)
	}

	if stub.healthCalls != 2 {
		t.Fatalf("expected 2 health queries (cached then uncached), got %d", stub.healthCalls)
	}

	want := []bool{true, false}
	if len(stub.healthFetchFromCache) != 2 ||
		stub.healthFetchFromCache[0] != want[0] || stub.healthFetchFromCache[1] != want[1] {
		t.Errorf("FetchFromCache sequence = %v, want %v (cache first, recompute only as fallback)",
			stub.healthFetchFromCache, want)
	}

	got := series["vmware_vsan_health_status"]
	if len(got) != 1 {
		t.Fatalf("expected exactly one health status series, got %d", len(got))
	}
	if got[0]["status"] != "yellow" {
		t.Errorf("health status = %q, want yellow (the recomputed value)", got[0]["status"])
	}
}

// TestVsanHealthEmitsUnknownWhenBothQueriesFail 锁死与 telegraf 的分歧点。
//
// telegraf 在两次都拿不到值时 return nil（vsan.go:362-363）静默跳过。
// 那样"健康检查失效"和"vSAN 不存在"在指标上完全无法区分 —— 一个该告警、
// 一个不该，而用户只能看到序列缺失。所以我们输出 unknown。
func TestVsanHealthEmitsUnknownWhenBothQueriesFail(t *testing.T) {
	stub := &vsanStub{
		config:            vsanEnabledConfig(false),
		space:             &vsantypes.VsanQuerySpaceUsageResponse{},
		cachedHealth:      vsanHealthResponse("", nil),
		uncachedHealthErr: errors.New("health service unavailable"),
	}

	series, values, err := collectVsanCluster(t, stub, testVsanCluster("domain-c7", "vsan-cluster"))

	// 错误照旧返回（调用方会记日志），但指标不能因此消失。
	if err == nil {
		t.Fatal("expected collectCluster() to report the health failure")
	}

	got := series["vmware_vsan_health_status"]
	if len(got) != 1 {
		t.Fatalf("expected exactly one health status series even when the health service fails, got %d", len(got))
	}
	if got[0]["status"] != vsanHealthUnknown {
		t.Errorf("health status = %q, want %q", got[0]["status"], vsanHealthUnknown)
	}
	if v := values["vmware_vsan_health_status"]; len(v) != 1 || v[0] != 1 {
		t.Errorf("health status value = %v, want [1] (the status lives in the label)", v)
	}

	// 容量查询在健康失败时仍应完成 —— 两者独立降级。
	if _, ok := series["vmware_vsan_capacity_bytes"]; !ok {
		t.Error("expected capacity metrics to survive a health query failure")
	}
}

// TestVsanDiskHealthComesFromHealthSummary 锁死设计稿 2.2.1 的修正：
// 盘健康从健康摘要的 PhysicalDisksHealth 取，不调
// VsanQueryClusterPhysicalDiskHealthSummary（那个要 ESXi root 密码）。
//
// 同时断言请求的 Fields 含 physicalDisksHealth —— 漏了它，真实 vCenter
// 会返回不带盘健康的摘要，盘级指标**静默消失**而集群健康照常。这是最难
// 发现的一类缺陷：所有测试都绿，只有生产环境少了一半指标。
func TestVsanDiskHealthComesFromHealthSummary(t *testing.T) {
	disks := []vsantypes.VsanPhysicalDiskHealthSummary{
		{
			Hostname: "esxi-01.example.com",
			Disks: []vsantypes.VsanPhysicalDiskHealth{
				{
					Name:          "mpx.vmhba1:C0:T1:L0",
					Uuid:          "52a1b2c3-0001",
					SummaryHealth: "green",
					Capacity:      2000 << 30,
					UsedCapacity:  800 << 30,
				},
				{
					Name:          "mpx.vmhba1:C0:T2:L0",
					Uuid:          "52a1b2c3-0002",
					SummaryHealth: "red",
					Capacity:      2000 << 30,
					UsedCapacity:  1900 << 30,
				},
			},
		},
		{
			// 关机或维护模式的主机：Error 非 nil，整台跳过。
			// 不该因此丢掉上面那台的盘 —— 集群里有主机不可达是常态。
			Hostname: "esxi-02.example.com",
			Error:    &vimtypes.HostNotConnected{},
			Disks: []vsantypes.VsanPhysicalDiskHealth{
				{Name: "should-not-appear", SummaryHealth: "green", Capacity: 1 << 30},
			},
		},
	}

	stub := &vsanStub{
		config: vsanEnabledConfig(true),
		space:  &vsantypes.VsanQuerySpaceUsageResponse{},
		health: vsanHealthResponse("green", disks),
	}

	series, values, err := collectVsanCluster(t, stub, testVsanCluster("domain-c7", "vsan-cluster"))
	if err != nil {
		t.Fatalf("collectCluster() returned error: %v", err)
	}

	// 请求里必须带 physicalDisksHealth。
	var hasDisksField bool
	for _, f := range stub.healthFields {
		if f == "physicalDisksHealth" {
			hasDisksField = true
			break
		}
	}
	if !hasDisksField {
		t.Errorf("health summary request Fields = %v, must include physicalDisksHealth "+
			"or disk metrics silently disappear against a real vCenter", stub.healthFields)
	}

	health := series["vmware_vsan_disk_health"]
	if len(health) != 2 {
		t.Fatalf("expected 2 disk health series (the errored host must be skipped), got %d", len(health))
	}

	for _, labels := range health {
		if labels["host"] == "esxi-02.example.com" {
			t.Error("disks of a host that reported an error must not be emitted")
		}
		if labels["device"] == "should-not-appear" {
			t.Error("emitted a disk from the errored host")
		}
	}

	// 盘级容量：两条盘各一条，且 label 集**不含** state 与 uuid。
	// 把会变化的 state 放进数值指标的 label，盘状态一变就产生新序列、
	// 旧序列变僵尸留在 TSDB 里。
	capacity := series["vmware_vsan_disk_capacity_bytes"]
	if len(capacity) != 2 {
		t.Fatalf("expected 2 disk capacity series, got %d", len(capacity))
	}
	for _, labels := range capacity {
		if _, ok := labels["state"]; ok {
			t.Error("disk_capacity_bytes must not carry the state label")
		}
		if _, ok := labels["uuid"]; ok {
			t.Error("disk_capacity_bytes must not carry the uuid label")
		}
	}

	if got := values["vmware_vsan_dedup_enabled"]; len(got) != 1 || got[0] != 1 {
		t.Errorf("vmware_vsan_dedup_enabled = %v, want [1]", got)
	}
}

// TestVsanDiskCapacityOmittedWhenUnknown 锁死"容量为 0 不输出"。
//
// Capacity 是 omitempty 的可选子字段，缓存的健康摘要里常常没有它。
// 输出 0 会让"没拿到容量"看起来像"这块盘容量是 0"，而后者在告警里
// 会被当成盘故障。
func TestVsanDiskCapacityOmittedWhenUnknown(t *testing.T) {
	disks := []vsantypes.VsanPhysicalDiskHealthSummary{{
		Hostname: "esxi-01.example.com",
		Disks: []vsantypes.VsanPhysicalDiskHealth{
			{Name: "mpx.vmhba1:C0:T1:L0", Uuid: "u1", SummaryHealth: "green"},
		},
	}}

	stub := &vsanStub{
		config: vsanEnabledConfig(false),
		space:  &vsantypes.VsanQuerySpaceUsageResponse{},
		health: vsanHealthResponse("green", disks),
	}

	series, _, err := collectVsanCluster(t, stub, testVsanCluster("domain-c7", "vsan-cluster"))
	if err != nil {
		t.Fatalf("collectCluster() returned error: %v", err)
	}

	// 健康序列照常输出 —— 盘存在这件事是确定的，只是容量未知。
	if got := series["vmware_vsan_disk_health"]; len(got) != 1 {
		t.Fatalf("expected the disk health series to be emitted, got %d", len(got))
	}

	for _, name := range []string{
		"vmware_vsan_disk_capacity_bytes",
		"vmware_vsan_disk_capacity_used_bytes",
	} {
		if got, ok := series[name]; ok {
			t.Errorf("expected %s to be absent when the API reports no capacity, got %d series",
				name, len(got))
		}
	}
}

// TestVsanCollectorSkipsESXi 锁死设计稿 2.4 节的降级 1：ESXi 直连整体跳过。
//
// 这条降级能用 vcsim 真测，不需要替身 —— vsan/simulator.go:20 只在 IsVPX()
// 时注册 vSAN 端点，所以 ESX 模型下连 VsanClusterGetConfig 都是 404。
// 真实 ESXi 同理没有 /vsanHealth。
//
// 断言"不创建 client"而不只是"不输出指标"：跳过必须发生在建通道之前，
// 否则每轮 scrape 都会对 ESXi 发一次注定 404 的请求。
func TestVsanCollectorSkipsESXi(t *testing.T) {
	ctx, s, cleanup := setupCollectorScrapeWithModel(t, simulator.ESX())
	defer cleanup()

	s.TargetType = collector.TargetTypeESXi

	var factoryCalls int
	c := &vsanCollector{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		newClient: func(context.Context, *collector.Scrape) (vsanRoundTripper, error) {
			factoryCalls++
			return &vsanStub{}, nil
		},
	}

	ch := make(chan prometheus.Metric, 100)
	if err := c.Update(ctx, ch, s); err != nil {
		t.Fatalf("vsan Update() returned error on ESXi: %v", err)
	}

	if got := drainMetrics(ch); len(got) != 0 {
		t.Errorf("expected no metrics on a direct ESXi connection, got %d", len(got))
	}

	if factoryCalls != 0 {
		t.Errorf("expected the vsan client not to be created on ESXi, got %d call(s)", factoryCalls)
	}
}

// TestVsanCollectorUsesStubOnVPX 是端到端冒烟：走完整的 Update 路径
// （含 ClusterComputeResource 的属性检索）而不只是 collectCluster。
//
// 它抓的是 Update 与 collectCluster 之间的接线错误 —— 比如属性列表漏了
// name，那样每条指标的 vmwcluster label 都会是空串，而 collectCluster
// 的单元测试完全看不到这个问题。
func TestVsanCollectorUsesStubOnVPX(t *testing.T) {
	ctx, s, cleanup := setupCollectorScrape(t)
	defer cleanup()

	stub := &vsanStub{
		config: vsanEnabledConfig(false),
		space:  &vsantypes.VsanQuerySpaceUsageResponse{},
		health: vsanHealthResponse("green", nil),
	}

	c := &vsanCollector{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		newClient: func(context.Context, *collector.Scrape) (vsanRoundTripper, error) {
			return stub, nil
		},
	}

	ch := make(chan prometheus.Metric, 500)
	if err := c.Update(ctx, ch, s); err != nil {
		t.Fatalf("vsan Update() returned error: %v", err)
	}

	metrics := drainMetrics(ch)
	if len(metrics) == 0 {
		t.Fatal("expected the vsan collector to emit metrics on the VPX model")
	}

	// VPX 模型有一个集群（domain-c*），所以 enabled 恰好一条。
	var enabledCount int
	for _, m := range metrics {
		name, labels, _ := labelsOf(t, m)
		if name != "vmware_vsan_enabled" {
			continue
		}
		enabledCount++

		if labels["vmwcluster"] == "" {
			t.Error("vmwcluster label is empty -- the property list must include name")
		}
		if labels["cmo"] == "" {
			t.Error("cmo label is empty")
		}
		if labels["vcenter"] != s.Target {
			t.Errorf("vcenter label = %q, want %q", labels["vcenter"], s.Target)
		}
	}

	if enabledCount == 0 {
		t.Error("expected at least one vmware_vsan_enabled series")
	}
}

// TestVsanDescLabelOrder 锁死每个 Desc 的 variableLabels 顺序。
//
// 顺序错配是静默故障：MustNewConstMetric 按位置传值，把 host 的值配到
// device 上不会 panic、不会报错，只会产出一份看起来正常但内容错位的指标。
// descs.go 的字段注释写了顺序，这个测试是那些注释的执行版本。
func TestVsanDescLabelOrder(t *testing.T) {
	d := descsFor("testns_vsan").vsan

	cases := []struct {
		name string
		desc *prometheus.Desc
		want []string
	}{
		{"enabled", d.enabled, []string{"cmo", "vmwcluster", "vcenter"}},
		{"dedup_enabled", d.dedupEnabled, []string{"cmo", "vmwcluster", "vcenter"}},
		{"capacity_bytes", d.capacityBytes, []string{"cmo", "vmwcluster", "vcenter"}},
		{"capacity_free_bytes", d.capacityFreeBytes, []string{"cmo", "vmwcluster", "vcenter"}},
		{"capacity_used_bytes", d.capacityUsedBytes, []string{"cmo", "vmwcluster", "vcenter"}},
		{"health_status", d.healthStatus, []string{"cmo", "vmwcluster", "status", "vcenter"}},
		{"disk_health", d.diskHealth,
			[]string{"cmo", "vmwcluster", "host", "device", "uuid", "state", "vcenter"}},
		{"disk_capacity_bytes", d.diskCapacityBytes,
			[]string{"cmo", "vmwcluster", "host", "device", "vcenter"}},
		{"disk_capacity_used_bytes", d.diskCapacityUsedBytes,
			[]string{"cmo", "vmwcluster", "host", "device", "vcenter"}},
	}

	for _, tc := range cases {
		if got := descLabelNames(t, tc.desc); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s label order = %v, want %v", tc.name, got, tc.want)
		}
	}
}
