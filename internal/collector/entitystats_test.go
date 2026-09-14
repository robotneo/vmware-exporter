package collector

import (
	"testing"
)

// TestEntityStatsAccumulatesAcrossCollectors 验证同一 (collector, kind) 的
// 多次上报会累加 —— host collector 与两个 esxcli collector 都统计 host 实体，
// 它们按 collector 名分开，但单个 collector 内部多台主机是逐台/分批上报的。
func TestEntityStatsAccumulatesAcrossReports(t *testing.T) {
	stats := NewEntityStats()

	stats.RecordEntities("vm", "vm", 10, 8, map[string]int{
		SkipReasonPoweredOff: 2,
	})
	// 第二次上报必须累加而不是覆盖（模拟分批或重复调用）。
	stats.RecordEntities("vm", "vm", 0, 3, map[string]int{
		SkipReasonPoweredOff: 1,
		SkipReasonSuspended:  1,
	})

	snaps := stats.snapshot()
	if len(snaps) != 1 {
		t.Fatalf("snapshot() returned %d rows, want 1 (same collector/kind must merge)", len(snaps))
	}

	got := snaps[0]
	// found/emitted 是两次上报之和。
	if got.found != 10 {
		t.Errorf("found = %d, want 10", got.found)
	}
	if got.emitted != 11 {
		t.Errorf("emitted = %d, want 11 (8+3 must accumulate, not overwrite)", got.emitted)
	}
	if got.skipped[SkipReasonPoweredOff] != 3 {
		t.Errorf("skipped[powered_off] = %d, want 3", got.skipped[SkipReasonPoweredOff])
	}
	if got.skipped[SkipReasonSuspended] != 1 {
		t.Errorf("skipped[suspended] = %d, want 1", got.skipped[SkipReasonSuspended])
	}
}

// TestEntityStatsSeparatesCollectorAndKind 验证不同 collector（即使统计同一
// kind，如 host 与 esxcli.host.nic）产生独立的行，不会互相串账。
func TestEntityStatsSeparatesCollectorAndKind(t *testing.T) {
	stats := NewEntityStats()

	stats.RecordEntities("host", "host", 5, 5, nil)
	stats.RecordEntities("esxcli.host.nic", "host", 5, 4, map[string]int{
		SkipReasonMaintenance: 1,
	})

	snaps := stats.snapshot()
	if len(snaps) != 2 {
		t.Fatalf("snapshot() returned %d rows, want 2", len(snaps))
	}

	byKey := map[string]entitySnapshot{}
	for _, s := range snaps {
		byKey[s.collectorName+"/"+s.kind] = s
	}

	host, ok := byKey["host/host"]
	if !ok {
		t.Fatalf("missing host/host row: %+v", byKey)
	}
	if host.found != 5 || host.emitted != 5 {
		t.Errorf("host row = found %d emitted %d, want 5/5", host.found, host.emitted)
	}
	if len(host.skipped) != 0 {
		t.Errorf("host skipped = %v, want empty", host.skipped)
	}

	nic, ok := byKey["esxcli.host.nic/host"]
	if !ok {
		t.Fatalf("missing esxcli.host.nic/host row: %+v", byKey)
	}
	if nic.skipped[SkipReasonMaintenance] != 1 {
		t.Errorf("nic skipped[maintenance] = %d, want 1", nic.skipped[SkipReasonMaintenance])
	}
}

// TestNilEntityStatsReportsAreNoop 保证裸 Scrape（未注入 stats，单元测试与
// 某些直接驱动 collector 的场景）调用上报不会 panic。
func TestNilEntityStatsReportsAreNoop(t *testing.T) {
	var stats *EntityStats

	// 不得 panic。
	stats.RecordEntities("vm", "vm", 1, 1, nil)

	var s *Scrape
	s.RecordEntities("vm", "vm", 1, 1, nil)
}

// TestSnapshotDoesNotAliasInternalMap 验证调用方改 snapshot 返回的 map
// 不会污染统计器内部状态（snapshot 返回的是拷贝）。
func TestSnapshotDoesNotAliasInternalMap(t *testing.T) {
	stats := NewEntityStats()
	stats.RecordEntities("host", "host", 3, 2, map[string]int{SkipReasonPoweredOff: 1})

	snaps := stats.snapshot()
	snaps[0].skipped[SkipReasonPoweredOff] = 999
	snaps[0].found = 999

	again := stats.snapshot()
	if again[0].found == 999 || again[0].skipped[SkipReasonPoweredOff] == 999 {
		t.Fatal("snapshot returned an aliased map/slice; mutating it changed the stats internals")
	}
}
