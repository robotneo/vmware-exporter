package vmwareCollectors

import (
	"context"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/performance"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

func TestFetchPropertiesDatacenter(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	model := simulator.VPX()
	defer model.Remove()

	if err := model.Create(); err != nil {
		t.Fatalf("failed to create simulator model: %v", err)
	}

	server := model.Service.NewServer()
	defer server.Close()

	client, err := govmomi.NewClient(ctx, server.URL, true)
	if err != nil {
		t.Fatalf("failed to create govmomi client: %v", err)
	}

	viewManager := view.NewManager(client.Client)

	var datacenters []mo.Datacenter

	err = fetchProperties(
		ctx,
		viewManager,
		client.Client,
		[]string{"Datacenter"},
		[]string{"name"},
		&datacenters,
		logger,
	)
	if err != nil {
		t.Fatalf("fetchProperties() returned error: %v", err)
	}

	if len(datacenters) == 0 {
		t.Fatal("expected at least one datacenter from simulator")
	}

	if datacenters[0].Name == "" {
		t.Fatal("expected datacenter name to be populated")
	}
}

func TestEmitPerformanceMetricsHostSystem(t *testing.T) {
	ch := make(chan prometheus.Metric, 1)

	countersSpec := map[string]*types.PerfCounterInfo{
		"cpu.usage.average": {
			NameInfo: &types.ElementDescription{
				Description: types.Description{Summary: "CPU usage"},
			},
			UnitInfo: &types.ElementDescription{
				Description: types.Description{Label: "percent"},
			},
		},
	}

	targetNames := map[string]string{
		"host-123": "esx01.example.local",
	}

	metrics := []performance.EntityMetric{
		{
			Entity: types.ManagedObjectReference{
				Type:  "HostSystem",
				Value: "host-123",
			},
			SampleInfo: []types.PerfSampleInfo{
				{
					Timestamp: time.Now(),
					Interval:  20,
				},
				{
					Timestamp: time.Now(),
					Interval:  20,
				},
			},
			Value: []performance.MetricSeries{
				{
					Name:     "cpu.usage.average",
					Instance: "",
					Value:    []int64{100, 300},
				},
			},
		},
	}

	emitPerformanceMetrics(
		ch,
		"vcenter01",
		"HostSystem",
		"vmware",
		"host",
		"",
		countersSpec,
		targetNames,
		metrics,
		testLogger(),
	)

	select {
	case metric := <-ch:
		pb := &dto.Metric{}
		if err := metric.Write(pb); err != nil {
			t.Fatalf("failed to write prometheus metric: %v", err)
		}

		if pb.Gauge == nil {
			t.Fatal("expected gauge metric")
		}

		if got := pb.Gauge.GetValue(); got != 200 {
			t.Fatalf("gauge value = %v, want 200", got)
		}

		assertLabel(t, pb, "vcenter", "vcenter01")
		assertLabel(t, pb, "host", "esx01.example.local")
		assertLabel(t, pb, "hostmo", "host-123")

	default:
		t.Fatal("expected one prometheus metric")
	}
}

func TestEmitPerformanceMetricsSkipsMetricWhenInstanceIsMissingButRequired(t *testing.T) {
	ch := make(chan prometheus.Metric, 1)

	countersSpec := map[string]*types.PerfCounterInfo{
		"disk.usage.average": {
			NameInfo: &types.ElementDescription{
				Description: types.Description{Summary: "Disk usage"},
			},
			UnitInfo: &types.ElementDescription{
				Description: types.Description{Label: "kilobytes"},
			},
		},
	}

	metrics := []performance.EntityMetric{
		{
			Entity: types.ManagedObjectReference{
				Type:  "VirtualMachine",
				Value: "vm-123",
			},
			SampleInfo: []types.PerfSampleInfo{
				{Timestamp: time.Now(), Interval: 20},
			},
			Value: []performance.MetricSeries{
				{
					Name:     "disk.usage.average",
					Instance: "",
					Value:    []int64{42},
				},
			},
		},
	}

	emitPerformanceMetrics(
		ch,
		"vcenter01",
		"VirtualMachine",
		"vmware",
		"vm",
		"*",
		countersSpec,
		map[string]string{"vm-123": "my-vm"},
		metrics,
		testLogger(),
	)

	select {
	case <-ch:
		t.Fatal("expected no metric to be emitted")
	default:
		// expected
	}
}

func assertLabel(t *testing.T, metric *dto.Metric, name, want string) {
	t.Helper()

	for _, label := range metric.Label {
		if label.GetName() == name {
			if got := label.GetValue(); got != want {
				t.Fatalf("label %q = %q, want %q", name, got, want)
			}
			return
		}
	}

	t.Fatalf("expected label %q to exist", name)
}

// TestEmitPerformanceMetricsSumsDeltaCounters 锁住本轮修正的数据正确性 bug。
//
// vCenter 把 cpu.ready.summation 这类计数器的 StatsType 声明为 delta，语义是
// 「每个样本 = 该采样区间内的增量」。原实现对所有计数器一律求平均，对 delta
// 那是错的：3 个 20 秒区间各 ready 了 100ms，这一分钟内一共 ready 了 300ms，
// 不是 100ms。
//
// 这个 bug 有个让它长期潜伏的性质：默认配置下 samples = interval/granularity
// = 20/20 = 1，求和与求平均结果相同。它只在用户显式调大 -vmware.interval 时
// 才显形 —— 而那正是打算降低抓取频率的用户会做的事。所以这里必须构造多样本。
func TestEmitPerformanceMetricsSumsDeltaCounters(t *testing.T) {
	ch := make(chan prometheus.Metric, 4)

	countersSpec := map[string]*types.PerfCounterInfo{
		"cpu.ready.summation": {
			NameInfo: &types.ElementDescription{
				Description: types.Description{Summary: "CPU ready time"},
			},
			// UnitInfo.Key 才是翻译单位的依据，Label 只用于 help 文案。
			UnitInfo: &types.ElementDescription{
				Key:         "millisecond",
				Description: types.Description{Label: "millisecond"},
			},
			StatsType: types.PerfStatsTypeDelta,
		},
	}

	metrics := []performance.EntityMetric{
		{
			Entity: types.ManagedObjectReference{Type: "HostSystem", Value: "host-123"},
			SampleInfo: []types.PerfSampleInfo{
				{Timestamp: time.Now(), Interval: 20},
				{Timestamp: time.Now(), Interval: 20},
				{Timestamp: time.Now(), Interval: 20},
			},
			Value: []performance.MetricSeries{
				{
					Name:     "cpu.ready.summation",
					Instance: "",
					// 三个不等的样本：如果实现求了平均会得到 100（整除后
					// 还会额外截断 133.33），与求和的 400 明显可分。
					Value: []int64{100, 100, 200},
				},
			},
		},
	}

	emitPerformanceMetrics(
		ch, "vcenter01", "HostSystem", "vmware", "host", "",
		countersSpec, map[string]string{"host-123": "esx01.example.local"},
		metrics, testLogger(),
	)

	select {
	case metric := <-ch:
		pb := &dto.Metric{}
		if err := metric.Write(pb); err != nil {
			t.Fatalf("failed to write prometheus metric: %v", err)
		}

		// delta 计数器必须导出为 counter：它是单调累加的语义载体，
		// rate() 只对 counter 有意义。gauge 会让 rate() 静默给出垃圾。
		if pb.Counter == nil {
			t.Fatalf("expected a counter metric for a delta counter, got %v", pb)
		}
		if pb.Gauge != nil {
			t.Error("a delta counter must not be emitted as a gauge")
		}

		// (100+100+200) ms = 400 ms = 0.4 s。
		// 求平均会得到 133ms（整除截断后）→ 0.133s，与 0.4 差三倍。
		if got := pb.Counter.GetValue(); got != 0.4 {
			t.Fatalf("counter value = %v, want 0.4 (the three deltas summed, in seconds); "+
				"averaging would give ~0.133", got)
		}

		// 名字必须带 _total 后缀且不含 rollup 后缀。
		desc := metric.Desc().String()
		if !strings.Contains(desc, "vmware_host_cpu_ready_seconds_total") {
			t.Errorf("delta counter should be named vmware_host_cpu_ready_seconds_total: %s", desc)
		}

		assertLabel(t, pb, "vcenter", "vcenter01")
		assertLabel(t, pb, "host", "esx01.example.local")
		assertLabel(t, pb, "hostmo", "host-123")

	default:
		t.Fatal("expected one prometheus metric")
	}
}

// TestEmitPerformanceMetricsAveragesNonDeltaCounters 是上一个测试的对照组。
//
// 只断言 delta 求和是不够的：一个「一律求和」的实现也能通过它，而那会让
// cpu.usage.average 这类瞬时量随 samples 数线性放大 —— 采样窗口调宽一倍，
// CPU 使用率就翻一倍。
//
// 顺带锁住浮点除法：原实现用 int64 整除，三个样本 1/1/2 求平均得 1 而非 1.33。
func TestEmitPerformanceMetricsAveragesNonDeltaCounters(t *testing.T) {
	ch := make(chan prometheus.Metric, 4)

	countersSpec := map[string]*types.PerfCounterInfo{
		"cpu.usage.average": {
			NameInfo: &types.ElementDescription{
				Description: types.Description{Summary: "CPU usage"},
			},
			UnitInfo: &types.ElementDescription{
				Key:         "percent",
				Description: types.Description{Label: "percent"},
			},
			StatsType: types.PerfStatsTypeRate,
		},
	}

	metrics := []performance.EntityMetric{
		{
			Entity: types.ManagedObjectReference{Type: "HostSystem", Value: "host-123"},
			SampleInfo: []types.PerfSampleInfo{
				{Timestamp: time.Now(), Interval: 20},
				{Timestamp: time.Now(), Interval: 20},
				{Timestamp: time.Now(), Interval: 20},
			},
			Value: []performance.MetricSeries{
				{
					Name:     "cpu.usage.average",
					Instance: "",
					// 平均值 400/3 = 133.33…，整除会截断到 133。
					Value: []int64{100, 100, 200},
				},
			},
		},
	}

	emitPerformanceMetrics(
		ch, "vcenter01", "HostSystem", "vmware", "host", "",
		countersSpec, map[string]string{"host-123": "esx01.example.local"},
		metrics, testLogger(),
	)

	select {
	case metric := <-ch:
		pb := &dto.Metric{}
		if err := metric.Write(pb); err != nil {
			t.Fatalf("failed to write prometheus metric: %v", err)
		}

		if pb.Gauge == nil {
			t.Fatalf("expected a gauge metric for a non-delta counter, got %v", pb)
		}

		// percent 的换算是 ÷10000（取值 100 表示 1%），所以
		// 133.333…% 的百分之一点单位换算成 ratio 是 0.0133333…
		// 这里用容差比较：只有这一条涉及无法精确表示的除法。
		want := (400.0 / 3.0) * 1e-4
		if got := pb.Gauge.GetValue(); math.Abs(got-want) > 1e-12 {
			t.Fatalf("gauge value = %v, want %v; integer division would give %v",
				got, want, 133*1e-4)
		}

		desc := metric.Desc().String()
		if !strings.Contains(desc, "vmware_host_cpu_usage_ratio") {
			t.Errorf("percent counter should be named vmware_host_cpu_usage_ratio: %s", desc)
		}

	default:
		t.Fatal("expected one prometheus metric")
	}
}
