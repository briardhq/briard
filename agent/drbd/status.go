package drbd

import (
	"encoding/json"
	"fmt"

	"briard.io/shared/model"
)

// statusResource is the subset of `drbdsetup status --json` the agent reads.
// encoding/json ignores the rest. The keys match real drbd-utils 9.x output,
// captured as the testdata fixtures from the nixosTest.
type statusResource struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	Devices []struct {
		Quorum    bool   `json:"quorum"`
		DiskState string `json:"disk-state"`
	} `json:"devices"`
	Connections []struct {
		PeerNodeID      int    `json:"peer-node-id"`
		Name            string `json:"name"`
		ConnectionState string `json:"connection-state"`
		PeerRole        string `json:"peer-role"`
		PeerDevices     []struct {
			PeerDiskState string `json:"peer-disk-state"`
			PeerClient    bool   `json:"peer-client"`
		} `json:"peer_devices"`
	} `json:"connections"`
}

// ParseStatus reads `drbdsetup status --json` output for the named resource into
// a QuorumState — the ground truth the agent follows: is this node the
// primary, does it currently have quorum, how many peers are connected. It only
// reads; it never decides or drives the lifecycle (CONTRIBUTING.md invariant 2).
func ParseStatus(raw []byte, resource string) (model.QuorumState, error) {
	c, err := ParseCluster(raw, resource)
	return c.QuorumState, err
}

// ParseCluster reads the same output into the fuller view: this node's QuorumState
// plus every peer's role and disk state, so a node can tell whether anyone
// else could take the workload. One parse of one sample — see model.Cluster for why
// the two halves must not be read separately.
func ParseCluster(raw []byte, resource string) (model.Cluster, error) {
	var resources []statusResource
	if err := json.Unmarshal(raw, &resources); err != nil {
		return model.Cluster{}, fmt.Errorf("drbd: parse status: %w", err)
	}
	for _, r := range resources {
		if r.Name != resource {
			continue
		}
		// Quorum is per-device; any volume without quorum means this node cannot
		// write (on-no-quorum=io-error) — treat the whole node as non-quorate.
		// This node's own storage, folded exactly like the peer fold below and for the
		// identical reason: a node with no devices at all must not come out
		// diskful-and-UpToDate by vacuous truth.
		quorate := len(r.Devices) > 0
		diskful := len(r.Devices) > 0
		uptodate := len(r.Devices) > 0
		for _, d := range r.Devices {
			if !d.Quorum {
				quorate = false
			}
			if d.DiskState == "Diskless" {
				diskful = false
			}
			if d.DiskState != "UpToDate" {
				uptodate = false
			}
		}
		connected := 0
		peers := make([]model.PeerState, 0, len(r.Connections))
		for _, c := range r.Connections {
			if c.ConnectionState == "Connected" {
				connected++
			}
			// Same shape as the quorum fold, and for the same reason: a peer with no
			// peer_devices at all (a disconnected one reports none) must not come out
			// diskful-and-UpToDate by vacuous truth — that peer is precisely the one a
			// demote would hand the workload to and get nothing back.
			diskful := len(c.PeerDevices) > 0
			uptodate := len(c.PeerDevices) > 0
			for _, d := range c.PeerDevices {
				if d.PeerClient || d.PeerDiskState == "Diskless" {
					diskful = false
				}
				if d.PeerDiskState != "UpToDate" {
					uptodate = false
				}
			}
			peers = append(peers, model.PeerState{
				Name:      c.Name,
				NodeID:    c.PeerNodeID,
				Connected: c.ConnectionState == "Connected",
				Role:      c.PeerRole,
				Diskful:   diskful,
				UpToDate:  uptodate,
			})
		}
		return model.Cluster{
			QuorumState: model.QuorumState{
				Primary:   r.Role == "Primary",
				Quorate:   quorate,
				Connected: connected,
				Diskful:   diskful,
				UpToDate:  uptodate,
			},
			Peers: peers,
		}, nil
	}
	return model.Cluster{}, fmt.Errorf("drbd: resource %q not in status", resource)
}

// PeerCounts reads `drbdsetup status --json` for the first resource and returns how many peers
// are CONFIGURED (connected or not — DRBD lists every configured connection) and how many are
// currently CONNECTED. These are the inputs to the deadman's "would the cluster keep quorum
// without me" gate: total cluster votes = peers + 1 (self). v0 is single-resource, so the
// first resource is the one; it errors if the status is empty. It only reads (CONTRIBUTING.md invariant 2).
func PeerCounts(raw []byte) (peers, connected int, err error) {
	var resources []statusResource
	if err := json.Unmarshal(raw, &resources); err != nil {
		return 0, 0, fmt.Errorf("drbd: parse status: %w", err)
	}
	if len(resources) == 0 {
		return 0, 0, fmt.Errorf("drbd: no resource in status")
	}
	r := resources[0]
	peers = len(r.Connections)
	for _, c := range r.Connections {
		if c.ConnectionState == "Connected" {
			connected++
		}
	}
	return peers, connected, nil
}
