package driver

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const testID = "12345678901234567890123456789012"

// newTestFile returns a file driver rooted in a fresh private subdirectory.
// t.TempDir() itself is created 0755 on some platforms, which Write's
// directory check rightly rejects, so tests let the driver create its own
// 0700 directory inside it.
func newTestFile(t *testing.T, minutes int) (*File, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sessions")
	return NewFile(dir, minutes), dir
}

func TestFileWriteReadRoundtrip(t *testing.T) {
	f, _ := newTestFile(t, 10)

	if err := f.Write(testID, "payload"); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	data, found, err := f.Read(testID)
	if err != nil || !found {
		t.Fatalf("Read: found=%v err=%v", found, err)
	}
	if data != "payload" {
		t.Fatalf("Read = %q, want %q", data, "payload")
	}
}

func TestFileReadMissing(t *testing.T) {
	f, _ := newTestFile(t, 10)

	if _, found, err := f.Read(testID); found || err != nil {
		t.Fatalf("Read of missing session: found=%v err=%v, want found=false err=nil", found, err)
	}
}

func TestFileReadExpired(t *testing.T) {
	f, dir := newTestFile(t, 10)

	if err := f.Write(testID, "payload"); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, testID), old, old); err != nil {
		t.Fatalf("Chtimes failed: %v", err)
	}

	if _, found, err := f.Read(testID); found || err != nil {
		t.Fatalf("Read of expired session: found=%v err=%v, want found=false err=nil", found, err)
	}
}

func TestFileWriteIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not meaningful on Windows")
	}

	dir := filepath.Join(t.TempDir(), "sessions")
	f := NewFile(dir, 10)

	if err := f.Write(testID, "secret"); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, testID))
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("session file mode = %o, want 600", perm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat dir failed: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("session dir mode = %o, want 700", perm)
	}
}

func TestFileTouchRefreshesTimestamp(t *testing.T) {
	f, dir := newTestFile(t, 10)

	if err := f.Write(testID, "payload"); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	path := filepath.Join(dir, testID)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes failed: %v", err)
	}

	found, err := f.Touch(testID)
	if err != nil || !found {
		t.Fatalf("Touch: found=%v err=%v", found, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if !info.ModTime().After(time.Now().Add(-time.Minute)) {
		t.Fatalf("Touch did not refresh mtime, got %v", info.ModTime())
	}
}

func TestFileTouchMissingReportsNotFound(t *testing.T) {
	f, _ := newTestFile(t, 10)

	if found, err := f.Touch(testID); found || err != nil {
		t.Fatalf("Touch of missing session: found=%v err=%v, want found=false err=nil", found, err)
	}
}

func TestFileDestroyMissingIsNoop(t *testing.T) {
	f, _ := newTestFile(t, 10)

	if err := f.Destroy(testID); err != nil {
		t.Fatalf("Destroy of a missing session should succeed, got: %v", err)
	}
}

func TestFileGcOnlyRemovesSessionFiles(t *testing.T) {
	f, dir := newTestFile(t, 10)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	old := time.Now().Add(-2 * time.Hour)
	makeFile := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile %s failed: %v", name, err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("Chtimes %s failed: %v", name, err)
		}
		return path
	}

	expiredSession := makeFile(strings.Repeat("a", 32))
	leftoverTmp := makeFile(strings.Repeat("b", 32) + "-123456" + tmpSuffix)
	unrelated := makeFile("someone-elses-file.txt")
	wrongLength := makeFile(strings.Repeat("c", 31))

	freshSession := filepath.Join(dir, strings.Repeat("d", 32))
	if err := os.WriteFile(freshSession, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile fresh failed: %v", err)
	}

	if err := f.Gc(600); err != nil {
		t.Fatalf("Gc failed: %v", err)
	}

	for _, path := range []string{expiredSession, leftoverTmp} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed by Gc", filepath.Base(path))
		}
	}
	for _, path := range []string{unrelated, wrongLength, freshSession} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s to survive Gc: %v", filepath.Base(path), err)
		}
	}
}

func TestFileGcMissingDirIsNoop(t *testing.T) {
	f := NewFile(filepath.Join(t.TempDir(), "does-not-exist"), 10)

	if err := f.Gc(600); err != nil {
		t.Fatalf("Gc on a missing directory should succeed, got: %v", err)
	}
}

func TestFileWriteLeavesNoTempFiles(t *testing.T) {
	f, dir := newTestFile(t, 10)

	if err := f.Write(testID, "payload"); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != testID {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected only the session file, got %v", names)
	}
}

func TestFileDefaultsToPrivateSubdirectory(t *testing.T) {
	f := NewFile("", 0)

	if f.path == os.TempDir() {
		t.Fatal("default path must not be the shared temp directory itself")
	}
	if f.path != filepath.Join(os.TempDir(), defaultDirName()) {
		t.Fatalf("default path = %s, want %s", f.path, filepath.Join(os.TempDir(), defaultDirName()))
	}
	if f.minutes != 120 {
		t.Fatalf("default minutes = %d, want 120", f.minutes)
	}
}

func TestFileReadAndTouchRejectUntrustedDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not meaningful on Windows")
	}

	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatalf("Chmod failed: %v", err)
	}

	f := NewFile(dir, 10)
	if _, _, err := f.Read(testID); err == nil {
		t.Fatal("expected Read to reject a group/other-accessible session directory")
	}
	if _, err := f.Touch(testID); err == nil {
		t.Fatal("expected Touch to reject a group/other-accessible session directory")
	}
	if err := f.Gc(600); err == nil {
		t.Fatal("expected Gc to reject a group/other-accessible session directory")
	}
}

func TestFileWriteRejectsSymlinkedDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}

	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink failed: %v", err)
	}

	f := NewFile(link, 10)
	if err := f.Write(testID, "payload"); err == nil {
		t.Fatal("expected Write to reject a symlinked session directory")
	}
}

func TestFileWriteRejectsPermissiveDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are not meaningful on Windows")
	}

	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	if err := os.Chmod(dir, 0o770); err != nil { // group access → session IDs listable
		t.Fatalf("Chmod failed: %v", err)
	}

	f := NewFile(dir, 10)
	if err := f.Write(testID, "payload"); err == nil {
		t.Fatal("expected Write to reject a group/other-accessible session directory")
	}
}
