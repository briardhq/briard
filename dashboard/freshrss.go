package main

// FreshRSS's user is found by role, never kept, as Mealie's and Navidrome's are (mealie.go,
// navidrome.go): the reverse proxy signs the user in by a username in a header (agent/freshrss),
// and the dashboard reads which at every session check (gate.go) off FreshRSS's own data -- its
// default user, which its installer makes from the catalog's env and FreshRSS counts as an admin.
// FreshRSS's own tool, run in its container, reads it, so briard stores nothing about it, and an
// undo or a restore of FreshRSS's data is followed at the next check.
//
// A phone's reader signs in with that user's API password, on the Google Reader API, the one path
// the reverse proxy leaves open. FreshRSS keeps only a bcrypt hash of it, so it cannot be read back
// as Navidrome's is. The three words "On your phone" sets are kept in FreshRSS's own data, beside
// the user, so an undo, a restore and an uninstall carry them as they carry the user, as Home
// Assistant's integration keeps its four. They are shown only while FreshRSS's own sign-in still
// takes them; otherwise the dialog offers a new password. A user with no API password at all is
// given three words at the first press, the way Navidrome's generated password is replaced.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"briard.io/agent/freshrss"
	"briard.io/agent/quadlet"
	"briard.io/shared/routes"
)

// freshrssAPI is the Google Reader API's path, which a phone's reader is given as the address.
const freshrssAPI = "/api/greader.php"

// freshrssKept is the file beside a user's own data that holds the words briard last set.
const freshrssKept = "briard-phone"

// freshrssAccount is one of FreshRSS's users, as its `user-info` tool says it.
type freshrssAccount struct {
	User    string `json:"user"`
	Default bool   `json:"default"`
}

// freshrssUser is the username the reverse proxy signs the user into FreshRSS as.
func (a *app) freshrssUser(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// Only a list the tool printed says who the default user is: a tool that could not read
	// FreshRSS's data fails, and a failure is never taken for an answer.
	out, err := a.inFreshRSS(ctx, "", freshrss.Root+"/cli/user-info.php", "--json")
	if err != nil {
		return "", fmt.Errorf("list FreshRSS's users: %w", err)
	}
	var accounts []freshrssAccount
	if err := json.Unmarshal(out, &accounts); err != nil {
		return "", fmt.Errorf("read FreshRSS's users: %w", err)
	}
	for _, u := range accounts {
		if u.Default {
			// A name FreshRSS's own rule allows has no slash; one that had would reach another
			// path in its data through the kept file.
			if u.User == "" || strings.ContainsAny(u.User, "/\\") || strings.HasPrefix(u.User, ".") {
				return "", fmt.Errorf("FreshRSS's default user is named %q", u.User)
			}
			return u.User, nil
		}
	}
	return "", errors.New("FreshRSS has no default user, so there is no account of yours to open; undo FreshRSS to before it lost yours, or reinstall it")
}

// inFreshRSS runs one of FreshRSS's tools in its container, as the user its image runs them as,
// with stdin, and returns what it printed to stdout.
func (a *app) inFreshRSS(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	if a.freshrssExec != nil {
		return a.freshrssExec(ctx, stdin, args...)
	}
	cmd := exec.CommandContext(ctx, "podman", append([]string{"exec", "-i", "--user", freshrss.User, quadlet.ContainerName(freshrss.Name, freshrss.Container)}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// freshrssSignIn is the username and password a phone's reader signs in to FreshRSS with, the
// password "" when no kept words sign in any more; `replace` first sets three new words and keeps
// them. The password returned is one FreshRSS's own sign-in took, never the one briard meant to set.
func (a *app) freshrssSignIn(ctx context.Context, s routes.App, replace bool) (user, password string, err error) {
	a.freshrssMu.Lock()
	defer a.freshrssMu.Unlock()
	user, err = a.freshrssUser(ctx)
	if err != nil {
		return "", "", err
	}
	rt, ok := s.Route(routes.ListenName)
	if !ok {
		return "", "", errors.New("the routing table has no route to FreshRSS")
	}
	base, err := s.Resolve(rt.To)
	if err != nil {
		return "", "", err
	}
	kept := freshrss.Data + "/users/" + user + "/" + freshrssKept
	if !replace {
		// The kept words, if any: a file that is not there prints nothing, and a tool that fails
		// is never taken for no words.
		out, err := a.inFreshRSS(ctx, "", "sh", "-c", `[ ! -e "$1" ] || cat "$1"`, "sh", kept)
		if err != nil {
			return "", "", fmt.Errorf("read your password in FreshRSS: %w", err)
		}
		if words := strings.TrimSpace(string(out)); words != "" {
			ok, err := freshrssSignsIn(ctx, base.String(), user, words)
			if err != nil || !ok {
				return user, "", err
			}
			return user, words, nil
		}
		// No words kept: a password the household set itself is theirs, and only the button
		// replaces it.
		out, err = a.inFreshRSS(ctx, "", "php", "-r", `$c = include $argv[1]; echo ($c["apiPasswordHash"] ?? "") === "" ? "none" : "set";`,
			freshrss.Data+"/users/"+user+"/config.php")
		if err != nil {
			return "", "", fmt.Errorf("read your account in FreshRSS: %w", err)
		}
		if strings.TrimSpace(string(out)) != "none" {
			return user, "", nil
		}
	}
	words, err := passphrase(3)
	if err != nil {
		return "", "", err
	}
	if _, err := a.inFreshRSS(ctx, "", freshrss.Root+"/cli/update-user.php", "--user", user, "--api-password", words); err != nil {
		return "", "", fmt.Errorf("set your password in FreshRSS: %w", err)
	}
	// Kept on the replicated volume, where stopping the guest is a power cut: written beside,
	// renamed over and synced, as any fact whose only copy is the file.
	if _, err := a.inFreshRSS(ctx, words, "sh", "-c", `umask 077 && cat > "$1.new" && mv -f "$1.new" "$1" && sync -f "$1"`, "sh", kept); err != nil {
		return "", "", fmt.Errorf("keep your password in FreshRSS: %w", err)
	}
	ok, err = freshrssSignsIn(ctx, base.String(), user, words)
	if err != nil {
		return "", "", err
	}
	if !ok {
		return "", "", errors.New("FreshRSS did not take the new password")
	}
	return user, words, nil
}

// freshrssSignsIn asks FreshRSS's Google Reader API whether a username and password sign in: the
// sign-in a phone's reader makes, by the reader's own request.
func freshrssSignsIn(ctx context.Context, base, user, password string) (bool, error) {
	form := url.Values{"Email": {user}, "Passwd": {password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+freshrssAPI+"/accounts/ClientLogin", strings.NewReader(form.Encode()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return false, fmt.Errorf("ask FreshRSS: %w", err)
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return false, nil
	}
	return false, fmt.Errorf("ask FreshRSS: its sign-in answered %s", resp.Status)
}
