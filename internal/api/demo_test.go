package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDemoPageServesOnlyPublicWidget(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleDemo(rec, httptest.NewRequest(http.MethodGet, "/demo?site_key=pk_demo", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"EpoCHA.render", "site_key", "/widget/v1.js", "onSuccess"} {
		if !strings.Contains(body, want) {
			t.Fatalf("demo 页面缺少 %q", want)
		}
	}
	// 测试页只能出现公开 site_key；不得包含 secret、管理路径或服务端验证凭据。
	for _, forbidden := range []string{"secret=", "/api/admin/", "EPOCHA_PEPPER", "EPOCHA_MASTER_KEY"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("demo 页面含禁止内容 %q", forbidden)
		}
	}
	if rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("content-type=%q", rec.Header().Get("Content-Type"))
	}
}
