package dashboard

import (
	"encoding/json"
	"errors"
)

// Account is the user's record: who every app is told it is talking to.
type Account struct {
	// ID is sixteen lowercase base32 characters, minted with the record and never changed: the
	// only thing briard's own state names a user by. Never shown -- it is no one's name for
	// themselves.
	ID string `json:"id"`
	// Username is fixed once written, and is the name an app's first account is made under
	// wherever briard chooses the name. No gated app finds the user by it: each signs in the
	// account its own data names as its admin, so a record a restore brings strands no app's
	// account.
	Username string `json:"username"`
	// Name and Language are seeds: each app keeps its own copy from its first sign-in, and nothing
	// is synced either way.
	Name     string `json:"name"`
	Language string `json:"language"`
}

// ErrNoAccount is a node with no record yet: no open has been redeemed since it was made.
var ErrNoAccount = errors.New("no user record yet; run `briard open` on the machine")

// ParseAccount reads the record's bytes. A file without an id is no record.
func ParseAccount(raw []byte) (Account, error) {
	var acct Account
	if err := json.Unmarshal(raw, &acct); err != nil || acct.ID == "" {
		return Account{}, ErrNoAccount
	}
	return acct, nil
}

// ValidUsername says a username is one every app accepts, the shape a record is written in:
// lowercase letters, digits, `_` and `-`, starting with a letter.
func ValidUsername(u string) bool {
	for i := 0; i < len(u); i++ {
		c := u[i]
		switch {
		case c >= 'a' && c <= 'z':
		case i > 0 && (c >= '0' && c <= '9' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return u != ""
}
