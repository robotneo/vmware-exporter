package vmwareCollectors

import (
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// TestResourcePoolCollectorEmitsPoolInfo 是基本冒烟：vcsim 的 VPX 模型至少有
// 一个资源池（每个 ComputeResource 自带一个 Resources 根池），所以 info 与
// 用量指标都必须出现。
func TestResourcePoolCollectorEmitsPoolInfo(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, s, cleanup := setupCollectorScrape(t)
	defer cleanup()

	c, err := NewresourcepoolCollector(logger)
	if err != nil {
		t.Fatalf("NewresourcepoolCollector() returned error: %v", err)
	}

	ch := make(chan prometheus.Metric, 20000)
	if err := c.Update(ctx, ch, s); err != nil {
		t.Fatalf("resourcepool Update() returned error: %v", err)
	}

	metrics := drainMetrics(ch)
	if len(metrics) == 0 {
		t.Fatal("expected resourcepool collector to emit metrics")
	}

	if !hasMetricWithLabels(metrics, "vmware_resourcepool_info",
		map[string]string{"vcenter": s.Target, "rp": "", "rpmo": ""}) {
		t.Fatal("expected vmware_resourcepool_info with rpmo/rp labels")
	}

	// 用量指标必须存在且带同一套实体标签。这条断言的价值在于它会抓到
	// 「Desc 的 variableLabels 顺序与 MustNewConstMetric 传值顺序错配」——
	// 那种错误不会 panic，只会把 rp 的值配到 rpmo 上。
	if !hasMetricWithLabels(metrics, "vmware_resourcepool_cpu_usage_hertz",
		map[string]string{"vcenter": s.Target, "rp": "", "rpmo": ""}) {
		t.Fatal("expected vmware_resourcepool_cpu_usage_hertz with rpmo/rp labels")
	}

	if !hasMetricWithLabels(metrics, "vmware_resourcepool_overall_status",
		map[string]string{"vcenter": s.Target, "rp": "", "rpmo": "", "status": ""}) {
		t.Fatal("expected vmware_resourcepool_overall_status with a status label")
	}
}

// TestResourcePoolDescLabelOrder 锁死 label 顺序。
//
// 与 host/vm 的同类测试同理：label 一旦从 constLabels 移到 variableLabels，
// 顺序错配既不报错也不 panic，只会静默把值配到错误的 label 上。这个测试是
// 唯一能在改动 Desc 时挡住它的东西。
func TestResourcePoolDescLabelOrder(t *testing.T) {
	d := descsFor("testns_rp").resourcePool

	cases := []struct {
		name string
		desc *prometheus.Desc
		want []string
	}{
		{"info", d.info, []string{"rpmo", "rp", "parentmo", "ownermo", "vcenter"}},
		{"infoSynthetic", d.infoSynthetic, []string{"rpmo", "rp", "parentmo", "ownermo", "vcenter", "synthetic"}},
		{"overallStatus", d.overallStatus, []string{"rpmo", "rp", "status", "vcenter"}},
		{"vm", d.vm, []string{"rpmo", "rp", "vmmo", "vcenter"}},
		{"cpuUsageHertz", d.cpuUsageHertz, []string{"rpmo", "rp", "vcenter"}},
		{"memUsageBytes", d.memUsageBytes, []string{"rpmo", "rp", "vcenter"}},
		{"cpuLimitHertz", d.cpuLimitHertz, []string{"rpmo", "rp", "vcenter"}},
		{"cpuLimited", d.cpuLimited, []string{"rpmo", "rp", "vcenter"}},
		{"memLimited", d.memLimited, []string{"rpmo", "rp", "vcenter"}},
		{"cpuShares", d.cpuShares, []string{"rpmo", "rp", "level", "vcenter"}},
		{"memShares", d.memShares, []string{"rpmo", "rp", "level", "vcenter"}},
	}

	for _, tc := range cases {
		if got := descLabelNames(t, tc.desc); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s label order = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// int64p 与 sharesp 是构造 ResourceAllocationInfo 用的取址助手。
// Reservation / Limit / Shares 在 govmomi 里全是指针，字面量取不了地址。
func int64p(v int64) *int64 { return &v }

func sharesp(level types.SharesLevel, shares int32) *types.SharesInfo {
	return &types.SharesInfo{Shares: shares, Level: level}
}

// emitOnePool 把单个构造好的 mo.ResourcePool 喂给 collector 的产出逻辑，
// 绕开 vcsim。
//
// 为什么不走 vcsim：vcsim 的资源池一律是默认配置（unlimited、normal shares），
// 没有办法让它产出「limit 已设置」「Shares 为 nil」这些必须覆盖的形态。
// 直接构造实体是唯一能覆盖全部分支的做法，而 emitPool 不碰网络，
// 传进去的就是属性检索的产物本身。
func emitOnePool(t *testing.T, pool mo.ResourcePool, esxi bool) map[string][]map[string]string {
	t.Helper()

	series, _ := emitOnePoolWithValues(t, pool, esxi)
	return series
}

// emitOnePoolWithValues 额外返回每条序列的数值。
//
// collectSeries 只保留 label —— 对 _limited 这种「值本身就是全部信息」的
// 指标不够用：序列存在但值为 1，恰好是 unlimited 判断写反时的表现。
func emitOnePoolWithValues(t *testing.T, pool mo.ResourcePool, esxi bool) (map[string][]map[string]string, map[string][]float64) {
	t.Helper()

	c := &resourcepoolCollector{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := &collector.Scrape{Namespace: "vmware", Target: "vc.example.com"}

	// 不要 close(ch)：drainMetrics 用的是 select + default，从已关闭的
	// channel 读会让 case 永远就绪并返回零值，于是它会无限追加 nil 而不是
	// 退出。缓冲区留够大就行，emitPool 写完即返回。
	ch := make(chan prometheus.Metric, 200)
	c.emitPool(ch, s, descsFor(s.Namespace).resourcePool, pool, esxi)

	series := map[string][]map[string]string{}
	values := map[string][]float64{}

	for _, m := range drainMetrics(ch) {
		name, labels, value := labelsOf(t, m)
		series[name] = append(series[name], labels)
		values[name] = append(values[name], value)
	}

	return series, values
}

func testPool(moid, name string) mo.ResourcePool {
	pool := mo.ResourcePool{
		Owner: types.ManagedObjectReference{Type: "ClusterComputeResource", Value: "domain-c7"},
	}
	pool.Self = types.ManagedObjectReference{Type: "ResourcePool", Value: moid}
	pool.Name = name
	pool.Parent = &types.ManagedObjectReference{Type: "ResourcePool", Value: "resgroup-1"}
	pool.OverallStatus = types.ManagedEntityStatusGreen

	return pool
}

// TestResourcePoolLimitUnlimitedOmitsSeries 锁死 D2 的裁定：unlimited 时
// **不输出** limit 序列，改由 _limited=0 表达。
//
// 这条是本 collector 最容易被"顺手改回去"的地方 —— 直接导出 -1 看起来更
// 简单，但那个 -1 会被 PromQL 当成真实数值：limit - usage 算出负数，
// sum(limit) 被污染，而且没有任何东西会报错。
func TestResourcePoolLimitUnlimitedOmitsSeries(t *testing.T) {
	pool := testPool("resgroup-42", "unlimited-pool")

	// -1 是 vSphere 的 unlimited 哨兵，nil 是属性缺失。两者对下游查询等价，
	// 所以这里一个用 -1、一个用 nil，两条路径一并覆盖。
	pool.Config.CpuAllocation = types.ResourceAllocationInfo{
		Reservation: int64p(0),
		Limit:       int64p(-1),
		Shares:      sharesp(types.SharesLevelNormal, 4000),
	}
	pool.Config.MemoryAllocation = types.ResourceAllocationInfo{
		Reservation: int64p(0),
		Limit:       nil,
		Shares:      sharesp(types.SharesLevelNormal, 163840),
	}

	series, values := emitOnePoolWithValues(t, pool, false)

	for _, omitted := range []string{
		"vmware_resourcepool_cpu_limit_hertz",
		"vmware_resourcepool_mem_limit_bytes",
	} {
		if got, ok := series[omitted]; ok {
			t.Errorf("%s was emitted for an unlimited pool (%d series); "+
				"a sentinel value like -1 would be treated as a real number by PromQL",
				omitted, len(got))
		}
	}

	// _limited 必须存在且为 0 —— 否则"有没有配限制"这个信息就彻底丢了，
	// 而缺失的 limit 序列本身无法区分"unlimited"与"collector 挂了"。
	for _, name := range []string{
		"vmware_resourcepool_cpu_limited",
		"vmware_resourcepool_mem_limited",
	} {
		requireSeries(t, series, name)

		got := values[name]
		if len(got) != 1 {
			t.Fatalf("%s: got %d series, want 1", name, len(got))
		}
		if got[0] != 0 {
			t.Errorf("%s = %v for an unlimited pool, want 0", name, got[0])
		}
	}
}

// TestResourcePoolLimitSetEmitsConvertedValue 覆盖 limit 已配置的分支，
// 并锁死两侧的单位换算。
//
// 单位是这里最容易错的地方，而且错了不会有任何报错：CPU 侧的 limit 是 MHz
// （要 ×1e6 变 hertz），内存侧是 MB（要 ×1048576 变字节）。两个系数不同，
// 抄错一个就是 6 个数量级或 20 倍的偏差。
func TestResourcePoolLimitSetEmitsConvertedValue(t *testing.T) {
	pool := testPool("resgroup-43", "limited-pool")

	pool.Config.CpuAllocation = types.ResourceAllocationInfo{
		Reservation: int64p(2000),
		Limit:       int64p(8000), // MHz
		Shares:      sharesp(types.SharesLevelCustom, 12000),
	}
	pool.Config.MemoryAllocation = types.ResourceAllocationInfo{
		Reservation: int64p(1024),
		Limit:       int64p(4096), // MB
		Shares:      sharesp(types.SharesLevelHigh, 327680),
	}

	series, values := emitOnePoolWithValues(t, pool, false)

	wantValues := map[string]float64{
		"vmware_resourcepool_cpu_limit_hertz":       8000 * 1e6,
		"vmware_resourcepool_cpu_reservation_hertz": 2000 * 1e6,
		"vmware_resourcepool_mem_limit_bytes":       4096 * 1048576,
		"vmware_resourcepool_mem_reservation_bytes": 1024 * 1048576,
		"vmware_resourcepool_cpu_limited":           1,
		"vmware_resourcepool_mem_limited":           1,
	}

	for name, want := range wantValues {
		requireSeries(t, series, name)

		got := values[name]
		if len(got) != 1 {
			t.Fatalf("%s: got %d series, want 1", name, len(got))
		}
		if got[0] != want {
			t.Errorf("%s = %v, want %v", name, got[0], want)
		}
	}

	// shares 的 level label 取 vSphere 的原始枚举值，数值取实际 share 数。
	// 两者都要：level 是用户配的语义，数值才能算相对权重。
	cpuShares := requireSeries(t, series, "vmware_resourcepool_cpu_shares")
	if got := cpuShares[0]["level"]; got != "custom" {
		t.Errorf("cpu_shares level label = %q, want %q", got, "custom")
	}
	if got := values["vmware_resourcepool_cpu_shares"][0]; got != 12000 {
		t.Errorf("cpu_shares = %v, want 12000", got)
	}

	memShares := requireSeries(t, series, "vmware_resourcepool_mem_shares")
	if got := memShares[0]["level"]; got != "high" {
		t.Errorf("mem_shares level label = %q, want %q", got, "high")
	}
}

// TestResourcePoolSurvivesNilPointers 是对 telegraf 那个坑的正面防守。
//
// mo.ResourcePool.Parent 与 ResourceAllocationInfo 的
// Reservation/Limit/Shares 全是指针，未设置时为 nil。telegraf 在自己的资源池
// 路径上就漏了同类检查（endpoint.go:801 裸解引用 VirtualMachine.ResourcePool，
// 而模板与孤立 VM 的这个字段可以为 nil），所以这里显式构造一个全 nil 的池。
//
// 期望不是"输出得漂亮"，而是**不 panic**，且缺失的配置项不产出序列而非
// 产出一个 0 —— 0 在 CPU reservation 上是有效值，分不出"没配"和"配成 0"。
func TestResourcePoolSurvivesNilPointers(t *testing.T) {
	pool := mo.ResourcePool{}
	pool.Self = types.ManagedObjectReference{Type: "ResourcePool", Value: "resgroup-99"}
	pool.Name = "bare-pool"
	pool.Parent = nil // 根池之上没有父实体
	pool.OverallStatus = types.ManagedEntityStatusGray

	// Config 整体留零值：CpuAllocation / MemoryAllocation 的三个指针都是 nil。
	series, values := emitOnePoolWithValues(t, pool, false)

	// info 必须照样产出，parentmo 为空串。空串比伪造一个 moid 诚实。
	info := requireSeries(t, series, "vmware_resourcepool_info")
	if got := info[0]["parentmo"]; got != "" {
		t.Errorf("parentmo = %q for a pool with nil Parent, want empty", got)
	}

	// 用量指标来自 Runtime（值类型，零值可用），必须产出。
	requireSeries(t, series, "vmware_resourcepool_cpu_usage_hertz")

	// 配置项全 nil -> 这些序列一条都不该有。
	for _, omitted := range []string{
		"vmware_resourcepool_cpu_reservation_hertz",
		"vmware_resourcepool_mem_reservation_bytes",
		"vmware_resourcepool_cpu_limit_hertz",
		"vmware_resourcepool_mem_limit_bytes",
		"vmware_resourcepool_cpu_shares",
		"vmware_resourcepool_mem_shares",
	} {
		if got, ok := series[omitted]; ok {
			t.Errorf("%s was emitted for a pool with nil allocation pointers (%d series); "+
				"a zero value is indistinguishable from an explicitly configured 0", omitted, len(got))
		}
	}

	// nil limit 与 -1 等价，_limited 仍要输出 0：下游需要能区分
	// "unlimited" 与 "这个池根本没被采到"。
	if got := values["vmware_resourcepool_cpu_limited"]; len(got) != 1 || got[0] != 0 {
		t.Errorf("cpu_limited = %v for nil limit, want exactly one series valued 0", got)
	}
}

// TestResourcePoolESXiRootPoolIsSynthetic 锁死 ESXi 伪对象的标注。
//
// 与 datacenter 对 ha-datacenter、cluster 对 ha-compute-res 的既有处理一致：
// ESXi 上不存在用户创建的资源池，ha-root-pool 是为了让 API 形状与 vCenter
// 一致而存在的占位对象。标 synthetic 让这件事在指标层面可见，而不是静默
// 混进真实数据里。
func TestResourcePoolESXiRootPoolIsSynthetic(t *testing.T) {
	pool := testPool(syntheticRootPoolMoid, "Resources")

	series := emitOnePool(t, pool, true)
	info := requireSeries(t, series, "vmware_resourcepool_info")

	if got := info[0]["synthetic"]; got != "true" {
		t.Errorf(`ha-root-pool on ESXi has synthetic=%q, want "true"`, got)
	}

	// 同一个池在 vCenter 上不该被标 synthetic —— vCenter 下叫 Resources 的
	// 根池是真实托管对象。这半边同样重要：只测正向的话，"无条件标 synthetic"
	// 这个实现也会通过。
	vcSeries := emitOnePool(t, testPool("resgroup-8", "Resources"), false)
	vcInfo := requireSeries(t, vcSeries, "vmware_resourcepool_info")

	if _, ok := vcInfo[0]["synthetic"]; ok {
		t.Error("a real vCenter resource pool was labelled synthetic")
	}
}

// TestResourcePoolSeriesCountIsLinearInEntities 验证序列数随实体数线性增长。
//
// 设计稿 1.5 节的要求。默认启用之后规模风险不能再靠"用户没开"回避，而这个
// collector 有一处天然的二次陷阱：给 VM 打资源池标签时，若照 telegraf 的做法
// 从 VM 侧反查池名（endpoint.go:737-745 在每台 VM 的循环里线性扫池 map），
// 复杂度就是 O(VM 数 × 池数)。
//
// 这里断言的是**序列数**而非耗时：耗时在 CI 上不稳定，而"每个池固定条数 +
// 每虚机一条"这个形状一旦被写成嵌套遍历，序列数会立刻爆成乘积关系。
// 序列数是可断言的代理指标，且失败信息能直接指出错在哪。
func TestResourcePoolSeriesCountIsLinearInEntities(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 默认 VPX 模型太小，池数与机器数都不足以让线性和二次区分开。
	model := simulator.VPX()
	model.Pool = 3
	model.Machine = 4

	ctx, s, cleanup := setupCollectorScrapeWithModel(t, model)
	defer cleanup()

	c, err := NewresourcepoolCollector(logger)
	if err != nil {
		t.Fatalf("NewresourcepoolCollector() returned error: %v", err)
	}

	ch := make(chan prometheus.Metric, 100000)
	if err := c.Update(ctx, ch, s); err != nil {
		t.Fatalf("resourcepool Update() returned error: %v", err)
	}

	series := collectSeries(t, ch)

	pools := len(requireSeries(t, series, "vmware_resourcepool_info"))
	if pools < 2 {
		t.Fatalf("simulator produced only %d resource pool(s); the linearity "+
			"assertion below cannot distinguish linear from quadratic", pools)
	}

	// info / overall_status / cpu_limited / mem_limited 是每池必出的四条
	// （前两条是身份，后两条无论 limit 是否配置都输出）。它们的条数必须与
	// 池数严格相等 —— 多出来就意味着某处在池的循环里又套了一层遍历。
	for _, perPool := range []string{
		"vmware_resourcepool_overall_status",
		"vmware_resourcepool_cpu_limited",
		"vmware_resourcepool_mem_limited",
		"vmware_resourcepool_cpu_usage_hertz",
		"vmware_resourcepool_mem_usage_bytes",
	} {
		if got := len(requireSeries(t, series, perPool)); got != pools {
			t.Errorf("%s has %d series for %d pools; want exactly one per pool "+
				"(a mismatch means the emit path gained a nested loop)", perPool, got, pools)
		}
	}

	// resourcepool_vm 是基数主项，按定义是「一虚机一条」。它的总数必须等于
	// 各池 vm 数之和，而不是池数 × 虚机数。
	vmSeries := series["vmware_resourcepool_vm"]
	seen := map[string]bool{}
	for _, labels := range vmSeries {
		key := labels["rpmo"] + "/" + labels["vmmo"]
		if seen[key] {
			t.Errorf("duplicate resourcepool_vm series for %s; the same VM was "+
				"emitted twice under the same pool", key)
		}
		seen[key] = true
	}

	// 每台虚机只能归属一个资源池，所以 vmmo 在全部序列里不允许重复。
	// 这条能抓到「把每个池的 vm 列表拼给了所有池」这类错误 —— 那种实现下
	// 序列总数恰好是乘积，且每条看起来都合法。
	byVM := map[string]int{}
	for _, labels := range vmSeries {
		byVM[labels["vmmo"]]++
	}
	for vmmo, count := range byVM {
		if count > 1 {
			t.Errorf("VM %s appears in %d resource pools; a VM belongs to exactly one, "+
				"so the pool-to-VM mapping is being cross-joined", vmmo, count)
		}
	}
}
