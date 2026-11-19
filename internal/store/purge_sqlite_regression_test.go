package store

import (
	"fmt"
	"testing"
	"time"
)

func TestPurgeSQLiteFullIndependentOfDiskAndPool(t *testing.T) {
	for _, n := range []int{1, 499, 500, 501, 601} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			d := newDB(t)
			s := mustSite(t, d, "purge-sqlite")
			now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
			ledgerSeedExpired(t, d, s.ID, n, now)
			for _, pragma := range []string{"page_count", "max_page_count", "freelist_count", "temp_store"} {
				var value int64
				if err := d.h.QueryRowContext(t.Context(), "PRAGMA "+pragma).Scan(&value); err != nil {
					t.Fatal(err)
				}
				t.Logf("%s=%d", pragma, value)
			}
			if err := IntegrityCheck(t.Context(), d.path); err != nil {
				t.Fatal(err)
			}
			counts, err := d.Purge(t.Context(), now)
			want := int64(n)
			if want > purgeBatchRows {
				want = purgeBatchRows
			}
			if err != nil || counts != (PurgeCounts{Challenges: want}) {
				t.Fatalf("isolated %d-row ledger: counts=%+v err=%v; want %d; no pool or response rows exist", n, counts, err, want)
			}
			if got := ledgerRows(t, d); got != n-int(want) {
				t.Fatalf("remaining=%d want=%d", got, n-int(want))
			}
		})
	}
}
