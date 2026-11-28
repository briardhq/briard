package navidrome

// Navidrome's accounts, as the dashboard reads and sets them.
//
// Navidrome's user is found by role, never kept: the reverse proxy signs the user in by a
// username in a header, and the dashboard reads which at every session check off Navidrome's own
// data -- its oldest admin (Admin). Nothing about it is stored on briard's side, so the reverse
// proxy follows a rename inside Navidrome and an undo or a restore of its data at the next check,
// within the half minute the reverse proxy keeps an answer.
//
// Navidrome makes no account of its own: the first name a header brings becomes its admin. So a
// Navidrome with no account yet has none to name, and the name it is then given is the one its
// first account is made under. Navidrome's own tool, run in its container, reads its accounts
// straight from its database, so briard needs no account in Navidrome to read them, and adds
// none.
//
// A phone's client signs in with Navidrome's own password, which the dashboard's "On your phone"
// shows (SignIn). Navidrome keeps it reversibly encrypted, because a Subsonic client signs in
// with a salted hash only the plain password can check, under a key that is Navidrome's published
// default while its PasswordEncryptionKey is unset, as Config leaves it. So the row is read with
// the sqlite3 Navidrome's own image ships and decrypted: what is shown is what Navidrome holds, a
// password the person set included. Until someone sets one, the account's password is the one its
// first header sign-in generated, a long random string no one was shown; that one is replaced,
// through Navidrome's own API, by words a person can type.

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
	"strings"
	"time"

	"briard.io/shared/routes"
)

// Exec runs a command in Navidrome's container, with stdin, and returns what it printed to
// stdout.
type Exec func(ctx context.Context, stdin string, args ...string) ([]byte, error)

// account is one of Navidrome's accounts, as its `user list` tool says it.
type account struct {
	Username  string    `json:"username"`
	Admin     bool      `json:"admin"`
	CreatedAt time.Time `json:"createdAt"`
}

// Admin is the username of Navidrome's oldest admin, the account the reverse proxy signs the
// user into: none for a Navidrome with no account at all yet, and "" for one whose accounts
// hold no admin.
func Admin(ctx context.Context, in Exec) (user string, none bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// The tool's banner and log go to stderr; its list alone to stdout. Only a list it printed
	// says Navidrome has no account: a tool that could not read the database fails, and a failure
	// is never taken for an empty Navidrome, which a header would then give a second account.
	out, err := in(ctx, "", "navidrome", "--nobanner", "user", "list", "--format", "json")
	if err != nil {
		return "", false, fmt.Errorf("list Navidrome's accounts: %w", err)
	}
	var accounts []account
	if err := json.Unmarshal(out, &accounts); err != nil {
		return "", false, fmt.Errorf("read Navidrome's accounts: %w", err)
	}
	if len(accounts) == 0 {
		return "", true, nil
	}
	var oldest *account
	for i, u := range accounts {
		if u.Admin && (oldest == nil || u.CreatedAt.Before(oldest.CreatedAt)) {
			oldest = &accounts[i]
		}
	}
	if oldest == nil {
		return "", false, nil
	}
	return oldest.Username, false, nil
}

// dbPath is Navidrome's database in its container: the image's data folder, which the
// catalog's manifest mounts from the app's data tier.
const dbPath = "/data/navidrome.db"

// Key is what Navidrome encrypts passwords under with no PasswordEncryptionKey set: the
// SHA-256 of its published default.
var Key = sha256.Sum256([]byte("just for obfuscation"))

// Generated starts the password Navidrome makes for an account a header sign-in creates, which
// only replacing it ends.
const Generated = "__NAVIDROME_AUTOGEN__"

// errNoAccount is a username Navidrome has no row for: a Navidrome nobody opened yet.
var errNoAccount = errors.New("no such account in Navidrome")

// SignIn is the password a phone's client signs in to Navidrome with as user, at base,
// Navidrome's address on this guest. A Navidrome nobody opened yet is given its account first,
// the way opening it would; a password still the generated one is replaced by words. The caller
// reaches the pod network from the address Navidrome trusts, as the reverse proxy does, so these
// requests sign in by header the same way. The password returned is read back from Navidrome
// after any change, never the one briard meant to set.
func SignIn(ctx context.Context, in Exec, base, user string, words func() (string, error)) (string, error) {
	api := api{base: base, user: user, client: &http.Client{Timeout: 10 * time.Second}}
	id, pw, err := password(ctx, in, user)
	if errors.Is(err, errNoAccount) {
		if err := api.do(ctx, http.MethodGet, "/app/", nil, nil); err != nil {
			return "", fmt.Errorf("make your account in Navidrome: %w", err)
		}
		id, pw, err = password(ctx, in, user)
	}
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(pw, Generated) {
		return pw, nil
	}
	// The user's own record, sent back whole with the password: Navidrome writes every field the
	// body has, so a body with the password alone would take the account's admin away.
	var rec map[string]any
	if err := api.do(ctx, http.MethodGet, "/api/user/"+url.PathEscape(id), nil, &rec); err != nil {
		return "", fmt.Errorf("read your account in Navidrome: %w", err)
	}
	typed, err := words()
	if err != nil {
		return "", err
	}
	rec["password"] = typed
	if err := api.do(ctx, http.MethodPut, "/api/user/"+url.PathEscape(id), rec, nil); err != nil {
		return "", fmt.Errorf("set your password in Navidrome: %w", err)
	}
	_, pw, err = password(ctx, in, user)
	return pw, err
}

// password reads an account's id and password off Navidrome's database, by the sqlite3 in its
// container, read-only.
func password(ctx context.Context, in Exec, user string) (id, password string, err error) {
	q := "SELECT id, password FROM user WHERE user_name = '" + strings.ReplaceAll(user, "'", "''") + "' COLLATE NOCASE"
	out, err := in(ctx, "", "sqlite3", "-readonly", "-json", dbPath, q)
	if err != nil {
		return "", "", fmt.Errorf("read your password in Navidrome: %w", err)
	}
	// sqlite3 prints nothing at all for no row, not an empty list.
	if len(bytes.TrimSpace(out)) == 0 {
		return "", "", errNoAccount
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
	password, err = Decrypt(rows[0].Password)
	if err != nil {
		return "", "", fmt.Errorf("read your password in Navidrome: %w", err)
	}
	return rows[0].ID, password, nil
}

// Decrypt is Navidrome's own decryption: base64 of a GCM nonce and the ciphertext.
func Decrypt(stored string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(Key[:])
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

// api is Navidrome's own API, signed in as user by header.
type api struct {
	base, user string
	client     *http.Client
}

func (n api) do(ctx context.Context, method, path string, body, out any) error {
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
	req.Header.Set(routes.UserHeader, n.user)
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
