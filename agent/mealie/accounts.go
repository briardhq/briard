package mealie

// Mealie's accounts, as the dashboard reads and sets them.
//
// Mealie's user is found by role, never kept. The reverse proxy signs the user into Mealie by a
// username in a header, read off Mealie itself at every session check (Accounts.Admin): the
// oldest admin account that is not briard's own. Nothing about it is stored on briard's side,
// so the reverse proxy follows whatever Mealie's data says -- a rename inside Mealie, an undo or a
// restore from briard's history or backup, a Mealie backup the user imported from a Mealie they
// ran before -- at the next check, within the half minute the reverse proxy keeps an answer.
//
// Briard's own account is how it reads that, and it is made again whenever it is gone. Only an
// admin lists Mealie's accounts. The reverse proxy's secret signs in any username, and Mealie
// makes an ordinary account for a name it does not know, so the dashboard signs in as
// `briard-admin` (a name no OS account the record comes from is likely to carry), which creates
// it on a Mealie that lacks it (a fresh one, one the user deleted it from, one restored from a
// backup that predates it); Mealie's own tool, inside its container, makes it an admin when it is
// not. A header makes no other account but one: a name the user changed inside Mealie, in the
// half minute the reverse proxy still sends the old one -- an ordinary account, never chosen.
//
// Mealie's first account is taken over, not adopted by name. Mealie makes an admin whenever it
// starts with no users: `admin`, with a password and an email every copy of Mealie ships with, and
// its API answers that password even with the login form turned off. While that email is held,
// the password, if it still answers, is changed to one briard generates and forgets (as Home
// Assistant's first account's is); then briard's own account gives the first one the record's
// name and a placeholder email from its username, which ends Mealie's "first login".
// Its username stays `admin` (takeOver says why), and it stays an ordinary Mealie account, so the
// same data under a Mealie anywhere else signs in with the tools Mealie gives everyone.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"briard.io/shared/routes"
)

// Exec runs a command in Mealie's container, with stdin, and returns what it printed.
type Exec func(ctx context.Context, stdin string, args ...string) ([]byte, error)

// Mealie's first account as it ships.
const (
	firstUser     = "admin"
	FirstPassword = "MyPassword"
	FirstEmail    = "changeme@example.com"
)

// OwnUser is briard's own account in Mealie, the one the dashboard reads Mealie's accounts as.
const OwnUser = "briard-admin"

// emailDomain is the placeholder email's: undeliverable, so an app's mail to it never leaves the
// house, and never a domain anyone owns. The user may replace it inside the app.
const emailDomain = "briard.local"

// account is one of Mealie's accounts, as its API says it; raw keeps every field, so a
// rename sends back the account as Mealie gave it with three fields changed.
type account struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Admin    bool   `json:"admin"`
	raw      map[string]any
}

// Accounts is Mealie as the dashboard reaches it: Base its address on this guest, Secret the
// reverse proxy's (KeyPath's), In a command in its container, and NewPassword a password to set
// and forget.
type Accounts struct {
	Base, Secret string
	In           Exec
	NewPassword  func() (string, error)
}

