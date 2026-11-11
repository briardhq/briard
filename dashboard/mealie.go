package main

// MEALIE'S USER IS FOUND BY ROLE, NEVER KEPT. The door signs the user into Mealie by a username
// in a header (agent/mealie), and the name it sends is read off Mealie itself at every session
// check (gate.go): the oldest admin account that is not briard's own. Nothing about it is stored
// on briard's side, so whatever Mealie's data says is what the door does -- a rename inside
// Mealie, an undo or a restore from briard's history or backup, a Mealie backup the user imported
// from a Mealie they ran before, each is followed at the next check, within the half minute the
// door keeps an answer.
//
// BRIARD'S OWN ACCOUNT IS HOW IT READS THAT, AND IT IS MADE AGAIN WHENEVER IT IS GONE. Only an
// admin lists Mealie's accounts. The door's secret signs in any username, and Mealie makes an
// ordinary account for a name it does not know, so the dashboard signs in as `briard-admin` -- a
// name no OS account the record comes from is likely to carry -- which makes it on a Mealie that
// lacks it (a fresh one, one the user deleted it from, one restored from a backup that predates
// it), and makes it an admin, when it is not, with Mealie's own tool for that, inside its
// container. A header makes no other account but one: a name the user changed inside Mealie in
// the half minute the door still sends the old one, which is an ordinary account, never chosen.
//
// MEALIE'S FIRST ACCOUNT IS TAKEN OVER, NOT ADOPTED BY NAME. Mealie makes an admin whenever it
// starts with no users, `admin` with a password and an email every copy of Mealie ships with, and
// its API answers that password even with the login form turned off. While that email is held,
// the password, if it still answers, is changed to one briard generates and forgets -- as Home
// Assistant's first account's is -- and then briard's own account gives the first one the
// record's name and a placeholder email from its username (account.go), which is what ends
// Mealie's "first login"; its username stays `admin` (takeOver says why). It stays an ordinary
// Mealie account, so the same data under a Mealie anywhere else signs in with the tools Mealie
// gives everyone.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"briard.io/agent/mealie"
	"briard.io/agent/quadlet"
	"briard.io/shared/routes"
)

// Mealie's first account as it ships.
const (
	mealieFirstUser     = "admin"
	mealieFirstPassword = "MyPassword"
	mealieFirstEmail    = "changeme@example.com"
)

// mealieOwnUser is briard's own account in Mealie, the one the dashboard reads Mealie's accounts
// as; mealieUserHeader is where Mealie reads a header sign-in's username, as the door sends it.
const (
	mealieOwnUser    = "briard-admin"
	mealieUserHeader = "X-Forwarded-User"
)

// emailDomain is the placeholder email's: undeliverable, so an app's mail to it never leaves the
// house, and never a domain anyone owns. The user may replace it inside the app.
const emailDomain = "briard.local"

// mealieAccount is one of Mealie's accounts, as its API says it; raw keeps every field, so a
// rename sends back the account as Mealie gave it with three fields changed.
type mealieAccount struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Admin    bool   `json:"admin"`
	raw      map[string]any
}

