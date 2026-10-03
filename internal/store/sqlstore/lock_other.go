//go:build !unix

package sqlstore

// lockFile is a no-op where flock is unavailable; run a single instance.
func lockFile(string) (func() error, error) {
	return func() error { return nil }, nil
}
