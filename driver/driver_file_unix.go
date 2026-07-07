//go:build !windows

package driver

import (
	"fmt"
	"os"
	"syscall"
)

// defaultDirName returns a per-user directory name so that multiple
// applications running as different users never share (or fight over) one
// predictable directory in the shared temp dir.
func defaultDirName() string {
	return fmt.Sprintf("sessions-%d", os.Getuid())
}

// checkDirTrusted rejects a session directory that is owned by another user
// or accessible by group/others. File names are session IDs and directory
// write access allows deleting or swapping session files, so anything less
// strict would hand session control to other local users.
func checkDirTrusted(path string, info os.FileInfo) error {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("session path [%s] is owned by uid %d, not the current user", path, stat.Uid)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("session path [%s] mode %o is accessible by group/others", path, perm)
	}
	return nil
}
