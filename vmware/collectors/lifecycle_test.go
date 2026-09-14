package vmwareCollectors

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/prezhdarov/vmware-exporter/internal/collector"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// 生命周期/状态面的集成与表驱动测试（Batch 0）。
//
// 这一层最容易退化回旧行为：把关机/维护实体从 _info 里过滤掉是"图变空却
// 不报错"的静默故障，所以用真实 simulator 锁住"实体仍可见 + 状态 label 正确
// + perf 不输出 + 跳过被计数"四件事同时成立。

// TestPoweredOffVMStillVisibleButSkippedByPerf 锁死 Batch 0-A 的核心口径：
// 关机 VM 必须继续出现在 vm_info（以及配置/容量/状态指标里），但 perf 性能
// 计数器不为它输出。旧实现整台 VM 随电源态从所有指标里消失，Prometheus 无法
// 区分"VM 不存在"与"VM 存在但没开机"。
func TestPoweredOffVMStillVisibleButSkippedByPerf(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, s, cleanup := setupCollectorScrape(t)
	defer cleanup()

	finder := find.NewFinder(s.Client, false)
	vms, err := finder.VirtualMachineList(ctx, "/DC0/vm/*")
	if err != nil {
		t.Fatalf("VirtualMachineList() returned error: %v", err)
	}
	if len(vms) < 2 {
		t.Fatalf("VPX model should provide at least 2 VMs, got %d", len(vms))
	}

	// 关掉其中一台（不等待 guest OS）。
	victim := vms[0]
	victimRef := victim.Reference().Value

	task, err := victim.PowerOff(ctx)
	if err != nil {
		t.Fatalf("PowerOff() returned error: %v", err)
	}
	if err := task.Wait(ctx); err != nil {
		t.Fatalf("PowerOff task failed: %v", err)
	}

	col, err := NewvmCollector(logger)
	if err != nil {
		t.Fatalf("NewvmCollector() returned error: %v", err)
	}

	ch := make(chan prometheus.Metric, 60000)
	if err := col.Update(ctx, ch, s); err != nil {
		t.Fatalf("vm Update() returned error: %v", err)
	}
	// Update 不关闭 ch（channel 由调用方持有），用非阻塞的 drainMetrics
	// 而不是 for range —— 后者会永久等待下一条指标而挂死。
	metrics := drainMetrics(ch)

	byName := map[string][]prometheus.Metric{}
	for _, m := range metrics {
		name, _, _ := labelsOf(t, m)
		byName[name] = append(byName[name], m)
	}

	// (1) vm_info 必须覆盖全部 VM，包括关机那台。
	if len(byName["vmware_vm_info"]) != len(vms) {
		t.Errorf("vm_info emitted %d series, want %d (powered-off VM must stay in vm_info)",
			len(byName["vmware_vm_info"]), len(vms))
	}
	if !seriesHasMoid(t, byName["vmware_vm_info"], "vmmo", victimRef) {
		t.Errorf("powered-off VM %s missing from vm_info; it must remain visible", victimRef)
	}

	// (2) power_state 必须带正确的状态 label。
	var gotState string
	for _, labels := range labelSet(t, byName["vmware_vm_power_state"]) {
		if labels["vmmo"] == victimRef {
			gotState = labels["state"]
		}
	}
	if gotState != "poweredOff" {
		t.Errorf("power_state for %s = %q, want poweredOff", victimRef, gotState)
	}

	// (3) 关机 VM 的配置容量仍应可见 —— 这是 breaking change 的另一半：
	// 分母不再随电源态缩水。
	if !seriesHasMoid(t, byName["vmware_vm_mem_capacity_bytes"], "vmmo", victimRef) {
		t.Errorf("mem_capacity_bytes missing for powered-off VM %s", victimRef)
	}

	// (4) 任何 perf 数据面指标都不该为关机 VM 输出。静态面指标白名单之外的
	// vmware_vm_* 都来自 perf（cpu_usage_hertz、mem_active_bytes 等）。
	for name, list := range byName {
		if staticVMFacet[name] || !strings.HasPrefix(name, "vmware_vm_") {
			continue
		}
		for _, labels := range labelSet(t, list) {
			if labels["vmmo"] == victimRef {
				t.Errorf("data-plane metric %s emitted for powered-off VM %s; perf must skip it",
					name, victimRef)
			}
		}
	}
}

