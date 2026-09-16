//go:build windows

package agent

import (
	"errors"
	"os"
)

// execSelfDefault: the agent targets linux/amd64 (design §5.1); Windows builds
// are never shipped, so the seam only has to keep the package compilable.
func execSelfDefault(string, []string, []string) error {
	return errors.New("self-update re-exec is not implemented on windows")
}

// dirWritable probes writability by creating a file — the closest Windows
// equivalent to the rename-permission check the unix build gets from Access.
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".fobe-writable-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}
