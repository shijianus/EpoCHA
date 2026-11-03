package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type auditedCreationState struct {
	Sites     []*Site
	Secrets   [][]*SiteSecret
	Origins   [][]string
	AdminKeys []*AdminKey
	Audit     []*AuditEntry
}

func auditedCreationSnapshot(t *testing.T, d *DB) auditedCreationState {
	t.Helper()
	var state auditedCreationState
	var err error
	if state.Sites, err = d.ListSites(t.Context(), 0, 500); err != nil {
		t.Fatal(err)
	}
	for _, site := range state.Sites {
		keys, err := d.ListSecrets(t.Context(), site.ID)
		if err != nil {
			t.Fatal(err)
		}
		origins, err := d.Origins(t.Context(), site.ID)
		if err != nil {
			t.Fatal(err)
		}
		state.Secrets = append(state.Secrets, keys)
		state.Origins = append(state.Origins, origins)
	}
	if state.AdminKeys, err = d.ListAdminKeys(t.Context()); err != nil {
		t.Fatal(err)
	}
	if state.Audit, err = d.ListAudit(t.Context(), 0, 500); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestAuditedCredentialRollback(t *testing.T) {
	for _, operation := range []string{"site.create", "secret.add", "secret.rotate", "admin_key.add"} {
		for _, failure := range []string{"audit_insert", "commit", "invalid_actor"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				d := newDB(t)
				site, first, err := d.CreateSite(t.Context(), "existing", "pk_existing", DefaultPolicy())
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				_, second, _, err := d.RotateSecret(t.Context(), site.ID, time.Hour, now)
				if err != nil {
					t.Fatal(err)
				}
				actorKey, _, err := d.NewAdminKeyWithID(t.Context(), "audited-actor")
				if err != nil {
					t.Fatal(err)
				}
				before := auditedCreationSnapshot(t, d)
				actor := actorKey.KeyID

				var injection string
				switch failure {
				case "audit_insert":
					injection = `CREATE TRIGGER reject_audit BEFORE INSERT ON audit
BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END`
				case "commit":
					injection = `CREATE TABLE audit_commit_failure (
site_id INTEGER REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED);
CREATE TRIGGER reject_audit_commit AFTER INSERT ON audit
BEGIN INSERT INTO audit_commit_failure(site_id) VALUES(-1); END`
				case "invalid_actor":
					actor = ""
				}
				if injection != "" {
					if _, err := d.h.ExecContext(t.Context(), injection); err != nil {
						t.Fatal(err)
					}
				}
				var plain string
				returnedMetadata := false
				switch operation {
				case "site.create":
					var created *Site
					created, plain, err = d.CreateSiteAudited(t.Context(), "new", "pk_new", DefaultPolicy(),
						[]string{"https://app.example.com"}, actor)
					returnedMetadata = created != nil
				case "secret.add":
					var key *SiteSecret
					key, plain, err = d.AddSecretAudited(t.Context(), site.ID, actor)
					returnedMetadata = key != nil
				case "secret.rotate":
					var key *SiteSecret
					var retired []*SiteSecret
					key, plain, retired, err = d.RotateSecretAudited(t.Context(), site.ID, 0, now.Add(time.Minute), actor)
					returnedMetadata = key != nil || retired != nil
				case "admin_key.add":
					var key *AdminKey
					key, plain, err = d.NewAdminKeyWithIDAudited(t.Context(), "new", actor)
					returnedMetadata = key != nil
				}
				if err == nil || plain != "" || returnedMetadata {
					t.Fatalf("failed operation returned success or credential material: err=%v metadata=%v plaintext=%v", err, returnedMetadata, plain != "")
				}
				if failure == "invalid_actor" && !errors.Is(err, ErrBadConfig) {
					t.Errorf("invalid actor: got %v, want ErrBadConfig", err)
				}
				if failure == "commit" && !strings.Contains(err.Error(), "提交") {
					t.Errorf("deferred constraint must fail at commit, got %v", err)
				}
				check := func(db *DB) {
					t.Helper()
					if after := auditedCreationSnapshot(t, db); !reflect.DeepEqual(after, before) {
						t.Error("failed transaction changed persisted business state or audit")
					}
					for i, old := range []string{first, second} {
						if _, err := db.FindSiteBySecret(t.Context(), old, now.Add(time.Minute)); err != nil {
							t.Errorf("failed operation invalidated old credential %d: %v", i, err)
						}
					}
				}
				// 另一个句柄及重开库只确认已提交状态，不能用失败事务自己的可见性冒充回滚。
				other, _ := newDBAt(t, d.path)
				check(other)
				if err := other.Close(); err != nil {
					t.Fatal(err)
				}
				if err := d.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, _ := newDBAt(t, d.path)
				check(reopened)
			})
		}
	}
}

func TestCreateSiteAuditedOriginWriteRollback(t *testing.T) {
	d := newDB(t)
	before := auditedCreationSnapshot(t, d)
	if _, err := d.h.ExecContext(t.Context(), `CREATE TRIGGER reject_second_origin BEFORE INSERT ON site_origins
WHEN NEW.origin='https://second.example.com'
BEGIN SELECT RAISE(ABORT, 'injected origin failure'); END`); err != nil {
		t.Fatal(err)
	}
	site, plain, err := d.CreateSiteAudited(t.Context(), "origin", "pk_origin", DefaultPolicy(),
		[]string{"https://first.example.com", "https://second.example.com"}, "k-audited-regression")
	if err == nil || site != nil || plain != "" {
		t.Fatalf("origin failure returned success or credential material: %v", err)
	}
	other, _ := newDBAt(t, d.path)
	if after := auditedCreationSnapshot(t, other); !reflect.DeepEqual(after, before) {
		t.Error("failed second origin left a site, credential, first origin, or success audit")
	}
}

func TestAuditedCreationReadFailureRollback(t *testing.T) {
	for _, operation := range []string{"site.create", "admin_key.add"} {
		t.Run(operation, func(t *testing.T) {
			d := newDB(t)
			before := auditedCreationSnapshot(t, d)
			injection := `CREATE TRIGGER corrupt_new_site AFTER INSERT ON sites
BEGIN UPDATE sites SET created_at='unparseable' WHERE id=NEW.id; END`
			if operation == "admin_key.add" {
				injection = `CREATE TRIGGER corrupt_new_admin AFTER INSERT ON admin_keys
BEGIN UPDATE admin_keys SET created_at='unparseable' WHERE key_id=NEW.key_id; END`
			}
			if _, err := d.h.ExecContext(t.Context(), injection); err != nil {
				t.Fatal(err)
			}
			var err error
			if operation == "site.create" {
				var site *Site
				var plain string
				site, plain, err = d.CreateSiteAudited(t.Context(), "new", "pk_new", DefaultPolicy(), nil, "k-audited-regression")
				if site != nil || plain != "" {
					t.Error("failed read returned site or plaintext")
				}
			} else {
				var key *AdminKey
				var plain string
				key, plain, err = d.NewAdminKeyWithIDAudited(t.Context(), "new", "k-audited-regression")
				if key != nil || plain != "" {
					t.Error("failed read returned key or plaintext")
				}
			}
			if err == nil {
				t.Fatal("expected invalid persisted timestamp to fail response read")
			}
			other, _ := newDBAt(t, d.path)
			if after := auditedCreationSnapshot(t, other); !reflect.DeepEqual(after, before) {
				t.Error("failed read committed business state or success audit")
			}
		})
	}
}
