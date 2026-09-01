package vmwareCollectors

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/types"
)

// descLabelNames 从 Desc 的 String() 里解析出 variableLabels 的名字与顺序。
//
// client_golang 没有导出读取 variableLabels 的接口，但 Desc.String() 的格式是
// 稳定的，形如：
//
//	Desc{fqName: "vmware_host_info", help: "...", constLabels: {}, variableLabels: {hostmo,host,cmo,vcenter}}
//
// 解析它是为了能断言顺序。这在这次改造里是必需的：label 从 constLabels 移到
// variableLabels 之后，顺序错配不会报错，只会把值静默配到错误的 label 上。
func descLabelNames(t *testing.T, d *prometheus.Desc) []string {
	t.Helper()

	s := d.String()

	const marker = "variableLabels: {"
	start := strings.Index(s, marker)
	if start < 0 {
		t.Fatalf("could not find variableLabels in Desc: %s", s)
	}

	rest := s[start+len(marker):]
	end := strings.Index(rest, "}")
	if end < 0 {
		t.Fatalf("malformed variableLabels in Desc: %s", s)
	}

	inner := rest[:end]
	if inner == "" {
		return nil
	}

	return strings.Split(inner, ",")
}

func TestDescsForCachesPerNamespace(t *testing.T) {
	first := descsFor("testns_cache")
	second := descsFor("testns_cache")

	if first != second {
		t.Fatal("descsFor() returned a different pointer for the same namespace; the cache is not working")
	}

	// 同一个 Desc 指针必须复用 —— 这正是把 Desc 提出热路径的目的。
	if first.host.info != second.host.info {
		t.Fatal("host info Desc was rebuilt on the second call")
	}

	other := descsFor("testns_cache_other")
	if other == first {
		t.Fatal("descsFor() returned the same set for different namespaces")
	}

	if !strings.HasPrefix(other.host.info.String(), `Desc{fqName: "testns_cache_other_host_info"`) {
		t.Fatalf("namespace was not applied to fqName: %s", other.host.info.String())
	}
}

// TestHostDescLabelOrder 锁死 host collector 的 label 顺序。
//
// 这些期望值必须与 host.go 里 MustNewConstMetric 的传值顺序逐一对应。改动任何
// 一处就必须同步改另一处 —— 这个测试就是那个"必须"的执行者。
func TestHostDescLabelOrder(t *testing.T) {
	d := descsFor("testns_hostlabels").host

	for _, tc := range []struct {
		name string
		desc *prometheus.Desc
		want []string
	}{
		{"info", d.info, []string{"hostmo", "host", "cmo", "vcenter"}},
		{"hardware_info", d.hardwareInfo, []string{"hostmo", "host", "vendor", "model", "cpu_type", "vcenter"}},
		{"software_info", d.softwareInfo, []string{"hostmo", "host", "software", "version", "build", "vcenter"}},
		{"cpu_corecount", d.cpuCoreCount, []string{"hostmo", "host", "vcenter"}},
		{"cpu_threadcount", d.cpuThreadCount, []string{"hostmo", "host", "vcenter"}},
		{"cpu_capacity", d.cpuCapacity, []string{"hostmo", "host", "vcenter"}},
		{"cpu_capacity_mhz", d.cpuCapacityMHz, []string{"hostmo", "host", "vcenter"}},
		{"mem_capacity", d.memCapacity, []string{"hostmo", "host", "vcenter"}},
		{"mem_capacity_bytes", d.memCapacityBytes, []string{"hostmo", "host", "vcenter"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := descLabelNames(t, tc.desc)
			assertLabelOrder(t, got, tc.want)
		})
	}
}

func TestVMDescLabelOrder(t *testing.T) {
	d := descsFor("testns_vmlabels").vm

	for _, tc := range []struct {
		name string
		desc *prometheus.Desc
		want []string
	}{
		{"info", d.info, []string{"vmmo", "vm", "hostmo", "vcenter"}},
		{"cpu_corecount", d.cpuCoreCount, []string{"vmmo", "vm", "hostmo", "vcenter"}},
		{"mem_capacity", d.memCapacity, []string{"vmmo", "vm", "hostmo", "vcenter"}},
		{"datastore_capacity_used", d.dsCapacityUsed, []string{"vmmo", "vm", "vcenter", "dsmo"}},
		{"datastore_capacity_used_bytes", d.dsCapacityUsedBytes, []string{"vmmo", "vm", "vcenter", "dsmo"}},
		{"snapshot_info", d.snapshotInfo, []string{"vmmo", "vm", "vcenter", "name"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := descLabelNames(t, tc.desc)
			assertLabelOrder(t, got, tc.want)
		})
	}
}

// TestSnapshotDescHasNoCreatedLabel 是 P1-4 的回归防线。
//
// created 是 CreateTime 的 RFC3339 形式，而 metric value 就是同一时间戳的
// Unix 秒数 —— label 里那份完全冗余，且时间戳做 label 会让每个快照占一条
// 独立序列，快照删除后序列仍以僵尸形式留在 TSDB 里直到过期。
func TestSnapshotDescHasNoCreatedLabel(t *testing.T) {
	d := descsFor("testns_snap").vm

	for _, name := range descLabelNames(t, d.snapshotInfo) {
		if name == "created" {
			t.Fatalf("snapshot_info still carries the high-cardinality created label: %s",
				d.snapshotInfo.String())
		}
	}
}

func assertLabelOrder(t *testing.T, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("label count = %d %v, want %d %v", len(got), got, len(want), want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("label[%d] = %q, want %q;\nfull order got=%v want=%v\n"+
				"variableLabels order must match the MustNewConstMetric argument order exactly, "+
				"otherwise values are silently assigned to the wrong labels",
				i, got[i], want[i], got, want)
		}
	}
}

