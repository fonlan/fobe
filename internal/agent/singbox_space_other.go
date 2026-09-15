//go:build !unix

package agent

import "errors"

// statfsFree has no portable statfs off unix; the space gate fails open
// (checkInstallSpace treats any error as "cannot determine, don't block").
func statfsFree(string) (freeBytes, freeInodes uint64, err error) {
	return 0, 0, errors.New("statfs unavailable on this platform")
}
