package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestAuditedSecretWritesRejectRevokedActor(t *testing.T) {
	d := newDB(t)
	site, _, err := d.CreateSite(t.Context(), "actor-check", "pk_actor-check", DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	actor, _, err := d.NewAdminKeyWithID(t.Context(), "actor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.RevokeAdminKey(t.Context(), actor.KeyID, time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}

	assertNoSuccessfulAudit := func(action string, before int) {
		t.Helper()
		entries, err := d.ListAudit(t.Context(), 0, 500)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, entry := range entries {
			if entry.Action == action && entry.Success {
				count++
			}
		}
		if count != before {
			t.Errorf("%s successful audit count=%d, want %d", action, count, before)
		}
	}
	countSuccess := func(action string) int {
		t.Helper()
		entries, err := d.ListAudit(t.Context(), 0, 500)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, entry := range entries {
			if entry.Action == action && entry.Success {
				n++
			}
		}
		return n
	}

	check := func(name, action string, call func() (bool, error)) {
		t.Helper()
		before := countSuccess(action)
		material, err := call()
		if err == nil || material {
			t.Errorf("%s with revoked actor returned material=%v, err=%v", name, material, err)
		}
		assertNoSuccessfulAudit(action, before)
	}

	check("create site", "site.create", func() (bool, error) {
		created, secret, err := d.CreateSiteAudited(t.Context(), "denied-create", "pk_denied-create", DefaultPolicy(), nil, actor.KeyID)
		return created != nil || secret != "", err
	})
	if _, err := d.SiteByKey(t.Context(), "pk_denied-create"); !errors.Is(err, ErrNotFound) {
		t.Errorf("denied create left a site: %v", err)
	}

	beforeSecrets := credentialSnapshot(t, d, site.ID)
	check("add site secret", "secret.add", func() (bool, error) {
		key, secret, err := d.AddSecretAudited(t.Context(), site.ID, actor.KeyID)
		return key != nil || secret != "", err
	})
	if got := credentialSnapshot(t, d, site.ID); !reflect.DeepEqual(got, beforeSecrets) {
		t.Error("denied add changed site credentials")
	}

	check("rotate site secret", "secret.rotate", func() (bool, error) {
		key, secret, retired, err := d.RotateSecretAudited(t.Context(), site.ID, time.Hour, time.Now().UTC(), actor.KeyID)
		return key != nil || secret != "" || len(retired) != 0, err
	})
	if got := credentialSnapshot(t, d, site.ID); !reflect.DeepEqual(got, beforeSecrets) {
		t.Error("denied rotation changed site credentials")
	}

	beforeAdminKeys, err := d.ListAdminKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	check("add admin key", "admin_key.add", func() (bool, error) {
		key, plain, err := d.NewAdminKeyWithIDAudited(t.Context(), "denied-admin", actor.KeyID)
		return key != nil || plain != "", err
	})
	afterAdminKeys, err := d.ListAdminKeys(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(afterAdminKeys) != len(beforeAdminKeys) {
		t.Errorf("denied admin-key creation changed key count: %d -> %d", len(beforeAdminKeys), len(afterAdminKeys))
	}
}

func TestAuditedSecretWritesHonorGraceAndCanceledContext(t *testing.T) {
	d := newDB(t)
	site := mustSite(t, d, "actor-grace")
	actor, _, err := d.NewAdminKeyWithID(t.Context(), "grace-actor")
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().UTC().Add(400 * time.Millisecond)
	if _, err := d.RevokeAdminKey(t.Context(), actor.KeyID, cutoff); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.AddSecretAudited(t.Context(), site.ID, actor.KeyID); err != nil {
		t.Fatalf("actor must remain valid before grace cutoff: %v", err)
	}
	if wait := time.Until(cutoff) + 30*time.Millisecond; wait > 0 {
		time.Sleep(wait)
	}
	before := credentialSnapshot(t, d, site.ID)
	key, secret, err := d.AddSecretAudited(t.Context(), site.ID, actor.KeyID)
	if err == nil || key != nil || secret != "" {
		t.Errorf("actor accepted at/after grace cutoff: key=%v secret=%t err=%v", key != nil, secret != "", err)
	}
	if got := credentialSnapshot(t, d, site.ID); !reflect.DeepEqual(got, before) {
		t.Error("expired grace actor changed site credentials")
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if created, plain, err := d.CreateSiteAudited(canceled, "canceled-create", "pk_canceled-create", DefaultPolicy(), nil, actor.KeyID); err == nil || created != nil || plain != "" {
		t.Errorf("canceled create returned success/material: site=%v secret=%t err=%v", created != nil, plain != "", err)
	}
	if key, plain, err := d.NewAdminKeyWithIDAudited(canceled, "canceled-admin", actor.KeyID); err == nil || key != nil || plain != "" {
		t.Errorf("canceled admin-key write returned success/material: key=%v plain=%t err=%v", key != nil, plain != "", err)
	}
	if _, err := d.SiteByKey(t.Context(), "pk_canceled-create"); !errors.Is(err, ErrNotFound) {
		t.Errorf("canceled create left a site: %v", err)
	}
}

func TestListSecretsDistinguishesMissingSiteFromEmptyExistingSite(t *testing.T) {
	d := newDB(t)
	site := mustSite(t, d, "empty-secrets")
	if _, err := d.h.ExecContext(t.Context(), `DELETE FROM site_secrets WHERE site_id=?`, site.ID); err != nil {
		t.Fatal(err)
	}
	secrets, err := d.ListSecrets(t.Context(), site.ID)
	if err != nil || secrets == nil || len(secrets) != 0 {
		t.Fatalf("existing site with no secrets: secrets=%v err=%v", secrets, err)
	}
	if secrets, err := d.ListSecrets(t.Context(), site.ID+1000); !errors.Is(err, ErrNotFound) || secrets != nil {
		t.Errorf("missing site list: secrets=%v err=%v, want ErrNotFound", secrets, err)
	}
}
