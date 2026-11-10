package main

// THE USER: one person, the admin, kept as a record and never as "the household". Several users
// are not planned, and when they come the admin stays one, with access to everything; what code
// cannot change on that day is what an app wrote into its own database at a first sign-in, so the
// one identity is a user from the start, in the shape whoever comes later will share.
//
// THE RECORD IS WRITTEN ONCE, at the first redeemed open, from what the host knew about the OS
// account that ran `briard open` (the handoff), with the defaults applied there and nowhere else.
// Every later open carries the same seed and it is ignored: an app keeps its own copy of the name
// and language from its first sign-in, and matches its account on the username, so the username
// must never move. A record with no id counts as none, so a file of another shape is replaced.
//
// It lives on the volume beside the device registry, and the nightly backup carries it.

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"

	"briard.io/shared/dashboard"
)

// account is the record: who every app is told it is talking to.
type account struct {
	// ID is sixteen lowercase base32 characters, minted with the record and never changed: the
	// only thing briard's own state names a user by. Never shown -- it is no one's name for
	// themselves.
	ID string `json:"id"`
	// Username is fixed once written: Home Assistant's first user, the subject the door signs into
	// an app, the account an app matches on.
	Username string `json:"username"`
	// Name and Language are seeds: each app keeps its own copy from its first sign-in, and nothing
	// is synced either way.
	Name     string `json:"name"`
	Language string `json:"language"`
}

// The defaults, applied when the record is written and nowhere else. Not `admin`: that is the
// account many apps seed for themselves, so a user of that name would meet theirs. Owner is
// Home Assistant's word for this person.
const (
	defaultUsername = "owner"
	defaultName     = "Owner"
	defaultLanguage = "en"
)

// errNoAccount is a dashboard with no record yet: no open has been redeemed since it was made.
var errNoAccount = errors.New("no user record yet; run `briard open` on the machine")

// account is the record on the volume. A file without an id is no record.
func (a *app) account() (account, error) {
	var acct account
	if err := a.readState(accountFile, &acct); err != nil || acct.ID == "" {
		return account{}, errNoAccount
	}
	return acct, nil
}

// keepAccount writes the record from an open's seed, unless one is already written.
func (a *app) keepAccount(seed dashboard.Handoff) error {
	if _, err := a.account(); err == nil {
		return nil
	}
	acct, err := newAccount(seed)
	if err != nil {
		return err
	}
	return a.writeState(accountFile, acct)
}

// newAccount is the record a seed makes: the username normalised, `root` read as nothing known (a
// root shell without sudo says nothing about the person), the name the capitalised username when
// the OS account had none -- or only its own username -- and a two-letter language.
func newAccount(seed dashboard.Handoff) (account, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return account{}, fmt.Errorf("mint the user's id: %w", err)
	}
	acct := account{
		ID:       strings.ToLower(base32.StdEncoding.EncodeToString(b[:])),
		Username: normaliseUsername(seed.Username),
		Name:     strings.TrimSpace(seed.Name),
		Language: strings.ToLower(seed.Language),
	}
	if acct.Username == "root" {
		acct.Name = ""
	}
	if acct.Username == "" || acct.Username == "root" {
		acct.Username = defaultUsername
	}
	if acct.Name == "" || strings.EqualFold(acct.Name, strings.TrimSpace(seed.Username)) {
		acct.Name = defaultName
		if acct.Username != defaultUsername {
			acct.Name = strings.ToUpper(acct.Username[:1]) + acct.Username[1:]
		}
	}
	if len(acct.Language) != 2 || !isLower(acct.Language[0]) || !isLower(acct.Language[1]) {
		acct.Language = defaultLanguage
	}
	return acct, nil
}

// normaliseUsername keeps what every app accepts in a username: lowercase letters, digits, `_`
// and `-`, starting with a letter. Anything else is dropped, so an account named in another script
// leaves nothing and gets the default.
func normaliseUsername(u string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(u) {
		switch {
		case r < 128 && isLower(byte(r)):
		case b.Len() > 0 && (r >= '0' && r <= '9' || r == '_' || r == '-'):
		default:
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isLower(c byte) bool { return c >= 'a' && c <= 'z' }