// staticVMFacet 是 vm collector 对每台 VM（含关机）都无条件输出的指标。
var staticVMFacet = map[string]bool{
	"vmware_vm_info":                          true,
	"vmware_vm_power_state":                   true,
	"vmware_vm_overall_status":                true,
	"vmware_vm_cpu_corecount":                 true,
	"vmware_vm_mem_capacity_bytes":            true,
	"vmware_vm_mem_capacity":                  true, // legacy 变体默认关，列上无害
	"vmware_vm_datastore_capacity_used_bytes": true,
	"vmware_vm_datastore_capacity_used":       true,
	"vmware_vm_snapshot_info":                 true,
}

func seriesHasMoid(t *testing.T, ms []prometheus.Metric, label, moid string) bool {
	t.Helper()
	for _, labels := range labelSet(t, ms) {
		if labels[label] == moid {
			return true
		}
	}
	return false
}

func labelSet(t *testing.T, ms []prometheus.Metric) []map[string]string {
	t.Helper()
	out := make([]map[string]string, 0, len(ms))
	for _, m := range ms {
		_, labels, _ := labelsOf(t, m)
		out = append(out, labels)
	}
	return out
}

// TestHostEligibilityAndSkipReasons 是表驱动单测，固定数据面可用性判定与
// 跳过原因的口径。host perf 与两个 esxcli collector 共用这套判定，任何条件
// 漂移（漏掉维护态/连接态）都会在这里失败，不必依赖 simulator 的维护任务语义。
func TestHostEligibilityAndSkipReasons(t *testing.T) {
	host := func(power, conn string, maint bool) mo.HostSystem {
		return mo.HostSystem{
			Runtime: types.HostRuntimeInfo{
				PowerState:        types.HostSystemPowerState(power),
				ConnectionState:   types.HostSystemConnectionState(conn),
				InMaintenanceMode: maint,
			},
		}
	}

	for _, tc := range []struct {
		name            string
		power, conn     string
		maint           bool
		eligible        bool
		wantSkipReasons []string
	}{
		{"healthy", "poweredOn", "connected", false, true, nil},
		{"powered off", "poweredOff", "connected", false, false, []string{collector.SkipReasonPoweredOff}},
		{"disconnected", "poweredOn", "disconnected", false, false, []string{collector.SkipReasonDisconnected}},
		{"not responding", "poweredOn", "notResponding", false, false, []string{collector.SkipReasonNotResponding}},
		{"maintenance", "poweredOn", "connected", true, false, []string{collector.SkipReasonMaintenance}},
		// 维护 + 断连同时命中：一个实体两个原因都要计数。
		{"maintenance and disconnected", "poweredOn", "disconnected", true, false,
			[]string{collector.SkipReasonMaintenance, collector.SkipReasonDisconnected}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := host(tc.power, tc.conn, tc.maint)
			if got := hostDataPlaneEligible(h); got != tc.eligible {
				t.Errorf("hostDataPlaneEligible = %v, want %v", got, tc.eligible)
			}

			reasons := hostSkipReasons(h)

			// 无论是否跳过，四个原因键必须始终存在（含 0），正常态与
			// 异常态的 label 集合因此保持稳定，告警不必写 absent()。
			if len(reasons) != 4 {
				t.Fatalf("reasons has %d keys, want the 4 prefilled reasons: %v",
					len(reasons), reasons)
			}

			want := map[string]int{}
			for _, r := range tc.wantSkipReasons {
				want[r] = 1
			}
			for reason, n := range reasons {
				if n != want[reason] {
					t.Errorf("reasons[%q] = %d, want %d; full reasons=%v",
						reason, n, want[reason], reasons)
				}
			}
		})
	}
}