func TestPerfEntityLabels(t *testing.T) {
	for _, tc := range []struct {
		moType string
		want   []string
	}{
		{"HostSystem", []string{"host", "hostmo"}},
		{"VirtualMachine", []string{"vm", "vmmo"}},
		{"Datastore", []string{"ds", "dsmo"}},
	} {
		t.Run(tc.moType, func(t *testing.T) {
			assertLabelOrder(t, perfEntityLabels(tc.moType), tc.want)
		})
	}

	// 未知类型必须返回 nil，让调用方跳过。原实现的 switch 没有 default，
	// 未知类型会产出只带 vcenter label 的指标 —— 该类型下所有实体的序列
	// 互相覆盖，最后只剩一条，而且完全看不出问题在哪。
	if got := perfEntityLabels("ResourcePool"); got != nil {
		t.Fatalf("perfEntityLabels(unknown) = %v, want nil so the caller skips instead of emitting a collapsed series", got)
	}
}

func TestPerfDescCachesAndDistinguishesInstanced(t *testing.T) {
	counterInfo := &types.PerfCounterInfo{
		NameInfo: &types.ElementDescription{
			Description: types.Description{Summary: "CPU usage"},
		},
		UnitInfo: &types.ElementDescription{
			Description: types.Description{Label: "percent"},
		},
	}

	plain := perfDesc("testns_perf", "host", "cpu.usage.average", "HostSystem", false, counterInfo)
	again := perfDesc("testns_perf", "host", "cpu.usage.average", "HostSystem", false, counterInfo)

	if plain != again {
		t.Fatal("perfDesc() rebuilt the Desc for identical arguments; the cache is not working")
	}

	instanced := perfDesc("testns_perf", "host", "cpu.usage.average", "HostSystem", true, counterInfo)
	if instanced == plain {
		t.Fatal("instanced and non-instanced variants must not share a Desc, their label sets differ")
	}

	assertLabelOrder(t, descLabelNames(t, plain), []string{"vcenter", "host", "hostmo"})
	assertLabelOrder(t, descLabelNames(t, instanced), []string{"vcenter", "host", "hostmo", "pfinstance"})

	// 计数器名里的点必须转成下划线，否则不是合法的 Prometheus 指标名。
	if !strings.HasPrefix(plain.String(), `Desc{fqName: "testns_perf_host_cpu_usage_average"`) {
		t.Fatalf("counter name was not sanitised: %s", plain.String())
	}
}

// ---------------------------------------------------------------------------
// 批次 2：端到端回读
//
// 批次 1 只证明了 Desc **声明** 的 label 顺序，这不够。真正的风险在另一侧：
// collector 用 MustNewConstMetric 传值的顺序是否与声明一致。两者错配时
// client_golang 不会报任何错 —— label 个数对得上就照样产出指标，只是值配错了
// label。表现出来就是 vendor="DC0_H0"、host="VMware, Inc." 这种，
// 查询语法合法、指标存在、数据全错。
//
// 所以下面走真实采集路径（simulator + collector.Update），回读每条序列的
// 键值配对。simulator 的字段值互不相同且各有辨识度，这是断言能生效的前提：
//
//	Vendor   = "VMware, Inc."
//	Model    = "VMware Virtual Platform"
//	CpuModel = "Intel(R) Core(TM) i7-3615QM CPU @ 2.30GHz"
//	Product  = VMware ESXi / 8.0.2 / 21997540
//	CpuMhz   = 2294
//	MemorySize = 4294430720
//
// 一旦顺序错配，这些值会落到别的 label 上，断言必然失败。
// ---------------------------------------------------------------------------

// labelsOf 把一条 metric 回读成 (fqName, labels) 二元组。
func labelsOf(t *testing.T, m prometheus.Metric) (string, map[string]string, float64) {
	t.Helper()

	desc := m.Desc().String()
	i := strings.Index(desc, `fqName: "`)
	if i < 0 {
		t.Fatalf("malformed Desc: %s", desc)
	}
	name := desc[i+len(`fqName: "`):]
	j := strings.Index(name, `"`)
	if j < 0 {
		t.Fatalf("malformed Desc: %s", desc)
	}
	name = name[:j]

	pb := &dto.Metric{}
	if err := m.Write(pb); err != nil {
		t.Fatalf("metric.Write() for %s returned error: %v", name, err)
	}

	labels := make(map[string]string, len(pb.Label))
	for _, l := range pb.Label {
		labels[l.GetName()] = l.GetValue()
	}

	return name, labels, pb.GetGauge().GetValue()
}

// collectSeries 跑一轮采集并按 fqName 归组回读结果。
func collectSeries(t *testing.T, ch <-chan prometheus.Metric) map[string][]map[string]string {
	t.Helper()

	out := map[string][]map[string]string{}
	for _, m := range drainMetrics(ch) {
		name, labels, _ := labelsOf(t, m)
		out[name] = append(out[name], labels)
	}
	return out
}

// collectValues 同 collectSeries，但保留 metric value。
func collectValues(t *testing.T, ch <-chan prometheus.Metric) map[string][]float64 {
	t.Helper()

	out := map[string][]float64{}
	for _, m := range drainMetrics(ch) {
		name, _, v := labelsOf(t, m)
		out[name] = append(out[name], v)
	}
	return out
}

// requireSeries 取指定指标的全部序列，一条都没有就直接失败 —— 指标压根没产出
// 时，后面的 label 断言会全部跳过而测试照样通过，那是最糟的假通过。
func requireSeries(t *testing.T, series map[string][]map[string]string, name string) []map[string]string {
	t.Helper()

	got, ok := series[name]
	if !ok || len(got) == 0 {
		t.Fatalf("%s was not emitted at all; emitted metrics: %v", name, sortedKeys(series))
	}
	return got
}

