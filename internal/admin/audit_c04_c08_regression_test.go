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
