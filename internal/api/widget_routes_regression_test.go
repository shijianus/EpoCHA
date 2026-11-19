package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/risk"
	"github.com/shijianus/EpoCHA/web/widget"
)

func TestPublicWidgetAssetsExactRoutes(t *testing.T) {
	src, err := risk.NewSource(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{src: src, now: time.Now, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	for _, asset := range []struct{ path, name string }{
		{widget.URLJS, widget.JSPath},
		{widget.URLWorker, widget.WorkerPath},
		{widget.URLInteractive, widget.InteractivePath},
	} {
		t.Run(asset.name, func(t *testing.T) {
			res, err := srv.Client().Get(srv.URL + asset.path)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatal(err)
			}
			want, err := widget.AssetBytes(asset.name)
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != http.StatusOK || len(body) == 0 || !bytes.Equal(body, want) {
				t.Fatalf("public resource status=%d bytes=%d exact=%v", res.StatusCode, len(body), bytes.Equal(body, want))
			}
			for header, value := range map[string]string{
				"Content-Type":           "text/javascript; charset=utf-8",
				"Cache-Control":          "public, max-age=3600",
				"X-Content-Type-Options": "nosniff",
			} {
				if got := res.Header.Get(header); got != value {
					t.Errorf("%s=%q want %q", header, got, value)
				}
			}
			if res.Header.Get("Access-Control-Allow-Origin") != "" || res.Header.Get("Pragma") != "" {
				t.Fatal("static response changed CORS/cache boundary")
			}
		})
	}
	for _, path := range []string{"/widget/", "/widget/widget.go", "/widget/v2.interactive.js", "/admin", "/admin/app.js", "/api/admin/sites", "/api/admin/pool"} {
		res, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("public boundary %s=%d want 404", path, res.StatusCode)
		}
	}
}
