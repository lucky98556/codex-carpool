package quota

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// migrateLegacyPluginDatabase performs one conservative, copy-only migration
// from the data directory used before the plugin-owned directory layout. The
// old files remain untouched, so an operator can roll back by restoring the
// previous .so. It runs only for the native plugin's fixed default path.
func migrateLegacyPluginDatabase(targetPath string) error {
	if filepath.Clean(targetPath) != filepath.Clean(defaultDatabasePath) {
		return nil
	}
	for _, source := range []string{previousPluginDatabasePath, legacyDefaultDatabasePath} {
		exists, err := databaseArtifactExists(source)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		return copyLegacyDatabaseFiles(source, targetPath)
	}
	return nil
}

func copyLegacyDatabaseFiles(sourcePath, targetPath string) error {
	markerPath := targetPath + ".migrating"
	if _, err := os.Lstat(markerPath); err == nil {
		if err := removeIncompleteMigration(targetPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect database migration marker: %w", err)
	}

	targetExists, err := databaseArtifactExists(targetPath)
	if err != nil {
		return err
	}
	if targetExists {
		return nil
	}
	if _, err := os.Stat(sourcePath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect legacy plugin database: %w", err)
	}

	legacyLock, err := acquireDatabaseLock(sourcePath)
	if err != nil {
		return fmt.Errorf("lock legacy plugin database before migration: %w", err)
	}
	defer legacyLock.release()

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
		return fmt.Errorf("create plugin data directory for migration: %w", err)
	}
	if err := os.Chmod(filepath.Dir(targetPath), 0o700); err != nil {
		return fmt.Errorf("restrict plugin data directory for migration: %w", err)
	}
	marker, err := os.OpenFile(markerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("start database migration: %w", err)
	}
	if err := marker.Close(); err != nil {
		return fmt.Errorf("close database migration marker: %w", err)
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := copyDatabaseArtifact(sourcePath+suffix, targetPath+suffix, suffix == ""); err != nil {
			return err
		}
	}
	if err := os.Remove(markerPath); err != nil {
		return fmt.Errorf("finish database migration: %w", err)
	}
	return nil
}

func databaseArtifactExists(path string) (bool, error) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("inspect plugin database target: %w", err)
		}
	}
	return false, nil
}

func removeIncompleteMigration(targetPath string) error {
	for _, suffix := range []string{"", "-wal", "-shm", ".copying", "-wal.copying", "-shm.copying", ".migrating"} {
		if err := os.Remove(targetPath + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove incomplete database migration artifact: %w", err)
		}
	}
	return nil
}

func copyDatabaseArtifact(sourcePath, targetPath string, required bool) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open legacy database artifact %q: %w", sourcePath, err)
	}
	defer source.Close()

	temporaryPath := targetPath + ".copying"
	target, err := os.OpenFile(temporaryPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create migrated database artifact %q: %w", targetPath, err)
	}
	if _, err := io.Copy(target, source); err != nil {
		_ = target.Close()
		return fmt.Errorf("copy legacy database artifact %q: %w", sourcePath, err)
	}
	if err := target.Sync(); err != nil {
		_ = target.Close()
		return fmt.Errorf("sync migrated database artifact %q: %w", targetPath, err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("close migrated database artifact %q: %w", targetPath, err)
	}
	if err := os.Rename(temporaryPath, targetPath); err != nil {
		return fmt.Errorf("publish migrated database artifact %q: %w", targetPath, err)
	}
	return nil
}
