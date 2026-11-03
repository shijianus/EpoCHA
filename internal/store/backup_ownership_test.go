package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBackupPreservesExistingPartialFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{{"empty", nil}, {"nonempty", []byte("operator-owned-partial")}} {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			d, source := newDBAt(t, filepath.Join(t.TempDir(), "source.db"))
			mustSite(t, d, "backup-ownership")
			dir := t.TempDir()
			now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
			name := "epocha-" + now.Format("20060102T150405Z") + "-ownership"
			paths := []string{filepath.Join(dir, "."+name+".db"+partialSuffix), filepath.Join(dir, "."+name+manifestSuffix+partialSuffix)}
			for _, path := range paths {
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			m, err := Backup(t.Context(), BackupOptions{SrcPath: source, DestDir: dir, Now: now, Commit: "ownership", Pepper: testPepper()})
			for _, path := range paths {
				got, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(got, body) {
					t.Errorf("preexisting partial changed: %s, err=%v", filepath.Base(path), readErr)
				}
			}
			if err != nil {
				t.Fatalf("independent staging must allow backup: %v", err)
			}
			if err := VerifyBackup(t.Context(), filepath.Join(dir, m.DBFile), filepath.Join(dir, m.Name+manifestSuffix)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBackupConcurrentSameNameDoesNotDeleteWinner(t *testing.T) {
	d, source := newDBAt(t, filepath.Join(t.TempDir(), "source.db"))
	mustSite(t, d, "backup-concurrent")
	dir := t.TempDir()
	opts := BackupOptions{SrcPath: source, DestDir: dir, Now: time.Now().UTC(), Commit: "same", Pepper: testPepper()}
	type outcome struct {
		manifest *Manifest
		err      error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; m, err := Backup(t.Context(), opts); results <- outcome{m, err} }()
	}
	close(start)
	wg.Wait()
	close(results)
	var winner *Manifest
	wins := 0
	for result := range results {
		if result.err == nil {
			wins++
			winner = result.manifest
		}
	}
	if wins != 1 {
		t.Fatalf("successful backups=%d, want one", wins)
	}
	if err := VerifyBackup(t.Context(), filepath.Join(dir, winner.DBFile), filepath.Join(dir, winner.Name+manifestSuffix)); err != nil {
		t.Fatalf("winning backup was damaged: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("unexpected owned temporary files: %v", entries)
	}
}

func TestBackupPreservesExistingManifest(t *testing.T) {
	d, source := newDBAt(t, filepath.Join(t.TempDir(), "source.db"))
	mustSite(t, d, "backup-conflict")
	dir := t.TempDir()
	now := time.Now().UTC()
	name := "epocha-" + now.Format("20060102T150405Z") + "-manifest"
	manifest := filepath.Join(dir, name+manifestSuffix)
	want := []byte("existing-manifest")
	if err := os.WriteFile(manifest, want, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(t.Context(), BackupOptions{SrcPath: source, DestDir: dir, Now: now, Commit: "manifest", Pepper: testPepper()}); err == nil {
		t.Fatal("existing manifest must reject backup")
	}
	got, err := os.ReadFile(manifest)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("existing manifest changed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, name+".db")); !os.IsNotExist(err) {
		t.Fatal("refused backup published a database")
	}
}

func TestBackupPublicationKeepsExistingDestination(t *testing.T) {
	for _, conflict := range []string{"database", "manifest"} {
		t.Run(conflict, func(t *testing.T) {
			dir := t.TempDir()
			db, manifest := filepath.Join(dir, "staged.db"), filepath.Join(dir, "staged.json")
			destDB, destManifest := filepath.Join(dir, "final.db"), filepath.Join(dir, "final.json")
			if err := os.WriteFile(db, []byte("new-database"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifest, []byte("new-manifest"), 0o600); err != nil {
				t.Fatal(err)
			}
			target := destDB
			if conflict == "manifest" {
				target = destManifest
			}
			if err := os.WriteFile(target, []byte("existing-destination"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := publishBackup(db, manifest, destDB, destManifest, func(string) error { t.Fatal("must not sync incomplete publication"); return nil }); err == nil {
				t.Fatal("publication overwrote a conflict")
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != "existing-destination" {
				t.Fatalf("conflicting file changed: %v", err)
			}
			if conflict == "database" {
				if _, err := os.Stat(destManifest); !os.IsNotExist(err) {
					t.Fatal("database conflict still published manifest")
				}
			} else {
				got, err := os.ReadFile(destDB)
				if err != nil || string(got) != "new-database" {
					t.Fatalf("partly published database was deleted: %v", err)
				}
			}
		})
	}
}

func TestBackupPublicationReportsSyncFailureAndKeepsBothFiles(t *testing.T) {
	dir := t.TempDir()
	db, manifest := filepath.Join(dir, "staged.db"), filepath.Join(dir, "staged.json")
	destDB, destManifest := filepath.Join(dir, "final.db"), filepath.Join(dir, "final.json")
	if err := os.WriteFile(db, []byte("database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("manifest"), 0o600); err != nil {
		t.Fatal(err)
	}
	warning, err := publishBackup(db, manifest, destDB, destManifest, func(string) error { return errors.New("injected sync error") })
	if err != nil || !strings.Contains(warning, "持久化尚未确认") {
		t.Fatalf("missing post-publication warning: %q %v", warning, err)
	}
	for _, path := range []string{destDB, destManifest} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("published file removed: %v", err)
		}
	}
}
