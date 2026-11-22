package host

import (
	"strings"
	"testing"

	"briard.io/shared/api"
)

// The floor a version may carry, against this briard's own release id. Ordered on the date
// field, as every ordering in the channel is: the same hour is not older, whatever the revision.
func TestBriardTooOldFor(t *testing.T) {
	const floor = "v6.26120110.abcdef0"
	for _, tc := range []struct {
		name, floor, briard string
		refused             bool
	}{
		{"a version with no floor runs on anything", "", "v5.26112015.d76f0bc", false},
		{"a version with no floor runs on a build of no release", "", "", false},
		{"an older briard", floor, "v5.26112015.d76f0bc", true},
		{"the release the floor names", floor, floor, false},
		{"the same hour, another revision", floor, "v6.26120110.1234567", false},
		{"a newer briard", floor, "v6.26121509.7654321", false},
		{"a build of no release has no date to compare", floor, "", true},
		{"a build of no release, named", floor, "v6.dirty", true},
	} {
		m := testManifest()
		m.MinBriard = tc.floor
		why := Config{Version: tc.briard}.briardTooOldFor(m)
		if (why != "") != tc.refused {
			t.Errorf("%s: refusal %q, want refused=%v", tc.name, why, tc.refused)
		}
		if tc.refused && (!strings.Contains(why, "Update Briard first") || !strings.Contains(why, m.Name)) {
			t.Errorf("%s: the refusal does not name the app and the remedy: %q", tc.name, why)
		}
	}
}

// TestAVersionThisBriardIsTooOldForIsRefusedUntouched: the install and the prewarm both refuse a
// version whose floor is above this briard, before the guest is asked for anything, and both go
// ahead once the briard is at the floor.
func TestAVersionThisBriardIsTooOldForIsRefusedUntouched(t *testing.T) {
	const floor = "v6.26120110.abcdef0"
	m := testManifest()
	m.MinBriard = floor

	for name, run := range map[string]func(Config, *fakeInstaller) api.DirectiveOutcome{"install": installApp, "prewarm": prewarm} {
		old := catalogFor(t, m)
		old.Version = "v5.26112015.d76f0bc"
		f := &fakeInstaller{primary: true, active: true, healthy: true}
		o := run(old, f)
		if o.State != api.OutcomeFailed || !strings.Contains(o.Detail, "Update Briard first") {
			t.Errorf("%s on an older briard: outcome = %+v, want a refusal naming the remedy", name, o)
		}
		if len(f.steps) != 0 || len(f.manifests) != 0 {
			t.Errorf("%s on an older briard still touched the node: steps=%v manifests=%d", name, f.steps, len(f.manifests))
		}

		current := catalogFor(t, m)
		current.Version = floor
		f = &fakeInstaller{primary: true, active: true, healthy: true}
		if o := run(current, f); o.State != api.OutcomeDone {
			t.Errorf("%s on the briard the floor names: outcome = %+v, want done", name, o)
		}
		if len(f.steps) == 0 {
			t.Errorf("%s on the briard the floor names did nothing", name)
		}
	}
}
