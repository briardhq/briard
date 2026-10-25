package platform

import "testing"

// DOWN AND STAYING DOWN is "inactive" alone. A failed unit needs someone, and one waiting out its
// RestartSec reads "activating" -- both are on their way back or broken, and the update must
// still reach them (it may be the fix), so neither reads as a node the user stopped.
func TestOnlyAnInactiveUnitIsStopped(t *testing.T) {
	for state, want := range map[string]bool{
		"inactive":     true,
		"failed":       false,
		"activating":   false,
		"active":       false,
		"deactivating": false,
		"reloading":    false,
		"":             false,
	} {
		if got := stoppedState(state); got != want {
			t.Errorf("stoppedState(%q) = %v, want %v", state, got, want)
		}
	}
}
