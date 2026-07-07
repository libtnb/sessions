//go:build windows

package driver

import "os"

// defaultDirName returns the default session directory name. The Windows
// temp directory is per-user already, so no uid suffix is needed.
func defaultDirName() string {
	return "sessions"
}

// checkDirTrusted is a no-op on Windows: Unix ownership and permission bits
// are not meaningful there, and the per-user temp directory provides the
// isolation the Unix checks exist for.
func checkDirTrusted(string, os.FileInfo) error {
	return nil
}