// TestClusterCapacityAndOverallStatusEmitted 覆盖 Batch 0-B/0-C：真实集群要
// 产出 overall_status 与五个容量/有效资源指标，且 CPU 总量单位是 hertz。
func TestClusterCapacityAndOverallStatusEmitted(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, s, cleanup := setupCollectorScrape(t)
	defer cleanup()

	col, err := NewClusterCollector(logger)
	if err != nil {
		t.Fatalf("NewClusterCollector() returned error: %v", err)
	}

	ch := make(chan prometheus.Metric, 10000)
	if err := col.Update(ctx, ch, s); err != nil {
		t.Fatalf("cluster Update() returned error: %v", err)
	}

	var maxCPUCap float64
	found := map[string]bool{}
	for _, m := range drainMetrics(ch) {
		name, labels, value := labelsOf(t, m)
		found[name] = true

		if name == "vmware_cluster_overall_status" {
			switch labels["status"] {
			case "green", "yellow", "red", "gray":
			default:
				t.Errorf("cluster_overall_status status = %q, want green|yellow|red|gray", labels["status"])
			}
		}
		if name == "vmware_cluster_cpu_capacity_hertz" && value > maxCPUCap {
			maxCPUCap = value
		}
	}

	for _, name := range []string{
		"vmware_cluster_overall_status",
		"vmware_cluster_effective_hosts",
		"vmware_cluster_cpu_capacity_hertz",
		"vmware_cluster_cpu_effective_hertz",
		"vmware_cluster_memory_capacity_bytes",
		"vmware_cluster_memory_effective_bytes",
	} {
		if !found[name] {
			t.Errorf("%s was not emitted", name)
		}
	}

	// VPX 集群有主机，total CPU 以 MHz 给（2294 量级），×1e6 后落在 1e9。
	// 漏乘 1e6 会停在 1e3 量级 —— 这同时锁住单位换算。
	if maxCPUCap < 1e9 {
		t.Errorf("cluster cpu_capacity_hertz max = %v, want >= 1e9 (MHz must be converted to hertz)",
			maxCPUCap)
	}
}

// TestOverallStatusCoverage 确认 vm/host/datastore 三类实体的 overall_status
// 在 VPX 模型下都有产出（Batch 0-B 全覆盖）。
func TestOverallStatusCoverage(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, s, cleanup := setupCollectorScrape(t)
	defer cleanup()

	cases := []struct {
		metric string
		create func(*slog.Logger) (collector.Collector, error)
		buf    int
	}{
		{"vmware_vm_overall_status", NewvmCollector, 60000},
		{"vmware_host_overall_status", NewhostCollector, 40000},
		{"vmware_datastore_overall_status", NewdatastoreCollector, 20000},
	}

	for _, tc := range cases {
		t.Run(tc.metric, func(t *testing.T) {
			col, err := tc.create(logger)
			if err != nil {
				t.Fatalf("constructor failed: %v", err)
			}
			ch := make(chan prometheus.Metric, tc.buf)
			if err := col.Update(ctx, ch, s); err != nil {
				t.Fatalf("Update failed: %v", err)
			}

			count := 0
			for _, m := range drainMetrics(ch) {
				name, labels, _ := labelsOf(t, m)
				if name != tc.metric {
					continue
				}
				count++
				switch labels["status"] {
				case "green", "yellow", "red", "gray", "":
				default:
					t.Errorf("%s status = %q outside the four-colour enum", tc.metric, labels["status"])
				}
			}
			if count == 0 {
				t.Fatalf("%s not emitted", tc.metric)
			}
		})
	}
}

// TestInfoMetricsCarryUUIDLabel 确认 host/vm 的 _info 带 uuid label，且值
// 非空（simulator 对两者都会生成 UUID）。
func TestInfoMetricsCarryUUIDLabel(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, s, cleanup := setupCollectorScrape(t)
	defer cleanup()

	for _, tc := range []struct {
		metric string
		create func(*slog.Logger) (collector.Collector, error)
		buf    int
	}{
		{"vmware_vm_info", NewvmCollector, 60000},
		{"vmware_host_info", NewhostCollector, 40000},
	} {
		t.Run(tc.metric, func(t *testing.T) {
			col, err := tc.create(logger)
			if err != nil {
				t.Fatalf("constructor failed: %v", err)
			}
			ch := make(chan prometheus.Metric, tc.buf)
			if err := col.Update(ctx, ch, s); err != nil {
				t.Fatalf("Update failed: %v", err)
			}

			seen := false
			for _, m := range drainMetrics(ch) {
				name, labels, _ := labelsOf(t, m)
				if name != tc.metric {
					continue
				}
				seen = true
				if labels["uuid"] == "" {
					t.Errorf("%s carries an empty uuid label: %v", name, labels)
				}
			}
			if !seen {
				t.Fatalf("%s not emitted", tc.metric)
			}
		})
	}
}