// mealieUser is the username the door signs the user into Mealie as.
func (a *app) mealieUser(ctx context.Context, s routes.App) (string, error) {
	acct, err := a.account()
	if err != nil {
		return "", err
	}
	rt, ok := s.Route(routes.ListenName)
	if !ok {
		return "", errors.New("Mealie has no address on this node")
	}
	base, err := s.Resolve(rt.To)
	if err != nil {
		return "", err
	}
	key, err := os.ReadFile(s.Gate)
	if err != nil {
		return "", fmt.Errorf("read the door's secret for Mealie: %w", err)
	}
	a.mealieMu.Lock()
	defer a.mealieMu.Unlock()
	// The check runs to its end even when the door stops waiting: the first one makes briard's own
	// account an admin, which takes seconds, and a check abandoned partway would leave the next to
	// start over, or Mealie mid-change with the lock already released.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	m := mealieAPI{base: base.String(), client: &http.Client{Timeout: 10 * time.Second}}
	var info struct {
		IsFirstLogin bool `json:"isFirstLogin"`
	}
	if err := m.do(ctx, http.MethodGet, "/api/app/about/startup-info", "", nil, nil, &info); err != nil {
		return "", errMealieStarting
	}
	own := http.Header{mealieUserHeader: {mealieOwnUser}, s.GateHeader: {strings.TrimSpace(string(key))}}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := m.do(ctx, http.MethodGet, "/api/auth/reverse-proxy", "", own, nil, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("sign in to Mealie as briard's own account: %v", err)
	}
	var self mealieAccount
	if err := m.do(ctx, http.MethodGet, "/api/users/self", tok.AccessToken, nil, nil, &self); err != nil {
		return "", fmt.Errorf("read briard's own account in Mealie: %w", err)
	}
	if !self.Admin {
		if out, err := a.inMealie(ctx, self.Email+"\n\n", "python", "-m", "mealie.scripts.make_admin"); err != nil {
			return "", fmt.Errorf("make briard's own account in Mealie an admin: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	accounts, err := m.accounts(ctx, tok.AccessToken)
	if err != nil {
		return "", err
	}
	if info.IsFirstLogin {
		for _, u := range accounts {
			if u.Email == mealieFirstEmail && u.ID != self.ID {
				if err := m.takeOver(ctx, tok.AccessToken, u, acct); err != nil {
					return "", err
				}
			}
		}
	}
	for _, u := range accounts {
		if u.Admin && u.ID != self.ID {
			return u.Username, nil
		}
	}
	return "", errors.New("Mealie has no admin account but briard's own, so there is no account of yours to open; undo Mealie to before it lost yours, or reinstall it")
}

// takeOver makes Mealie's first account the user's: its shipped password changed while it still
// answers it -- the open door, so first -- then its name and email the record's.
//
// ITS USERNAME STAYS MEALIE'S `admin`. Nothing matches on it any more, and a name that never
// changes is one the door's half-minute answer cannot outlive: a Mealie back at its first account
// is `admin` again, so a sign-in in that half minute lands on it rather than making a stray
// account of a name it no longer has.
//
// A PASSWORD THAT NO LONGER ANSWERS IS NO HOLE: a takeover cut short after it, or a user who
// changed it, leaves an account nobody signs in to by the shipped password, and the change of name
// and email, made with briard's own account, needs none.
func (m mealieAPI) takeOver(ctx context.Context, own string, first mealieAccount, acct account) error {
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	form := url.Values{"username": {first.Username}, "password": {mealieFirstPassword}}
	if err := m.do(ctx, http.MethodPost, "/api/auth/token", "", nil, form, &tok); err == nil && tok.AccessToken != "" {
		pw, err := newPassword()
		if err != nil {
			return err
		}
		if err := m.do(ctx, http.MethodPut, "/api/users/password", tok.AccessToken, nil,
			map[string]string{"currentPassword": mealieFirstPassword, "newPassword": pw}, nil); err != nil {
			// A change that lost a race to an earlier check's -- one the door stopped waiting for,
			// which Mealie finished all the same -- is refused for the password it was given: the
			// shipped one, refused at sign-in now, is that same end. Only that refusal says so.
			var st mealieStatus
			if again := m.do(ctx, http.MethodPost, "/api/auth/token", "", nil, form, nil); !errors.As(again, &st) || st.code != http.StatusUnauthorized {
				return fmt.Errorf("change the password of Mealie's first account: %w", err)
			}
		}
	}
	first.raw["fullName"], first.raw["email"] = acct.Name, acct.Username+"@"+emailDomain
	if err := m.do(ctx, http.MethodPut, "/api/admin/users/"+url.PathEscape(first.ID), own, nil, first.raw, nil); err != nil {
		return fmt.Errorf("make Mealie's first account yours: %w", err)
	}
	return nil
}

// accounts is every account Mealie has, oldest first.
func (m mealieAPI) accounts(ctx context.Context, token string) ([]mealieAccount, error) {
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	q := url.Values{"perPage": {"-1"}, "orderBy": {"created_at"}, "orderDirection": {"asc"}}
	if err := m.do(ctx, http.MethodGet, "/api/admin/users?"+q.Encode(), token, nil, nil, &page); err != nil {
		return nil, fmt.Errorf("list Mealie's accounts: %w", err)
	}
	out := make([]mealieAccount, len(page.Items))
	for i, raw := range page.Items {
		if err := json.Unmarshal(raw, &out[i]); err != nil {
			return nil, fmt.Errorf("read Mealie's accounts: %w", err)
		}
		if err := json.Unmarshal(raw, &out[i].raw); err != nil {
			return nil, fmt.Errorf("read Mealie's accounts: %w", err)
		}
	}
	return out, nil
}

// inMealie runs a command in Mealie's container with stdin, unless a test put another in its
// place.
func (a *app) inMealie(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	if a.mealieExec != nil {
		return a.mealieExec(ctx, stdin, args...)
	}
	cmd := exec.CommandContext(ctx, "podman", append([]string{"exec", "-i", quadlet.ContainerName(mealie.Name, mealie.Container)}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	return cmd.CombinedOutput()
}

// mealieStatus is an answer from Mealie's API other than a 2xx.
type mealieStatus struct {
	method, path string
	code         int
}

func (e mealieStatus) Error() string {
	return fmt.Sprintf("%s %s answered %d", e.method, e.path, e.code)
}

var errMealieStarting = errors.New("Mealie is still starting; try again in a moment")

// mealieAPI is Mealie's REST API on its in-guest address.
type mealieAPI struct {
	base   string
	client *http.Client
}

// do sends one request -- a form for url.Values, JSON otherwise -- and decodes a 2xx answer into
// out; anything else is an error carrying the status.
func (m mealieAPI) do(ctx context.Context, method, path, token string, header http.Header, body, out any) error {
	var r io.Reader
	ctype := ""
	switch b := body.(type) {
	case nil:
	case url.Values:
		r, ctype = strings.NewReader(b.Encode()), "application/x-www-form-urlencoded"
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		r, ctype = bytes.NewReader(raw), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, m.base+path, r)
	if err != nil {
		return err
	}
	for k, v := range header {
		req.Header[http.CanonicalHeaderKey(k)] = v
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return mealieStatus{method, strings.SplitN(path, "?", 2)[0], resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