func sortedKeys(m map[string][]map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// moidPattern 匹配 managed object reference 的 Value 形态。
//
// 实际形态比想象的杂：host-21、vm-62、datastore-59，以及 ComputeResource 的
// domain-s24（standalone）与 domain-c28（cluster）—— 类型前缀后面可能跟一个
// 单字母的种类标记，紧贴着数字，中间没有分隔符。
//
// 用形态而非具体编号来区分 "moid" 与 "人类可读名字"，是本批次的核心手段：
// host 与 hostmo 这类成对 label 一旦互换，两侧的形态都会不匹配，
// 而断言不必依赖 simulator 具体分配到哪个编号。
var moidPattern = regexp.MustCompile(`^[a-z]+-[a-z]?\d+$`)

func assertIsMoid(t *testing.T, metric, label, value, wantPrefix string) {
	t.Helper()

	if !moidPattern.MatchString(value) {
		t.Errorf("%s: label %q = %q, want a managed object id like %s42 or %ss42.\n"+
			"  a human-readable name here means this label and its companion are swapped",
			metric, label, value, wantPrefix, wantPrefix)
		return
	}
	if !strings.HasPrefix(value, wantPrefix) {
		t.Errorf("%s: label %q = %q, want prefix %q", metric, label, value, wantPrefix)
	}
}

func assertIsNotMoid(t *testing.T, metric, label, value string) {
	t.Helper()

	if value == "" {
		t.Errorf("%s: label %q is empty, want a human-readable name", metric, label)
		return
	}
	if moidPattern.MatchString(value) {
		t.Errorf("%s: label %q = %q, want a human-readable name but got a managed object id.\n"+
			"  this label and its companion moid label are swapped",
			metric, label, value)
	}
}

// assertLabels 逐个断言 label 的键值配对。
func assertLabels(t *testing.T, metric string, got, want map[string]string) {
	t.Helper()

	for key, expected := range want {
		actual, ok := got[key]
		if !ok {
			t.Errorf("%s: label %q is missing; got %v", metric, key, got)
			continue
		}
		if actual != expected {
			t.Errorf("%s: label %q = %q, want %q\n"+
				"  this is what a variableLabels order mismatch looks like: the value is real\n"+
				"  data, just attached to the wrong label. full series: %v",
				metric, key, actual, expected, got)
		}
	}
}

// entityPairs 抽取每条序列的 (moidLabel, nameLabel) 配对，用于跨 Desc 交叉验证。
func entityPairs(series []map[string]string, moidLabel, nameLabel string) map[string]string {
	out := make(map[string]string, len(series))
	for _, s := range series {
		out[s[moidLabel]] = s[nameLabel]
	}
	return out
}

func TestHostCollectorLabelValuePairing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loginData, cleanup := setupCollectorLoginData(t)
	defer cleanup()

	collector, err := NewhostCollector(logger)
	if err != nil {
		t.Fatalf("NewhostCollector() returned error: %v", err)
	}

	ch := make(chan prometheus.Metric, 40000)
	if err := collector.Update(ch, "vmware", nil, loginData, map[string]string{}); err != nil {
		t.Fatalf("host Update() returned error: %v", err)
	}

	series := collectSeries(t, ch)
	target := loginData["target"].(string)

	// hardware_info 是最有说服力的一条：六个 label 的值互不相同，
	// 任意两个位置调换都会被下面的断言抓到。
	hardware := requireSeries(t, series, "vmware_host_hardware_info")
	for _, s := range hardware {
		assertLabels(t, "vmware_host_hardware_info", s, map[string]string{
			"vendor":   "VMware, Inc.",
			"model":    "VMware Virtual Platform",
			"cpu_type": "Intel(R) Core(TM) i7-3615QM CPU @ 2.30GHz",
			"vcenter":  target,
		})
		assertIsMoid(t, "vmware_host_hardware_info", "hostmo", s["hostmo"], "host-")
		assertIsNotMoid(t, "vmware_host_hardware_info", "host", s["host"])
	}

	software := requireSeries(t, series, "vmware_host_software_info")
	for _, s := range software {
		assertLabels(t, "vmware_host_software_info", s, map[string]string{
			"software": "VMware ESXi",
			"version":  "8.0.2",
			"build":    "21997540",
			"vcenter":  target,
		})
		assertIsMoid(t, "vmware_host_software_info", "hostmo", s["hostmo"], "host-")
		assertIsNotMoid(t, "vmware_host_software_info", "host", s["host"])
	}

	// info 的 cmo 是 ComputeResource 的 moid，与 hostmo 同为 moid 形态，
	// 靠前缀区分（domain-* vs host-*）确保两者没有互换。
	info := requireSeries(t, series, "vmware_host_info")
	for _, s := range info {
		assertLabels(t, "vmware_host_info", s, map[string]string{"vcenter": target})
		assertIsMoid(t, "vmware_host_info", "hostmo", s["hostmo"], "host-")
		assertIsMoid(t, "vmware_host_info", "cmo", s["cmo"], "domain-")
		assertIsNotMoid(t, "vmware_host_info", "host", s["host"])
	}

	// 交叉验证：同一个 hostmo 在每个 Desc 上都必须映射到同一个 host 名。
	// 单个 Desc 内部顺序错配可以被上面的形态断言抓到，但如果有人把某一个
	// Desc 的 hostmo/host 两个位置一起换了，形态断言就失效了 —— 那种情况下
	// 这个交叉比对是唯一的防线。
	want := entityPairs(hardware, "hostmo", "host")
	for _, name := range []string{
		"vmware_host_info",
		"vmware_host_software_info",
		"vmware_host_cpu_corecount",
		"vmware_host_cpu_threadcount",
		"vmware_host_cpu_capacity",
		"vmware_host_cpu_capacity_mhz",
		"vmware_host_mem_capacity",
		"vmware_host_mem_capacity_bytes",
	} {
		got := entityPairs(requireSeries(t, series, name), "hostmo", "host")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: hostmo->host mapping %v disagrees with vmware_host_hardware_info %v",
				name, got, want)
		}
	}

	for _, name := range []string{
		"vmware_host_cpu_corecount",
		"vmware_host_cpu_threadcount",
		"vmware_host_cpu_capacity",
		"vmware_host_cpu_capacity_mhz",
		"vmware_host_mem_capacity",
		"vmware_host_mem_capacity_bytes",
	} {
		for _, s := range requireSeries(t, series, name) {
			assertLabels(t, name, s, map[string]string{"vcenter": target})
			// 这些 Desc 只有三个 label，其中两个是成对的 moid/name。
			// 顺序一错，moid 位置上就会出现主机名。
			assertIsMoid(t, name, "hostmo", s["hostmo"], "host-")
			assertIsNotMoid(t, name, "host", s["host"])
		}
	}
}

