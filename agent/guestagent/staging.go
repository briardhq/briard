package guestagent

import (
	"context"
	"fmt"
	"log"
	"strings"
)

// STAGE, THEN COMMIT: a service install is not the volume's identity until its health gate says so.
//
// An install has to put the new manifest on the volume BEFORE its gate decides, because converge
// renders from the volume. Written straight over `<name>.json`, that left a window of minutes in
// which the volume named a version nothing had accepted -- and anything that interrupted the
// install inside it (a power cut, an agent restart, a failover) left the household converging to
// that version for good, because what the revert needed to undo it lived only in the installing
// process's memory.
//
// So the volume holds the install in two files beside the accepted manifest, and every fact the
// undo needs is one of them:
//
//	<name>.json       the ACCEPTED manifest -- untouched until the commit
//	<name>.json.next  the STAGED manifest -- its presence is what "an install is pending" means
//	<name>.rollback   the ring member that puts the data back, absent when there is none (a fresh
//	                  install, or a re-install of the same manifest)
//
// The commit is ONE rename, `.json.next` over `.json`. Nothing else is the decision, so there is
// no state in which the install is half-accepted. Undoing is the same procedure whichever way the
// install ended -- a gate that failed, or a crash nobody saw -- because it reads only these files
// (agent/host's rollBack): stop, put the data back, discard the stage, converge to the accepted.
//
// Converge holds every staged service back except the one an install names as live: a node that
// promotes onto a pending install does NOT start it -- neither the staged version, which nothing
// accepted, nor the accepted one, whose data the staged version may already have migrated. The
// service stays down until the host's undo has put the data back, which is the only order in
// which starting it is safe.
//
// Flock-scoped, so on the replicated volume and `sync -f`'d: the node that finishes the undo may
// be a different node from the one that started the install.
const (
	verbServiceStage   = "service.stage"   // ensure the data, record the rollback point, write .json.next
	verbServiceCommit  = "service.commit"  // .json.next -> .json, then drop .rollback
	verbServiceDiscard = "service.discard" // drop .json.next, then .rollback
	verbServicePending = "service.pending" // every staged install, with what undoing it needs
)

func stagedPath(name string) string   { return manifestPath(name) + ".next" }
func rollbackPath(name string) string { return manifestDir + "/" + name + ".rollback" }

// serviceStageRequest is service.provision's request plus the rollback point (service.stage).
type serviceStageRequest struct {
	Name     string   `json:"name"`
	DataDir  string   `json:"data_dir"`
	Subdirs  []string `json:"subdirs,omitempty"`
	Manifest string   `json:"manifest"`
	// Rollback is the ring member that undoes this install's data, "" when there is none.
	Rollback string `json:"rollback,omitempty"`
}

// serviceConvergeRequest names the install being run right now, if any (service.converge).
type serviceConvergeRequest struct {
	Live string `json:"live,omitempty"`
}

// PendingInstall is one staged install, as the volume records it (service.pending): everything an
// undo needs, read from disk rather than remembered by whoever started it.
type PendingInstall struct {
	Name     string `json:"name"`
	Staged   string `json:"staged"`             // the manifest nothing has accepted
	Accepted string `json:"accepted,omitempty"` // the manifest to go back to; "" = none (a fresh install)
	Rollback string `json:"rollback,omitempty"` // the member that puts the data back; "" = no data to put back
}

// writeDurably writes a volume file whole or not at all (tmp + rename), then flushes the
// filesystem so the rename itself survives a power cut. A torn `.rollback` would name a member that
// does not exist, and the undo would then leave the service stopped.
func writeDurably(x Executor, run func(string, ...string) error, path, body string) error {
	tmp := path + ".tmp"
	if err := x.WriteFile(tmp, []byte(body)); err != nil {
		return err
	}
	if err := run("mv", "-T", tmp, path); err != nil {
		return err
	}
	return run("sync", "-f", manifestDir)
}

// stageService serves service.stage: the data the new version needs, then the rollback point,
// then the staged manifest -- in that order, so a crash part-way leaves either no `.json.next`
// (nothing pending; the accepted manifest still stands) or a `.json.next` whose `.rollback` is
// already durable.
//
// A STALE `.rollback` IS REMOVED when this install has none. One left by an earlier install that
// crashed after its discard would otherwise be read as THIS install's rollback point, and undoing
// a fresh install would put back some other day's data.
func stageService(ctx context.Context, x Executor, run func(string, ...string) error, req serviceStageRequest) error {
	if req.Name == "" || req.DataDir == "" || req.Manifest == "" {
		return fmt.Errorf("service.stage: need a name, a data dir and a manifest")
	}
	if err := safeUnitName(req.Name); err != nil { // the name becomes a path element
		return err
	}
	if req.Rollback != "" && strings.ContainsAny(req.Rollback, "\n") {
		return fmt.Errorf("service.stage: rollback point %q is not one line", req.Rollback)
	}
	if err := ensureServiceData(ctx, x, run, req.DataDir, req.Subdirs); err != nil {
		return err
	}
	if req.Rollback != "" {
		if err := writeDurably(x, run, rollbackPath(req.Name), req.Rollback+"\n"); err != nil {
			return err
		}
	} else if err := run("rm", "-f", rollbackPath(req.Name)); err != nil {
		return err
	}
	if err := writeDurably(x, run, stagedPath(req.Name), req.Manifest); err != nil {
		return err
	}
	// Every caller has the service stopped (provisionService says why this matters).
	if err := RecordServiceStop(ctx, x, req.Name, "success"); err != nil {
		log.Printf("service.stage %s: could not record the volume as flushed (%v); its next member will read as crash-consistent", req.Name, err)
	}
	return nil
}

