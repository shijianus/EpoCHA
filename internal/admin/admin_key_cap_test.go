package admin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/shijianus/EpoCHA/internal/config"
)

func TestP0AdminKeyHistoryHTTPBoundary(t *testing.T) {
	x := newHarness(t, nil)
	db, err := sql.Open("sqlite", x.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.ExecContext(t.Context(), `
WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < ?)
INSERT INTO admin_keys(key_id,label,key_mac,created_at,revoked_at)
SELECT printf('history-%04d',n),printf('history-%04d',n),printf('%064x',n),?,? FROM seq`,
		255, "2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	r := x.do(http.MethodPost, "/api/admin/admin-keys", map[string]any{"label": "overflow"})
	if r.status != http.StatusConflict || r.got["admin_key"] != nil {
		t.Errorf("history cap HTTP status=%d returned_plaintext=%v", r.status, r.got["admin_key"] != nil)
	}
	r = x.do(http.MethodGet, "/api/admin/admin-keys", nil)
	keys, ok := r.got["admin_keys"].([]any)
	if r.status != http.StatusOK || !ok || len(keys) != 256 {
		t.Errorf("bounded list status=%d count=%d", r.status, len(keys))
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO admin_keys(key_id,label,key_mac,created_at,revoked_at) VALUES(?,?,?,?,?)`,
		"flood", "flood", "flood", "2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	r = x.do(http.MethodGet, "/api/admin/admin-keys", nil)
	if r.status != http.StatusConflict || r.got["admin_keys"] != nil {
		t.Fatalf("overfull list not rejected: status=%d", r.status)
	}
}

func TestP0ProductionAdminRejectsUnrestrictedRegistry(t *testing.T) {
	x := newHarness(t, nil)
	cfg := &config.Config{RateMaxState: 4096}
	if err := json.Unmarshal([]byte(`{"ProductionProfile":true}`), cfg); err != nil {
		t.Fatal(err)
	}
	if srv, err := New(cfg, x.db, nil, nil, x.gens); srv != nil || err == nil {
		t.Fatal("production admin admitted experimental registry")
	}
}
