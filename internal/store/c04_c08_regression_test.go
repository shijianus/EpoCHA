package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestC04AuditedCredentialWritesRecheckActor(t *testing.T) {
	for _, operation := range []string{"site.create", "secret.add", "secret.rotate", "admin_key.add"} {
		t.Run(operation, func(t *testing.T) {
			d := newDB(t)
			actorKey, _, err := d.NewAdminKeyWithID(t.Context(), "actor")
			if err != nil {
				t.Fatal(err)
			}
			site, _, err := d.CreateSite(t.Context(), "existing", "pk_existing", DefaultPolicy())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.RevokeAdminKey(t.Context(), actorKey.KeyID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			before := auditedCreationSnapshot(t, d)

			var (
				plain    string
				material bool
			)
			switch operation {
			case "site.create":
				var created *Site
				created, plain, err = d.CreateSiteAudited(t.Context(), "new", "pk_new", DefaultPolicy(), nil, actorKey.KeyID)
				material = created != nil || plain != ""
			case "secret.add":
				var key *SiteSecret
				key, plain, err = d.AddSecretAudited(t.Context(), site.ID, actorKey.KeyID)
				material = key != nil || plain != ""
			case "secret.rotate":
				var key *SiteSecret
				var retired []*SiteSecret
				key, plain, retired, err = d.RotateSecretAudited(t.Context(), site.ID, 0, time.Now().UTC(), actorKey.KeyID)
				if key != nil || retired != nil {
					material = true
				}
			case "admin_key.add":
				var key *AdminKey
				key, plain, err = d.NewAdminKeyWithIDAudited(t.Context(), "new", actorKey.KeyID)
				material = key != nil || plain != ""
			}
			if !errors.Is(err, ErrRevoked) {
				t.Fatalf("revoked actor must be rejected inside the write transaction: %v", err)
			}
			if plain != "" || material {
				t.Fatalf("rejected operation returned credential material: material=%v plain=%t", material, plain != "")
			}
			if after := auditedCreationSnapshot(t, d); !reflect.DeepEqual(after, before) {
				t.Fatal("revoked actor left a site, credential, or success audit")
			}
		})
	}
}

func TestC04ActorGraceExpiryCancellationAndOrdering(t *testing.T) {
	newFixture := func(t *testing.T) (*DB, *AdminKey, *Site) {
		t.Helper()
		d := newDB(t)
		actor, _, err := d.NewAdminKeyWithID(t.Context(), "actor")
		if err != nil {
			t.Fatal(err)
		}
		site, _, err := d.CreateSite(t.Context(), "existing", "pk_actor", DefaultPolicy())
		if err != nil {
			t.Fatal(err)
		}
		return d, actor, site
	}

	t.Run("current_valid_and_grace", func(t *testing.T) {
		d, actor, site := newFixture(t)
		if _, _, err := d.AddSecretAudited(t.Context(), site.ID, actor.KeyID); err != nil {
			t.Fatalf("currently valid actor rejected: %v", err)
		}
		cutoff := time.Now().UTC().Add(250 * time.Millisecond)
		if _, err := d.RevokeAdminKey(t.Context(), actor.KeyID, cutoff); err != nil {
			t.Fatal(err)
		}
		if _, _, err := d.AddSecretAudited(t.Context(), site.ID, actor.KeyID); err != nil {
			t.Fatalf("actor inside grace period rejected: %v", err)
		}
		for time.Now().UTC().Before(cutoff.Add(50 * time.Millisecond)) {
			time.Sleep(10 * time.Millisecond)
		}
		if _, _, err := d.AddSecretAudited(t.Context(), site.ID, actor.KeyID); !errors.Is(err, ErrRevoked) {
			t.Fatalf("grace-expired actor must be rejected: %v", err)
		}
	})

	t.Run("cancellation_rolls_back", func(t *testing.T) {
		d, actor, site := newFixture(t)
		before := auditedCreationSnapshot(t, d)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, plain, err := d.AddSecretAudited(ctx, site.ID, actor.KeyID)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled audited write: %v", err)
		}
		if plain != "" {
			t.Fatal("canceled audited write returned plaintext")
		}
		if after := auditedCreationSnapshot(t, d); !reflect.DeepEqual(after, before) {
			t.Fatal("canceled audited write changed persisted state")
		}
	})

	t.Run("write_before_revoke_commits_then_later_revoke", func(t *testing.T) {
		d, actor, site := newFixture(t)
		if _, _, err := d.AddSecretAudited(t.Context(), site.ID, actor.KeyID); err != nil {
			t.Fatalf("write-before-revoke should commit: %v", err)
		}
		if _, err := d.RevokeAdminKey(t.Context(), actor.KeyID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if _, _, err := d.AddSecretAudited(t.Context(), site.ID, actor.KeyID); !errors.Is(err, ErrRevoked) {
			t.Fatalf("write after committed revoke must reject: %v", err)
		}
	})
}

func TestC04AuditedWriteActorAuditFailureRollsBack(t *testing.T) {
	d := newDB(t)
	actor, _, err := d.NewAdminKeyWithID(t.Context(), "actor")
	if err != nil {
		t.Fatal(err)
	}
	site, _, err := d.CreateSite(t.Context(), "existing", "pk_existing", DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	before := auditedCreationSnapshot(t, d)
	if _, err := d.h.ExecContext(t.Context(), `CREATE TRIGGER c04_reject_success_audit BEFORE INSERT ON audit
BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	key, plain, err := d.AddSecretAudited(t.Context(), site.ID, actor.KeyID)
	if err == nil || key != nil || plain != "" {
		t.Fatalf("audit failure returned success or plaintext: key=%v plain=%t err=%v", key != nil, plain != "", err)
	}
	if after := auditedCreationSnapshot(t, d); !reflect.DeepEqual(after, before) {
		t.Fatal("actor audit failure committed the credential")
	}
}

func TestC08ListSecretsParentResourceSemantics(t *testing.T) {
	d := newDB(t)
	if got, err := d.ListSecrets(t.Context(), 999999); !errors.Is(err, ErrNotFound) || got != nil {
		t.Fatalf("missing parent must return ErrNotFound, got rows=%v err=%v", got, err)
	}
	site, _, err := d.CreateSite(t.Context(), "empty", "pk_empty", DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.h.ExecContext(t.Context(), `DELETE FROM site_secrets WHERE site_id=?`, site.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := d.ListSecrets(t.Context(), site.ID); err != nil || len(got) != 0 {
		t.Fatalf("existing site with no secrets must remain a valid empty list: rows=%v err=%v", got, err)
	}
	if _, err := d.SetSiteEnabled(t.Context(), site.ID, false); err != nil {
		t.Fatal(err)
	}
	if got, err := d.ListSecrets(t.Context(), site.ID); err != nil || len(got) != 0 {
		t.Fatalf("disabled existing site must remain a valid empty list: rows=%v err=%v", got, err)
	}
}
