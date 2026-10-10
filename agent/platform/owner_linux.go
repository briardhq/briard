package platform

import (
	"fmt"
	"os"
	"syscall"
)

// ChownLike gives path the owner and group of dir. A folder a person owns stays theirs whole when
// a root process writes into it: without this they could not delete, move or copy what it wrote.
func ChownLike(dir, path string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: no owner to read", dir)
	}
	return os.Lchown(path, int(st.Uid), int(st.Gid))
}
