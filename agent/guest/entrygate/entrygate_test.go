package entrygate

import "testing"

func e(id, domain string, s State) Entry { return Entry{ID: id, Domain: domain, State: s} }

func TestAssess(t *testing.T) {
	tests := []struct {
		name     string
		pre      []Entry
		post     []Entry
		want     Verdict
		wantDoms []string // domains named in the findings (order-independent)
	}{
		{
			name: "all still loaded -> pass",
			pre:  []Entry{e("1", "hue", StateLoaded), e("2", "mqtt", StateLoaded)},
			post: []Entry{e("1", "hue", StateLoaded), e("2", "mqtt", StateLoaded)},
			want: Pass,
		},
		{
			name: "flaky-of-40 recovers within the window -> pass",
			// Weather was retrying pre and post but settled to loaded; cloud was
			// retrying pre and still retrying post but was never reliable, so excluded.
			pre:  []Entry{e("1", "hue", StateLoaded), e("2", "weather", StateSetupRetry), e("3", "cloud", StateSetupRetry)},
			post: []Entry{e("1", "hue", StateLoaded), e("2", "weather", StateLoaded), e("3", "cloud", StateSetupRetry)},
			want: Pass,
		},
		{
			name:     "migration_error on a single entry -> rollback (always)",
			pre:      []Entry{e("1", "hue", StateLoaded)},
			post:     []Entry{e("1", "hue", StateMigrationError)},
			want:     Rollback,
			wantDoms: []string{"hue"},
		},
		{
			name: "migration_error even on a pre-flaky entry -> rollback",
			// Migration failures are upgrade-caused regardless of the pre-state, so the
			// was-loaded exclusion does not apply to them.
			pre:      []Entry{e("1", "cloud", StateSetupRetry)},
			post:     []Entry{e("1", "cloud", StateMigrationError)},
			want:     Rollback,
			wantDoms: []string{"cloud"},
		},
		{
			name:     "single was-loaded -> setup_error -> hold (ambiguous middle)",
			pre:      []Entry{e("1", "hue", StateLoaded), e("2", "mqtt", StateLoaded)},
			post:     []Entry{e("1", "hue", StateSetupError), e("2", "mqtt", StateLoaded)},
			want:     Hold,
			wantDoms: []string{"hue"},
		},
		{
			name:     "cluster of was-loaded -> setup_error -> rollback",
			pre:      []Entry{e("1", "hue", StateLoaded), e("2", "mqtt", StateLoaded), e("3", "zwave", StateLoaded)},
			post:     []Entry{e("1", "hue", StateSetupError), e("2", "mqtt", StateSetupError), e("3", "zwave", StateLoaded)},
			want:     Rollback,
			wantDoms: []string{"hue", "mqtt"},
		},
		{
			name: "was-flaky -> setup_error is excluded (not attributable)",
			// Cloud was setup_retry pre (not reliable), so its terminal failure post is
			// not the upgrade's fault -> excluded -> pass.
			pre:  []Entry{e("1", "hue", StateLoaded), e("2", "cloud", StateSetupRetry)},
			post: []Entry{e("1", "hue", StateLoaded), e("2", "cloud", StateSetupError)},
			want: Pass,
		},
		{
			name:     "was-loaded -> still setup_retry at the deadline -> hold",
			pre:      []Entry{e("1", "hue", StateLoaded)},
			post:     []Entry{e("1", "hue", StateSetupRetry)},
			want:     Hold,
			wantDoms: []string{"hue"},
		},
		{
			name:     "was-loaded -> not_loaded (disabled by the upgrade) -> hold",
			pre:      []Entry{e("1", "hue", StateLoaded)},
			post:     []Entry{e("1", "hue", StateNotLoaded)},
			want:     Hold,
			wantDoms: []string{"hue"},
		},
		{
			name: "entry new in post is ignored in S1",
			pre:  []Entry{e("1", "hue", StateLoaded)},
			post: []Entry{e("1", "hue", StateLoaded), e("2", "brandnew", StateSetupError)},
			want: Pass,
		},
		{
			name: "one terminal + one non-settling -> hold, both findings surfaced",
			pre:  []Entry{e("1", "hue", StateLoaded), e("2", "mqtt", StateLoaded)},
			post: []Entry{e("1", "hue", StateSetupError), e("2", "mqtt", StateSetupRetry)},
			// One terminal (below the cluster threshold) + one non-settling -> hold,
			// carrying both the terminal and the retry finding.
			want:     Hold,
			wantDoms: []string{"hue", "mqtt"},
		},
		{
			name: "migration_error dominates a hold",
			// A lone terminal regression would be a Hold, but a concurrent
			// migration_error forces Rollback (rollbacks reach the threshold via the
			// always-rollback migration finding).
			pre:      []Entry{e("1", "hue", StateLoaded), e("2", "mqtt", StateLoaded)},
			post:     []Entry{e("1", "hue", StateSetupError), e("2", "mqtt", StateMigrationError)},
			want:     Rollback,
			wantDoms: []string{"hue", "mqtt"},
		},
		{
			name: "empty pre and post -> pass",
			pre:  nil,
			post: nil,
			want: Pass,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Assess(tc.pre, tc.post)
			if got.Verdict != tc.want {
				t.Errorf("verdict = %q, want %q (reason: %s)", got.Verdict, tc.want, got.Reason())
			}
			gotDoms := map[string]bool{}
			for _, f := range got.Findings {
				gotDoms[f.Domain] = true
			}
			if len(gotDoms) != len(tc.wantDoms) {
				t.Errorf("findings domains = %v, want %v", keys(gotDoms), tc.wantDoms)
			}
			for _, d := range tc.wantDoms {
				if !gotDoms[d] {
					t.Errorf("missing finding for domain %q (got %v)", d, keys(gotDoms))
				}
			}
		})
	}
}

// TestFindingsSorted proves the reason is deterministic regardless of sample order.
func TestFindingsSorted(t *testing.T) {
	pre := []Entry{e("1", "zwave", StateLoaded), e("2", "hue", StateLoaded), e("3", "mqtt", StateLoaded)}
	post := []Entry{e("1", "zwave", StateSetupError), e("2", "hue", StateSetupError), e("3", "mqtt", StateSetupError)}
	got := Assess(pre, post)
	if got.Verdict != Rollback {
		t.Fatalf("verdict = %q, want rollback", got.Verdict)
	}
	wantOrder := []string{"hue", "mqtt", "zwave"} // sorted by domain
	if len(got.Findings) != len(wantOrder) {
		t.Fatalf("findings = %d, want %d", len(got.Findings), len(wantOrder))
	}
	for i, f := range got.Findings {
		if f.Domain != wantOrder[i] {
			t.Errorf("finding[%d].Domain = %q, want %q", i, f.Domain, wantOrder[i])
		}
	}
}

// TestClusterThresholdPolicy proves the threshold is honored: at threshold=1 a lone
// terminal regression rolls back instead of holding.
func TestClusterThresholdPolicy(t *testing.T) {
	pre := []Entry{e("1", "hue", StateLoaded)}
	post := []Entry{e("1", "hue", StateSetupError)}

	if v := (Policy{ClusterThreshold: 1}).Assess(pre, post).Verdict; v != Rollback {
		t.Errorf("threshold=1: verdict = %q, want rollback", v)
	}
	if v := (Policy{ClusterThreshold: 2}).Assess(pre, post).Verdict; v != Hold {
		t.Errorf("threshold=2: verdict = %q, want hold", v)
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
