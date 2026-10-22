//go:build !unix

package configupdate

// lockConfigUpdates is a no-op where flock is unavailable; the device and the
// host test environment are both unix.
func lockConfigUpdates(string) (func(), error) {
	return func() {}, nil
}