func TestVMCollectorLabelValuePairing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loginData, cleanup := setupCollectorLoginData(t)
	defer cleanup()

	collector, err := NewvmCollector(logger)
	if err != nil {
		t.Fatalf("NewvmCollector() returned error: %v", err)
	}

	ch := make(chan prometheus.Metric, 60000)
	if err := collector.Update(ch, "vmware", nil, loginData, map[string]string{}); err != nil {
		t.Fatalf("vm Update() returned error: %v", err)
	}

	series := collectSeries(t, ch)
	target := loginData["target"].(string)

	// vmmo, vm, hostmo, vcenter —— 三个实体 label 里有两个是 moid，
	// 靠前缀（vm- / host-）区分，确保 vmmo 与 hostmo 没有互换。
	info := requireSeries(t, series, "vmware_vm_info")
	for _, s := range info {
		assertLabels(t, "vmware_vm_info", s, map[string]string{"vcenter": target})
		assertIsMoid(t, "vmware_vm_info", "vmmo", s["vmmo"], "vm-")
		assertIsMoid(t, "vmware_vm_info", "hostmo", s["hostmo"], "host-")
		assertIsNotMoid(t, "vmware_vm_info", "vm", s["vm"])
	}

	for _, name := range []string{"vmware_vm_cpu_corecount", "vmware_vm_mem_capacity"} {
		for _, s := range requireSeries(t, series, name) {
			assertLabels(t, name, s, map[string]string{"vcenter": target})
			assertIsMoid(t, name, "vmmo", s["vmmo"], "vm-")
			assertIsMoid(t, name, "hostmo", s["hostmo"], "host-")
			assertIsNotMoid(t, name, "vm", s["vm"])
		}
	}

	// datastore_capacity_used 的 label 顺序是 vmmo, vm, vcenter, dsmo ——
	// 注意 vcenter 夹在中间，与其他 Desc 不同。这正是顺序容易写错的地方：
	// 如果按 "实体 label 在前、vcenter 在后" 的直觉传值，dsmo 位置上会出现
	// target 地址，而 vcenter 上会出现 datastore moid。
	for _, name := range []string{
		"vmware_vm_datastore_capacity_used",
		"vmware_vm_datastore_capacity_used_bytes",
	} {
		for _, s := range requireSeries(t, series, name) {
			assertLabels(t, name, s, map[string]string{"vcenter": target})
			assertIsMoid(t, name, "vmmo", s["vmmo"], "vm-")
			assertIsMoid(t, name, "dsmo", s["dsmo"], "datastore-")
			assertIsNotMoid(t, name, "vm", s["vm"])
		}
	}

	// 交叉验证 vmmo -> vm 的映射在每个 Desc 上一致。
	want := entityPairs(info, "vmmo", "vm")
	for _, name := range []string{
		"vmware_vm_cpu_corecount",
		"vmware_vm_mem_capacity",
		"vmware_vm_datastore_capacity_used",
		"vmware_vm_datastore_capacity_used_bytes",
	} {
		got := entityPairs(requireSeries(t, series, name), "vmmo", "vm")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: vmmo->vm mapping %v disagrees with vmware_vm_info %v", name, got, want)
		}
	}
}

