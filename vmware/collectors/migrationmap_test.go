package vmwareCollectors

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/simulator"
)

// updateGolden 重新生成 scripts/metric_migration_map.json。
//
// 名字里带 golden 是因为这个 JSON 本质上是一份 golden file：内容由实现决定，
// 手改它没有意义 —— 下一次跑测试就会被判为与实现不一致。
var updateGolden = flag.Bool("update", false,
	"regenerate scripts/metric_migration_map.json from the implementation")

// migrationEntry 是 scripts/metric_migration_map.json 里的一条记录。
type migrationEntry struct {
	Counter   string  `json:"counter"`
	Subsystem string  `json:"subsystem"`
	Old       string  `json:"old"`
	New       string  `json:"new"`
	Factor    float64 `json:"factor"`
	Type      string  `json:"type"`
	Unit      string  `json:"unit"`
	StatsType string  `json:"stats_type"`
}

// staticMigrations 是静态指标（非性能计数器）的改名映射。
//
// 这些指标的换算写在 host.go / vm.go / datastore.go 的发射段里，不经过
// translatePerfCounter，所以推导不出来，只能列在这里。
// TestLegacyMetricsAgreeWithReplacements 会在这里的因子与实际发射值脱钩时报错。
var staticMigrations = []migrationEntry{
	{Old: "vmware_host_cpu_capacity", New: "vmware_host_cpu_capacity_hertz", Factor: 1e6, Type: "gauge"},
	{Old: "vmware_host_cpu_capacity_mhz", New: "vmware_host_cpu_capacity_hertz", Factor: 1e6, Type: "gauge"},
	{Old: "vmware_host_mem_capacity", New: "vmware_host_mem_capacity_bytes", Factor: 1, Type: "gauge"},
	{Old: "vmware_vm_mem_capacity", New: "vmware_vm_mem_capacity_bytes", Factor: 1048576, Type: "gauge"},
	{Old: "vmware_vm_datastore_capacity_used", New: "vmware_vm_datastore_capacity_used_bytes", Factor: 1, Type: "gauge"},
	{Old: "vmware_datastore_capacity", New: "vmware_datastore_capacity_bytes", Factor: 1, Type: "gauge"},
	{Old: "vmware_datastore_free", New: "vmware_datastore_free_bytes", Factor: 1, Type: "gauge"},
}

// buildMigrationMap 从计数器元数据推导出完整的改名映射。
//
// 这是 scripts/metric_migration_map.json 的事实来源。
func buildMigrationMap(t *testing.T) []migrationEntry {
	t.Helper()

	_, perf, ctx := newSimClient(t, simulator.VPX())

	byName, err := perf.CounterInfoByName(ctx)
	if err != nil {
		t.Fatalf("CounterInfoByName: %v", err)
	}

	// 这里必须列出**所有**带性能计数器的 collector。漏掉一个不会有任何报错，
	// 只会让那个 collector 的指标在迁移脚本里被判为「不需要改名」而原样留下。
	sets := []struct {
		subsystem string
		counters  [][]string
	}{
		{hostSubsystem, [][]string{cHostCounters, iHostCounters}},
		{vmSubsystem, [][]string{cVMCounters, iVMCounters}},
		{datastoreSubsystem, [][]string{datastoreCounters}},
	}

	var out []migrationEntry

	for _, set := range sets {
		for _, list := range set.counters {
			for _, counter := range list {
				info, ok := byName[counter]
				if !ok {
					// simulator 的计数器目录不含这一条。真实 vCenter 有，
					// 所以这不是错误 —— 但要留个记录，否则映射表静默缺项。
					t.Logf("counter %q is not in the simulator catalogue; not in the map", counter)
					continue
				}

				spec, ok := translatePerfCounter(counter, info)
				if !ok {
					t.Logf("counter %q has an unmapped unit; exported under its old name", counter)
					continue
				}

				vt := "gauge"
				if spec.ValueType == prometheus.CounterValue {
					vt = "counter"
				}

				out = append(out, migrationEntry{
					Counter:   counter,
					Subsystem: set.subsystem,
					Old:       prometheus.BuildFQName("vmware", set.subsystem, strings.ReplaceAll(counter, ".", "_")),
					New:       prometheus.BuildFQName("vmware", set.subsystem, spec.Name),
					Factor:    spec.Factor,
					Type:      vt,
					Unit:      info.UnitInfo.GetElementDescription().Key,
					StatsType: string(info.StatsType),
				})
			}
		}
	}

	for _, e := range staticMigrations {
		e.Counter = "(static)"
		e.StatsType = "absolute"
		out = append(out, e)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Old != out[j].Old {
			return out[i].Old < out[j].Old
		}
		return out[i].New < out[j].New
	})

	return out
}

// TestMigrationMapMatchesImplementation 锁住 scripts/metric_migration_map.json
// 与实现一致。
//
// 那个 JSON 被 scripts/migrate_dashboards.py 用来改写 dashboard 表达式。它是从
// 本测试导出的，但一旦落地成文件就有了脱钩的可能：改了 perfUnitRules 里的因子、
// 给 perfNameOverrides 加了一条、或者往 cHostCounters 里塞了新计数器，JSON 都
// 不会自动跟上，而 dashboard 会继续指向旧名或用错的因子。Grafana 对此不报错，
// 只显示空面板或错数字。
//
// 用 -update 重新生成：
//
//	go test ./vmware/collectors/ -run TestMigrationMapMatchesImplementation -update
func TestMigrationMapMatchesImplementation(t *testing.T) {
	want := buildMigrationMap(t)

	path := filepath.Join("..", "..", "scripts", "metric_migration_map.json")

	if *updateGolden {
		b, err := json.MarshalIndent(want, "", "  ")
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("regenerated %s with %d entries", path, len(want))
		return
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (run with -update to generate it)", path, err)
	}

	var got []migrationEntry
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}

	index := func(entries []migrationEntry) map[string]migrationEntry {
		m := make(map[string]migrationEntry, len(entries))
		for _, e := range entries {
			m[e.Old+" -> "+e.New] = e
		}
		return m
	}

	gotIdx, wantIdx := index(got), index(want)

	for key, w := range wantIdx {
		g, ok := gotIdx[key]
		if !ok {
			t.Errorf("scripts/metric_migration_map.json is missing %q; "+
				"the dashboards will keep referencing the old name. Run with -update", key)
			continue
		}
		if g.Factor != w.Factor {
			t.Errorf("%s: factor in the JSON is %g, implementation says %g; "+
				"the dashboards would cancel out the wrong amount. Run with -update",
				key, g.Factor, w.Factor)
		}
		if g.Type != w.Type {
			t.Errorf("%s: type in the JSON is %q, implementation says %q. Run with -update",
				key, g.Type, w.Type)
		}
	}

	for key := range gotIdx {
		if _, ok := wantIdx[key]; !ok {
			t.Errorf("scripts/metric_migration_map.json has a stale entry %q "+
				"that the implementation no longer produces. Run with -update", key)
		}
	}
}
