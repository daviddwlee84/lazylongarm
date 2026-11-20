//go:build !unix

package config

import (
	"errors"
	"os"
	"time"
)

// lockFile uses an exclusive-create lock file where flock is unavailable.
func lockFile(path string) (func(), error) {
	for i := 0; i < 50; i++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, errors.New("config is locked by another process: " + path)
}

// LockFile is exported for other state files (operation journals).
func LockFile(path string) (func(), error) { return lockFile(path) }
