package main

// Live pull progress ([V3b.31j]): how far a service install's download has got, read off the
// guest itself while the bytes move. Nothing here asks podman anything.
//
// Three local facts make a fraction:
//   - the TOTAL is the manifest's Size, which the host wrote to the dashboard's tmpfs
//     (shared/dashboard.Pull) before the first byte, because the manifest reaches the volume
//     only after the pull;
//   - FINISHED layers are in podman's own layer store, overlay-layers/layers.json, each with its
//     compressed size and creation time -- every layer created since the pull started is this
//     pull's, so no journal parsing and no digest matching;
//   - the layer IN FLIGHT is the bytes under the pull unit's private tmp (PrivateTmp, [B.56]),
//     which shrinks as a layer completes exactly as the finished sum grows by it.
//
// The number is monotonic to within a poll, honest about what it cannot know (no total -> no
// bar), and never reaches 100 before the routes table says the service is there.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"briard.io/shared/dashboard"
)

// pullPaths is where the three facts live; tests point them at temp dirs.
type pullPaths struct {
	records string // where the host's pull records land (shared/dashboard.Dir)
	layers  string // podman's overlay-layers/layers.json
	// tmp is the directory the pull unit's PrivateTmp roots under. /var/tmp, not /tmp: a pull
	// stages through /var/tmp/container_images_storage<random> ([B.56], measured), and
	// PrivateTmp gives the unit its own of BOTH, each under its own systemd-private-* root.
	tmp string
}

var defaultPullPaths = pullPaths{records: dashboard.Dir, layers: "/var/lib/containers/storage/overlay-layers/layers.json", tmp: "/var/tmp"}

// progressView is what the Installing card shows.
type progressView struct {
	Done, Total int64
	Percent     int
	DoneGB      string
	TotalGB     string
}

// layer is the slice of podman's layer record this reads.
type layer struct {
	CompressedSize int64     `json:"compressed-size"`
	Created        time.Time `json:"created"`
}

// pullProgress reads the record the host left for the service and, when there is one, measures
// the pull against it. nil when nothing is recorded -- an older host, or nothing pulling.
func (a *app) pullProgress(service string) *progressView {
	raw, err := os.ReadFile(filepath.Join(a.pulls.records, filepath.Base(dashboard.PullPath(service))))
	if err != nil {
		return nil
	}
	var p dashboard.Pull
	if err := json.Unmarshal(raw, &p); err != nil || p.Size <= 0 {
		return nil
	}
	done := a.pulls.finishedSince(p.Started) + a.pulls.inFlight(service)
	if done > p.Size {
		done = p.Size
	}
	pct := int(done * 100 / p.Size)
	if pct > 99 {
		pct = 99 // the last percent is the manifest landing and the routes table, not bytes
	}
	return &progressView{Done: done, Total: p.Size, Percent: pct, DoneGB: gb(done), TotalGB: gb(p.Size)}
}

// finishedSince sums the compressed size of every layer podman committed at or after t.
func (pp pullPaths) finishedSince(t time.Time) int64 {
	raw, err := os.ReadFile(pp.layers)
	if err != nil {
		return 0
	}
	var layers []layer
	if err := json.Unmarshal(raw, &layers); err != nil {
		return 0
	}
	var n int64
	for _, l := range layers {
		if !l.Created.Before(t) {
			n += l.CompressedSize
		}
	}
	return n
}

// inFlight is the bytes under the service's pull units' private tmp: systemd names each
// PrivateTmp root systemd-private-<boot>-<unit>-<random>, and the units are
// briard-<service>-<container>-image.service.
func (pp pullPaths) inFlight(service string) int64 {
	dirs, _ := filepath.Glob(filepath.Join(pp.tmp, "systemd-private-*-briard-"+service+"-*-image.service-*"))
	var n int64
	for _, d := range dirs {
		_ = filepath.WalkDir(d, func(_ string, e os.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return nil
			}
			if info, err := e.Info(); err == nil {
				n += info.Size()
			}
			return nil
		})
	}
	return n
}

// gb renders bytes the way the card says them: two decimals of GB, MB below that.
func gb(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.2f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.0f MB", float64(n)/1e6)
	}
	return fmt.Sprintf("%.0f kB", float64(n)/1e3)
}
