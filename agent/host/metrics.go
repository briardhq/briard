package host

import (
	"sort"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/telemetry"
)

// dashboardMetrics is the closed allowlist of resource fields that leave a node as cloud
// aggregates: product-health signals fit for a cloud dashboard -- disk capacity used
// and system load -- not the soak's internal leak instruments (agent/service RSS+FDs,
// log/store sizes, guest kernel errors, restart counts), which stay home on shared/telemetry
// . Expanding this set is a deliberate, visible act -- and a
// cheap one: the cloud rollup is long-format (metric name as data), so a new metric is new
// rows, not a schema change. A zero value means "unread this cycle" (the telemetry
// convention) and is skipped, so a transient read miss never poisons a bucket's min with a
// spurious 0 (a node's formatted volume is never legitimately 0, and a running
// node's load is virtually never exactly 0.00).
func dashboardMetrics(r *telemetry.NodeResources) map[string]float64 {
	out := map[string]float64{}
	if r.VolumeUsedKB > 0 {
		out["volume_used_kb"] = float64(r.VolumeUsedKB)
	}
	if r.Load1 > 0 {
		out["load1"] = r.Load1
	}
	return out
}

// metricBucket is the running min/max/sum/count for one metric over one time bucket -- an
// online accumulator, so no raw sample is ever retained.
type metricBucket struct {
	min, max, sum float64
	n             int
}

func (b *metricBucket) add(v float64) {
	if b.n == 0 || v < b.min {
		b.min = v
	}
	if b.n == 0 || v > b.max {
		b.max = v
	}
	b.sum += v
	b.n++
}

// metricsAggregator rolls a node's per-cycle resource samples into per-window min/max/avg
// aggregates (the window is one hour in production; a short window in the soak/tests
// exercises rollover quickly). It is an online accumulator: it never stores raw samples, only
// the running aggregate per (bucket, metric). The current bucket is re-uploaded each cycle so
// a dashboard sees the in-flight hour live; a just-completed bucket rides along until an
// upload acks it (prune), then drops out. Bounded: at most maxBuckets are retained, so a long
// cloud outage drops the oldest completed buckets with a log (an honest gap -- telemetry is a
// signal, not a gate). Not restart-durable (in-memory, matching the best-effort telemetry
// stance); a persisted node buffer with age-out is future work.
type metricsAggregator struct {
	window     time.Duration
	maxBuckets int
	buckets    map[time.Time]map[string]*metricBucket
	logf       func(string, ...any)
}

// newMetricsAggregator builds an aggregator bucketing on window (<=0 -> one hour, the
// production default). maxBuckets bounds retention across a cloud outage.
func newMetricsAggregator(window time.Duration, logf func(string, ...any)) *metricsAggregator {
	if window <= 0 {
		window = time.Hour
	}
	return &metricsAggregator{
		window:     window,
		maxBuckets: 48, // ~2 days of hourly buckets before an outage starts dropping the oldest
		buckets:    map[time.Time]map[string]*metricBucket{},
		logf:       logf,
	}
}

// Add folds one cycle's sample into its time bucket (now truncated to the window). Only the
// allowlisted, present (non-zero) dashboard metrics are folded; a cycle with none is a no-op.
func (a *metricsAggregator) add(now time.Time, r *telemetry.NodeResources) {
	if r == nil {
		return
	}
	m := dashboardMetrics(r)
	if len(m) == 0 {
		return
	}
	start := now.UTC().Truncate(a.window)
	mb := a.buckets[start]
	if mb == nil {
		mb = map[string]*metricBucket{}
		a.buckets[start] = mb
		a.enforceBound()
	}
	for name, v := range m {
		acc := mb[name]
		if acc == nil {
			acc = &metricBucket{}
			mb[name] = acc
		}
		acc.add(v)
	}
}

// Snapshot renders every retained bucket as wire aggregates (min/max/avg + sample count), in
// bucket-then-metric order -- what the observe loop uploads each cycle.
func (a *metricsAggregator) snapshot() []api.MetricAggregate {
	starts := make([]time.Time, 0, len(a.buckets))
	for start := range a.buckets {
		starts = append(starts, start)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	var out []api.MetricAggregate
	for _, start := range starts {
		mb := a.buckets[start]
		names := make([]string, 0, len(mb))
		for name := range mb {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			acc := mb[name]
			out = append(out, api.MetricAggregate{
				Metric: name, PeriodStart: start,
				Min: acc.min, Max: acc.max, Avg: acc.sum / float64(acc.n), Samples: acc.n,
			})
		}
	}
	return out
}

// Prune drops every bucket before now's bucket: after a successful upload the completed
// buckets are durably in the cloud, so only the in-flight bucket stays resident to keep
// accumulating and re-upserting. Called only on upload success -- an unacked bucket is kept
// and retried next cycle.
func (a *metricsAggregator) prune(now time.Time) {
	cur := now.UTC().Truncate(a.window)
	for start := range a.buckets {
		if start.Before(cur) {
			delete(a.buckets, start)
		}
	}
}

// EnforceBound caps retained buckets, dropping the oldest with a log when a sustained cloud
// outage backs them up past maxBuckets -- an honest gap, never a silent truncation (CONTRIBUTING.md).
func (a *metricsAggregator) enforceBound() {
	for len(a.buckets) > a.maxBuckets {
		var oldest time.Time
		first := true
		for start := range a.buckets {
			if first || start.Before(oldest) {
				oldest, first = start, false
			}
		}
		delete(a.buckets, oldest)
		if a.logf != nil {
			a.logf("metrics: dropped oldest aggregate bucket %s (cloud unreachable past %d buckets)",
				oldest.Format(time.RFC3339), a.maxBuckets)
		}
	}
}
