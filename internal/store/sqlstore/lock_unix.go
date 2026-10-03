//go:build unix

package sqlstore

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockFile takes an exclusive, non-blocking flock for the process lifetime.
func lockFile(path string) (func() error, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: derived from the operator-configured database path
	if err != nil {
		return nil, fmt.Errorf("sqlite: open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("sqlite: database is in use by another smtp-handler process (lock %s); "+
				"SQLite supports a single instance - use PostgreSQL to run replicas", path)
		}
		return nil, fmt.Errorf("sqlite: lock %s: %w", path, err)
	}
	return func() error {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return f.Close()
	}, nil
}
