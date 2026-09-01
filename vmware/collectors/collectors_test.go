package vmwareCollectors

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

func TestHostCollectorUpdateEmitsHostInfo(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loginData, cleanup := setupCollectorLoginData(t)
	defer cleanup()

	collector, err := NewhostCollector(logger)
	if err != nil {
		t.Fatalf("NewhostCollector() returned error: %v", err)
	}

	ch := make(chan prometheus.Metric, 20000)
	if err := collector.Update(ch, "vmware", nil, loginData, map[string]string{}); err != nil {
		t.Fatalf("host Update() returned error: %v", err)
	}

	metrics := drainMetrics(ch)
	if len(metrics) == 0 {
		t.Fatal("expected host collector to emit metrics")
	}

	if !hasMetricWithLabels(metrics, "vmware_host_info", map[string]string{"vcenter": loginData["target"].(string), "host": "", "hostmo": ""}) {
		t.Fatal("expected vmware_host_info metric with host labels")
	}
}

func TestVMCollectorUpdateEmitsVMInfo(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loginData, cleanup := setupCollectorLoginData(t)
	defer cleanup()

	collector, err := NewvmCollector(logger)
	if err != nil {
		t.Fatalf("NewvmCollector() returned error: %v", err)
	}

	ch := make(chan prometheus.Metric, 30000)
	if err := collector.Update(ch, "vmware", nil, loginData, map[string]string{}); err != nil {
		t.Fatalf("vm Update() returned error: %v", err)
	}

	metrics := drainMetrics(ch)
	if len(metrics) == 0 {
		t.Fatal("expected vm collector to emit metrics")
	}

	if !hasMetricWithLabels(metrics, "vmware_vm_info", map[string]string{"vcenter": loginData["target"].(string), "vm": "", "vmmo": "", "hostmo": ""}) {
		t.Fatal("expected vmware_vm_info metric with vm labels")
	}
}

func TestDatastoreCollectorUpdateEmitsDatastoreInfo(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loginData, cleanup := setupCollectorLoginData(t)
	defer cleanup()

	collector, err := NewdatastoreCollector(logger)
	if err != nil {
		t.Fatalf("NewdatastoreCollector() returned error: %v", err)
	}

	ch := make(chan prometheus.Metric, 30000)
	if err := collector.Update(ch, "vmware", nil, loginData, map[string]string{}); err != nil {
		t.Fatalf("datastore Update() returned error: %v", err)
	}

	metrics := drainMetrics(ch)
	if len(metrics) == 0 {
		t.Fatal("expected datastore collector to emit metrics")
	}

	if !hasMetricWithLabels(metrics, "vmware_datastore_info", map[string]string{"vcenter": loginData["target"].(string), "ds": "", "dsmo": "", "pfinstance": ""}) {
		t.Fatal("expected vmware_datastore_info metric with datastore labels")
	}
}

func TestScrapePerformanceEmitsMetrics(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loginData, cleanup := setupCollectorLoginData(t)
	defer cleanup()

	refs, names := getHostRefsAndNames(t, loginData, logger)
	if len(refs) == 0 {
		t.Fatal("expected at least one host reference from simulator")
	}

	ch := make(chan prometheus.Metric, 5000)
	scrapePerformance(
		loginData["ctx"].(context.Context),
		ch,
		logger,
		1,
		20,
		loginData["perf"].(*performance.Manager),
		loginData["target"].(string),
		"HostSystem",
		"vmware",
		"host",
		"",
		[]string{"cpu.usage.average"},
		loginData["counters"].(map[string]*types.PerfCounterInfo),
		refs,
		names,
	)

	metrics := drainMetrics(ch)
	if len(metrics) == 0 {
		t.Fatal("expected scrapePerformance to emit metrics")
	}

	if !hasMetricWithLabels(metrics, "vmware_host_cpu_usage_average", map[string]string{"vcenter": loginData["target"].(string), "host": "", "hostmo": ""}) {
		t.Fatal("expected vmware_host_cpu_usage_average metric with host labels")
	}
}

func TestScrapePerformanceWithNoTargetsDoesNotPanicOrEmit(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loginData, cleanup := setupCollectorLoginData(t)
	defer cleanup()

	ch := make(chan prometheus.Metric, 10)
	scrapePerformance(
		loginData["ctx"].(context.Context),
		ch,
		logger,
		1,
		20,
		loginData["perf"].(*performance.Manager),
		loginData["target"].(string),
		"HostSystem",
		"vmware",
		"host",
		"",
		[]string{"cpu.usage.average"},
		loginData["counters"].(map[string]*types.PerfCounterInfo),
		nil,
		map[string]string{},
	)

	if got := len(drainMetrics(ch)); got != 0 {
		t.Fatalf("expected 0 metrics, got %d", got)
	}
}

