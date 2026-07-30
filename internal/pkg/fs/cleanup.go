package fs

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func DeleteFilesOlderThan(dirPath string, maxAge time.Duration, now time.Time) (int, error) {
	if maxAge <= 0 {
		return 0, fmt.Errorf("max age must be greater than 0")
	}

	fileInfo, err := os.Stat(dirPath)
	if err != nil {
		return 0, fmt.Errorf("failed to access directory: %v", err)
	}
	if !fileInfo.IsDir() {
		return 0, fmt.Errorf("path is not a directory: %s", dirPath)
	}

	cutoff := now.Add(-maxAge)
	deleted := 0
	err = filepath.WalkDir(dirPath, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.ModTime().Before(cutoff) {
			return nil
		}

		if err := os.Remove(path); err != nil {
			return err
		}
		deleted++
		return nil
	})
	if err != nil {
		return deleted, fmt.Errorf("failed to clean old files: %v", err)
	}

	return deleted, nil
}
