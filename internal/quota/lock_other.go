//go:build !linux

package quota

import "fmt"

// The native plugin itself is Linux-only. Keeping this stub makes accidental
// non-Linux test builds fail clearly instead of silently skipping the lease.
type databaseLock struct{}

func acquireDatabaseLock(string) (*databaseLock, error) {
	return nil, fmt.Errorf("codex-carpool supports Linux only")
}

func (lock *databaseLock) release() error { return nil }