// TestDeprecatedAndReplacementMetricsAgree 锁死双写过渡期的核心保证：
// 旧指标与新指标必须**逐条序列**同值。
//
// 这三对指标之所以是双写而不是改名，是因为随仓库分发的 dashboard 大量引用旧名
// （host_mem_capacity 29 处、host_cpu_capacity 20 处）。双写的意义在于用户可以
// 分批迁移，而迁移的前提是两者数值可以互换 —— 如果哪天有人给新指标加了单位换算
// 却忘了旧指标，用户在迁移过程中会看到图表数值突变，且没有任何报错。
func TestDeprecatedAndReplacementMetricsAgree(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loginData, cleanup := setupCollectorLoginData(t)
	defer cleanup()

	hostCol, err := NewhostCollector(logger)
	if err != nil {
		t.Fatalf("NewhostCollector() returned error: %v", err)
	}
	hostCh := make(chan prometheus.Metric, 40000)
	if err := hostCol.Update(hostCh, "vmware", nil, loginData, map[string]string{}); err != nil {
		t.Fatalf("host Update() returned error: %v", err)
	}

	vmCol, err := NewvmCollector(logger)
	if err != nil {
		t.Fatalf("NewvmCollector() returned error: %v", err)
	}
	vmCh := make(chan prometheus.Metric, 60000)
	if err := vmCol.Update(vmCh, "vmware", nil, loginData, map[string]string{}); err != nil {
		t.Fatalf("vm Update() returned error: %v", err)
	}

	// 按 (fqName, 序列化后的 label 集) 索引取值，逐序列比对而不是比总和 ——
	// 比总和会让 "两条序列的值互换" 这类错误蒙混过关。
	values := map[string]map[string]float64{}
	for _, ch := range []<-chan prometheus.Metric{hostCh, vmCh} {
		for _, m := range drainMetrics(ch) {
			name, labels, v := labelsOf(t, m)
			if values[name] == nil {
				values[name] = map[string]float64{}
			}
			values[name][labelKey(labels)] = v
		}
	}

	pairs := []struct {
		deprecated, replacement string
		// 双写的两个指标 label 集完全相同，所以 key 可以直接比。
	}{
		{"vmware_host_cpu_capacity", "vmware_host_cpu_capacity_mhz"},
		{"vmware_host_mem_capacity", "vmware_host_mem_capacity_bytes"},
		{"vmware_vm_datastore_capacity_used", "vmware_vm_datastore_capacity_used_bytes"},
	}

	for _, p := range pairs {
		t.Run(p.deprecated, func(t *testing.T) {
			oldVals, ok := values[p.deprecated]
			if !ok || len(oldVals) == 0 {
				t.Fatalf("%s was not emitted; the deprecated metric must keep working for a full release cycle", p.deprecated)
			}
			newVals, ok := values[p.replacement]
			if !ok || len(newVals) == 0 {
				t.Fatalf("%s was not emitted; the replacement metric is missing", p.replacement)
			}

			if len(oldVals) != len(newVals) {
				t.Fatalf("series count mismatch: %s has %d, %s has %d",
					p.deprecated, len(oldVals), p.replacement, len(newVals))
			}

			for key, want := range oldVals {
				got, ok := newVals[key]
				if !ok {
					t.Errorf("%s has series %s but %s does not; the label sets must stay identical "+
						"so users can swap one for the other", p.deprecated, key, p.replacement)
					continue
				}
				if got != want {
					t.Errorf("%s{%s} = %v but %s = %v; during the deprecation window the two must "+
						"be numerically interchangeable", p.deprecated, key, want, p.replacement, got)
				}
			}
		})
	}

	// 顺带确认 deprecation 说明真的挂在旧指标上、且指向的名字与实际注册的一致。
	// help 里手写替代指标名很容易与 BuildFQName 的结果脱钩。
	descs := descsFor("vmware")
	for _, tc := range []struct {
		name string
		desc *prometheus.Desc
		want string
	}{
		{"host cpu_capacity", descs.host.cpuCapacity, "vmware_host_cpu_capacity_mhz"},
		{"host mem_capacity", descs.host.memCapacity, "vmware_host_mem_capacity_bytes"},
		{"vm datastore_capacity_used", descs.vm.dsCapacityUsed, "vmware_vm_datastore_capacity_used_bytes"},
	} {
		s := tc.desc.String()
		if !strings.Contains(s, "DEPRECATED") {
			t.Errorf("%s: help text carries no DEPRECATED marker: %s", tc.name, s)
		}
		if !strings.Contains(s, tc.want) {
			t.Errorf("%s: help text does not point at %q: %s", tc.name, tc.want, s)
		}
	}

	// vm_mem_capacity 是唯一一个 help 本来就正确的（MemorySizeMB 确实是 MB），
	// 所以它不该被标记 deprecated，也不该有 _bytes 变体 —— 换算单位会改变数值，
	// 属于另一类破坏性变更。
	if s := descs.vm.memCapacity.String(); strings.Contains(s, "DEPRECATED") {
		t.Errorf("vmware_vm_mem_capacity must not be deprecated, its help was already correct: %s", s)
	}
	if _, ok := values["vmware_vm_mem_capacity_bytes"]; ok {
		t.Error("vmware_vm_mem_capacity_bytes must not exist; converting MB to bytes would change the value")
	}
}

// labelKey 把 label 集序列化成稳定的字符串，用作跨指标比对的键。
func labelKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}

// TestSnapshotInfoKeepsTimestampAsValue 是 P1-4 的另一半防线。
//
// 批次 1 已经断言 Desc 上没有 created label，但那只证明 label 没了 —— 如果
// 有人顺手把 metric value 也一起改掉（例如改成常量 1.0），"移除冗余 label" 就
// 变成了 "丢失快照创建时间"，而这是该指标唯一的信息量。
//
// created label 被移除的理由是它与 value 冗余：label 里是 RFC3339 字符串，
// value 是同一时刻的 Unix 秒数。前提正是 value 携带时间戳，所以必须锁住。
func TestSnapshotInfoKeepsTimestampAsValue(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loginData, cleanup := setupCollectorLoginData(t)
	defer cleanup()

	ctx := loginData["ctx"].(context.Context)
	client := loginData["client"].(*vim25.Client)

	// simulator 默认不带快照，得自己造一个。
	finder := find.NewFinder(client, false)
	vms, err := finder.VirtualMachineList(ctx, "/DC0/vm/*")
	if err != nil {
		t.Fatalf("VirtualMachineList() returned error: %v", err)
	}
	if len(vms) == 0 {
		t.Fatal("simulator produced no virtual machines")
	}

	before := time.Now().Add(-time.Minute)

	task, err := vms[0].CreateSnapshot(ctx, "batch3-snap", "created by the test", false, false)
	if err != nil {
		t.Fatalf("CreateSnapshot() returned error: %v", err)
	}
	if err := task.Wait(ctx); err != nil {
		t.Fatalf("CreateSnapshot task failed: %v", err)
	}

	after := time.Now().Add(time.Minute)

	collector, err := NewvmCollector(logger)
	if err != nil {
		t.Fatalf("NewvmCollector() returned error: %v", err)
	}

	ch := make(chan prometheus.Metric, 60000)
	if err := collector.Update(ch, "vmware", nil, loginData, map[string]string{}); err != nil {
		t.Fatalf("vm Update() returned error: %v", err)
	}

	found := false
	for _, m := range drainMetrics(ch) {
		name, labels, value := labelsOf(t, m)
		if name != "vmware_vm_snapshot_info" {
			continue
		}
		found = true

		if _, ok := labels["created"]; ok {
			t.Errorf("vmware_vm_snapshot_info still carries a created label: %v\n"+
				"  a timestamp as a label gives every snapshot its own series, and those series\n"+
				"  linger in the TSDB as zombies after the snapshot is deleted", labels)
		}

		assertLabels(t, "vmware_vm_snapshot_info", labels, map[string]string{
			"vcenter": loginData["target"].(string),
			"name":    "batch3-snap",
		})
		assertIsMoid(t, "vmware_vm_snapshot_info", "vmmo", labels["vmmo"], "vm-")
		assertIsNotMoid(t, "vmware_vm_snapshot_info", "vm", labels["vm"])

		// value 必须落在快照创建前后的时间窗内。这既排除了 "改成常量 1.0"，
		// 也排除了 "误用毫秒/纳秒" 这类量级错误。
		if value < float64(before.Unix()) || value > float64(after.Unix()) {
			t.Errorf("vmware_vm_snapshot_info value = %v, want a Unix timestamp in [%d, %d]\n"+
				"  the value is the only place the creation time survives now that the\n"+
				"  created label is gone",
				value, before.Unix(), after.Unix())
		}
	}

	if !found {
		t.Fatal("vmware_vm_snapshot_info was not emitted even though a snapshot exists")
	}
}

