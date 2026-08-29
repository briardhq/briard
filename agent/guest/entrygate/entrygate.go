// Package entrygate is the S1 (stateless) health-gate signal for a Home Assistant
// service upgrade. It turns two samples of HA's per-config-entry setup
// state — one taken pre-upgrade at steady state, one taken post-upgrade after the
// retry backoff has settled — into a Verdict: keep the upgrade, hold for a human, or
// auto-roll-back the {code+data} snapshot.
//
// The signal is deliberately *differential* and rides HA's own state machine rather
// than a single snapshot's raw shape. The pre sample is the confounder control: an
// integration that was already flaky (setup_retry) or down (not_loaded) before the
// upgrade is excluded, so only entries that were solidly `loaded` and regressed count.
// And HA's transient-vs-terminal split does the denoising for us — a network/WAN flap
// during the settle window lands in setup_retry (HA retries it), while a genuine
// incompatibility lands in the terminal setup_error / migration_error. So the gate can
// trust a terminal regression without the fleet cross-check that S3 will later add.
//
// This package is pure: no I/O, no HA client, no clock. Fetching the samples and the
// settle poll live at the Manager seam (guest.ReadinessAssessor); the denoising
// *doctrine* lives here, unit-tested in isolation. S2 (a cross-boot reliability prior)
// and S3 (fleet correlation) layer on top later — they strengthen "was loaded
// once" into "reliably loaded" and rule out environmental confounders across homes;
// neither changes this function's contract.
package entrygate

import (
	"fmt"
	"sort"
	"strings"
)

// State is a Home Assistant config-entry setup state — the subset the health-gate
// reasons about (HA emits a few more, e.g. failed_unload, which collapse to "not a
// signal"). See homeassistant.config_entries.ConfigEntryState.
type State string

const (
	// StateLoaded is set up and running — the one good terminal state.
	StateLoaded State = "loaded"
	// StateSetupRetry is failed-but-HA-is-retrying-with-backoff: transient by
	// definition, and where most network/environmental flakiness lives. Never a
	// rollback signal on its own (a 40-integration home routinely has 1–2 here).
	StateSetupRetry State = "setup_retry"
	// StateSetupInProgress is still-booting: transient, treated like setup_retry.
	StateSetupInProgress State = "setup_in_progress"
	// StateSetupError is failed *terminally* — HA has given up retrying. A
	// previously-loaded entry landing here is the core regression signal.
	StateSetupError State = "setup_error"
	// StateMigrationError is a config-entry migration failure — the upgrade-specific
	// class (cousin of the recorder schema migration). Always a
	// high-confidence trigger: it only happens because the version bumped, so it is
	// never an environmental confounder.
	StateMigrationError State = "migration_error"
	// StateNotLoaded is disabled / never set up.
	StateNotLoaded State = "not_loaded"
)

// Entry is one config entry's identity and state at a single sample.
type Entry struct {
	ID     string // entry_id — stable across the upgrade, the join key between pre and post
	Domain string // integration domain (hue, mqtt, …): the human reason + the S3 fleet key
	State  State
}

// Verdict is the gate's decision. Ordered by consequence: Pass keeps the upgrade,
// Rollback restores the pre-upgrade snapshot, Hold is the ambiguous middle — freeze,
// alert, one-tap rollback (delivery lands with the notify channel; until then a
// Hold leans on the rollback window + user-as-sensor and is surfaced, not acted on).
type Verdict string

const (
	Pass     Verdict = "pass"
	Hold     Verdict = "hold"
	Rollback Verdict = "rollback"
)

// Finding records one entry that drove a non-Pass verdict, for the log/UI reason.
type Finding struct {
	ID, Domain string
	Was, Now   State
	Reason     string
}

// Result is the gate outcome plus the findings that produced it.
type Result struct {
	Verdict  Verdict
	Findings []Finding
}

// Policy carries the one tunable in the S1 gate: how many terminal regressions make a
// *cluster* (high-confidence auto-rollback) versus a single ambiguous one (hold).
type Policy struct {
	// ClusterThreshold is the count of was-loaded→setup_error regressions at or above
	// which the gate auto-rolls-back instead of holding. Biased by consequence
	// Hold: a single terminal regression is the ambiguous middle — it might be a
	// genuinely-removed/changed integration, so hold and let the user or the rollback
	// window decide; two or more independent integrations going terminal at once is
	// far likelier a version bug than N simultaneous environmental failures (which
	// HA's retry state machine would have parked in setup_retry anyway), so roll back.
	// A migration_error is always a cluster-of-one — it is deterministic and
	// upgrade-caused, never confoundable.
	ClusterThreshold int
}