// commitService serves service.commit: the rename IS the decision, flushed before the rollback
// point is dropped, so a crash between the two leaves an accepted install with a leftover
// `.rollback` -- which the next stage overwrites or removes -- and never a pending one without it.
func commitService(ctx context.Context, x Executor, run func(string, ...string) error, name string) error {
	if err := safeUnitName(name); err != nil {
		return err
	}
	if err := run("mv", "-T", stagedPath(name), manifestPath(name)); err != nil {
		return err
	}
	if err := run("sync", "-f", manifestDir); err != nil {
		return err
	}
	if err := run("rm", "-f", rollbackPath(name)); err != nil {
		return err
	}
	return run("sync", "-f", manifestDir)
}

// discardService serves service.discard, the undo's last volume write: the staged manifest first
// -- its absence is what ends "pending" -- then the rollback point. Idempotent: an undo interrupted
// after this and run again finds nothing to discard.
func discardService(ctx context.Context, x Executor, run func(string, ...string) error, name string) error {
	if err := safeUnitName(name); err != nil {
		return err
	}
	if err := run("rm", "-f", stagedPath(name)); err != nil {
		return err
	}
	if err := run("sync", "-f", manifestDir); err != nil {
		return err
	}
	if err := run("rm", "-f", rollbackPath(name)); err != nil {
		return err
	}
	return run("sync", "-f", manifestDir)
}

// pendingInstalls serves service.pending. An absent directory is no installs at all, so nothing
// pending -- the same reading manifestNames gives it.
func pendingInstalls(ctx context.Context, x Executor) ([]PendingInstall, error) {
	names, err := stagedNames(ctx, x)
	if err != nil {
		return nil, err
	}
	out := []PendingInstall{}
	for _, n := range names {
		staged, err := x.ReadFile(stagedPath(n))
		if err != nil {
			return nil, fmt.Errorf("service.pending: read %s: %w", n, err)
		}
		p := PendingInstall{Name: n, Staged: string(staged)}
		if raw, err := x.ReadFile(manifestPath(n)); err == nil {
			p.Accepted = string(raw)
		}
		if raw, err := x.ReadFile(rollbackPath(n)); err == nil {
			p.Rollback = strings.TrimSpace(string(raw))
		}
		out = append(out, p)
	}
	return out, nil
}

// stagedNames lists the services with a `.json.next` on the volume.
func stagedNames(ctx context.Context, x Executor) ([]string, error) {
	out, err := x.Run(ctx, "ls", "-1", manifestDir)
	if err != nil {
		return nil, nil // absent directory: see manifestNames
	}
	var names []string
	for _, n := range nonEmptyLines(out) {
		if strings.HasSuffix(n, ".json.next") {
			names = append(names, strings.TrimSuffix(n, ".json.next"))
		}
	}
	return names, nil
}

// ServiceStage records an install on the volume WITHOUT making it the identity: the data it needs,
// the rollback point ("" for none) and the staged manifest (service.stage).
func (g *Client) ServiceStage(ctx context.Context, name, dataDir string, subdirs []string, manifest, rollback string) error {
	return g.c.Call(ctx, verbServiceStage, serviceStageRequest{
		Name: name, DataDir: dataDir, Subdirs: subdirs, Manifest: manifest, Rollback: rollback,
	}, nil)
}

// ServiceCommit makes a staged install the volume's identity: one rename (service.commit).
func (g *Client) ServiceCommit(ctx context.Context, name string) error {
	return g.c.Call(ctx, verbServiceCommit, serviceInstalledRequest{Name: name}, nil)
}

// ServiceDiscard drops a staged install and its rollback point (service.discard).
func (g *Client) ServiceDiscard(ctx context.Context, name string) error {
	return g.c.Call(ctx, verbServiceDiscard, serviceInstalledRequest{Name: name}, nil)
}

// ServicePending lists every staged install on the volume (service.pending).
func (g *Client) ServicePending(ctx context.Context) ([]PendingInstall, error) {
	var out []PendingInstall
	err := g.c.Call(ctx, verbServicePending, nil, &out)
	return out, err
}