// TestPerfMetricLabelValuePairing 覆盖 emitPerformanceMetrics 的端到端行为。
//
// 这条路径的 label 顺序是 vcenter, <name>, <moid>[, pfinstance] —— 注意实体
// 名在 moid **之前**，与 host/vm collector 里 moid 在前的顺序正好相反。
// 两处顺序不一致本身就是错配的高危来源。
func TestPerfMetricLabelValuePairing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loginData, cleanup := setupCollectorLoginData(t)
	defer cleanup()

	refs, names := getHostRefsAndNames(t, loginData, logger)
	if len(refs) == 0 {
		t.Fatal("expected at least one host reference from simulator")
	}

	for _, tc := range []struct {
		name       string
		instance   string
		wantPFInst bool
	}{
		{"non-instanced", "", false},
		{"instanced", "*", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan prometheus.Metric, 5000)
			scrapePerformance(
				loginData["ctx"].(context.Context), ch, logger, 1, 20,
				loginData["perf"].(*performance.Manager), loginData["target"].(string),
				"HostSystem", "vmware", "host", tc.instance,
				[]string{"cpu.usage.average"},
				loginData["counters"].(map[string]*types.PerfCounterInfo), refs, names,
			)

			metrics := drainMetrics(ch)
			if len(metrics) == 0 {
				t.Fatal("scrapePerformance emitted nothing")
			}

			for _, m := range metrics {
				name, labels, _ := labelsOf(t, m)
				if name != "vmware_host_cpu_usage_average" {
					t.Fatalf("unexpected metric name %q", name)
				}

				assertLabels(t, name, labels, map[string]string{
					"vcenter": loginData["target"].(string),
				})
				assertIsMoid(t, name, "hostmo", labels["hostmo"], "host-")
				assertIsNotMoid(t, name, "host", labels["host"])

				// hostmo -> host 的映射必须与传进去的 names map 一致。
				// 这直接验证了 targetNames[metric.Entity.Value] 的取值没有
				// 与 Entity.Value 本身放错位置。
				if want := names[labels["hostmo"]]; labels["host"] != want {
					t.Errorf("%s: host = %q for hostmo %q, want %q",
						name, labels["host"], labels["hostmo"], want)
				}

				got, ok := labels["pfinstance"]
				if tc.wantPFInst && !ok {
					t.Errorf("%s: pfinstance label is missing on an instanced scrape: %v", name, labels)
				}
				if !tc.wantPFInst && ok {
					t.Errorf("%s: pfinstance = %q leaked onto a non-instanced scrape; "+
						"the label set must match the Desc exactly", name, got)
				}
			}
		})
	}
}

// BenchmarkHostCollectorUpdate 用来证明 P2-4 的效果：Desc 构造次数与实体数无关。
//
// 直接测 Desc 构造次数没有可移植的办法（client_golang 不暴露计数器），所以
// 换个角度：清空缓存后跑一轮，再复用缓存跑 N 轮，观察 descsCache 的规模不随
// 轮次增长。真正的性能收益体现在 b.N 增大时每轮耗时不上升。
func BenchmarkHostCollectorUpdate(b *testing.B) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	loginData, cleanup := setupCollectorLoginData(b)
	defer cleanup()

	collector, err := NewhostCollector(logger)
	if err != nil {
		b.Fatalf("NewhostCollector() returned error: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ch := make(chan prometheus.Metric, 40000)
		if err := collector.Update(ch, "vmware", nil, loginData, map[string]string{}); err != nil {
			b.Fatalf("host Update() returned error: %v", err)
		}
		drainMetrics(ch)
	}
}

// TestDescCacheSizeIsIndependentOfEntityCount 是上面 benchmark 的可断言版本。
//
// 原实现每个实体造一套 Desc，所以 "Desc 数量" 正比于实体数。改造后 Desc 按
// namespace 缓存，跑多少轮、有多少实体都只有一套。缓存条目数就是最直接的证据。
func TestDescCacheSizeIsIndependentOfEntityCount(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loginData, cleanup := setupCollectorLoginData(t)
	defer cleanup()

	// 用独立 namespace，避免与其他测试共享缓存导致计数受污染。
	const ns = "benchns"

	collector, err := NewhostCollector(logger)
	if err != nil {
		t.Fatalf("NewhostCollector() returned error: %v", err)
	}

	// 只看本 namespace 带来的增量。缓存是包级共享的，同包其他测试也会往里写，
	// 所以断言缓存的绝对规模会让这个测试依赖执行顺序。
	descsMu.Lock()
	baseline := len(descsCache)
	if _, exists := descsCache[ns]; exists {
		descsMu.Unlock()
		t.Fatalf("namespace %q is already cached; pick a namespace no other test uses", ns)
	}
	descsMu.Unlock()

	var entityCount int
	for round := 0; round < 3; round++ {
		ch := make(chan prometheus.Metric, 40000)
		if err := collector.Update(ch, ns, nil, loginData, map[string]string{}); err != nil {
			t.Fatalf("host Update() returned error: %v", err)
		}

		series := collectSeries(t, ch)
		entityCount = len(requireSeries(t, series, ns+"_host_info"))

		descsMu.Lock()
		grew := len(descsCache) - baseline
		_, present := descsCache[ns]
		descsMu.Unlock()

		if !present {
			t.Fatalf("round %d: descsCache has no entry for namespace %q", round, ns)
		}
		if grew != 1 {
			t.Fatalf("round %d: this namespace added %d cache entries, want exactly 1; "+
				"the cache must be keyed by namespace, not by entity or by scrape round",
				round, grew)
		}
	}

	if entityCount < 2 {
		t.Fatalf("the simulator produced only %d hosts; this test needs several entities "+
			"to be meaningful", entityCount)
	}

	// 关键断言：实体数 >= 2，而 Desc 集合仍然只有一套（同一指针）。
	first := descsFor(ns)
	second := descsFor(ns)
	if first != second {
		t.Fatal("descsFor() returned different pointers for the same namespace")
	}
}

