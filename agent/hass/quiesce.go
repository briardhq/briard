package hass

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// QUIESCING HOME ASSISTANT FOR A RING MEMBER ([B.143]) — the pair of calls that make the nightly
// member application-consistent instead of crash-consistent.
//
// WHY HOME ASSISTANT DOES THE LOCKING AND NOT US. The recorder's SQLite database is the one part
// of /config that a snapshot of a RUNNING service can catch mid-write, and Home Assistant already
// has the exact mechanism its own backups use (`recorder/util.py`, measured against the pinned
// 2026.7.1): a `PRAGMA wal_checkpoint(TRUNCATE)` followed by a held `BEGIN IMMEDIATE`. Taking that
// lock ourselves over a second connection would work and would also be worse in three ways —
// it needs sqlite in the guest image's tool profile, it makes the recorder THREAD collide with us
// (its writes fail and retry, in a household's log, while we hold the row), and it cannot tell us
// whether the lock survived. Going through the recorder's own task queue means Home Assistant
// stops writing and buffers instead of colliding.
//
// ⚠️ AND IT REPORTS WHETHER THE LOCK HELD, which is the half that decides the member's class.
// While locked, the recorder checks its backlog every 10s; if events pile up it logs "the backup
// cannot be trusted and must be restarted", resumes writing and sets `queue_overflow` — so
// `unlock_database()` returns FALSE and the member is crash-consistent after all. We do not have
// to infer that: Release says so, and the sidecar is written from what it says.
//
// ⚠️ THE API IS INTERNAL, and that is a deliberate acceptance rather than an oversight. It is not
// a documented integration surface, so a future Home Assistant may rename it — but it is what
// Home Assistant's OWN backup depends on, so it cannot quietly stop existing, and the fallback
// when it does is a member tagged `crash` rather than a member that lies. The nixosTest asserts
// the nightly comes out `quiesced` against the pinned image, so a rename lands as a red rig.
//
// TWO CALLS, NOT A CALLBACK. Home Assistant holds the lock across an operation that happens
// outside it — exactly as it does for its own backups, which lock, copy the file and unlock. The
// alternative (our integration calling back into the agent to ask for the snapshot while it holds)
// needs a new verb on the inbound channel that a titled member would let a compromised custom
// component call at will, and puts the decision inside the container. The objection to holding a
// lock across an outside step is the one Home Assistant already answered: if we die between the
// two calls, its own backlog check breaks the lock in about ten seconds and says so loudly.
const quiescePath = "/api/briard/quiesce"

// Hold asks Home Assistant to stop writing to its recorder database and hold still.
//
// ITS FAILURE IS NEVER FATAL to the caller: a Home Assistant that is down, starting, or running
// without our integration cannot quiesce, and the answer to that is a member taken anyway and
// labelled for what it is. The caller reads an error as "take it crash-consistent", never as
// "do not take it".
func Hold(ctx context.Context, x Executor, port int) error {
	base, access, err := connect(ctx, x, port)
	if err != nil {
		return err
	}
	_, err = quiesce(ctx, base, access, true)
	return err
}

// Release lets it write again, and reports whether the lock was held for the WHOLE window — Home
// Assistant's own `unlock_database()` answer. False means the recorder broke the lock while we
// were snapshotting, so the member it protected is crash-consistent.
//
// ⚠️ CALL IT EVEN WHEN THE SNAPSHOT FAILED. A lock nobody releases is released by Home Assistant
// itself, but only after its backlog check notices — up to ten seconds of a household's events
// buffered in memory for nothing.
func Release(ctx context.Context, x Executor, port int) (bool, error) {
	base, access, err := connect(ctx, x, port)
	if err != nil {
		return false, err
	}
	return quiesce(ctx, base, access, false)
}

// quiesce is the one request both halves make. `held` is meaningful only on release.
func quiesce(ctx context.Context, base, access string, hold bool) (bool, error) {
	body := `{"hold":false}`
	if hold {
		body = `{"hold":true}`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+quiescePath, strings.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("hass quiesce: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("hass quiesce(hold=%t): %w", hold, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 404 is the interesting one and it is not an error in the household's world: an older
		// integration, or one that did not load. Named so the journal says which, because the
		// caller's fallback is the same either way and the diagnosis is not.
		return false, fmt.Errorf("hass quiesce(hold=%t): HTTP %d", hold, resp.StatusCode)
	}
	var out struct {
		Held bool `json:"held"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("hass quiesce(hold=%t): the answer does not parse: %w", hold, err)
	}
	return out.Held, nil
}
