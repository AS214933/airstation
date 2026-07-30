package fs

import (
	"context"
	"log/slog"
	"time"
)

func RunFileCleanup(ctx context.Context, dirPath string, maxAge, interval time.Duration, log *slog.Logger) {
	cleanupFiles(dirPath, maxAge, log)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cleanupFiles(dirPath, maxAge, log)
		}
	}
}

func cleanupFiles(dirPath string, maxAge time.Duration, log *slog.Logger) {
	deleted, err := DeleteFilesOlderThan(dirPath, maxAge, time.Now())
	if err != nil {
		log.Warn("Temporary file cleanup failed", slog.String("error", err.Error()))
		return
	}
	if deleted > 0 {
		log.Info("Temporary files cleaned", slog.Int("deleted", deleted), slog.Duration("maxAge", maxAge))
	}
}
