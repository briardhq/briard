package guestagent

import "errors"

// lockFile refuses: what it guards (the app converge, the Files view) is kept inside the guest,
// which is Linux, and a Windows host links this package only for the client half that talks to it.
func lockFile(string) (func(), error) {
	return nil, errors.New("kept only inside the guest")
}
