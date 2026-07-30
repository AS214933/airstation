package fs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeleteFilesOlderThanDeletesExpiredFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	oldFile := filepath.Join(dir, "old.m4s")
	newFile := filepath.Join(dir, "new.m4s")
	nestedDir := filepath.Join(dir, "nested")
	nestedOldFile := filepath.Join(nestedDir, "old.mp4")

	if err := os.WriteFile(oldFile, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newFile, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(nestedDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nestedOldFile, []byte("nested old"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldFile, now.Add(-25*time.Hour), now.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newFile, now.Add(-23*time.Hour), now.Add(-23*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(nestedOldFile, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}

	deleted, err := DeleteFilesOlderThan(dir, 24*time.Hour, now)
	if err != nil {
		t.Fatalf("delete files older than: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatalf("old file still exists or stat failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(nestedOldFile); !os.IsNotExist(err) {
		t.Fatalf("nested old file still exists or stat failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Fatalf("new file was removed: %v", err)
	}
	if _, err := os.Stat(nestedDir); err != nil {
		t.Fatalf("nested directory was removed: %v", err)
	}
}

func TestDeleteFilesOlderThanRejectsNonPositiveMaxAge(t *testing.T) {
	_, err := DeleteFilesOlderThan(t.TempDir(), 0, time.Now())
	if err == nil {
		t.Fatal("expected error")
	}
}
