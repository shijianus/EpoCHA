package api

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/generator"
)

func TestRefreshCooldownHTTPRetryAcrossWindow(t *testing.T) {
	h := newHarness(t, nil)
	site := h.siteWithPool(t, "retry", "sk-retry", []string{generator.BankShotID}, 3)
	h.clock = time.Unix(596, 0).UTC()
	key := "site:" + strconv.FormatInt(site.ID, 10) + ":sess:cooldown-session"
	for i := 1; i <= 10; i++ {
		h.srv.tracker.Assess(key, 0)
		if i == 6 {
			h.clock = h.clock.Add(3 * time.Second)
		}
	}
	h.clock = time.Unix(601, 0).UTC()
	for i := 0; i < 3; i++ {
		r := h.challengeReplyWith(map[string]any{"site_key": "sk-retry", "session": "cooldown-session"})
		if r.status != http.StatusTooManyRequests || r.headers.Get("Retry-After") != "898" {
			t.Fatalf("cross-window retry status=%d retry=%q", r.status, r.headers.Get("Retry-After"))
		}
	}
	if r := h.challengeReplyWith(map[string]any{"site_key": "sk-retry"}); r.status == http.StatusTooManyRequests {
		t.Fatal("legacy no-session request inherited the session cooldown")
	}
	h.clock = time.Unix(1499, 0).UTC()
	if r := h.challengeReplyWith(map[string]any{"site_key": "sk-retry", "session": "cooldown-session"}); r.status == http.StatusTooManyRequests {
		t.Fatal("deadline did not release HTTP cooldown")
	}
}
