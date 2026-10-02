//go:build linux

package archive

import (
	"golang.org/x/sys/unix"
	"os"
)

func renameExclusive(parent *os.File, from, to string) error {
	return unix.Renameat2(int(parent.Fd()), from, int(parent.Fd()), to, unix.RENAME_NOREPLACE)
}
