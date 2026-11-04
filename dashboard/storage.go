package main

// THE STORAGE CARD: what an app keeps on this machine, per storage tier -- its data and its
// History, the household's files in it and their Versions -- and how far each may grow. The guest
// measures it (service-usage, relayed by the host like the History's list); the page loads the
// card once it is up, and the card asks again while the guest is still walking the app's areas,
// so the page never waits on the walk.
//
// ON DISK IS THE NUMBER, because it is what fills the disk: the volume is compressed, so the
// files' own size is said beside it only where compression made a difference. The History and
// the Versions are what deleting them would free, which is why they can be far smaller than what
// they keep.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"briard.io/shared/api"
	"briard.io/shared/dashboard"
)

// storageView is the card: one tier per row pair, the app's data then the household's files,
// and how far each may grow.
type storageView struct {
	Service   string // the app's slug, for the card's poll
	Measuring bool   // the guest is still walking: the card asks again
	Error     string
	Tiers     []storageTierView
	Room      string // "" when neither tier has a cap
}

// storageTierView is one tier in words: what it holds now under Name, its older copies under
// Past ("History" or "Versions").
type storageTierView struct {
	Name, Now, Past, PastWords string
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
		name, past, point, with, room string
		u                             *dashboard.TierUsage
	}{
		{"Data", "History", "snapshot", "with its history", "its data and history", u.Data},
		{"Your files", "Versions", "version", "with their versions", "your files and their versions", u.Files},
	} {
		// EVERY APP HAS A FILES AREA, empty until it uses it, and an empty one is not shown -- as
		// it is not exported. An empty area and a month of its versions stay under a megabyte.
		if t.u == nil || t.u == u.Files && t.u.Total < dashboard.FilesAreaMin {
			continue
		}
		v.Tiers = append(v.Tiers, storageTierView{Name: t.name, Now: nowWords(*t.u, u.Measuring, t.with), Past: t.past, PastWords: pastWords(*t.u, t.point)})
		if t.u.Cap >= 0 {
			room = append(room, gb(t.u.Cap)+" for "+t.room)
		}
	}
	if len(room) > 0 {
		v.Room = "Room: up to " + strings.Join(room, ", and ") + ". No app can take the last of the space from the others."
	}
	return v
}

// nowWords is what the tier's area holds now: on disk, and its size where compression shrank it.
func nowWords(t dashboard.TierUsage, measuring bool, with string) string {
	switch {
	case !t.Measured && measuring:
		return "Measuring… (" + gb(t.Total) + " " + with + ")"
	case !t.Measured:
		return "Not measured (" + gb(t.Total) + " " + with + ")"
	case t.Uncompressed > t.Current:
		return gb(t.Current) + " on disk · " + gb(t.Uncompressed) + " before compression"
	}
	return gb(t.Current) + " on disk"
}

// pastWords is what the tier's older copies hold, and how many it keeps.
func pastWords(t dashboard.TierUsage, point string) string {
	points := fmt.Sprintf("%d %ss", t.Members, point)
	switch t.Members {
	case 0:
		return "None yet"
	case 1:
		points = "1 " + point
	}
	if !t.Measured {
		return points
	}
	return gb(t.History) + " · " + points
}
