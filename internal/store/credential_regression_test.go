package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestRotateSecretGraceThenImmediate(t *testing.T) {
	d := newDB(t)
	site, first, err := d.CreateSite(t.Context(), "rotate", "pk_rotate", DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, second, _, err := d.RotateSecret(t.Context(), site.ID, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	at := now.Add(time.Minute)
	fresh, third, retired, err := d.RotateSecret(t.Context(), site.ID, 0, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(retired) != 2 {
		t.Errorf("immediate rotation retired %d credentials, want both older credentials", len(retired))
	}
	for _, key := range retired {
		if !key.RevokedAt.Equal(at) || key.KeyID == fresh.KeyID {
			t.Errorf("unexpected retired metadata: %+v", key)
		}
	}
	check := func(db *DB) {
		t.Helper()
		for i, plain := range []string{first, second} {
			if _, err := db.FindSiteBySecret(t.Context(), plain, at.Add(-time.Nanosecond)); err != nil {
				t.Errorf("old credential %d should remain valid just before cutoff: %v", i, err)
			}
			if _, err := db.FindSiteBySecret(t.Context(), plain, at); !errors.Is(err, ErrNotFound) {
				t.Errorf("old credential %d accepted at immediate cutoff: %v", i, err)
			}
		}
		if got, err := db.FindSiteBySecret(t.Context(), third, at); err != nil || got.ID != site.ID {
			t.Errorf("new credential unusable: %v", err)
		}
	}
	check(d)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _ := newDBAt(t, d.path)
	check(reopened)
}

func TestRotateSecretPreservesEarlierDeadline(t *testing.T) {
	d := newDB(t)
	site, first, err := d.CreateSite(t.Context(), "rotate", "pk_rotate", DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	secondKey, second, old, err := d.RotateSecret(t.Context(), site.ID, time.Hour, now)
	if err != nil || len(old) != 1 {
		t.Fatalf("first rotation: %v", err)
	}
	firstID := old[0].KeyID
	_, third, retired, err := d.RotateSecret(t.Context(), site.ID, 2*time.Hour, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(retired) != 2 {
		t.Errorf("rotation must account for both effective old credentials, got %d", len(retired))
	}
	firstKey, err := d.SecretByKey(t.Context(), site.ID, firstID)
	if err != nil || !firstKey.RevokedAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("earlier grace cutoff changed: %+v, %v", firstKey, err)
	}
	secondKey, err = d.SecretByKey(t.Context(), site.ID, secondKey.KeyID)
	if err != nil || !secondKey.RevokedAt.Equal(now.Add(121*time.Minute)) {
		t.Fatalf("second credential cutoff: %+v, %v", secondKey, err)
	}
	if _, err := d.FindSiteBySecret(t.Context(), first, firstKey.RevokedAt.Add(-time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.FindSiteBySecret(t.Context(), first, firstKey.RevokedAt); !errors.Is(err, ErrNotFound) {
		t.Fatalf("first credential accepted at cutoff: %v", err)
	}
	_, _, retired, err = d.RotateSecret(t.Context(), site.ID, 0, firstKey.RevokedAt)
	if err != nil || len(retired) != 2 {
		t.Fatalf("rotation at expired cutoff: retired=%d, %v", len(retired), err)
	}
	unchanged, err := d.SecretByKey(t.Context(), site.ID, firstID)
	if err != nil || !unchanged.RevokedAt.Equal(firstKey.RevokedAt) {
		t.Fatalf("expired history changed: %+v, %v", unchanged, err)
	}
	for i, plain := range []string{first, second, third} {
		if _, err := d.FindSiteBySecret(t.Context(), plain, firstKey.RevokedAt); !errors.Is(err, ErrNotFound) {
			t.Errorf("old credential %d accepted after immediate rotation: %v", i, err)
		}
	}
}

func TestCredentialRevokeDeadlineMonotonic(t *testing.T) {
	for _, kind := range []string{"site", "admin"} {
		t.Run(kind, func(t *testing.T) {
			d := newDB(t)
			var revoke func(*DB, time.Time) (time.Time, error)
			var match func(*DB, time.Time) error
			if kind == "site" {
				site, plain, err := d.CreateSite(t.Context(), "revoke", "pk_revoke", DefaultPolicy())
				if err != nil {
					t.Fatal(err)
				}
				keys := credentialSnapshot(t, d, site.ID)
				revoke = func(db *DB, at time.Time) (time.Time, error) {
					key, err := db.RevokeSecret(t.Context(), site.ID, keys[0].KeyID, at)
					if err != nil {
						return time.Time{}, err
					}
					return key.RevokedAt, nil
				}
				match = func(db *DB, at time.Time) error {
					_, err := db.FindSiteBySecret(t.Context(), plain, at)
					return err
				}
			} else {
				key, plain, err := d.NewAdminKeyWithID(t.Context(), "revoke")
				if err != nil {
					t.Fatal(err)
				}
				revoke = func(db *DB, at time.Time) (time.Time, error) {
					got, err := db.RevokeAdminKey(t.Context(), key.KeyID, at)
					if err != nil {
						return time.Time{}, err
					}
					return got.RevokedAt, nil
				}
				match = func(db *DB, at time.Time) error {
					_, err := db.MatchAdminKey(t.Context(), plain, at)
					return err
				}
			}
			now := time.Now().UTC()
			for _, step := range []struct{ at, want time.Time }{
				{now.Add(time.Hour), now.Add(time.Hour)},
				{now.Add(2 * time.Hour), now.Add(time.Hour)},
				{now.Add(30 * time.Minute), now.Add(30 * time.Minute)},
				{now.Add(30 * time.Minute), now.Add(30 * time.Minute)},
				{now, now},
				{now.Add(time.Minute), now},
				{now.Add(time.Hour), now},
			} {
				cutoff, err := revoke(d, step.at)
				if err != nil || !cutoff.Equal(step.want) {
					t.Errorf("revoke(%s): cutoff=%s, want=%s, err=%v", step.at, cutoff, step.want, err)
				}
				if err := match(d, step.want.Add(-time.Nanosecond)); err != nil {
					t.Errorf("credential rejected just before cutoff: %v", err)
				}
				if err := match(d, step.want); !errors.Is(err, ErrNotFound) {
					t.Errorf("credential accepted at cutoff: %v", err)
				}
			}

			other, _ := newDBAt(t, d.path)
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					db := []*DB{d, other}[i%2]
					cutoff, err := revoke(db, now.Add(time.Duration(i+1)*time.Minute))
					if err != nil || !cutoff.Equal(now) {
						t.Errorf("concurrent repeated revoke extended cutoff: %s, %v", cutoff, err)
					}
				}(i)
			}
			close(start)
			wg.Wait()
			if err := other.Close(); err != nil {
				t.Fatal(err)
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, _ := newDBAt(t, d.path)
			if cutoff, err := revoke(reopened, now.Add(time.Hour)); err != nil || !cutoff.Equal(now) {
				t.Errorf("restarted repeated revoke extended cutoff: %s, %v", cutoff, err)
			}
			if err := match(reopened, now); !errors.Is(err, ErrNotFound) {
				t.Errorf("credential revived after restart: %v", err)
			}
		})
	}
}

func TestAddSecretCountsGraceCredentials(t *testing.T) {
	d := newDB(t)
	site := mustSite(t, d, "cap")
	now := time.Now().UTC()
	for i := 1; i < MaxSecretsPerSite; i++ {
		if _, _, _, err := d.RotateSecret(t.Context(), site.ID, time.Hour, now); err != nil {
			t.Fatalf("rotation %d below cap: %v", i, err)
		}
	}
	before := credentialSnapshot(t, d, site.ID)
	key, plain, err := d.AddSecret(t.Context(), site.ID)
	if !errors.Is(err, ErrBadConfig) || key != nil || plain != "" {
		t.Errorf("add at effective cap: err=%v, metadata returned=%v, plaintext returned=%v", err, key != nil, plain != "")
	}
	if after := credentialSnapshot(t, d, site.ID); !reflect.DeepEqual(after, before) {
		t.Fatal("rejected add changed credentials")
	}
	if _, err := d.RevokeSecret(t.Context(), site.ID, before[0].KeyID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.AddSecret(t.Context(), site.ID); err != nil {
		t.Fatalf("expired credential must release its slot: %v", err)
	}
}

func TestRotateSecretAtCredentialLimit(t *testing.T) {
	for _, transition := range []time.Duration{0, time.Hour} {
		t.Run(transition.String(), func(t *testing.T) {
			d := newDB(t)
			site := mustSite(t, d, "cap")
			for i := 1; i < MaxSecretsPerSite; i++ {
				if _, _, err := d.AddSecret(t.Context(), site.ID); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC()
			keys := credentialSnapshot(t, d, site.ID)
			if _, err := d.RevokeSecret(t.Context(), site.ID, keys[0].KeyID, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			before := credentialSnapshot(t, d, site.ID)
			fresh, plain, retired, err := d.RotateSecret(t.Context(), site.ID, transition, now)
			if transition > 0 {
				if !errors.Is(err, ErrBadConfig) || fresh != nil || plain != "" || retired != nil {
					t.Errorf("grace rotation at cap must fail without credentials: %v", err)
				}
				if after := credentialSnapshot(t, d, site.ID); !reflect.DeepEqual(after, before) {
					t.Error("rejected rotation changed old cutoffs or inserted a credential")
				}
				return
			}
			if err != nil || fresh == nil || plain == "" || len(retired) != MaxSecretsPerSite {
				t.Fatalf("immediate rotation must replace a full set atomically: retired=%d, err=%v", len(retired), err)
			}
			for _, key := range retired {
				if !key.RevokedAt.Equal(now) {
					t.Errorf("immediate rotation left an old credential active: %+v", key)
				}
			}
			if _, err := d.FindSiteBySecret(t.Context(), plain, now); err != nil {
				t.Fatalf("replacement credential unusable: %v", err)
			}
		})
	}
}

func TestRotateSecretReusesSlotAtGraceDeadline(t *testing.T) {
	d := newDB(t)
	site := mustSite(t, d, "boundary")
	for i := 1; i < MaxSecretsPerSite; i++ {
		if _, _, err := d.AddSecret(t.Context(), site.ID); err != nil {
			t.Fatal(err)
		}
	}
	keys := credentialSnapshot(t, d, site.ID)
	cutoff := time.Now().UTC().Add(time.Hour)
	if _, err := d.RevokeSecret(t.Context(), site.ID, keys[0].KeyID, cutoff); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := d.RotateSecret(t.Context(), site.ID, time.Hour, cutoff.Add(-time.Nanosecond)); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("grace credential must count just before its cutoff: %v", err)
	}
	if _, _, retired, err := d.RotateSecret(t.Context(), site.ID, time.Hour, cutoff); err != nil || len(retired) != MaxSecretsPerSite-1 {
		t.Fatalf("exactly expired credential must release a slot: retired=%d, err=%v", len(retired), err)
	}
}

func TestRotateSecretRollbackOnFailure(t *testing.T) {
	for name, injection := range map[string]string{
		"revoke": `CREATE TRIGGER fail_revoke BEFORE UPDATE OF revoked_at ON site_secrets
BEGIN SELECT RAISE(ABORT, 'injected revoke failure'); END`,
		"insert": `CREATE TRIGGER fail_insert BEFORE INSERT ON site_secrets
BEGIN SELECT RAISE(ABORT, 'injected insert failure'); END`,
		"commit": `CREATE TABLE credential_commit_failure (
site_id INTEGER REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED);
CREATE TRIGGER fail_commit AFTER INSERT ON site_secrets
BEGIN INSERT INTO credential_commit_failure(site_id) VALUES(-1); END`,
	} {
		t.Run(name, func(t *testing.T) {
			d := newDB(t)
			site := mustSite(t, d, "rollback")
			if _, _, _, err := d.RotateSecret(t.Context(), site.ID, time.Hour, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			before := credentialSnapshot(t, d, site.ID)
			if _, err := d.h.ExecContext(t.Context(), injection); err != nil {
				t.Fatal(err)
			}
			fresh, plain, retired, err := d.RotateSecret(t.Context(), site.ID, 0, time.Now().UTC())
			if err == nil || fresh != nil || plain != "" || retired != nil {
				t.Errorf("injected %s failure returned success or credential material: %v", name, err)
			}
			// 独立句柄只观察已提交状态，避免把失败事务里尚未回滚的写入当成落库事实。
			other, _ := newDBAt(t, d.path)
			if after := credentialSnapshot(t, other, site.ID); !reflect.DeepEqual(after, before) {
				t.Error("failed rotation committed a new credential or changed an old cutoff")
			}
			if err := other.Close(); err != nil {
				t.Fatal(err)
			}
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, _ := newDBAt(t, d.path)
			if after := credentialSnapshot(t, reopened, site.ID); !reflect.DeepEqual(after, before) {
				t.Error("failed rotation changed persisted state after restart")
			}
		})
	}
}

func TestCredentialCapacityConcurrent(t *testing.T) {
	for _, twoHandles := range []bool{false, true} {
		for _, operation := range []string{"add", "rotate", "mixed"} {
			t.Run(fmt.Sprintf("two_handles=%v/%s", twoHandles, operation), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "epocha.db")
				a, _ := newDBAt(t, path)
				b := a
				if twoHandles {
					b, _ = newDBAt(t, path)
				}
				site := mustSite(t, a, "concurrent")
				for i := 1; i < MaxSecretsPerSite-1; i++ {
					if _, _, err := a.AddSecret(t.Context(), site.ID); err != nil {
						t.Fatal(err)
					}
				}
				now := time.Now().UTC()
				keys := credentialSnapshot(t, a, site.ID)
				if _, err := a.RevokeSecret(t.Context(), site.ID, keys[0].KeyID, now.Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				errs := make([]error, 12)
				start := make(chan struct{})
				var wg sync.WaitGroup
				for i := range errs {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						<-start
						db := []*DB{a, b}[i%2]
						if operation == "rotate" || operation == "mixed" && i%3 == 0 {
							_, _, _, errs[i] = db.RotateSecret(t.Context(), site.ID, time.Hour, now)
						} else {
							_, _, errs[i] = db.AddSecret(t.Context(), site.ID)
						}
					}(i)
				}
				close(start)
				wg.Wait()
				succeeded := 0
				for _, err := range errs {
					if err == nil {
						succeeded++
					} else if !errors.Is(err, ErrBadConfig) {
						t.Errorf("unexpected concurrent failure: %v", err)
					}
				}
				if succeeded != 1 {
					t.Errorf("one effective slot must admit exactly one writer, got %d", succeeded)
				}
				keys = credentialSnapshot(t, a, site.ID)
				active := 0
				for _, key := range keys {
					if key.RevokedAt.IsZero() || key.RevokedAt.After(now) {
						active++
					}
				}
				if active != MaxSecretsPerSite || len(keys) != MaxSecretsPerSite {
					t.Errorf("capacity drift: active=%d total=%d, want %d", active, len(keys), MaxSecretsPerSite)
				}
			})
		}
	}
}

func credentialSnapshot(t *testing.T, d *DB, siteID int64) []*SiteSecret {
	t.Helper()
	keys, err := d.ListSecrets(t.Context(), siteID)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}
