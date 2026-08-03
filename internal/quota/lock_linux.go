//go:build linux

package quota

import (
	"fmt"
	"os"
	"syscall"
)

// databaseLock holds a Linux advisory lock for the lifetime of the plugin.
// The lock file is intentionally retained after shutdown; flock releases when
// the descriptor closes, while a stable path prevents a delete/recreate race.
type databaseLock struct {
	file *os.File
}

func acquireDatabaseLock(databasePath string) (*databaseLock, error) {
	file, err := os.OpenFile(databasePath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open quota database lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("quota database is already in use by another CLIProxyAPI instance: %w", err)
	}
	return &databaseLock{file: file}, nil
}

func (lock *databaseLock) release() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
