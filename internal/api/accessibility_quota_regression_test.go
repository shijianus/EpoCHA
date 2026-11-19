package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/shijianus/EpoCHA/internal/dispatch"
	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/store"
)

func accessibilityRequest(key string) map[string]any {
	return map[string]any{"site_key": key, "accessibility": true}
}

func apiSQL(t *testing.T, h *harness, statement string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+h.dbPath+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), statement, args...); err != nil {
		t.Fatal(err)
	}
}

func TestAccessibilityFailedIssueDoesNotConsumeQuota(t *testing.T) {
	for _, failure := range []string{"empty_pool", "scheduler", "commit", "dto", "panic"} {
		t.Run(failure, func(t *testing.T) {
			h := newHarness(t, nil)
			h.siteWithPool(t, "quota", "sk-quota", []string{generator.AccessibleChoiceID}, 2)
			fx := accessibleFixture(t, 2, failure)
			if failure != "empty_pool" {
				h.seedQuestions(t, 2, fx)
			}
			signer := h.srv.signer
			switch failure {
			case "scheduler":
				h.srv.scheduler = nil
			case "commit":
				apiSQL(t, h, `CREATE TABLE quota_commit_failure(site_id INTEGER REFERENCES sites(id) DEFERRABLE INITIALLY DEFERRED);
CREATE TRIGGER reject_quota_commit AFTER INSERT ON challenges
BEGIN INSERT INTO quota_commit_failure(site_id) VALUES(-1); END`)
			case "dto":
				apiSQL(t, h, `UPDATE pool SET payload_json='{}'`)
			case "panic":
				h.srv.signer = nil
			}
			first := h.challengeReplyWith(accessibilityRequest("sk-quota"))
			if first.status == http.StatusOK {
				t.Fatal("injected failed issue returned success")
			}
			switch failure {
			case "empty_pool":
				h.seedQuestions(t, 2, fx)
			case "scheduler":
				h.srv.scheduler = dispatch.NewScheduler()
			case "commit":
				apiSQL(t, h, `DROP TRIGGER reject_quota_commit`)
			case "dto":
				raw, err := json.Marshal(fx.Payload)
				if err != nil {
					t.Fatal(err)
				}
				apiSQL(t, h, "UPDATE pool SET payload_json=?", string(raw))
				h.seedQuestions(t, 2, fx)
			case "panic":
				h.srv.signer = signer
			}
			second := h.challengeReplyWith(accessibilityRequest("sk-quota"))
			if second.status != http.StatusOK {
				t.Fatalf("failed %s issue consumed quota: first=%d retry=%d %s", failure, first.status, second.status, second.body)
			}
			if third := h.challengeReplyWith(accessibilityRequest("sk-quota")); third.status != http.StatusTooManyRequests {
				t.Fatalf("successful issue did not consume quota: %d", third.status)
			}
		})
	}
}

func TestAccessibilityConcurrentIssueAndRestart(t *testing.T) {
	h := newHarness(t, nil)
	h.siteWithPool(t, "quota-concurrent", "sk-quota-concurrent", []string{generator.AccessibleChoiceID}, 2)
	for i := 0; i < 16; i++ {
		h.seedQuestions(t, 2, accessibleFixture(t, 2, "concurrent"))
	}
	other, err := store.Open(t.Context(), h.dbPath, h.cfg.Pepper)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	second, err := New(h.cfg, other, h.srv.signer, h.gens, nil)
	if err != nil {
		t.Fatal(err)
	}
	second.now = h.srv.now
	start := make(chan struct{})
	statuses := make(chan int, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		handler := h.srv.Handler()
		if i%2 == 1 {
			handler = second.Handler()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := httptest.NewRequest(http.MethodPost, "/api/challenge", strings.NewReader(`{"site_key":"sk-quota-concurrent","accessibility":true}`))
			r.RemoteAddr = "203.0.113.7:5555"
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			statuses <- w.Code
		}()
	}
	close(start)
	wg.Wait()
	close(statuses)
	successes := 0
	for status := range statuses {
		if status == http.StatusOK {
			successes++
		} else if status != http.StatusTooManyRequests {
			t.Errorf("competing issue status=%d", status)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent independent handles issued %d challenges, want 1", successes)
	}
	// A new API instance has no in-memory gate state; committed quota remains authoritative.
	h.srv = second
	if r := h.challengeReplyWith(accessibilityRequest("sk-quota-concurrent")); r.status != http.StatusTooManyRequests {
		t.Fatalf("new API instance lost quota: %d", r.status)
	}
}

type failedChallengeWriter struct {
	header http.Header
	status int
}

func (w *failedChallengeWriter) Header() http.Header    { return w.header }
func (w *failedChallengeWriter) WriteHeader(status int) { w.status = status }
func (w *failedChallengeWriter) Write([]byte) (int, error) {
	return 0, errors.New("injected response loss")
}

func TestAccessibilityCommittedResponseLossConsumesQuota(t *testing.T) {
	h := newHarness(t, nil)
	h.siteWithPool(t, "quota-loss", "sk-quota-loss", []string{generator.AccessibleChoiceID}, 2)
	h.seedQuestions(t, 2, accessibleFixture(t, 2, "loss"))
	r := httptest.NewRequest(http.MethodPost, "/api/challenge", strings.NewReader(`{"site_key":"sk-quota-loss","accessibility":true}`))
	r.RemoteAddr = "203.0.113.7:5555"
	r.Header.Set("Content-Type", "application/json")
	w := &failedChallengeWriter{header: make(http.Header)}
	h.srv.Handler().ServeHTTP(w, r)
	if w.status != http.StatusOK {
		t.Fatalf("issue did not commit before transport failure: %d", w.status)
	}
	if retry := h.challengeReplyWith(accessibilityRequest("sk-quota-loss")); retry.status != http.StatusTooManyRequests {
		t.Fatalf("response loss refunded committed challenge: %d", retry.status)
	}
}
