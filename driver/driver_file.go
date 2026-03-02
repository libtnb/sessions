package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/libtnb/utils/file"
)

type File struct {
	path    string
	minutes int
	mu      sync.Mutex
}

func NewFile(path string, minutes int) *File {
	if path == "" {
		path = os.TempDir()
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
	f.mu.Lock()
	defer f.mu.Unlock()

	return os.Remove(f.getFilePath(id))
}

func (f *File) Gc(maxLifetime int) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	cutoffTime := time.Now().Add(-time.Duration(maxLifetime) * time.Second)

	if !file.Exists(f.path) {
		return nil
	}

	return filepath.Walk(f.path, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && info.ModTime().Before(cutoffTime) {
			return os.Remove(path)
		}

		return nil
	})
}

func (f *File) Touch(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := f.getFilePath(id)
	if !file.Exists(path) {
		return fmt.Errorf("session [%s] not found", id)
	}

	now := time.Now()
	return os.Chtimes(path, now, now)
}

func (f *File) Read(id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	path := f.getFilePath(id)
	if file.Exists(path) {
		modified, err := file.LastModified(path, time.UTC.String())
		if err != nil {
			return "", err
		}
		if modified.After(time.Now().Add(-time.Duration(f.minutes) * time.Minute)) {
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			return string(data), nil
		}
	}

	return "", fmt.Errorf("session [%s] not found", id)
}

func (f *File) Write(id string, data string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return file.WriteString(f.getFilePath(id), data)
}

func (f *File) getFilePath(id string) string {
	return filepath.Join(f.path, id)
}
