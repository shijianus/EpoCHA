package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/store"
)

func TestPurgeLoopRunsPromptlyAndStopsOnCancellation(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "cleanup.db"), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	site, _, err := db.CreateSite(t.Context(), "cleanup", "sk-cleanup", store.SitePolicy{PowBits: 12})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Microsecond)
	challenge := &store.Challenge{ID: "cleanup-expired", SiteID: site.ID, PowSeed: []byte("0123456789abcdef"), PowBits: 12,
		CreatedAt: base.Add(-2 * time.Hour), ExpiresAt: base.Add(-time.Hour)}
	if err := db.InsertChallenge(t.Context(), challenge, store.Caps{MaxActive: 2, MaxActivePerSite: 2}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go purgeLoop(ctx, db, slog.New(slog.NewTextHandler(io.Discard, nil)), done)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, err := db.Challenge(context.Background(), challenge.ID)
		if err == store.ErrNotFound {
			cancel()
			select {
			case <-done:
				return
			case <-time.After(time.Second):
				t.Fatal("purgeLoop did not stop after cancellation")
			}
		}
		if err != nil {
			cancel()
			t.Fatalf("read challenge: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("purgeLoop waited for its long periodic tick instead of cleaning promptly")
}
