package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAccessibilityQuotaRollbackAndCommit(t *testing.T) {
	for _, failure := range []string{"pool_empty", "prepare", "panic", "cancel", "commit"} {
		t.Run(failure, func(t *testing.T) {
			d := newDB(t)
			site := questionSite(t, d, "quota")
			if failure != "pool_empty" {
				seedQuestions(t, d, testSpec, 2, 48*time.Hour)
			}
			if failure == "commit" {
				_, err := d.h.ExecContext(t.Context(), `CREATE TABLE quota_commit_failure(site_id INTEGER REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED);
CREATE TRIGGER quota_commit_failure_trigger AFTER INSERT ON challenges
BEGIN INSERT INTO quota_commit_failure(site_id) VALUES(-1); END`)
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			prepare := func(*PoolQuestion) error {
				switch failure {
				case "prepare":
					return errors.New("injected preparation failure")
				case "panic":
					panic("injected preparation panic")
				case "cancel":
					cancel()
				}
				return nil
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil && failure != "panic" {
						t.Fatalf("unexpected panic: %v", rec)
					}
				}()
				if _, err := d.IssueChallengePrepared(ctx, newChallengeRow(site.ID, time.Minute), testCaps, &testSpec, "source", prepare); err == nil {
					t.Fatal("injected issue failure returned nil")
				}
			}()
			if failure == "commit" {
				if _, err := d.h.ExecContext(t.Context(), `DROP TRIGGER quota_commit_failure_trigger`); err != nil {
					t.Fatal(err)
				}
			}
			other, _ := newDBAt(t, d.path)
			for _, table := range []string{"accessibility_quotas", "challenges"} {
				var n int
				if err := other.h.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
					t.Fatalf("failed issue left %s rows=%d err=%v", table, n, err)
				}
			}
			if failure == "pool_empty" {
				seedQuestions(t, d, testSpec, 1, 48*time.Hour)
			}
			if _, err := other.IssueChallengePrepared(t.Context(), newChallengeRow(site.ID, time.Minute), testCaps, &testSpec, "source", nil); err != nil {
				t.Fatalf("retry after %s: %v", failure, err)
			}
			if _, err := d.IssueChallengePrepared(t.Context(), newChallengeRow(site.ID, time.Minute), testCaps, &testSpec, "source", nil); !errors.Is(err, ErrAccessibilityQuota) {
				t.Fatalf("committed quota lost across handles: %v", err)
			}
		})
	}
}

func TestAccessibilityQuotaCapacityPrivacyExpiryAndRecovery(t *testing.T) {
	d := newDB(t)
	site := questionSite(t, d, "quota-capacity")
	seedQuestions(t, d, testSpec, 4, 72*time.Hour)
	now := time.Now().UTC()
	tx, err := d.h.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxAccessibilityQuotas; i++ {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO accessibility_quotas(site_id,source_mac,expires_at) VALUES(?,?,?)`, site.ID,
			d.accessibilitySourceMAC(fmt.Sprint(i)), formatTime(now.Add(AccessibilityQuotaWindow))); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	c := newChallengeRow(site.ID, time.Minute)
	c.CreatedAt, c.ExpiresAt = now, now.Add(time.Minute)
	if _, err := d.IssueChallengePrepared(t.Context(), c, testCaps, &testSpec, "new-source", nil); !errors.Is(err, ErrAccessibilityQuota) {
		t.Fatalf("capacity must refuse without evicting quotas: %v", err)
	}
	if retry, err := d.AccessibilityRetryAfter(t.Context(), site.ID, "new-source", now); err != nil || retry != AccessibilityQuotaWindow {
		t.Fatalf("capacity retry=%v err=%v", retry, err)
	}
	c = newChallengeRow(site.ID, time.Minute)
	c.CreatedAt = now.Add(AccessibilityQuotaWindow)
	c.ExpiresAt = c.CreatedAt.Add(time.Minute)
	const sentinel = "SOURCE-PRIVACY-SENTINEL"
	if _, err := d.IssueChallengePrepared(t.Context(), c, testCaps, &testSpec, sentinel, nil); err != nil {
		t.Fatalf("expired quotas not released: %v", err)
	}
	var n int
	var raw []byte
	if err := d.h.QueryRowContext(t.Context(), `SELECT count(*), source_mac FROM accessibility_quotas`).Scan(&n, &raw); err != nil || n != 1 || len(raw) != 32 || strings.Contains(string(raw), sentinel) {
		t.Fatalf("quota storage bounds/privacy n=%d bytes=%d err=%v", n, len(raw), err)
	}
	if err := d.transform(t.Context(), strings.Repeat("r", 32), c.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if err := d.h.QueryRowContext(t.Context(), `SELECT count(*) FROM accessibility_quotas`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("restore retained quota: %d %v", n, err)
	}
}

func TestAccessibilityQuotaIndependentHandlesConcurrent(t *testing.T) {
	d := newDB(t)
	other, _ := newDBAt(t, d.path)
	site := questionSite(t, d, "quota-race")
	seedQuestions(t, d, testSpec, 16, time.Hour)
	start := make(chan struct{})
	results := make(chan error, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		handle := d
		if i%2 == 1 {
			handle = other
		}
		c := newChallengeRow(site.ID, time.Minute)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := handle.IssueChallengePrepared(t.Context(), c, testCaps, &testSpec, "shared-source", nil)
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
		} else if !errors.Is(err, ErrAccessibilityQuota) {
			t.Errorf("competing quota issue: %v", err)
		}
	}
	if success != 1 {
		t.Fatalf("issued=%d want 1", success)
	}
}
