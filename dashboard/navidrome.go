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
//
// A phone's client signs in with Navidrome's own password, which "On your phone" (phone.go)
// shows. Navidrome keeps it reversibly encrypted, because a Subsonic client signs in with a salted
// hash only the plain password can check, under a key that is Navidrome's published default while
// its PasswordEncryptionKey is unset, as agent/navidrome leaves it. So the dashboard reads the row
// with the sqlite3 Navidrome's own image ships and decrypts it: what is shown is what Navidrome
// holds, a password the person set included. Until someone sets one, the account's password is the
// one its first header sign-in generated, a long random string no one was shown; that one is
// replaced, through Navidrome's own API, by three words a person can type.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"briard.io/agent/navidrome"
	"briard.io/agent/quadlet"
	"briard.io/shared/routes"
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

// navidromeDB is Navidrome's database in its container: the image's data folder, which the
// catalog's manifest mounts from the app's data tier.
const navidromeDB = "/data/navidrome.db"

// navidromeKey is what Navidrome encrypts passwords under with no PasswordEncryptionKey set: the
// SHA-256 of its published default. navidromeGenerated starts the password Navidrome makes for an
// account a header sign-in creates, which only replacing it ends.
var navidromeKey = sha256.Sum256([]byte("just for obfuscation"))

const navidromeGenerated = "__NAVIDROME_AUTOGEN__"

// navidromeUserHeader is where Navidrome reads a header sign-in's username, as the reverse proxy
// sends it (agent/navidrome). The dashboard reaches the pod network from the address Navidrome
// trusts for it, as the reverse proxy does, so its own requests sign in the same way.
const navidromeUserHeader = "X-Forwarded-User"

// errNoNavidromeAccount is a username Navidrome has no row for: a Navidrome nobody opened yet.
var errNoNavidromeAccount = errors.New("no such account in Navidrome")

// navidromeSignIn is the username and password a phone's client signs in to Navidrome with. A
// Navidrome nobody opened yet is given its account first, the way opening it would; a password
// still the generated one is replaced. The password returned is read back from Navidrome after
// any change, never the one briard meant to set.
func (a *app) navidromeSignIn(ctx context.Context, s routes.App) (user, password string, err error) {
	a.navidromeMu.Lock()
	defer a.navidromeMu.Unlock()
	user, err = a.navidromeUser(ctx)
	if err != nil {
		return "", "", err
	}
	rt, ok := s.Route(routes.ListenName)
	if !ok {
		return "", "", errors.New("the routing table has no route to Navidrome")
	}
	base, err := s.Resolve(rt.To)
	if err != nil {
		return "", "", err
	}
	api := navidromeAPI{base: base.String(), user: user, client: &http.Client{Timeout: 10 * time.Second}}
	id, password, err := a.navidromePassword(ctx, user)
	if errors.Is(err, errNoNavidromeAccount) {
		if err := api.do(ctx, http.MethodGet, "/app/", nil, nil); err != nil {
			return "", "", fmt.Errorf("make your account in Navidrome: %w", err)
		}
		id, password, err = a.navidromePassword(ctx, user)
	}
	if err != nil {
		return "", "", err
	}
	if !strings.HasPrefix(password, navidromeGenerated) {
		return user, password, nil
	}
	// The user's own record, sent back whole with the password: Navidrome writes every field the
	// body has, so a body with the password alone would take the account's admin away.
	var rec map[string]any
	if err := api.do(ctx, http.MethodGet, "/api/user/"+url.PathEscape(id), nil, &rec); err != nil {
		return "", "", fmt.Errorf("read your account in Navidrome: %w", err)
	}
	words, err := passphrase()
	if err != nil {
		return "", "", err
	}
	rec["password"] = words
	if err := api.do(ctx, http.MethodPut, "/api/user/"+url.PathEscape(id), rec, nil); err != nil {
		return "", "", fmt.Errorf("set your password in Navidrome: %w", err)
	}
	if _, password, err = a.navidromePassword(ctx, user); err != nil {
		return "", "", err
	}
	return user, password, nil
}

// navidromePassword reads an account's id and password off Navidrome's database, by the sqlite3
// in its container, read-only.
func (a *app) navidromePassword(ctx context.Context, user string) (id, password string, err error) {
	q := "SELECT id, password FROM user WHERE user_name = '" + strings.ReplaceAll(user, "'", "''") + "' COLLATE NOCASE"
	out, err := a.inNavidrome(ctx, "sqlite3", "-readonly", "-json", navidromeDB, q)
	if err != nil {
		return "", "", fmt.Errorf("read your password in Navidrome: %w", err)
	}
	// sqlite3 prints nothing at all for no row, not an empty list.
	if len(bytes.TrimSpace(out)) == 0 {
		return "", "", errNoNavidromeAccount
	}
	var rows []struct {
		ID       string `json:"id"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return "", "", fmt.Errorf("read your password in Navidrome: %w", err)
	}
	if len(rows) != 1 {
		return "", "", fmt.Errorf("read your password in Navidrome: %d accounts named %q", len(rows), user)
	}
	password, err = navidromeDecrypt(rows[0].Password)
	if err != nil {
		return "", "", fmt.Errorf("read your password in Navidrome: %w", err)
	}
	return rows[0].ID, password, nil
}

// navidromeDecrypt is Navidrome's own decryption: base64 of a GCM nonce and the ciphertext.
func navidromeDecrypt(stored string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(navidromeKey[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("the stored password is too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("the stored password does not decrypt with Navidrome's default key")
	}
	return string(plain), nil
}

// navidromeAPI is Navidrome's own API, signed in as user by header.
type navidromeAPI struct {
	base, user string
	client     *http.Client
}

func (n navidromeAPI) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, n.base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set(navidromeUserHeader, n.user)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