// TestEmitPerformanceMetricsDoesNotLeakInstanceLabel 复现修复前的 pfinstance
// 泄漏 bug（Stage 4 顺带发现，方案里没列）。
//
// 原实现把 labelMap 提到两层循环外面复用。一旦某个 value 带 instance 就往
// map 里写了 pfinstance，而下一个不带 instance 的 value 走同一个 map ——
// 陈旧的 pfinstance 不会被清除，直接泄漏过去。
//
// 危险之处在于它不会 panic 也不会报错：非 instanced 的 Desc 声明了 3 个
// variableLabels，泄漏后传 4 个值就报错了；但如果两者恰好都是 instanced
// Desc，就会产出一条 instance 值完全错误的序列 —— 静默的数据污染。
//
// 这里手工构造 "instanced value 后面紧跟 non-instanced value" 的数据，
// simulator 未必自然产出这种组合，靠它来碰运气覆盖不了这个 bug。
func TestEmitPerformanceMetricsDoesNotLeakInstanceLabel(t *testing.T) {
	counterInfo := &types.PerfCounterInfo{
		NameInfo: &types.ElementDescription{
			Description: types.Description{Summary: "Network bytes received"},
		},
		UnitInfo: &types.ElementDescription{
			Description: types.Description{Label: "kiloBytesPerSecond"},
		},
	}

	countersSpec := map[string]*types.PerfCounterInfo{
		"net.bytesRx.average": counterInfo,
	}

	sampleInfo := []types.PerfSampleInfo{{Interval: 20, Timestamp: time.Now()}}

	metrics := []performance.EntityMetric{
		{
			Entity:     types.ManagedObjectReference{Type: "HostSystem", Value: "host-21"},
			SampleInfo: sampleInfo,
			Value: []performance.MetricSeries{
				// 先来一条带 instance 的。
				{Name: "net.bytesRx.average", Instance: "vmnic0", Value: []int64{100}},
				// 紧跟一条同名但不带 instance 的。原实现会把上面的 "vmnic0"
				// 泄漏到这一条上。
				{Name: "net.bytesRx.average", Instance: "", Value: []int64{200}},
			},
		},
	}

	ch := make(chan prometheus.Metric, 100)
	emitPerformanceMetrics(
		ch, "vc.example.com", "HostSystem", "vmware", "host", "",
		countersSpec,
		map[string]string{"host-21": "esx01.example.com"},
		metrics,
		testLogger(),
	)

	var instanced, plain int
	for _, m := range drainMetrics(ch) {
		name, labels, value := labelsOf(t, m)
		if name != "vmware_host_net_bytesRx_average" {
			t.Fatalf("unexpected metric name %q", name)
		}

		assertLabels(t, name, labels, map[string]string{
			"vcenter": "vc.example.com",
			"hostmo":  "host-21",
			"host":    "esx01.example.com",
		})

		switch value {
		case 100:
			instanced++
			assertLabels(t, name, labels, map[string]string{"pfinstance": "vmnic0"})
		case 200:
			plain++
			if got, ok := labels["pfinstance"]; ok {
				t.Errorf("the non-instanced sample carries pfinstance=%q; the instance label "+
					"leaked from the previous sample. full series: %v", got, labels)
			}
		default:
			t.Errorf("unexpected value %v with labels %v", value, labels)
		}
	}

	if instanced != 1 || plain != 1 {
		t.Fatalf("expected exactly one instanced and one non-instanced series, got %d and %d",
			instanced, plain)
	}
}

// TestEmitPerformanceMetricsSkipsUnknownEntityType 覆盖另一处顺带修复：
// 原实现的实体类型 switch 没有 default 分支。
//
// 未知类型下 label 集只剩 vcenter，于是该类型的**所有**实体共用一条序列，
// 后写的覆盖先写的，最后只留一条 —— 指标看起来正常存在，值却是随机某个实体的。
// 现在的行为是记日志并整体跳过。
func TestEmitPerformanceMetricsSkipsUnknownEntityType(t *testing.T) {
	counterInfo := &types.PerfCounterInfo{
		NameInfo: &types.ElementDescription{
			Description: types.Description{Summary: "CPU usage"},
		},
		UnitInfo: &types.ElementDescription{
			Description: types.Description{Label: "percent"},
		},
	}

	metrics := []performance.EntityMetric{
		{
			Entity:     types.ManagedObjectReference{Type: "ResourcePool", Value: "resgroup-8"},
			SampleInfo: []types.PerfSampleInfo{{Interval: 20, Timestamp: time.Now()}},
			Value: []performance.MetricSeries{
				{Name: "cpu.usage.average", Value: []int64{42}},
			},
		},
	}

	ch := make(chan prometheus.Metric, 100)
	emitPerformanceMetrics(
		ch, "vc.example.com", "ResourcePool", "vmware", "rp", "",
		map[string]*types.PerfCounterInfo{"cpu.usage.average": counterInfo},
		map[string]string{"resgroup-8": "Resources"},
		metrics,
		testLogger(),
	)

	if got := len(drainMetrics(ch)); got != 0 {
		t.Fatalf("emitted %d metrics for an unsupported entity type, want 0; emitting a "+
			"vcenter-only series would make every entity of that type collapse into one", got)
	}
}

