package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"briard.io/agent/install"
	"briard.io/agent/selfupdate"
)

// channel is the release channel the web installer installs from: the one-liner's default.
const channel = "https://get.briard.io"

// The two payloads, told apart by what they carry. The Store's exe carries the release's arm, and
// its install.ps1 installs from it. The download page's carries only keyring.pem, the release
// public key, so a copy saved to disk months ago still installs the release that is stable when it
// runs, never its own: it fetches stable's signed manifest and the install.ps1 it names, verifies
// both against that key, and the script fetches and verifies the rest itself, as the one-liner
// does -- one script, nothing new on the install path.
const (
	installScript = "install.ps1"
	keyringFile   = "keyring.pem"
)

// prepare readies dir -- the unpacked payload -- for install.ps1 and returns what the script's run
// adds to the environment. Nothing fetched is written unless the manifest's signature verified,
// and install.ps1 is kept only when it matches the manifest's hash, so a refusal here means the
// script never runs.
func prepare(ctx context.Context, dir, base string, say func(string, ...any)) ([]string, error) {
	if _, err := os.Stat(filepath.Join(dir, installScript)); err == nil {
		return []string{"BRIARD_ARTIFACTS=" + dir}, nil
	}
	pem, err := os.ReadFile(filepath.Join(dir, keyringFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("this program carries neither an install nor the release key")
	}
	if err != nil {
		return nil, err
	}
	kr, err := selfupdate.NewKeyring(pem)
	if err != nil {
		return nil, fmt.Errorf("the release key this program carries: %w", err)
	}
	// The windows arm by name, not install.PlatformHost: this program installs Windows whatever
	// it was built to be tested on.
	f := &install.Fetcher{BaseURL: base, Chain: install.ChainBriard, Platform: "windows-amd64", Keyring: kr,
		Client: &http.Client{Timeout: 2 * time.Minute}}
	say("briard: fetching the current stable release from %s ...", base)
	man, _, err := f.Manifest(ctx, install.TargetStable)
	if err != nil {
		return nil, unreached(base, fmt.Errorf("stable's manifest: %w", err), say)
	}
	for _, a := range man.Artifacts {
		if a.Name == installScript {
			// Fetched aside, and moved in only once it matched: a mismatch is left where the
			// install never looks.
			tmp, err := os.MkdirTemp(dir, ".fetch-")
			if err != nil {
				return nil, err
			}
			defer os.RemoveAll(tmp)
			if err := f.Artifact(ctx, man.Version, tmp, a); err != nil {
				return nil, unreached(base, fmt.Errorf("stable's %s: %w", installScript, err), say)
			}
			if err := os.Rename(filepath.Join(tmp, installScript), filepath.Join(dir, installScript)); err != nil {
				return nil, err
			}
			say("briard: release %s and its %s verified", man.Version, installScript)
			return nil, nil
		}
	}
	return nil, fmt.Errorf("stable (%s) names no %s", man.Version, installScript)
}

// unreached says a fetch the channel never answered -- no network, no name, refused, timed out,
// TLS cut short -- as what the household can do about it, and keeps the transport's own words in
// the log. An answer the channel did give (a 404, a signature that does not verify) is no network
// problem, and is said as it is.
func unreached(base string, err error, say func(string, ...any)) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	say("briard: %v", err)
	host := base
	if u, perr := url.Parse(base); perr == nil && u.Host != "" {
		host = u.Host
	}
	return fmt.Errorf("could not reach %s. Check this computer's internet connection, then run Briard Setup again", host)
}
