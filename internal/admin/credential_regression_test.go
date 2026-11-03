package admin

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/store"
)

func TestCredentialRotationGraceThenImmediateHTTP(t *testing.T) {
	x := newHarness(t, nil)
	created, first := x.createSite("rotate", "pk_rotate")
	id := siteID(created)
	base := pathSite(id) + "/secrets"
	grace := x.do(http.MethodPost, base+"/rotate", map[string]any{"transition_sec": 3600})
	if grace.status != http.StatusOK {
		t.Fatalf("grace rotation: status=%d", grace.status)
	}
	second, _ := grace.got["secret"].(string)
	immediate := x.do(http.MethodPost, base+"/rotate", map[string]any{"transition_sec": 0})
	if immediate.status != http.StatusOK {
		t.Fatalf("immediate rotation: status=%d", immediate.status)
	}
	third, _ := immediate.got["secret"].(string)
	retired, _ := immediate.got["retired"].([]any)
	if len(retired) != 2 {
		t.Errorf("immediate rotation retired %d credentials, want 2", len(retired))
	}
	for _, entry := range retired {
		if key := entry.(map[string]any); key["active"] != false || key["revoked_at"] == "" {
			t.Errorf("immediate rotation returned active or undated retired metadata: %v", key)
		}
	}
	check := func() {
		t.Helper()
		for i, plain := range []string{first, second} {
			if _, err := x.db.FindSiteBySecret(t.Context(), plain, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("old credential %d still accepted: %v", i, err)
			}
		}
		if third == "" {
			t.Fatal("rotation did not return new credential")
		}
		if _, err := x.db.FindSiteBySecret(t.Context(), third, time.Now().UTC()); err != nil {
			t.Fatalf("new credential rejected: %v", err)
		}
	}
	check()
	x.restart(t)
	check()
}

func TestCredentialRepeatedRevokeHTTP(t *testing.T) {
	for _, kind := range []string{"site", "admin"} {
		t.Run(kind, func(t *testing.T) {
			x := newHarness(t, nil)
			var path, plain string
			if kind == "site" {
				created, secret := x.createSite("revoke", "pk_revoke")
				plain = secret
				base := pathSite(siteID(created)) + "/secrets"
				list := x.do(http.MethodGet, base, nil)
				keyID := list.got["secrets"].([]any)[0].(map[string]any)["key_id"].(string)
				path = base + "/" + keyID + "/revoke"
			} else {
				created := x.do(http.MethodPost, "/api/admin/admin-keys", map[string]any{"label": "revoke"})
				if created.status != http.StatusCreated {
					t.Fatalf("add admin credential: status=%d", created.status)
				}
				plain = created.got["admin_key"].(string)
				keyID := created.got["key"].(map[string]any)["key_id"].(string)
				path = "/api/admin/admin-keys/" + keyID + "/revoke"
			}
			revoke := func(seconds int) map[string]any {
				t.Helper()
				r := x.do(http.MethodPost, path, map[string]any{"transition_sec": seconds})
				if r.status != http.StatusOK {
					t.Fatalf("revoke: status=%d", r.status)
				}
				return r.got["key"].(map[string]any)
			}
			grace := revoke(60)
			if grace["active"] != true || grace["revoked_at"] == "" {
				t.Fatalf("grace metadata: %v", grace)
			}
			if extended := revoke(3600); extended["revoked_at"] != grace["revoked_at"] {
				t.Errorf("repeat revoke extended grace cutoff: %v -> %v", grace, extended)
			}
			immediate := revoke(0)
			if immediate["active"] != false {
				t.Fatal("immediate revoke returned active=true")
			}
			check := func() {
				t.Helper()
				for _, seconds := range []int{0, 3600} {
					again := revoke(seconds)
					if again["active"] != false || again["revoked_at"] != immediate["revoked_at"] {
						t.Errorf("repeated revoke changed terminal cutoff: %v -> %v", immediate, again)
					}
					if kind == "site" {
						if _, err := x.db.FindSiteBySecret(t.Context(), plain, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
							t.Errorf("revoked site credential authenticated: %v", err)
						}
					} else if r := x.doAuth(http.MethodGet, "/api/admin/whoami", nil, "Bearer "+plain); r.status != http.StatusUnauthorized {
						t.Errorf("revoked admin credential authenticated: status=%d", r.status)
					}
				}
			}
			check()
			x.restart(t)
			check()
		})
	}
}

func TestCredentialGraceCapacityHTTP(t *testing.T) {
	x := newHarness(t, nil)
	created, _ := x.createSite("cap", "pk_cap")
	base := pathSite(siteID(created)) + "/secrets"
	for i := 1; i < store.MaxSecretsPerSite; i++ {
		r := x.do(http.MethodPost, base+"/rotate", map[string]any{"transition_sec": 3600})
		if r.status != http.StatusOK {
			t.Fatalf("rotation %d below capacity: status=%d", i, r.status)
		}
	}
	before := x.do(http.MethodGet, base, nil)
	for _, suffix := range []string{"", "/rotate"} {
		var body any
		if suffix != "" {
			body = map[string]any{"transition_sec": 3600}
		}
		r := x.do(http.MethodPost, base+suffix, body)
		if r.status != http.StatusBadRequest || r.got["error"] != CodeBadRequest {
			t.Errorf("effective cap must reject %q: status=%d, code=%v", suffix, r.status, r.got["error"])
		}
		if _, ok := r.got["secret"]; ok {
			t.Error("rejected operation returned plaintext")
		}
		if after := x.do(http.MethodGet, base, nil); after.body != before.body {
			t.Error("rejected operation changed credential metadata")
		}
	}
	immediate := x.do(http.MethodPost, base+"/rotate", map[string]any{"transition_sec": 0})
	if immediate.status != http.StatusOK {
		t.Fatalf("full-set immediate rotation must succeed: status=%d", immediate.status)
	}
	retired, _ := immediate.got["retired"].([]any)
	if len(retired) != store.MaxSecretsPerSite {
		t.Errorf("full-set rotation retired %d, want %d", len(retired), store.MaxSecretsPerSite)
	}
	list := x.do(http.MethodGet, base, nil)
	active := 0
	for _, entry := range list.got["secrets"].([]any) {
		if entry.(map[string]any)["active"] == true {
			active++
		}
	}
	if active != 1 {
		t.Errorf("immediate rotation left %d effective credentials, want 1", active)
	}
}
