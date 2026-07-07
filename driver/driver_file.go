package driver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	sessionIDLength = 32
	tmpSuffix       = ".tmp"
)

// File is a session driver that stores each session in its own file.
//
// Concurrent access to the same session is serialized by the Manager's
// per-session locks, and writes are atomic (temp file + rename), so the
// driver itself needs no locking.
type File struct {
	path    string
	minutes int
}

// NewFile creates a file driver that stores sessions under path, treating
// sessions older than minutes as expired. An empty path defaults to a
// dedicated per-user directory inside os.TempDir() ("sessions-<uid>" on
// Unix, "sessions" on Windows where the temp directory is per-user already);
// minutes <= 0 defaults to 120.
func NewFile(path string, minutes int) *File {
	if path == "" {
		path = filepath.Join(os.TempDir(), defaultDirName())
	}
	if minutes <= 0 {
		minutes = 120
	}
	return &File{
		path:    path,
		minutes: minutes,
	}
}

func (f *File) Close() error {
	return nil
}

func (f *File) Destroy(id string) error {
	exists, err := f.trustDir()
	if err != nil || !exists {
		return err
	}
	if err = os.Remove(f.getFilePath(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Gc removes expired session files. Only files that look like session data
// (32 alphanumeric characters, or leftover temp files from atomic writes)
// are removed, so a directory shared with other applications stays intact.
func (f *File) Gc(maxLifetime int) error {
	exists, err := f.trustDir()
	if err != nil || !exists {
		return err
	}

	entries, err := os.ReadDir(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	cutoff := time.Now().Add(-time.Duration(maxLifetime) * time.Second)

	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !isSessionFileName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err = os.Remove(filepath.Join(f.path, entry.Name())); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (f *File) Touch(id string) (bool, error) {
	exists, err := f.trustDir()
	if err != nil || !exists {
		return false, err
	}

	now := time.Now()
	if err = os.Chtimes(f.getFilePath(id), now, now); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (f *File) Read(id string) (string, bool, error) {
	exists, err := f.trustDir()
	if err != nil || !exists {
		return "", false, err
	}

	path := f.getFilePath(id)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if info.IsDir() ||
		!info.ModTime().After(time.Now().Add(-time.Duration(f.minutes)*time.Minute)) {
		return "", false, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) { // removed between Stat and ReadFile
			return "", false, nil
		}
		return "", false, err
	}
	return string(data), true, nil
}

// Write persists the session data atomically: the data is written to a
// temporary file (0600) which is then renamed over the target, so a
// concurrent Read never observes a partially written session.
func (f *File) Write(id string, data string) error {
	if err := f.ensureDir(); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(f.path, id+"-*"+tmpSuffix)
	if err != nil {
		return err
	}
	if _, err = tmp.WriteString(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err = tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), f.getFilePath(id))
}

// ensureDir creates the session directory if needed and verifies it is
// trustworthy.
func (f *File) ensureDir() error {
	if err := os.MkdirAll(f.path, 0o700); err != nil {
		return err
	}
	_, err := f.trustDir()
	return err
}

// trustDir verifies that the session directory, when it exists, is a real,
// private directory. The default path lives in a shared temp directory with
// a predictable name, so a pre-created symlink, foreign-owned or
// group/other-accessible directory (which would let a local attacker list
// session IDs, replay expired session files or delete session files) is
// rejected by every operation, not just writes. It returns whether the
// directory exists.
func (f *File) trustDir() (bool, error) {
	info, err := os.Lstat(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() {
		return true, fmt.Errorf("session path [%s] is not a directory", f.path)
	}
	return true, checkDirTrusted(f.path, info)
}

func (f *File) getFilePath(id string) string {
	// Base guards against path traversal if the driver is used directly
	// with an unvalidated ID.
	return filepath.Join(f.path, filepath.Base(id))
}

func isValidSessionID(id string) bool {
	if len(id) != sessionIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') &&
			(c < 'A' || c > 'Z') &&
			(c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}

// isSessionFileName reports whether name is a session file ("<32 alnum>")
// or a leftover temp file from an interrupted atomic write ("<32 alnum>-<random>.tmp").
func isSessionFileName(name string) bool {
	if isValidSessionID(name) {
		return true
	}
	return strings.HasSuffix(name, tmpSuffix) &&
		len(name) > sessionIDLength+len(tmpSuffix) &&
		name[sessionIDLength] == '-' &&
		isValidSessionID(name[:sessionIDLength])
}
