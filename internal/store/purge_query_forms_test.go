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
			for _, prefix := range []string{"none", "response", "pool"} {
				t.Run(prefix, func(t *testing.T) {
					tx, err := d.h.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					if prefix != "none" {
						if _, err := tx.ExecContext(t.Context(), `DELETE FROM responses WHERE id IN (SELECT id FROM responses WHERE used=1 OR expires_at<=? LIMIT ?)`, formatTime(now), 500); err != nil {
							t.Fatal(err)
						}
					}
					if prefix == "pool" {
						if _, err := tx.ExecContext(t.Context(), `DELETE FROM pool WHERE id IN (SELECT id FROM pool WHERE status=? UNION ALL SELECT id FROM pool WHERE status=? AND expires_at<=? AND challenge_id IS NULL UNION ALL SELECT p.id FROM pool p JOIN challenges c ON c.id=p.challenge_id WHERE p.status=? AND (c.expires_at<=? OR c.status IN (?,?)) LIMIT ?)`, PoolTerminal, PoolReady, formatTime(now), PoolAssigned, formatTime(now), statusRedeemed, statusInvalid, 500); err != nil {
							t.Fatal(err)
						}
					}
					res, err := tx.ExecContext(t.Context(), tc.sql, formatTime(now), 500)
					if err != nil {
						t.Fatal(err)
					}
					n, err := res.RowsAffected()
					if err != nil || n != 500 {
						t.Fatalf("deleted=%d err=%v", n, err)
					}
				})
			}
		})
	}
}
