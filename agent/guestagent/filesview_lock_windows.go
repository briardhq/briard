package guestagent

import "errors"

// lockFilesView refuses: the view is kept inside the guest, which is Linux, and a Windows host
// links this package only for the client half that talks to it.
func lockFilesView() (func(), error) {
	return nil, errors.New("files view: kept only inside the guest")
}
