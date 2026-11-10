package main

// MEALIE'S FIRST ACCOUNT, TAKEN OVER AT THE FIRST OPEN. Mealie makes an admin at its first start,
// `admin` with a password every copy of Mealie ships with, and its API answers that password even
// with the login form turned off. The door signs the user in by username and makes nobody new, so
// before the first browser goes through, that account becomes the user's: its password changed to
// one briard generates and forgets -- as Home Assistant's first is -- then its username, name and
// email the record's (account.go). It stays an ordinary Mealie account, so the same data under a
// Mealie anywhere else signs in with the tools Mealie gives everyone.
//
// THE PASSWORD GOES FIRST. Mealie says it still holds its first account while one carries the
// email it ships with, and the rename is what ends that. A rename that failed after the password
// changed leaves an account nobody can sign in to, which the next open says; the other order could
// leave the user's account answering the shipped password with nothing to notice.

import (
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

// Mealie's first account as it ships.
const (
	mealieFirstUser     = "admin"
	mealieFirstPassword = "MyPassword"
)

// emailDomain is the placeholder email's: undeliverable, so an app's mail to it never leaves the
// house, and never a domain anyone owns. The user may replace it inside the app.
const emailDomain = "briard.local"

// takeOverMealie makes Mealie's first account the user's, unless that is done.
func (a *app) takeOverMealie(ctx context.Context, s routes.App) error {
	acct, err := a.account()
	if err != nil {
		return err
	}
	rt, ok := s.Route(routes.ListenName)
	if !ok {
		return errors.New("Mealie has no address on this node")
	}
	base, err := s.Resolve(rt.To)
	if err != nil {
		return err
	}
	a.mealieMu.Lock()
	defer a.mealieMu.Unlock()
	m := mealieAPI{base: base.String(), client: &http.Client{Timeout: 10 * time.Second}}
	var info struct {
		IsFirstLogin bool `json:"isFirstLogin"`
	}
	if err := m.do(ctx, http.MethodGet, "/api/app/about/startup-info", "", nil, &info); err != nil {
		return errMealieStarting
	}
	if !info.IsFirstLogin {
		return nil
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	form := url.Values{"username": {mealieFirstUser}, "password": {mealieFirstPassword}}
	if err := m.do(ctx, http.MethodPost, "/api/auth/token", "", form, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("Mealie's first account no longer answers the password it shipped with, so it cannot be made yours; reinstall Mealie (%v)", err)
	}
	var self map[string]any
	if err := m.do(ctx, http.MethodGet, "/api/users/self", tok.AccessToken, nil, &self); err != nil {
		return fmt.Errorf("read Mealie's first account: %w", err)
	}
	id, _ := self["id"].(string)
	if id == "" {
		return errors.New("Mealie's first account has no id")
	}
	pw, err := newPassword()
	if err != nil {
		return err
	}
	if err := m.do(ctx, http.MethodPut, "/api/users/password", tok.AccessToken,
		map[string]string{"currentPassword": mealieFirstPassword, "newPassword": pw}, nil); err != nil {
		return fmt.Errorf("change the password of Mealie's first account: %w", err)
	}
	self["username"], self["fullName"], self["email"] = acct.Username, acct.Name, acct.Username+"@"+emailDomain
	if err := m.do(ctx, http.MethodPut, "/api/admin/users/"+url.PathEscape(id), tok.AccessToken, self, nil); err != nil {
		return fmt.Errorf("make Mealie's first account yours: %w", err)
	}
	return nil
}

var errMealieStarting = errors.New("Mealie is still starting; try again in a moment")

// mealieAPI is Mealie's REST API on its in-guest address.
type mealieAPI struct {
	base   string
	client *http.Client
}

// do sends one request -- a form for url.Values, JSON otherwise -- and decodes a 2xx answer into
// out; anything else is an error carrying the status.
func (m mealieAPI) do(ctx context.Context, method, path, token string, body, out any) error {
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
		r, ctype = strings.NewReader(string(raw)), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, m.base+path, r)
	if err != nil {
		return err
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
		return fmt.Errorf("%s %s answered %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
