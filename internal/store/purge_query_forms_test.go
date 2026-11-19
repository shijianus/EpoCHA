package store

import (
	"testing"
	"time"
)

func TestPurgeSQLiteDeleteQueryForms(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"text-id-in", `DELETE FROM challenges WHERE id IN (SELECT id FROM challenges WHERE expires_at<=? AND NOT EXISTS (SELECT 1 FROM responses r WHERE r.challenge_id=challenges.id) AND NOT EXISTS (SELECT 1 FROM pool p WHERE p.challenge_id=challenges.id) LIMIT ?)`},
		{"rowid-in", `DELETE FROM challenges WHERE rowid IN (SELECT c.rowid FROM challenges c WHERE expires_at<=? AND NOT EXISTS (SELECT 1 FROM responses r WHERE r.challenge_id=c.id) AND NOT EXISTS (SELECT 1 FROM pool p WHERE p.challenge_id=c.id) LIMIT ?)`},
		{"materialized-text-id", `WITH eligible AS MATERIALIZED (SELECT id FROM challenges WHERE expires_at<=? AND NOT EXISTS (SELECT 1 FROM responses r WHERE r.challenge_id=challenges.id) AND NOT EXISTS (SELECT 1 FROM pool p WHERE p.challenge_id=challenges.id) LIMIT ?) DELETE FROM challenges WHERE id IN (SELECT id FROM eligible)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDB(t)
			s := mustSite(t, d, "query-forms")
			now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
			ledgerSeedExpired(t, d, s.ID, 601, now)
			res, err := d.h.ExecContext(t.Context(), tc.sql, formatTime(now), 500)
			if err != nil {
				t.Fatal(err)
			}
			n, err := res.RowsAffected()
			if err != nil || n != 500 {
				t.Fatalf("deleted=%d err=%v", n, err)
			}
		})
	}
}
