//go:build !darwin && !linux

package archive

import (
	"errors"
	"os"
)

// Refuse to emulate an exclusive directory rename with a racy exists check.
func renameExclusive(parent *os.File, from, to string) error {
	return errors.New("archive: atomic exclusive publication unsupported on this platform")
}
