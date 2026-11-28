package freshrss

// FreshRSS's accounts, as the dashboard reads and sets them.
//
// FreshRSS's user is found by role, never kept: the reverse proxy signs the user in by a
// username in a header, and the dashboard reads which at every session check off FreshRSS's own
// data -- its default user, which its installer makes from the catalog's env and FreshRSS counts
// as an admin (Admin). FreshRSS's own tool, run in its container, reads it, so briard stores
// nothing about it, and an undo or a restore of FreshRSS's data is followed at the next check.
//
// A phone's reader signs in with that user's API password, on the Google Reader API, the one path
// the reverse proxy leaves open (SignIn). FreshRSS keeps only a bcrypt hash of it, so it cannot
// be read back. The words the dashboard's "On your phone" sets are kept in FreshRSS's own data,
// beside the user, so an undo, a restore and an uninstall carry them as they carry the user. They
// are shown only while FreshRSS's own sign-in still takes them; otherwise the dialog offers a new
// password. A user with no API password at all is given words at the first press.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Exec runs one of FreshRSS's tools in its container, as User, with stdin, and returns what it
// printed to stdout.
type Exec func(ctx context.Context, stdin string, args ...string) ([]byte, error)

// API is the Google Reader API's path, which a phone's reader is given as the address.
const API = "/api/greader.php"

// kept is the file beside a user's own data that holds the words briard last set.
const kept = "briard-phone"

// account is one of FreshRSS's users, as its `user-info` tool says it.
type account struct {
	User    string `json:"user"`
	Default bool   `json:"default"`
}

// Admin is FreshRSS's default user, the account the reverse proxy signs the user into, "" for a
// FreshRSS that names none.
func Admin(ctx context.Context, in Exec) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// Only a list the tool printed says who the default user is: a tool that could not read
	// FreshRSS's data fails, and a failure is never taken for an answer.
	out, err := in(ctx, "", Root+"/cli/user-info.php", "--json")
	if err != nil {
		return "", fmt.Errorf("list FreshRSS's users: %w", err)
	}
	var accounts []account
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
	return "", nil
}

// SignIn is the password a phone's reader signs in to FreshRSS with as user, at base, FreshRSS's
// address on this guest: "" when no kept words sign in any more. `replace` first sets new words
// and keeps them. The password returned is one FreshRSS's own sign-in took, never the one briard
// meant to set.
func SignIn(ctx context.Context, in Exec, base, user string, replace bool, words func() (string, error)) (string, error) {
	keptPath := Data + "/users/" + user + "/" + kept
	if !replace {
		// The kept words, if any: a file that is not there prints nothing, and a tool that fails
		// is never taken for no words.
		out, err := in(ctx, "", "sh", "-c", `[ ! -e "$1" ] || cat "$1"`, "sh", keptPath)
		if err != nil {
			return "", fmt.Errorf("read your password in FreshRSS: %w", err)
		}
		if typed := strings.TrimSpace(string(out)); typed != "" {
			ok, err := signsIn(ctx, base, user, typed)
			if err != nil || !ok {
				return "", err
			}
			return typed, nil
		}
		// No words kept: a password the household set itself is theirs, and only the button
		// replaces it.
		out, err = in(ctx, "", "php", "-r", `$c = include $argv[1]; echo ($c["apiPasswordHash"] ?? "") === "" ? "none" : "set";`,
			Data+"/users/"+user+"/config.php")
		if err != nil {
			return "", fmt.Errorf("read your account in FreshRSS: %w", err)
		}
		if strings.TrimSpace(string(out)) != "none" {
			return "", nil
		}
	}
	typed, err := words()
	if err != nil {
		return "", err
	}
	if _, err := in(ctx, "", Root+"/cli/update-user.php", "--user", user, "--api-password", typed); err != nil {
		return "", fmt.Errorf("set your password in FreshRSS: %w", err)
	}
	// Kept on the replicated volume, where stopping the guest is a power cut: written beside,
	// renamed over and synced, as any fact whose only copy is the file.
	if _, err := in(ctx, typed, "sh", "-c", `umask 077 && cat > "$1.new" && mv -f "$1.new" "$1" && sync -f "$1"`, "sh", keptPath); err != nil {
		return "", fmt.Errorf("keep your password in FreshRSS: %w", err)
	}
	// FreshRSS reads the user's settings through PHP's opcache, which FreshRSS's image has look
	// at a file again only every two seconds: until then its sign-in still checks the old hash.
	// So a refusal is asked again until Settles has passed, and only then believed.
	deadline := time.Now().Add(Settles)
	for {
		ok, err := signsIn(ctx, base, user, typed)
		if err != nil {
			return "", err
		}
		if ok {
			return typed, nil
		}
		if time.Now().After(deadline) {
			return "", errors.New("FreshRSS did not take the new password")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Settles is how long a new password may take to reach FreshRSS's sign-in: its image's
// opcache.revalidate_freq, two seconds, with room.
var Settles = 5 * time.Second

// signsIn asks FreshRSS's Google Reader API whether a username and password sign in: the sign-in
// a phone's reader makes, by the reader's own request.
func signsIn(ctx context.Context, base, user, password string) (bool, error) {
	form := url.Values{"Email": {user}, "Passwd": {password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+API+"/accounts/ClientLogin", strings.NewReader(form.Encode()))
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
