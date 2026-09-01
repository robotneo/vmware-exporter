package vmwareCollectors

import (
	"context"
	"io"
	"log/slog"
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
