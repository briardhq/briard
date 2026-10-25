package host

import "os"

// restrictDoor makes the admin socket root's: connect needs write on the socket file, and the
// bind honours the umask, so an explicit chmod is the only way to know the mode. There is no
// install user here -- the door is root's on Linux.
func restrictDoor(sock, _ string) error { return os.Chmod(sock, 0o600) }

// doorRefusal is the identity on Linux: a refused connect is EACCES, which is fs.ErrPermission.
func doorRefusal(err error) error { return err }