// TestClusterDatastoreEmitsOneSeriesPerDatastore 锁死方案 B 的破坏性变更。
//
// 旧行为：整个集群的 datastore moid 用逗号拼成一个 label 值，一条序列。
// 新行为：一个 datastore 一条序列，dsmo 是单个 moid。
//
// 之所以要测：这是本次唯一的指标改名 + label 语义变更，而改名是静默失效的。
// 如果哪天有人"顺手"把循环改回拼接，指标名还在、序列还在、查询也不报错，
// 只有 join 会莫名失效。
func TestClusterDatastoreEmitsOneSeriesPerDatastore(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 默认的 VPX 模型只有一个 datastore，那样 "逗号拼接" 与 "每个一条序列"
	// 产出完全一样的结果，这个测试就成了摆设。所以这里显式要求 3 个。
	model := simulator.VPX()
	model.Datastore = 3

	loginData, cleanup := setupCollectorLoginDataWithModel(t, model)
	defer cleanup()

	collector, err := NewClusterCollector(logger)
	if err != nil {
		t.Fatalf("NewClusterCollector() returned error: %v", err)
	}

	ch := make(chan prometheus.Metric, 10000)
	if err := collector.Update(ch, "vmware", nil, loginData, map[string]string{}); err != nil {
		t.Fatalf("cluster Update() returned error: %v", err)
	}

	series := collectSeries(t, ch)

	// 旧名字必须彻底消失，否则 CHANGELOG 里的改名说明就是假的。
	for _, gone := range []string{"vmware_cluster_datastores", "vmware_compute_datastores"} {
		if _, ok := series[gone]; ok {
			t.Errorf("%s is still emitted; it was renamed to the singular form", gone)
		}
	}

	// VPX 模型带一个集群，所以走的是 cluster 分支而不是 compute 兜底分支。
	dsSeries := requireSeries(t, series, "vmware_cluster_datastore")

	// 3 个 datastore -> 3 条序列。拼接实现只会产出 1 条。
	if len(dsSeries) != 3 {
		t.Fatalf("got %d vmware_cluster_datastore series for 3 datastores, want 3; "+
			"a single series means the moids were joined into one label value again", len(dsSeries))
	}

	for _, s := range dsSeries {
		assertLabels(t, "vmware_cluster_datastore", s, map[string]string{
			"vcenter": loginData["target"].(string),
		})
		assertIsMoid(t, "vmware_cluster_datastore", "cmo", s["cmo"], "domain-")

		// 核心断言：dsmo 是**单个** moid，不是逗号拼接的列表。
		if strings.Contains(s["dsmo"], ",") {
			t.Errorf("vmware_cluster_datastore: dsmo = %q contains a comma; each datastore "+
				"must get its own series so that dsmo can be used in a join", s["dsmo"])
			continue
		}
		assertIsMoid(t, "vmware_cluster_datastore", "dsmo", s["dsmo"], "datastore-")
	}

}

// TestClusterMetricHelpTextsAreDistinct 覆盖三处 help 错抄的修正。
//
// 原实现里 cluster_info、cluster_datastores、compute_info 的 help 全是
// "This is basic cluster info to be used for parent reference" —— 从 cluster_info
// 复制粘贴过来的。help 写错不影响数值，但会让 /metrics 的 # HELP 行毫无参考价值，
// 也让 Grafana 的指标浏览器给出错误提示。
//
// 断言方式是 help 必须互不相同：只要有人再复制粘贴一次，这个测试就会失败。
func TestClusterMetricHelpTextsAreDistinct(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loginData, cleanup := setupCollectorLoginData(t)
	defer cleanup()

	collector, err := NewClusterCollector(logger)
	if err != nil {
		t.Fatalf("NewClusterCollector() returned error: %v", err)
	}

	ch := make(chan prometheus.Metric, 10000)
	if err := collector.Update(ch, "vmware", nil, loginData, map[string]string{}); err != nil {
		t.Fatalf("cluster Update() returned error: %v", err)
	}

	helpByMetric := map[string]string{}
	for _, m := range drainMetrics(ch) {
		desc := m.Desc().String()

		i := strings.Index(desc, `fqName: "`)
		name := desc[i+len(`fqName: "`):]
		name = name[:strings.Index(name, `"`)]

		j := strings.Index(desc, `help: "`)
		if j < 0 {
			t.Fatalf("no help in Desc: %s", desc)
		}
		help := desc[j+len(`help: "`):]
		help = help[:strings.Index(help, `"`)]

		helpByMetric[name] = help
	}

	if len(helpByMetric) < 2 {
		t.Fatalf("expected at least 2 distinct cluster metrics, got %v", helpByMetric)
	}

	seen := map[string]string{}
	for name, help := range helpByMetric {
		if help == "" {
			t.Errorf("%s has an empty help text", name)
			continue
		}
		if other, dup := seen[help]; dup {
			t.Errorf("%s and %s share the same help text %q; it was copy-pasted and must "+
				"describe what each metric actually measures", name, other, help)
			continue
		}
		seen[help] = name

		// 这是被替换掉的原文，任何 cluster 指标都不该再出现它。
		if strings.Contains(help, "to be used for parent reference") &&
			!strings.HasSuffix(name, "_info") {
			t.Errorf("%s still carries the copy-pasted help text %q", name, help)
		}
	}
}
