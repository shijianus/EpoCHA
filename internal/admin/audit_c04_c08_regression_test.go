package admin

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/store"
)

type gatedBody struct {
	started chan struct{}
	release chan struct{}
	data    *bytes.Reader
}

func (b *gatedBody) Read(p []byte) (int, error) {
	select {
	case <-b.started:
	default:
		close(b.started)
	}
	<-b.release
	return b.data.Read(p)
}

func (b *gatedBody) Close() error { return nil }

func TestInFlightCreateRejectsActorRevokedBeforeWrite(t *testing.T) {
	x := newHarness(t, nil)
	body := []byte(`{"name":"in-flight","site_key":"pk_in-flight"}`)
	gate := &gatedBody{started: make(chan struct{}), release: make(chan struct{}), data: bytes.NewReader(body)}
	req := httptest.NewRequest(http.MethodPost, "/api/admin/sites", nil)
	req.Header.Set("Authorization", "Bearer "+sentinelAdminKey)
	req.Header.Set("Content-Type", "application/json")
	req.Body = gate
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		x.srv.Handler().ServeHTTP(response, req)
		close(done)
	}()
	select {
	case <-gate.started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach body read after authentication")
	}
	who := x.do(http.MethodGet, "/api/admin/whoami", nil)
	actorID, _ := who.got["key_id"].(string)
	if actorID == "" {
		t.Fatal("whoami did not identify bootstrap actor")
	}
	if revoked := x.do(http.MethodPost, "/api/admin/admin-keys/"+actorID+"/revoke", map[string]any{"transition_sec": 0}); revoked.status != http.StatusOK {
		t.Fatalf("revoke bootstrap actor: status=%d body=%s", revoked.status, revoked.body)
	}
	close(gate.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request did not finish")
	}
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"error":"unauthorized"`) {
		t.Errorf("revoked in-flight actor: status=%d body=%s, want 401 unauthorized", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"secret"`) {
		t.Errorf("rejected create disclosed one-time secret: %s", response.Body.String())
	}
	if _, err := x.db.SiteByKey(t.Context(), "pk_in-flight"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoked in-flight actor still created the site: %v", err)
	}
	entries, err := x.db.ListAudit(t.Context(), 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Action == "site.create" && entry.Target == "site:1" && entry.Success {
			t.Error("rejected in-flight create wrote a successful audit")
		}
	}
}

