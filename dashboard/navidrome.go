package main

// Navidrome's user is found by role, never kept, as Mealie's is (mealie.go): the reverse proxy
// signs the user in by a username in a header (agent/navidrome), and the dashboard reads which at
// every session check (gate.go) off Navidrome's own data -- its oldest admin. Nothing about it is
// stored on briard's side, so the reverse proxy follows a rename inside Navidrome and an undo or a
// restore of its data at the next check, within the half minute the reverse proxy keeps an answer.
//
// Navidrome makes no account of its own: the first name a header brings becomes its admin. So a
// Navidrome with no account yet is answered with the record's username (account.go), and the
// person is who it is made for. Navidrome's own tool, run in its container, reads its accounts
// straight from its database, so briard needs no account in Navidrome to read them, and adds
// none.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"briard.io/agent/navidrome"
	"briard.io/agent/quadlet"
)

// navidromeAccount is one of Navidrome's accounts, as its `user list` tool says it.
type navidromeAccount struct {
	Username  string    `json:"username"`
	Admin     bool      `json:"admin"`
	CreatedAt time.Time `json:"createdAt"`
}

// navidromeUser is the username the reverse proxy signs the user into Navidrome as.
func (a *app) navidromeUser(ctx context.Context) (string, error) {
	acct, err := a.account()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// The tool's banner and log go to stderr; its list alone to stdout. Only a list it printed
	// says Navidrome has no account: a tool that could not read the database fails, and a failure
	// is never taken for an empty Navidrome, which a header would then give a second account.
	out, err := a.inNavidrome(ctx, "navidrome", "--nobanner", "user", "list", "--format", "json")
	if err != nil {
		return "", fmt.Errorf("list Navidrome's accounts: %w", err)
	}
	var accounts []navidromeAccount
	if err := json.Unmarshal(out, &accounts); err != nil {
		return "", fmt.Errorf("read Navidrome's accounts: %w", err)
	}
	if len(accounts) == 0 {
		return acct.Username, nil
	}
	var oldest *navidromeAccount
	for i, u := range accounts {
		if u.Admin && (oldest == nil || u.CreatedAt.Before(oldest.CreatedAt)) {
			oldest = &accounts[i]
		}
	}
	if oldest == nil {
		return "", errors.New("Navidrome has no admin account, so there is no account of yours to open; undo Navidrome to before it lost yours, or reinstall it")
	}
	return oldest.Username, nil
}

// inNavidrome runs a command in Navidrome's container and returns what it printed to stdout.
func (a *app) inNavidrome(ctx context.Context, args ...string) ([]byte, error) {
	if a.navidromeExec != nil {
		return a.navidromeExec(ctx, args...)
	}
	cmd := exec.CommandContext(ctx, "podman", append([]string{"exec", quadlet.ContainerName(navidrome.Name, navidrome.Container)}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
