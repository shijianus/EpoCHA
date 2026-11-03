package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/shijianus/EpoCHA/internal/store"
)

func TestLedgerPurgeScheduleKeepsUpWith600PerMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "ledger.db"), bytes.Repeat([]byte("p"), 32))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		sites := make([]*store.Site, 2)
		for i := range sites {
			sites[i], _, err = db.CreateSite(t.Context(), fmt.Sprintf("ledger-site-%d", i), fmt.Sprintf("ledger-key-%d", i), store.SitePolicy{
				PowBits: 12, RateSiteRPM: 300, ChallengeTTL: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go purgeLoop(ctx, db, slog.New(slog.NewTextHandler(io.Discard, nil)), done)
		defer func() { cancel(); <-done }()
		synctest.Wait()

		caps := store.Caps{MaxTotal: 1200, MaxActive: 1000, MaxActivePerSite: 500}
		const rounds = 4
		for minute := range rounds {
			for i := range 600 {
				now := time.Now().UTC()
				c := &store.Challenge{ID: fmt.Sprintf("round-%d-%03d", minute, i), SiteID: sites[i%2].ID,
					PowSeed: []byte("0123456789abcdef"), PowBits: 12, CreatedAt: now, ExpiresAt: now.Add(time.Second)}
				if err := db.InsertChallenge(t.Context(), c, caps); err != nil {
					t.Fatalf("minute %d legitimate admission %d: %v", minute, i, err)
				}
				if i%3 == 0 {
					r, err := db.Redeem(t.Context(), c.ID, now, time.Minute)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := db.ConsumeResponse(t.Context(), r.ID, c.SiteID, now); err != nil {
						t.Fatal(err)
					}
				}
			}
			time.Sleep(time.Minute + time.Second)
			synctest.Wait()
			remaining := 0
			for _, state := range []string{"issued", "redeemed", "invalid"} {
				n, err := db.CountChallenges(t.Context(), state)
				if err != nil {
					t.Fatal(err)
				}
				remaining += n
			}
			if remaining != 0 {
				t.Errorf("minute %d: legal 600/min left %d expired/terminal rows after scheduled cleanup", minute+1, remaining)
			}
			t.Logf("fake minute=%d issued=600 abandoned=400 consumed=200 remaining=%d", minute+1, remaining)
		}
	})
}

func TestLedgerPurgeRoundHasFiniteBatchesAndYields(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		calls := 0
		counts, err := purgeLedgerRound(t.Context(), started, func(ctx context.Context, now time.Time) (store.PurgeCounts, error) {
			calls++
			if !now.Equal(started) {
				t.Errorf("one round must use one expiration cutoff: %v != %v", now, started)
			}
			deadline, ok := ctx.Deadline()
			if !ok || !deadline.Equal(started.Add(purgeBudget)) {
				t.Errorf("batch lacks the round deadline: deadline=%v ok=%t", deadline, ok)
			}
			if want := time.Duration(calls-1) * purgeYield; time.Since(started) != want {
				t.Errorf("batch %d elapsed=%s, want cancellable yield total %s", calls, time.Since(started), want)
			}
			return store.PurgeCounts{Responses: 500, Pool: 500, Challenges: 500}, nil
		})
		if err != nil || calls != purgeMaxBatches || counts != (store.PurgeCounts{Responses: 500 * purgeMaxBatches, Pool: 500 * purgeMaxBatches, Challenges: 500 * purgeMaxBatches}) {
			t.Fatalf("finite round: calls=%d counts=%+v err=%v", calls, counts, err)
		}
		if purgeEvery != 10*time.Second || purgeMaxBatches != 8 || purgeBudget != 250*time.Millisecond || purgeYield != 5*time.Millisecond {
			t.Fatalf("scheduler contract changed: every=%s batches=%d budget=%s yield=%s", purgeEvery, purgeMaxBatches, purgeBudget, purgeYield)
		}
	})
}

func TestLedgerPurgeRoundStopsOnEmptyAndError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stopErr error
	}{
		{"empty", nil},
		{"failed-batch", errors.New("synthetic purge failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				counts, err := purgeLedgerRound(t.Context(), time.Now(), func(context.Context, time.Time) (store.PurgeCounts, error) {
					calls++
					if calls == 1 {
						return store.PurgeCounts{Challenges: 37}, nil
					}
					if tc.stopErr != nil {
						// A failed transaction's provisional counts must not be reported as deleted.
						return store.PurgeCounts{Responses: 500, Challenges: 500}, tc.stopErr
					}
					return store.PurgeCounts{}, nil
				})
				if calls != 2 || counts != (store.PurgeCounts{Challenges: 37}) || !errors.Is(err, tc.stopErr) {
					t.Fatalf("early stop: calls=%d counts=%+v err=%v", calls, counts, err)
				}
			})
		})
	}
}

func TestLedgerPurgeRoundHonorsBudgetAndCancellation(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			calls := 0
			counts, err := purgeLedgerRound(t.Context(), start, func(ctx context.Context, _ time.Time) (store.PurgeCounts, error) {
				calls++
				if calls == 1 {
					return store.PurgeCounts{Challenges: 500}, nil
				}
				<-ctx.Done()
				return store.PurgeCounts{Challenges: 500}, ctx.Err()
			})
			if !errors.Is(err, context.DeadlineExceeded) || calls != 2 || counts.Challenges != 500 || time.Since(start) != purgeBudget {
				t.Fatalf("deadline: calls=%d counts=%+v err=%v elapsed=%s", calls, counts, err, time.Since(start))
			}
		})
	})
	t.Run("cancel-during-yield", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			start := time.Now()
			cancellationDone := make(chan struct{})
			go func() { defer close(cancellationDone); time.Sleep(purgeYield / 2); cancel() }()
			defer func() { <-cancellationDone }()
			calls := 0
			counts, err := purgeLedgerRound(ctx, start, func(context.Context, time.Time) (store.PurgeCounts, error) {
				calls++
				return store.PurgeCounts{Challenges: 500}, nil
			})
			if !errors.Is(err, context.Canceled) || calls != 1 || counts.Challenges != 500 || time.Since(start) != purgeYield/2 {
				t.Fatalf("yield cancellation: calls=%d counts=%+v err=%v elapsed=%s", calls, counts, err, time.Since(start))
			}
		})
	})
	t.Run("already-cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		calls := 0
		_, err := purgeLedgerRound(ctx, time.Now(), func(context.Context, time.Time) (store.PurgeCounts, error) {
			calls++
			return store.PurgeCounts{}, nil
		})
		if !errors.Is(err, context.Canceled) || calls != 0 {
			t.Fatalf("pre-cancelled round: calls=%d err=%v", calls, err)
		}
	})
}

func TestLedgerPurgeLoopCancellationBeforeFirstTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go purgeLoop(ctx, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), done)
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("cleanup scheduler did not stop before first tick")
		}
	})
}
