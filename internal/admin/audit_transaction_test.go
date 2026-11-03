package admin

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/store"
)

func auditTestDB(t *testing.T, x *h) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(x.dbPath)+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func auditBusinessSnapshot(t *testing.T, db *sql.DB) [][][]string {
	t.Helper()
	queries := []string{
		`SELECT id, name, site_key, enabled, version FROM sites ORDER BY id`,
		`SELECT key_id, site_id, secret_mac, created_at, revoked_at FROM site_secrets ORDER BY key_id`,
		`SELECT site_id, origin, created_at FROM site_origins ORDER BY site_id, origin`,
		`SELECT key_id, label, key_mac, created_at, revoked_at FROM admin_keys ORDER BY key_id`,
	}
	out := make([][][]string, 0, len(queries))
	for _, query := range queries {
		rows, err := db.QueryContext(t.Context(), query)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		table := [][]string{}
		for rows.Next() {
			values := make([]sql.NullString, len(columns))
			dest := make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			row := make([]string, len(columns))
			for i, value := range values {
				if value.Valid {
					row[i] = "value:" + value.String
				} else {
					row[i] = "null"
				}
			}
			table = append(table, row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		out = append(out, table)
	}
	return out
}

func TestCreateSiteInvalidOriginsAtomicHTTP(t *testing.T) {
	tooMany := make([]string, store.MaxOriginsPerSite+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("https://origin%d.example.com", i)
	}
	for name, origins := range map[string][]string{
		"invalid_after_valid": {"https://valid.example.com", "null"},
		"too_many":            tooMany,
	} {
		t.Run(name, func(t *testing.T) {
			x := newHarness(t, nil)
			db := auditTestDB(t, x)
			before := auditBusinessSnapshot(t, db)
			r := x.do(http.MethodPost, "/api/admin/sites", map[string]any{
				"name": "atomic", "site_key": "pk_atomic", "origins": origins,
			})
			if r.status != http.StatusBadRequest || r.got["error"] != CodeBadRequest {
				t.Fatalf("invalid origins: status=%d, code=%v", r.status, r.got["error"])
			}
			if _, ok := r.got["secret"]; ok {
				t.Error("failed creation returned plaintext")
			}
			if after := auditBusinessSnapshot(t, db); !reflect.DeepEqual(after, before) {
				t.Error("invalid origins left a site, credential, or partial origin list")
			}
			x.restart(t)
			if after := auditBusinessSnapshot(t, db); !reflect.DeepEqual(after, before) {
				t.Error("invalid-origin creation persisted after restart")
			}
			r = x.do(http.MethodPost, "/api/admin/sites", map[string]any{
				"name": "atomic", "site_key": "pk_atomic", "origins": []string{"https://valid.example.com"},
			})
			if r.status != http.StatusCreated {
				t.Fatalf("corrected creation must reuse the same name and site_key: status=%d", r.status)
			}
		})
	}
}

func TestCredentialCreationAuditFailureHTTP(t *testing.T) {
	for _, operation := range []string{"site.create", "secret.add", "secret.rotate", "admin_key.add"} {
		t.Run(operation, func(t *testing.T) {
			x := newHarness(t, nil)
			site, first, err := x.db.CreateSite(t.Context(), "existing", "pk_existing", store.DefaultPolicy())
			if err != nil {
				t.Fatal(err)
			}
			_, second, _, err := x.db.RotateSecret(t.Context(), site.ID, time.Hour, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			path := pathSite(site.ID) + "/secrets"
			var body any
			wantStatus := http.StatusCreated
			switch operation {
			case "site.create":
				path = "/api/admin/sites"
				body = map[string]any{"name": "atomic", "site_key": "pk_atomic", "origins": []string{
					"https://APP.example.com:443", "https://app.example.com", "http://localhost:8080",
				}}
			case "secret.rotate":
				path += "/rotate"
				body = map[string]any{"transition_sec": 0}
				wantStatus = http.StatusOK
			case "admin_key.add":
				path = "/api/admin/admin-keys"
				body = map[string]any{"label": "atomic"}
			}
			db := auditTestDB(t, x)
			before := auditBusinessSnapshot(t, db)
			if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER reject_success_audit BEFORE INSERT ON audit
WHEN NEW.success=1 BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END`); err != nil {
				t.Fatal(err)
			}
			r := x.do(http.MethodPost, path, body)
			if r.status != http.StatusInternalServerError || r.got["error"] != CodeInternal {
				t.Fatalf("audit failure: status=%d, code=%v", r.status, r.got["error"])
			}
			for _, field := range []string{"secret", "admin_key"} {
				if _, ok := r.got[field]; ok {
					t.Errorf("audit failure returned %s plaintext", field)
				}
			}
			check := func() {
				t.Helper()
				if after := auditBusinessSnapshot(t, db); !reflect.DeepEqual(after, before) {
					t.Error("audit failure committed business changes or changed an old credential cutoff")
				}
				for i, plain := range []string{first, second} {
					if _, err := x.db.FindSiteBySecret(t.Context(), plain, time.Now().UTC()); err != nil {
						t.Errorf("failed operation invalidated old credential %d: %v", i, err)
					}
				}
			}
			check()
			x.restart(t)
			check()
			if _, err := db.ExecContext(t.Context(), `DROP TRIGGER reject_success_audit`); err != nil {
				t.Fatal(err)
			}
			r = x.do(http.MethodPost, path, body)
			if r.status != wantStatus {
				t.Fatalf("retry after confirmed rollback: status=%d, want=%d", r.status, wantStatus)
			}
			plainField := "secret"
			if operation == "admin_key.add" {
				plainField = "admin_key"
			}
			plain, _ := r.got[plainField].(string)
			if plain == "" {
				t.Fatal("committed response did not return the new credential")
			}
			var target string
			switch operation {
			case "site.create":
				id := siteID(r)
				target = "site:" + itoa(id)
				origins, err := x.db.Origins(t.Context(), id)
				if err != nil || !reflect.DeepEqual(origins, []string{"http://localhost:8080", "https://app.example.com"}) {
					t.Fatalf("creation lost normalized origins: %v, %v", origins, err)
				}
			case "secret.add", "secret.rotate":
				target = "site:" + itoa(site.ID) + "/" + r.got["key"].(map[string]any)["key_id"].(string)
			case "admin_key.add":
				target = "admin_key:" + r.got["key"].(map[string]any)["key_id"].(string)
			}
			if operation == "admin_key.add" {
				if _, err := x.db.MatchAdminKey(t.Context(), plain, time.Now().UTC()); err != nil {
					t.Fatalf("new admin credential rejected: %v", err)
				}
			} else if _, err := x.db.FindSiteBySecret(t.Context(), plain, time.Now().UTC()); err != nil {
				t.Fatalf("new site credential rejected: %v", err)
			}
			entries, err := x.db.ListAudit(t.Context(), 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			actorID, err := x.db.MatchAdminKey(t.Context(), sentinelAdminKey, time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			successes := 0
			for _, entry := range entries {
				if entry.Success {
					successes++
					if entry.Action != operation || entry.Target != target || entry.Actor != actorID {
						t.Errorf("incorrect success audit: %+v", entry)
					}
				}
			}
			if successes != 1 {
				t.Errorf("one committed operation must have exactly one success audit, got %d", successes)
			}
			auditJSON, err := json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(x.logs.String(), plain) || strings.Contains(string(auditJSON), plain) {
				t.Error("audit or logs contained plaintext")
			}
		})
	}
}

func TestEmergencyTighteningSurvivesAuditFailureHTTP(t *testing.T) {
	for _, operation := range []string{"site.disable", "site.revoke", "secret.revoke", "admin_key.revoke"} {
		t.Run(operation, func(t *testing.T) {
			x := newHarness(t, nil)
			site, plain, err := x.db.CreateSite(t.Context(), "existing", "pk_existing", store.DefaultPolicy())
			if err != nil {
				t.Fatal(err)
			}
			method, path := http.MethodPost, pathSite(site.ID)+"/disable"
			switch operation {
			case "site.revoke":
				method, path = http.MethodDelete, pathSite(site.ID)
			case "secret.revoke":
				keys, err := x.db.ListSecrets(t.Context(), site.ID)
				if err != nil || len(keys) != 1 {
					t.Fatalf("initial credentials: count=%d, err=%v", len(keys), err)
				}
				path = pathSite(site.ID) + "/secrets/" + keys[0].KeyID + "/revoke"
			case "admin_key.revoke":
				key, keyPlain, err := x.db.NewAdminKeyWithID(t.Context(), "emergency")
				if err != nil {
					t.Fatal(err)
				}
				plain = keyPlain
				path = "/api/admin/admin-keys/" + key.KeyID + "/revoke"
			}
			db := auditTestDB(t, x)
			if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER reject_audit BEFORE INSERT ON audit
BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END`); err != nil {
				t.Fatal(err)
			}
			r := x.do(method, path, nil)
			if r.status != http.StatusInternalServerError || r.got["error"] != CodeInternal {
				t.Fatalf("audit failure: status=%d, code=%v", r.status, r.got["error"])
			}
			check := func() {
				t.Helper()
				switch operation {
				case "site.disable", "site.revoke":
					got, err := x.db.SiteByID(t.Context(), site.ID)
					if err != nil || got.Enabled || got.Version != site.Version+1 {
						t.Fatalf("audit failure rolled back site tightening: %+v, %v", got, err)
					}
				case "secret.revoke":
					if _, err := x.db.FindSiteBySecret(t.Context(), plain, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("audit failure revived site credential: %v", err)
					}
				case "admin_key.revoke":
					if _, err := x.db.MatchAdminKey(t.Context(), plain, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("audit failure revived admin credential: %v", err)
					}
				}
			}
			check()
			x.restart(t)
			check()
		})
	}
}

type disconnectedAuditResponse struct {
	header http.Header
	status int
	writes int
}

func (w *disconnectedAuditResponse) Header() http.Header    { return w.header }
func (w *disconnectedAuditResponse) WriteHeader(status int) { w.status = status }
func (w *disconnectedAuditResponse) Write([]byte) (int, error) {
	w.writes++
	return 0, errors.New("injected client disconnect")
}

func TestCredentialCommitSurvivesResponseFailureHTTP(t *testing.T) {
	x := newHarness(t, nil)
	site, first, err := x.db.CreateSite(t.Context(), "existing", "pk_existing", store.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, pathSite(site.ID)+"/secrets/rotate", strings.NewReader(`{"transition_sec":0}`))
	r.Header.Set("Authorization", "Bearer "+sentinelAdminKey)
	w := &disconnectedAuditResponse{header: http.Header{}}
	x.srv.Handler().ServeHTTP(w, r)
	if w.status != http.StatusOK || w.writes != 1 {
		t.Fatalf("expected one failed response write after success: status=%d, writes=%d", w.status, w.writes)
	}
	x.restart(t)
	keys, err := x.db.ListSecrets(t.Context(), site.ID)
	if err != nil || len(keys) != 2 {
		t.Fatalf("committed credential missing after disconnect: count=%d, err=%v", len(keys), err)
	}
	if _, err := x.db.FindSiteBySecret(t.Context(), first, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("disconnect rolled back committed rotation: %v", err)
	}
	entries, err := x.db.ListAudit(t.Context(), 0, 100)
	if err != nil || len(entries) != 1 || !entries[0].Success || entries[0].Action != "secret.rotate" {
		t.Fatalf("committed rotation audit missing after disconnect: count=%d, err=%v", len(entries), err)
	}
}
