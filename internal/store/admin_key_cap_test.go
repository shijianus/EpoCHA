package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const p0AdminHistoryLimit = 256

func seedP0AdminHistory(t *testing.T, d *DB, n int) {
	t.Helper()
	_, err := d.h.ExecContext(t.Context(), `
WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < ?)
INSERT INTO admin_keys(key_id,label,key_mac,created_at,revoked_at)
SELECT printf('history-%04d',n),printf('history-%04d',n),printf('%064x',n),?,? FROM seq`,
		n, formatTime(time.Unix(100, 0)), formatTime(time.Unix(200, 0)))
	if err != nil {
		t.Fatal(err)
	}
}

func TestP0AdminKeyHistoryBoundary(t *testing.T) {
	d := newDB(t)
	seedP0AdminHistory(t, d, p0AdminHistoryLimit-1)
	a, _, err := d.NewAdminKeyWithID(t.Context(), "last-slot")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := d.ListAdminKeys(t.Context())
	if err != nil || len(keys) != p0AdminHistoryLimit {
		t.Fatalf("boundary list: len=%d error=%v", len(keys), err)
	}
	for i := 0; i < p0AdminHistoryLimit-1; i++ {
		if keys[i].KeyID != fmt.Sprintf("history-%04d", i+1) {
			t.Fatalf("non-deterministic ordering at %d", i)
		}
	}
	key, plain, err := d.NewAdminKeyWithIDAudited(t.Context(), "overflow", a.KeyID)
	if key != nil || plain != "" || !errors.Is(err, ErrConflict) {
		t.Errorf("history cap not enforced: key=%v plaintext=%v error=%v", key != nil, plain != "", err)
	}
	var rows, audits int
	if err := d.h.QueryRowContext(t.Context(), `SELECT count(*) FROM admin_keys`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := d.h.QueryRowContext(t.Context(), `SELECT count(*) FROM audit WHERE action='admin_key.add' AND success=1`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if rows != p0AdminHistoryLimit || audits != 0 {
		t.Errorf("rejected write changed state: rows=%d successful_audits=%d", rows, audits)
	}
}

func TestP0AdminKeyFloodedListFailsClosed(t *testing.T) {
	d := newDB(t)
	seedP0AdminHistory(t, d, p0AdminHistoryLimit+1)
	keys, err := d.ListAdminKeys(t.Context())
	if keys != nil || !errors.Is(err, ErrConflict) {
		t.Fatalf("oversized history exposed: len=%d error=%v", len(keys), err)
	}
	if _, _, err := d.NewAdminKeyWithID(t.Context(), "already-over-limit"); !errors.Is(err, ErrConflict) {
		t.Fatalf("over-limit database accepted write: %v", err)
	}
}

func TestP0AdminKeyConcurrentIndependentHandles(t *testing.T) {
	d, path := newDBAt(t, filepath.Join(t.TempDir(), "shared.db"))
	other, _ := newDBAt(t, path)
	seedP0AdminHistory(t, d, p0AdminHistoryLimit-1)
	start := make(chan struct{})
	results := make(chan error, 16)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			db := []*DB{d, other}[i%2]
			_, _, err := db.NewAdminKeyWithID(t.Context(), fmt.Sprintf("racer-%d", i))
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrConflict) {
			t.Errorf("unexpected concurrent error: %v", err)
		}
	}
	var rows int
	if err := d.h.QueryRowContext(t.Context(), `SELECT count(*) FROM admin_keys`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if success != 1 || rows != p0AdminHistoryLimit {
		t.Fatalf("last slot: successes=%d rows=%d", success, rows)
	}
}

func TestP0AdminKeyRecoveryCannotBypassHistoryCap(t *testing.T) {
	d := newDB(t)
	seedP0AdminHistory(t, d, p0AdminHistoryLimit-1)
	a, _, err := d.NewAdminKeyWithID(t.Context(), "live-before-recovery")
	if err != nil {
		t.Fatal(err)
	}
	report, err := d.RecoverAdminKey(t.Context(), strings.Repeat("recovery-test-", 4), time.Now().UTC())
	if !errors.Is(err, ErrConflict) || (report != nil && (report.Committed || report.Adopted)) {
		t.Errorf("recovery bypassed cap: error=%v", err)
	}
	current, err := d.AdminKeyByKey(t.Context(), a.KeyID)
	if err != nil || !current.RevokedAt.IsZero() {
		t.Errorf("rejected recovery revoked live credential: error=%v", err)
	}
}
