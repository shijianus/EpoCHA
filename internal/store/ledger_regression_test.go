package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func ledgerChallenge(id string, siteID int64, now time.Time, ttl time.Duration) *Challenge {
	return &Challenge{ID: id, SiteID: siteID, PowSeed: []byte("0123456789abcdef"), PowBits: 12,
		CreatedAt: now, ExpiresAt: now.Add(ttl)}
}

func ledgerRows(t *testing.T, d *DB) int {
	t.Helper()
	var n int
	if err := d.h.QueryRowContext(t.Context(), `SELECT count(*) FROM challenges`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func ledgerSeedExpired(t *testing.T, d *DB, siteID int64, n int, now time.Time) {
	t.Helper()
	// A bounded, set-based fixture avoids 100000 individual write transactions at the default boundary.
	_, err := d.h.ExecContext(t.Context(), `
WITH RECURSIVE rows(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM rows WHERE n<?)
INSERT INTO challenges(id, site_id, pow_seed, pow_bits, status, attempts, created_at, expires_at)
SELECT printf('history-%06d',n), ?, x'0123456789abcdef', 12, 'issued', 0, ?, ? FROM rows`,
		n, siteID, formatTime(now.Add(-2*time.Minute)), formatTime(now.Add(-time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if got := ledgerRows(t, d); got != n {
		t.Fatalf("fixture rows=%d, want %d", got, n)
	}
}

func TestLedgerTotalCapIncludesExpiredAndTerminalAcrossSites(t *testing.T) {
	for _, state := range []string{"expired-issued", "invalid", "redeemed", "consumed"} {
		t.Run(state, func(t *testing.T) {
			d := newDB(t)
			a, b := mustSite(t, d, "ledger-a"), mustSite(t, d, "ledger-b")
			base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
			caps := Caps{MaxTotal: 1, MaxActive: 10, MaxActivePerSite: 5}
			c := ledgerChallenge("retained", a.ID, base, time.Minute)
			if err := d.InsertChallenge(t.Context(), c, caps); err != nil {
				t.Fatal(err)
			}
			now := base.Add(time.Second)
			switch state {
			case "expired-issued":
				now = c.ExpiresAt
			case "invalid":
				if _, err := d.RedeemAttempt(t.Context(), c.ID, now, time.Minute, false); err != nil {
					t.Fatal(err)
				}
			case "redeemed", "consumed":
				r, err := d.Redeem(t.Context(), c.ID, now, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				if state == "consumed" {
					if _, err := d.ConsumeResponse(t.Context(), r.ID, a.ID, now); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := d.InsertChallenge(t.Context(), ledgerChallenge("over-total", b.ID, now, time.Minute), caps); !errors.Is(err, ErrCapExceeded) {
				t.Errorf("history state=%s must consume the global total slot even across sites; got %v", state, err)
			}
			if got := ledgerRows(t, d); got != 1 {
				t.Errorf("rejected issuance changed ledger rows: got %d, want 1", got)
			}
		})
	}
}

func TestLedgerActiveCapsRemainIndependent(t *testing.T) {
	d := newDB(t)
	a, b := mustSite(t, d, "ledger-active-a"), mustSite(t, d, "ledger-active-b")
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	caps := Caps{MaxTotal: 20, MaxActive: 3, MaxActivePerSite: 2}
	for _, row := range []*Challenge{
		ledgerChallenge("a1", a.ID, base, time.Hour),
		ledgerChallenge("a2", a.ID, base, time.Hour),
	} {
		if err := d.InsertChallenge(t.Context(), row, caps); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.InsertChallenge(t.Context(), ledgerChallenge("site-over", a.ID, base, time.Hour), caps); !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("site active cap lost: %v", err)
	}
	if err := d.InsertChallenge(t.Context(), ledgerChallenge("b1", b.ID, base, time.Hour), caps); err != nil {
		t.Fatalf("other site should still fit: %v", err)
	}
	if err := d.InsertChallenge(t.Context(), ledgerChallenge("global-over", b.ID, base, time.Hour), caps); !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("global active cap lost: %v", err)
	}
	if got := ledgerRows(t, d); got != 3 {
		t.Fatalf("active rejections changed total: %d", got)
	}
	if _, err := d.RedeemAttempt(t.Context(), "a1", base, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertChallenge(t.Context(), ledgerChallenge("after-terminal", a.ID, base, time.Hour), caps); err != nil {
		t.Fatalf("terminal row must free active capacity, not total capacity: %v", err)
	}
	if err := d.InsertChallenge(t.Context(), ledgerChallenge("after-expiry", b.ID, base.Add(time.Hour), time.Hour), caps); err != nil {
		t.Fatalf("expiry must free active capacity while history remains: %v", err)
	}
	if got := ledgerRows(t, d); got != 5 {
		t.Fatalf("active cap tests must retain all history, got %d", got)
	}
}

func TestLedgerZeroTotalUsesExplicitDefaultAndNegativeRejected(t *testing.T) {
	if DefaultMaxLedgerRows != 100000 {
		t.Fatalf("default changed: %d", DefaultMaxLedgerRows)
	}
	d := newDB(t)
	s := mustSite(t, d, "ledger-default")
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	legacy := Caps{MaxActive: 10, MaxActivePerSite: 5}
	negative := legacy
	negative.MaxTotal = -1
	if err := d.InsertChallenge(t.Context(), ledgerChallenge("negative", s.ID, base, time.Minute), negative); !errors.Is(err, ErrBadConfig) {
		t.Errorf("negative total must be invalid, not unlimited: %v", err)
		return
	}
	ledgerSeedExpired(t, d, s.ID, DefaultMaxLedgerRows-1, base)
	if err := d.InsertChallenge(t.Context(), ledgerChallenge("default-last-slot", s.ID, base, time.Minute), legacy); err != nil {
		t.Fatalf("zero total must not inherit the active cap: %v", err)
	}
	if err := d.InsertChallenge(t.Context(), ledgerChallenge("default-overflow", s.ID, base, time.Minute), legacy); !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("omitted MaxTotal must not disable the total bound: %v", err)
	}
	if got := ledgerRows(t, d); got != DefaultMaxLedgerRows {
		t.Fatalf("default boundary rows=%d, want %d", got, DefaultMaxLedgerRows)
	}
}

func TestLedgerAdmissionRecoversOnlyAfterSafePurge(t *testing.T) {
	d := newDB(t)
	s := mustSite(t, d, "ledger-recovery")
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	caps := Caps{MaxTotal: 2, MaxActive: 10, MaxActivePerSite: 5}
	expired := ledgerChallenge("referenced", s.ID, base, time.Minute)
	live := ledgerChallenge("maximum-ttl", s.ID, base, time.Hour)
	for _, c := range []*Challenge{expired, live} {
		if err := d.InsertChallenge(t.Context(), c, caps); err != nil {
			t.Fatal(err)
		}
	}
	r, err := d.Redeem(t.Context(), expired.ID, base.Add(59*time.Second), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := base.Add(2 * time.Minute)
	counts, err := d.Purge(t.Context(), now)
	if err != nil || counts != (PurgeCounts{}) {
		t.Fatalf("purge must keep the live response reference and maximum-TTL challenge: %+v, %v", counts, err)
	}
	if err := d.InsertChallenge(t.Context(), ledgerChallenge("still-full", s.ID, now, time.Minute), caps); !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("valid references must continue to occupy total capacity: %v", err)
	}
	if _, err := d.ConsumeResponse(t.Context(), r.ID, s.ID, now); err != nil {
		t.Fatalf("purge broke a valid response: %v", err)
	}
	counts, err = d.Purge(t.Context(), now)
	if err != nil || counts.Responses != 1 || counts.Challenges != 1 {
		t.Fatalf("safe reclamation: %+v, %v", counts, err)
	}
	if err := d.InsertChallenge(t.Context(), ledgerChallenge("recovered", s.ID, now, time.Minute), caps); err != nil {
		t.Fatalf("admission did not recover after purge: %v", err)
	}
	if _, err := d.ConsumeResponse(t.Context(), r.ID, s.ID, now); !errors.Is(err, ErrConsumed) {
		t.Fatalf("purged response replay: %v", err)
	}
	if _, err := d.Redeem(t.Context(), expired.ID, now, time.Minute); !errors.Is(err, ErrNotRedeemable) {
		t.Fatalf("purged challenge revived: %v", err)
	}
	if _, err := d.Redeem(t.Context(), live.ID, now, time.Minute); err != nil {
		t.Fatalf("maximum-TTL challenge was lost: %v", err)
	}
}

func TestLedgerConcurrentLastSlotAcrossTwoHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	a, _ := newDBAt(t, path)
	b, _ := newDBAt(t, path)
	s := mustSite(t, a, "ledger-race")
	base := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	caps := Caps{MaxTotal: 2, MaxActive: 10, MaxActivePerSite: 5}
	if err := a.InsertChallenge(t.Context(), ledgerChallenge("history", s.ID, base.Add(-time.Minute), time.Second), caps); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, db := range []*DB{a, b} {
		wg.Add(1)
		go func(i int, db *DB) {
			defer wg.Done()
			<-start
			results <- db.InsertChallenge(t.Context(), ledgerChallenge(fmt.Sprintf("contender-%d", i), s.ID, base, time.Minute), caps)
		}(i, db)
	}
	close(start)
	wg.Wait()
	close(results)
	wins, capped := 0, 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrCapExceeded):
			capped++
		default:
			t.Fatalf("last-slot contender must be admitted or capacity-rejected: %v", err)
		}
	}
	if wins != 1 || capped != 1 || ledgerRows(t, a) != 2 {
		t.Fatalf("two-handle last slot: wins=%d capped=%d rows=%d", wins, capped, ledgerRows(t, a))
	}
}

func TestLedgerCapAndPoolClaimRollbackTogether(t *testing.T) {
	d := newDB(t)
	s := questionSite(t, d, "ledger-pool")
	base := time.Now().UTC()
	caps := Caps{MaxTotal: 1, MaxActive: 10, MaxActivePerSite: 5}
	ledgerSeedExpired(t, d, s.ID, 1, base)
	ids := seedQuestions(t, d, testSpec, 1, 2*time.Hour)
	blocked := ledgerChallenge("blocked-claim", s.ID, base, time.Minute)
	if _, err := d.IssueChallengeWithQuestion(t.Context(), blocked, caps, &testSpec); !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("total cap must stop issuance before claiming the ready question: %v", err)
	}
	var status string
	var binding sql.NullString
	if err := d.h.QueryRowContext(t.Context(), `SELECT status, challenge_id FROM pool WHERE id=?`, ids[0]).Scan(&status, &binding); err != nil {
		t.Fatal(err)
	}
	if status != PoolReady || binding.Valid || ledgerRows(t, d) != 1 {
		t.Fatalf("cap rejection left a claim/ledger mutation: status=%s binding=%v rows=%d", status, binding.Valid, ledgerRows(t, d))
	}
	if _, err := d.Purge(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	// A failed pool claim must roll back the provisional challenge and free the final total slot.
	missing := testSpec
	missing.Difficulty++
	if _, err := d.IssueChallengeWithQuestion(t.Context(), ledgerChallenge("empty-bucket", s.ID, base, time.Minute), caps, &missing); !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("empty bucket: %v", err)
	}
	if got := ledgerRows(t, d); got != 0 {
		t.Fatalf("failed pool claim consumed ledger capacity: %d", got)
	}
	q, err := d.IssueChallengeWithQuestion(t.Context(), ledgerChallenge("claim-after-purge", s.ID, base, time.Minute), caps, &testSpec)
	if err != nil || q == nil || q.ID != ids[0] || q.ChallengeID != "claim-after-purge" {
		t.Fatalf("original question and last ledger slot were not recoverable: question=%v err=%v", q != nil, err)
	}
}

func TestLedgerPurgeKeepsPerTransactionBatch500(t *testing.T) {
	d := newDB(t)
	s := mustSite(t, d, "ledger-batch")
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	ledgerSeedExpired(t, d, s.ID, 601, now)
	for _, statement := range []string{
		`INSERT INTO responses(id, challenge_id, site_id, created_at, expires_at, used)
SELECT id, id, site_id, created_at, expires_at, 1 FROM challenges`,
		`INSERT INTO pool(id, generator_id, version, difficulty, status, payload_json, verifier_blob, created_at, expires_at, challenge_id)
SELECT id, 'arithmetic-baseline-v1', 'v1', 2, 'terminal', '{}', x'01', created_at, expires_at, id FROM challenges`,
	} {
		if _, err := d.h.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range []int64{500, 101, 0} {
		got, err := d.Purge(t.Context(), now)
		if err != nil || got != (PurgeCounts{Responses: want, Pool: want, Challenges: want}) {
			t.Fatalf("batch=%d counts=%+v err=%v, want %d per table", i, got, err, want)
		}
	}
}

func TestLedgerCancelledPurgeDoesNotFreeAdmission(t *testing.T) {
	d := newDB(t)
	s := mustSite(t, d, "ledger-cancel")
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	ledgerSeedExpired(t, d, s.ID, 1, now)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := d.Purge(ctx, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled purge: %v", err)
	}
	if err := d.InsertChallenge(t.Context(), ledgerChallenge("cancelled-cap", s.ID, now, time.Minute), Caps{MaxTotal: 1, MaxActive: 10, MaxActivePerSite: 5}); !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("cancelled purge must not appear to release capacity: %v", err)
	}
}