// DefaultPolicy is the shipped S1 policy: a lone terminal regression holds, two roll back.
var DefaultPolicy = Policy{ClusterThreshold: 2}

// Assess runs the default-policy S1 verdict over a pre/post entry-state pair.
func Assess(pre, post []Entry) Result { return DefaultPolicy.Assess(pre, post) }

// Assess compares a pre-upgrade steady-state sample against a settled post-upgrade
// sample and returns the S1 verdict. It is the whole S1 signal:
//
//   - migration_error on any entry            → Rollback   (upgrade-specific, deterministic)
//   - a cluster of was-loaded→setup_error      → Rollback   (≥ ClusterThreshold; a version bug)
//   - a single was-loaded→setup_error          → Hold       (ambiguous middle)
//   - was-loaded→(setup_retry / in_progress /  → Hold       (did not settle back)
//     not_loaded) still at the settle deadline
//   - everything else                          → Pass
//
// Entries that were not `loaded` pre-upgrade are excluded (they were not reliable, so a
// post-upgrade failure can't be attributed to the upgrade), as are entries new in post
// (S1 makes no claim about integrations the upgrade added). The HTTP-200 liveness floor
// sits *beneath* this: if HA never answers, the gate never gets a post sample and the
// upgrade deadline trips a rollback on its own.
func (p Policy) Assess(pre, post []Entry) Result {
	preState := make(map[string]State, len(pre))
	for _, e := range pre {
		preState[e.ID] = e.State
	}

	// Migrations always roll back (deterministic, upgrade-caused, threshold-immune);
	// regressions (was-loaded→setup_error) are cluster-gated; nonSettled are holds.
	var migrations, regressions, nonSettled []Finding
	for _, e := range post {
		was := preState[e.ID] // "" if the entry is new in post

		switch {
		case e.State == StateMigrationError:
			// Always a rollback, regardless of the pre-state: a migration only runs
			// because the version bumped, so its failure is unambiguously the upgrade.
			migrations = append(migrations, finding(e, was, "config-entry migration failed"))

		case was != StateLoaded:
			// Not reliable before the upgrade (flaky, down, disabled, or brand new) —
			// a post failure isn't ours to attribute. Excluded.

		case e.State == StateSetupError:
			// Was solidly loaded, now terminally failed: the core regression signal.
			regressions = append(regressions, finding(e, was, "was loaded, now terminally failed"))

		case e.State == StateSetupRetry || e.State == StateSetupInProgress || e.State == StateNotLoaded:
			// Was loaded, but did not settle back to loaded within the window. Could be
			// transient/environmental (setup_retry) — hold, don't auto-roll-back.
			nonSettled = append(nonSettled, finding(e, was, "was loaded, did not settle back"))

			// E.State == StateLoaded (or any other) → still healthy → Pass contributor.
		}
	}

	// A migration_error, or a *cluster* of terminal regressions, is high-confidence →
	// Rollback (carrying every terminal finding). A single regression is the ambiguous
	// middle, and a non-settling entry never auto-rolls-back → Hold. Otherwise Pass.
	switch {
	case len(migrations) > 0 || len(regressions) >= p.ClusterThreshold:
		return Result{Verdict: Rollback, Findings: sortFindings(concat(migrations, regressions))}
	case len(regressions) > 0:
		return Result{Verdict: Hold, Findings: sortFindings(concat(regressions, nonSettled))}
	case len(nonSettled) > 0:
		return Result{Verdict: Hold, Findings: sortFindings(nonSettled)}
	default:
		return Result{Verdict: Pass}
	}
}

func finding(e Entry, was State, reason string) Finding {
	return Finding{ID: e.ID, Domain: e.Domain, Was: was, Now: e.State, Reason: reason}
}

func concat(a, b []Finding) []Finding { return append(append([]Finding{}, a...), b...) }

// sortFindings orders findings deterministically (domain, then id) so a verdict's
// reason string is stable across runs — the samples arrive in HA's arbitrary order.
func sortFindings(fs []Finding) []Finding {
	sort.Slice(fs, func(i, j int) bool {
		if fs[i].Domain != fs[j].Domain {
			return fs[i].Domain < fs[j].Domain
		}
		return fs[i].ID < fs[j].ID
	})
	return fs
}

// Reason renders a Result's findings as a single human/log line.
func (r Result) Reason() string {
	if len(r.Findings) == 0 {
		return string(r.Verdict)
	}
	var b strings.Builder
	b.WriteString(string(r.Verdict))
	b.WriteByte(':')
	for _, f := range r.Findings {
		fmt.Fprintf(&b, " %s(%s→%s: %s)", f.Domain, f.Was, f.Now, f.Reason)
	}
	return b.String()
}