func TestScrapePerformanceWithNilPerfManagerDoesNotPanicOrEmit(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	loginData, cleanup := setupCollectorLoginData(t)
	defer cleanup()

	refs, names := getHostRefsAndNames(t, loginData, logger)
	if len(refs) == 0 {
		t.Fatal("expected at least one host reference from simulator")
	}

	ch := make(chan prometheus.Metric, 10)
	scrapePerformance(
		loginData["ctx"].(context.Context),
		ch,
		logger,
		1,
		20,
		nil,
		loginData["target"].(string),
		"HostSystem",
		"vmware",
		"host",
		"",
		[]string{"cpu.usage.average"},
		loginData["counters"].(map[string]*types.PerfCounterInfo),
		refs,
		names,
	)

	if got := len(drainMetrics(ch)); got != 0 {
		t.Fatalf("expected 0 metrics, got %d", got)
	}
}

// setupCollectorLoginData 接 testing.TB 而不是 *testing.T，这样 benchmark
// 也能复用同一套 simulator 脚手架，不必再造一份。
func setupCollectorLoginData(t testing.TB) (map[string]interface{}, func()) {
	t.Helper()
	return setupCollectorLoginDataWithModel(t, simulator.VPX())
}

// setupCollectorLoginDataWithModel 允许调用方定制 simulator 模型。
//
// 默认的 VPX 模型规模很小（1 个 datastore、1 个独立主机、1 个 3 节点集群），
// 对多数测试够用，但有些断言必须要多个同类实体才有意义 —— 例如验证
// "每个 datastore 一条序列" 时，只有一个 datastore 的话拼接与拆分的结果
// 完全一样，测试就成了摆设。
func setupCollectorLoginDataWithModel(t testing.TB, model *simulator.Model) (map[string]interface{}, func()) {
	t.Helper()

	ctx := context.Background()
	if err := model.Create(); err != nil {
		t.Fatalf("failed to create simulator model: %v", err)
	}

	server := model.Service.NewServer()

	client, err := govmomi.NewClient(ctx, server.URL, true)
	if err != nil {
		server.Close()
		model.Remove()
		t.Fatalf("failed to create govmomi client: %v", err)
	}

	perfManager := performance.NewManager(client.Client)
	counters, err := perfManager.CounterInfoByName(ctx)
	if err != nil {
		server.Close()
		model.Remove()
		t.Fatalf("failed to fetch counters: %v", err)
	}

	cancelCtx, cancel := context.WithCancel(ctx)

	loginData := map[string]interface{}{
		"ctx":      cancelCtx,
		"cancel":   cancel,
		"client":   client.Client,
		"view":     view.NewManager(client.Client),
		"perf":     perfManager,
		"counters": counters,
		"interval": int32(20),
		"samples":  int32(1),
		"target":   server.URL.Host,
	}

	cleanup := func() {
		cancel()
		server.Close()
		model.Remove()
	}

	return loginData, cleanup
}

func getHostRefsAndNames(t testing.TB, loginData map[string]interface{}, logger *slog.Logger) ([]types.ManagedObjectReference, map[string]string) {
	t.Helper()

	var hosts []mo.HostSystem
	if err := fetchProperties(
		loginData["ctx"].(context.Context),
		loginData["view"].(*view.Manager),
		loginData["client"].(*vim25.Client),
		[]string{"HostSystem"},
		[]string{"summary", "runtime"},
		&hosts,
		logger,
	); err != nil {
		t.Fatalf("fetchProperties() returned error: %v", err)
	}

	refs := make([]types.ManagedObjectReference, 0, len(hosts))
	names := make(map[string]string, len(hosts))

	for _, host := range hosts {
		refs = append(refs, host.Self)
		names[host.Self.Value] = host.Summary.Config.Name
	}

	return refs, names
}

func drainMetrics(ch <-chan prometheus.Metric) []prometheus.Metric {
	metrics := make([]prometheus.Metric, 0)
	for {
		select {
		case metric := <-ch:
			metrics = append(metrics, metric)
		default:
			return metrics
		}
	}
}

func hasMetricWithLabels(metrics []prometheus.Metric, fqName string, requiredLabels map[string]string) bool {
	for _, metric := range metrics {
		desc := metric.Desc().String()
		if !strings.Contains(desc, `fqName: "`+fqName+`"`) {
			continue
		}

		pb := &dto.Metric{}
		if err := metric.Write(pb); err != nil {
			continue
		}

		labelValues := make(map[string]string, len(pb.Label))
		for _, lbl := range pb.Label {
			labelValues[lbl.GetName()] = lbl.GetValue()
		}

		ok := true
		for key, expected := range requiredLabels {
			value, exists := labelValues[key]
			if !exists {
				ok = false
				break
			}
			if expected != "" && value != expected {
				ok = false
				break
			}
			if expected == "" && value == "" {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}

	return false
}