// Admin is the username the reverse proxy signs the user into Mealie as, "" for a Mealie with no
// admin account but briard's own. A Mealie still at its first account is taken over on the way,
// for the person of that name and username. One call at a time: the caller holds a second check
// while the first reads or takes over the accounts, so the two never race through the password
// change.
func (a Accounts) Admin(ctx context.Context, name, username string) (string, error) {
	// The check runs to its end even when the reverse proxy stops waiting: the first one makes
	// briard's own account an admin, which takes seconds, and a check abandoned partway would leave
	// the next to start over, or Mealie mid-change with the lock already released.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	m := api{base: a.Base, client: &http.Client{Timeout: 10 * time.Second}}
	var info struct {
		IsFirstLogin bool `json:"isFirstLogin"`
	}
	if err := m.do(ctx, http.MethodGet, "/api/app/about/startup-info", "", nil, nil, &info); err != nil {
		return "", ErrStarting
	}
	own := http.Header{routes.UserHeader: {OwnUser}, SecretHeader: {a.Secret}}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := m.do(ctx, http.MethodGet, "/api/auth/reverse-proxy", "", own, nil, &tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("sign in to Mealie as briard's own account: %v", err)
	}
	var self account
	if err := m.do(ctx, http.MethodGet, "/api/users/self", tok.AccessToken, nil, nil, &self); err != nil {
		return "", fmt.Errorf("read briard's own account in Mealie: %w", err)
	}
	if !self.Admin {
		if out, err := a.In(ctx, self.Email+"\n\n", "python", "-m", "mealie.scripts.make_admin"); err != nil {
			return "", fmt.Errorf("make briard's own account in Mealie an admin: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	accounts, err := m.accounts(ctx, tok.AccessToken)
	if err != nil {
		return "", err
	}
	if info.IsFirstLogin {
		for _, u := range accounts {
			if u.Email == FirstEmail && u.ID != self.ID {
				if err := m.takeOver(ctx, tok.AccessToken, u, a.NewPassword, name, username); err != nil {
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
	return "", nil
}

// takeOver makes Mealie's first account the user's: its shipped password changed while it still
// answers it -- the open door, so first -- then its name and email the record's.
//
// Its username stays Mealie's `admin`. Nothing matches on it any more, and a name that never
// changes is one the reverse proxy's half-minute answer cannot outlive: a Mealie back at its first
// account is `admin` again, so a sign-in in that half minute lands on it rather than making a
// stray account of a name it no longer has. A password that no longer answers is no hole: a
// takeover cut short after it, or a user who changed it, leaves an account nobody signs in to by
// the shipped password, and the change of name and email, made with briard's own account, needs
// none.
func (m api) takeOver(ctx context.Context, own string, first account, newPassword func() (string, error), name, username string) error {
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	form := url.Values{"username": {first.Username}, "password": {FirstPassword}}
	if err := m.do(ctx, http.MethodPost, "/api/auth/token", "", nil, form, &tok); err == nil && tok.AccessToken != "" {
		pw, err := newPassword()
		if err != nil {
			return err
		}
		if err := m.do(ctx, http.MethodPut, "/api/users/password", tok.AccessToken, nil,
			map[string]string{"currentPassword": FirstPassword, "newPassword": pw}, nil); err != nil {
			// A change that lost a race to an earlier check's -- one the reverse proxy stopped
			// waiting for, which Mealie finished all the same -- is refused for the password it was
			// given: the shipped one, refused at sign-in now, is that same end. Only that refusal
			// says so.
			var st status
			if again := m.do(ctx, http.MethodPost, "/api/auth/token", "", nil, form, nil); !errors.As(again, &st) || st.code != http.StatusUnauthorized {
				return fmt.Errorf("change the password of Mealie's first account: %w", err)
			}
		}
	}
	first.raw["fullName"], first.raw["email"] = name, username+"@"+emailDomain
	if err := m.do(ctx, http.MethodPut, "/api/admin/users/"+url.PathEscape(first.ID), own, nil, first.raw, nil); err != nil {
		return fmt.Errorf("make Mealie's first account yours: %w", err)
	}
	return nil
}

// accounts is every account Mealie has, oldest first.
func (m api) accounts(ctx context.Context, token string) ([]account, error) {
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	q := url.Values{"perPage": {"-1"}, "orderBy": {"created_at"}, "orderDirection": {"asc"}}
	if err := m.do(ctx, http.MethodGet, "/api/admin/users?"+q.Encode(), token, nil, nil, &page); err != nil {
		return nil, fmt.Errorf("list Mealie's accounts: %w", err)
	}
	out := make([]account, len(page.Items))
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

// status is an answer from Mealie's API other than a 2xx.
type status struct {
	method, path string
	code         int
}

func (e status) Error() string {
	return fmt.Sprintf("%s %s answered %d", e.method, e.path, e.code)
}

var ErrStarting = errors.New("Mealie is still starting; try again in a moment")

// api is Mealie's REST API on its in-guest address.
type api struct {
	base   string
	client *http.Client
}

// do sends one request -- a form for url.Values, JSON otherwise -- and decodes a 2xx answer into
// out; anything else is an error carrying the status.
func (m api) do(ctx context.Context, method, path, token string, header http.Header, body, out any) error {
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
		return status{method, strings.SplitN(path, "?", 2)[0], resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
