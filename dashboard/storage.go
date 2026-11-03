package main

// THE STORAGE CARD: what an app keeps on this machine, per tier -- its data as it is now, and its
// history -- and how far each may grow. The guest measures it (service-usage, relayed by the host
// like the History's list); the page loads the card once it is up, and the card asks again while
// the guest is still walking the app's files, so the page never waits on the walk.
//
// ON DISK IS THE NUMBER, because it is what fills the disk: the volume is compressed, so the
// files' own size is said beside it only where compression made a difference. The history is
// what deleting it would free, which is why it can be far smaller than the data it keeps.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/dashboard"
)

// storageView is the card: one tier per row pair, the app's then the household's files', and
// how far each may grow.
type storageView struct {
	Service   string // the app's slug, for the card's poll
	Measuring bool   // the guest is still walking: the card asks again
	Error     string
	Tiers     []storageTierView
	Room      string // "" when neither tier has a cap
}

// userShown is what a user area must hold, with its history, before the card shows it.
const userShown = 1_000_000

// storageTierView is one tier in words: Name is "App" or "User".
type storageTierView struct {
	Name, Data, History string
}

// storage asks the host what the app keeps, and says it.
func (a *app) storage(ctx context.Context, service string) storageView {
	v := storageView{Service: service}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	o, err := a.port.Submit(ctx, api.Directive{Kind: api.DirectiveServiceUsage, Payload: service})
	if err == nil && o.State != api.OutcomeDone {
		err = fmt.Errorf("%s", o.Detail)
	}
	var u dashboard.AppUsage
	if err == nil {
		err = json.Unmarshal([]byte(o.Detail), &u)
	}
	if err != nil {
		v.Error = err.Error()
		return v
	}
	v.Measuring = u.Measuring
	var room []string
	for _, t := range []struct {
		name, room string
		u          *dashboard.TierUsage
	}{{"App", "its data and history", u.Service}, {"User", "your files and their history", u.User}} {
		// EVERY APP HAS A USER AREA, empty until it uses it, and an empty one is not shown -- as
		// it is not exported. An empty area and a month of its snapshots stay under a megabyte.
		if t.u == nil || t.name == "User" && t.u.Total < userShown {
			continue
		}
		v.Tiers = append(v.Tiers, storageTierView{Name: t.name, Data: dataWords(*t.u, u.Measuring), History: historyWords(*t.u)})
		if t.u.Cap >= 0 {
			room = append(room, gb(t.u.Cap)+" for "+t.room)
		}
	}
	if len(room) > 0 {
		v.Room = "Room: up to " + strings.Join(room, ", and ") + ". No app can take the last of the space from the others."
	}
	return v
}

// dataWords is the tier's current data: on disk, and the files' size where compression shrank them.
func dataWords(t dashboard.TierUsage, measuring bool) string {
	switch {
	case !t.Measured && measuring:
		return "Measuring… (" + gb(t.Total) + " with its history)"
	case !t.Measured:
		return "Not measured (" + gb(t.Total) + " with its history)"
	case t.Files > t.Current:
		return gb(t.Current) + " on disk · " + gb(t.Files) + " before compression"
	}
	return gb(t.Current) + " on disk"
}

// historyWords is what the tier's history holds, and how many points it keeps.
func historyWords(t dashboard.TierUsage) string {
	points := fmt.Sprintf("%d snapshots", t.Members)
	switch t.Members {
	case 0:
		return "None yet"
	case 1:
		points = "1 snapshot"
	}
	if !t.Measured {
		return points
	}
	return gb(t.History) + " · " + points
}
