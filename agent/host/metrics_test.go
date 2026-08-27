package host

import (
	"testing"
	"time"

	"briard.io/shared/telemetry"
)

// DashboardMetrics is the privacy allowlist: only the product-health subset crosses, never
// the soak's leak instruments (agent RSS/FDs, per-service footprints, log/store sizes, kernel errors).
func TestDashboardMetricsAllowlist(t *testing.T) {
	r := &telemetry.NodeResources{
		VolumeUsedKB: 5000, Load1: 0.75,
		AgentRSSKB: 123, AgentFDs: 9,
		Payloads:  []telemetry.ServiceResources{{Name: "ha", RSSKB: 456, FDs: 7, Restarts: 2}},
		LogSizeKB: 999, PodmanStoreKB: 888, SnapshotCount: 3,
		KernelErrors: []string{"oops"},
	}
	m := dashboardMetrics(r)
	if len(m) != 2 || m["volume_used_kb"] != 5000 || m["load1"] != 0.75 {
		t.Fatalf("dashboardMetrics = %+v, want exactly volume_used_kb + load1", m)
	}
	// A zero field is "unread this cycle" and is skipped, never a spurious 0 sample.
	if got := dashboardMetrics(&telemetry.NodeResources{Load1: 1.2}); len(got) != 1 || got["load1"] != 1.2 {
		t.Fatalf("zero volume skipped: got %+v, want just load1", got)
	}
}

// The aggregator folds successive samples into per-window min/max/avg, keyed by bucket.
func TestAggregatorRollupMinMaxAvg(t *testing.T) {
	a := newMetricsAggregator(time.Minute, nil)
	base := time.Unix(0, 0).UTC() // a minute boundary

	a.add(base.Add(1*time.Second), &telemetry.NodeResources{VolumeUsedKB: 100, Load1: 0.2})
	a.add(base.Add(2*time.Second), &telemetry.NodeResources{VolumeUsedKB: 300, Load1: 0.6})
	a.add(base.Add(3*time.Second), &telemetry.NodeResources{VolumeUsedKB: 200, Load1: 0.4})

	snap := a.snapshot()
	byMetric := map[string]int{}
	for _, s := range snap {
		byMetric[s.Metric]++
		if !s.PeriodStart.Equal(base) {
			t.Fatalf("%s bucket = %v, want %v", s.Metric, s.PeriodStart, base)
		}
		switch s.Metric {
		case "volume_used_kb":
			if s.Min != 100 || s.Max != 300 || s.Avg != 200 || s.Samples != 3 {
				t.Errorf("volume = %+v, want min100 max300 avg200 n3", s)
			}
		case "load1":
			if s.Min != 0.2 || s.Max != 0.6 || s.Samples != 3 {
				t.Errorf("load1 = %+v, want min0.2 max0.6 n3", s)
			}
		}
	}
	if byMetric["volume_used_kb"] != 1 || byMetric["load1"] != 1 {
		t.Fatalf("snapshot metrics = %+v, want one bucket each", byMetric)
	}
}

// Across a window boundary the completed bucket rides along until an upload prunes it; the
// in-flight bucket stays resident so it keeps accumulating and re-upserting.
func TestAggregatorRolloverAndPrune(t *testing.T) {
	a := newMetricsAggregator(time.Minute, nil)
	m0 := time.Unix(0, 0).UTC()
	m1 := m0.Add(time.Minute)

	a.add(m0.Add(10*time.Second), &telemetry.NodeResources{Load1: 0.5})
	a.add(m1.Add(10*time.Second), &telemetry.NodeResources{Load1: 0.9})

	// Both buckets are offered for upload (the completed one is now final).
	if snap := a.snapshot(); len(snap) != 2 || !snap[0].PeriodStart.Equal(m0) || !snap[1].PeriodStart.Equal(m1) {
		t.Fatalf("snapshot = %+v, want m0 then m1", snap)
	}
	// A successful upload prunes everything before the current bucket.
	a.prune(m1.Add(20 * time.Second))
	snap := a.snapshot()
	if len(snap) != 1 || !snap[0].PeriodStart.Equal(m1) {
		t.Fatalf("after prune = %+v, want just the current bucket m1", snap)
	}
}

// A sustained outage can't grow the buffer without bound: the oldest completed buckets are
// dropped (with a log) past maxBuckets.
func TestAggregatorBounded(t *testing.T) {
	var logs int
	a := newMetricsAggregator(time.Minute, func(string, ...any) { logs++ })
	a.maxBuckets = 3
	base := time.Unix(0, 0).UTC()
	for i := 0; i < 6; i++ {
		a.add(base.Add(time.Duration(i)*time.Minute), &telemetry.NodeResources{Load1: 0.1})
	}
	if len(a.buckets) != 3 {
		t.Fatalf("retained %d buckets, want 3 (bounded)", len(a.buckets))
	}
	if logs == 0 {
		t.Fatal("dropping oldest buckets must log (no silent truncation)")
	}
	// The retained buckets are the newest three.
	snap := a.snapshot()
	if !snap[0].PeriodStart.Equal(base.Add(3 * time.Minute)) {
		t.Fatalf("oldest retained = %v, want minute 3", snap[0].PeriodStart)
	}
}
