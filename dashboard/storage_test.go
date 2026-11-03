package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"briard.io/shared/api"
	"briard.io/shared/dashboard"
)

// usageAnswers answers the app page's two reads: the ring (members), and what the app stores --
// whatever the test last set, or a failure when it set an error.
type usageAnswers struct {
	mu    sync.Mutex
	usage dashboard.AppUsage
	fail  string
}

func (u *usageAnswers) set(usage dashboard.AppUsage) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.usage, u.fail = usage, ""
}

func answerUsage(port *fakePort, members string) *usageAnswers {
	u := &usageAnswers{}
	go func() {
		for range port.waiting {
			port.mu.Lock()
			d := port.asked[len(port.asked)-1]
			port.mu.Unlock()
			switch d.Kind {
			case api.DirectiveServiceMembers:
				port.answer <- api.DirectiveOutcome{State: api.OutcomeDone, Detail: members}
			case api.DirectiveServiceUsage:
				u.mu.Lock()
				raw, _ := json.Marshal(u.usage)
				o := api.DirectiveOutcome{State: api.OutcomeDone, Detail: string(raw)}
				if u.fail != "" {
					o = api.DirectiveOutcome{State: api.OutcomeFailed, Detail: u.fail}
				}
				u.mu.Unlock()
				port.answer <- o
			default:
				port.answer <- api.DirectiveOutcome{State: api.OutcomeFailed, Detail: "not this test's"}
			}
		}
	}()
	return u
}

// THE STORAGE CARD loads itself: the page asks nothing of it, and offers a way without a script.
// While the guest measures, the card has the totals and the counts and asks again; once it has
// the numbers it says the data on disk with the files' size beside it where compression shrank
// them, the history's size and count, and the room -- and stops asking. An app
// keeping nothing in its user area has no user rows.
func TestTheStorageCardLoadsItselfAndStopsAskingOnceMeasured(t *testing.T) {
	r := newRig(t)
	c := r.trust()
	port := newFakePort()
	r.app.port = port
	u := answerUsage(port, ring())

	page := r.get(t, "/apps/home-assistant", c)
	if !strings.Contains(page, `hx-get="/f/storage?app=home-assistant" hx-trigger="load"`) || !strings.Contains(page, `href="/apps/home-assistant?storage"`) {
		t.Fatalf("the page does not load its storage card, or offers no way without a script:\n%s", page)
	}
	port.mu.Lock()
	for _, d := range port.asked {
		if d.Kind == api.DirectiveServiceUsage {
			t.Error("rendering the page asked what the app stores; the card asks for itself")
		}
	}
	port.mu.Unlock()

	u.set(dashboard.AppUsage{Measuring: true,
		Service: &dashboard.TierUsage{Total: 3e9, Cap: 40e9, Members: 3},
		User:    &dashboard.TierUsage{Total: 50e9, Cap: -1, Members: 1}})
	body := r.get(t, "/f/storage?app=home-assistant", c)
	for _, want := range []string{
		`hx-get="/f/storage?app=home-assistant" hx-trigger="every 5s"`,
		"<h3>App data</h3><p>Measuring… (3.00 GB with its history)</p>",
		"<h3>App history</h3><p>3 snapshots</p>",
		"<h3>User data</h3><p>Measuring… (50.00 GB with its history)</p>",
		"<h3>User history</h3><p>1 snapshot</p>",
		"Room: up to 40.00 GB for its data and history. No app",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("while measuring, the card does not say %q:\n%s", want, body)
		}
	}

	u.set(dashboard.AppUsage{
		Service: &dashboard.TierUsage{Total: 3e9, Cap: 40e9, Members: 3, Measured: true, Current: 2e9, Files: 5e9, History: 1e9},
		User:    &dashboard.TierUsage{Total: 50e9, Cap: 300e9, Members: 0, Measured: true, Current: 50e9, Files: 50e9}})
	for _, body := range []string{r.get(t, "/f/storage?app=home-assistant", c), card(r.get(t, "/apps/home-assistant?storage", c), "storage")} {
		for _, want := range []string{
			"<h3>App data</h3><p>2.00 GB on disk · 5.00 GB before compression</p>",
			"<h3>App history</h3><p>1.00 GB · 3 snapshots</p>",
			"<h3>User data</h3><p>50.00 GB on disk</p>", // nothing to say when compression bought nothing
			"<h3>User history</h3><p>None yet</p>",
			"Room: up to 40.00 GB for its data and history, and 300.00 GB for your files and their history.",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("once measured, the card does not say %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, "hx-trigger") {
			t.Errorf("a measured card still asks again:\n%s", body)
		}
	}

	// Every app has a user area; an empty one with a month of its snapshots is not shown.
	u.set(dashboard.AppUsage{Service: &dashboard.TierUsage{Total: 3e9, Cap: -1, Members: 3},
		User: &dashboard.TierUsage{Total: 35 * 16384, Cap: 300e9, Members: 34, Measured: true}})
	body = r.get(t, "/f/storage?app=home-assistant", c)
	if !strings.Contains(body, "Not measured (3.00 GB with its history)") || strings.Contains(body, "User data") || strings.Contains(body, "Room") {
		t.Errorf("an unmeasured app with an empty user area, and no cap of its own:\n%s", body)
	}

	u.mu.Lock()
	u.fail = "this guest is too old to measure an app; update the guest OS first"
	u.mu.Unlock()
	if body := r.get(t, "/f/storage?app=home-assistant", c); !strings.Contains(body, "Could not read it: this guest is too old") {
		t.Errorf("a failed read is not said:\n%s", body)
	}
	if resp := r.do("GET", "/f/storage?app=immich", c, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an app not offered has a storage card: %d", resp.StatusCode)
	}
}