func TestInFlightProtectedSiteWritesRejectRevokedActor(t *testing.T) {
	tests := []struct {
		name         string
		method       string
		path         func(int64) string
		body         func(int64) []byte
		skipInFlight bool
		check        func(*testing.T, *h, int64)
	}{
		{
			name: "lower pow bits", method: http.MethodPut,
			path: func(id int64) string { return pathSite(id) },
			body: func(int64) []byte { return []byte(`{"pow_bits":13}`) },
			check: func(t *testing.T, x *h, id int64) {
				site, err := x.db.SiteByID(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				if site.Policy.PowBits != 14 {
					t.Fatalf("pow bits changed: %d", site.Policy.PowBits)
				}
			},
		},
		{
			name: "switch to pow only", method: http.MethodPut,
			path: func(id int64) string { return pathSite(id) },
			body: func(int64) []byte {
				return []byte(`{"verify_mode":"pow_question","generator_id":"arithmetic-baseline-v1","difficulty":3}`)
			},
			check: func(t *testing.T, x *h, id int64) {
				site, err := x.db.SiteByID(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				// createSite 不带 verify_mode ⇒ withDefaults 落成 pow_only；被拒更新不得把它改成 pow_question。
				if site.Policy.VerifyMode != store.ModePowOnly {
					t.Fatalf("verify mode changed: %s (version=%d)", site.Policy.VerifyMode, site.Version)
				}
			},
		},
		{
			name: "allow missing origin", method: http.MethodPut,
			path: func(id int64) string { return pathSite(id) },
			body: func(int64) []byte { return []byte(`{"allow_missing_origin":true}`) },
			check: func(t *testing.T, x *h, id int64) {
				site, err := x.db.SiteByID(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				if site.Policy.AllowMissingOrigin {
					t.Fatal("allow_missing_origin changed")
				}
			},
		},
		{
			name: "replace origins", method: http.MethodPut,
			path: func(id int64) string { return pathSite(id) + "/origins" },
			body: func(int64) []byte { return []byte(`{"origins":["https://new.example.com"]}`) },
			check: func(t *testing.T, x *h, id int64) {
				origins, err := x.db.Origins(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				if len(origins) != 0 {
					t.Fatalf("origins changed: %v", origins)
				}
			},
		},
		{
			name: "enable site", method: http.MethodPost,
			path: func(id int64) string { return pathSite(id) + "/enable" },
			// 启停路径不读请求体，没有 body 屏障点；该顺序由 store 层
			// TestSetSiteEnabledRejectsRevokedActorInTransaction 钉住（同一写槽裁决）。
			skipInFlight: true,
			check:        func(*testing.T, *h, int64) {},
		},
	}
	for _, tc := range tests {
		if tc.skipInFlight {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			x := newHarness(t, nil)
			created, _ := x.createSite("in-flight-"+strings.ReplaceAll(tc.name, " ", "-"), "pk-in-flight-"+strings.ReplaceAll(tc.name, " ", "-"))
			id := siteID(created)
			if got := x.do(http.MethodPost, pathSite(id)+"/disable", map[string]any{}); got.status != http.StatusOK {
				t.Fatalf("disable: %d %s", got.status, got.body)
			}
			entries0, err := x.db.ListAudit(t.Context(), 0, 500)
			if err != nil {
				t.Fatal(err)
			}
			// ListAudit 按 id DESC 返回；记录 setup 后的最大 id，比较时只看"新增"条目。
			setupMaxID := int64(0)
			for _, entry := range entries0 {
				if entry.ID > setupMaxID {
					setupMaxID = entry.ID
				}
			}
			body := tc.body(id)
			gate := &gatedBody{started: make(chan struct{}), release: make(chan struct{}), data: bytes.NewReader(body)}
			req := httptest.NewRequest(tc.method, tc.path(id), nil)
			req.Header.Set("Authorization", "Bearer "+sentinelAdminKey)
			req.Header.Set("Content-Type", "application/json")
			req.Body = gate
			rr := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { x.srv.Handler().ServeHTTP(rr, req); close(done) }()
			select {
			case <-gate.started:
			case <-time.After(2 * time.Second):
				t.Fatal("in-flight request did not reach body read")
			}
			who := x.do(http.MethodGet, "/api/admin/whoami", nil)
			actorID, _ := who.got["key_id"].(string)
			if actorID == "" {
				t.Fatal("missing actor key id")
			}
			if revoked := x.do(http.MethodPost, "/api/admin/admin-keys/"+actorID+"/revoke", map[string]any{"transition_sec": 0}); revoked.status != http.StatusOK {
				t.Fatalf("revoke: %d %s", revoked.status, revoked.body)
			}
			close(gate.release)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("in-flight request did not finish")
			}
			if rr.Code != http.StatusUnauthorized || !strings.Contains(rr.Body.String(), `"error":"unauthorized"`) {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			tc.check(t, x, id)
			entries, err := x.db.ListAudit(t.Context(), 0, 500)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != len(entries0) {
				for _, entry := range entries {
					// setup 自身的撤销动作（admin_key.revoke）是合法成功审计，不属于被拒操作。
					if entry.ID > setupMaxID && entry.Success && entry.Action != "admin_key.revoke" {
						t.Fatalf("rejected operation wrote a successful audit: %+v", entry)
					}
				}
			}
		})
	}
}

func TestC08ListSecretsParentHTTPAdditional(t *testing.T) {
	x := newHarness(t, nil)
	if got := x.do(http.MethodGet, "/api/admin/sites/999999/secrets", nil); got.status != http.StatusNotFound || got.got["error"] != CodeNotFound {
		t.Fatalf("nonexistent parent must return 404: status=%d body=%s", got.status, got.body)
	}
	created, _ := x.createSite("existing-empty-test", "pk_existing-empty-test")
	got := x.do(http.MethodGet, pathSite(siteID(created))+"/secrets", nil)
	if got.status != http.StatusOK {
		t.Fatalf("existing site's credential list: status=%d body=%s", got.status, got.body)
	}
}

func TestTextImageGeneratorNoteUsesHistoricalOCRBaseline(t *testing.T) {
	experimental, ready := generatorLifecycle("text-image-v1")
	if !experimental || ready {
		t.Fatalf("lifecycle flags = (%v,%v), want experimental and not production-ready", experimental, ready)
	}
	note := noteForGenerator("text-image-v1")
	for _, want := range []string{"96%", "31%", "3%", "历史"} {
		if !strings.Contains(note, want) {
			t.Errorf("generator note %q omits historical measured baseline marker %q", note, want)
		}
	}
	if strings.Contains(note, "本环境无离线 OCR") {
		t.Errorf("generator note retains outdated no-OCR wording: %q", note)
	}
}

var _ io.ReadCloser = (*gatedBody)(nil)
