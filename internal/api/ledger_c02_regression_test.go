package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/config"
	"github.com/shijianus/EpoCHA/internal/store"
	"github.com/shijianus/EpoCHA/internal/token"
)

func TestLedgerAPIWiresTotalCapAndReturns429UntilPurge(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) {
		cfg.MaxLedgerRows = 2
		cfg.MaxActiveChallenges = 20
		cfg.MaxActivePerSite = 10
	})
	if h.srv.caps.MaxTotal != 2 || h.srv.caps.MaxActive != 20 || h.srv.caps.MaxActivePerSite != 10 {
		t.Errorf("API capacity wiring=%+v", h.srv.caps)
	}
	for _, siteKey := range []string{h.siteA.SiteKey, h.siteB.SiteKey} {
		r := h.postJSON("/api/challenge", map[string]string{"site_key": siteKey})
		if r.status != http.StatusOK {
			t.Fatalf("initial admission: status=%d body=%s", r.status, r.body)
		}
	}
	h.setClock(time.Minute)
	r := h.postJSON("/api/challenge", map[string]string{"site_key": h.siteB.SiteKey})
	if r.status != http.StatusTooManyRequests || !r.hasCode(CodeRateLimited) || r.headers.Get("Retry-After") != "1" {
		t.Fatalf("expired history must still reject at total cap: status=%d body=%s retry=%q", r.status, r.body, r.headers.Get("Retry-After"))
	}
	assertKeySet(t, r, "error-codes")
	counts, err := h.db.Purge(t.Context(), h.clock)
	if err != nil || counts.Challenges != 2 {
		t.Fatalf("safe purge: %+v %v", counts, err)
	}
	r = h.postJSON("/api/challenge", map[string]string{"site_key": h.siteB.SiteKey})
	if r.status != http.StatusOK || r.str("token") == "" {
		t.Fatalf("admission after purge: status=%d body=%s", r.status, r.body)
	}
}

func TestLedgerAPIExistingDirectConfigUsesStoreDefault(t *testing.T) {
	h := newHarness(t, nil)
	if h.cfg.MaxLedgerRows != 0 {
		t.Fatalf("fixture must exercise omitted total field: %d", h.cfg.MaxLedgerRows)
	}
	r := h.postJSON("/api/challenge", map[string]string{"site_key": h.siteA.SiteKey})
	if r.status != http.StatusOK {
		t.Fatalf("legacy direct configuration must retain bounded default admission: status=%d body=%s", r.status, r.body)
	}
}

func TestLedgerAPIValidResponseSurvivesCapacityPressure(t *testing.T) {
	h := newHarness(t, func(cfg *config.Config) { cfg.MaxLedgerRows = 1 })
	r := h.postJSON("/api/challenge", map[string]string{"site_key": h.siteA.SiteKey})
	if r.status != http.StatusOK {
		t.Fatalf("initial challenge: %d %s", r.status, r.body)
	}
	claims, err := h.srv.signer.Verify(token.PurposeChallenge, r.str("token"), h.clock)
	if err != nil {
		t.Fatal(err)
	}
	h.setClock(59 * time.Second)
	response, err := h.db.Redeem(t.Context(), claims.CID, h.clock, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h.setClock(time.Second)
	counts, err := h.db.Purge(t.Context(), h.clock)
	if err != nil || counts.Challenges != 0 || counts.Responses != 0 {
		t.Fatalf("live response dependency must be retained: %+v %v", counts, err)
	}
	r = h.postJSON("/api/challenge", map[string]string{"site_key": h.siteB.SiteKey})
	if r.status != http.StatusTooManyRequests || !r.hasCode(CodeRateLimited) {
		t.Fatalf("terminal history remains capped: %d %s", r.status, r.body)
	}
	if _, err := h.db.ConsumeResponse(t.Context(), response.ID, h.siteA.ID, h.clock); err != nil {
		t.Fatalf("capacity pressure invalidated a still-valid response: %v", err)
	}
	if _, err := h.db.Purge(t.Context(), h.clock); err != nil {
		t.Fatal(err)
	}
	r = h.postJSON("/api/challenge", map[string]string{"site_key": h.siteB.SiteKey})
	if r.status != http.StatusOK {
		t.Fatalf("consumed response did not release admission after purge: %d %s", r.status, r.body)
	}
	if _, err := h.db.Challenge(t.Context(), claims.CID); err != store.ErrNotFound {
		t.Fatalf("expired terminal challenge was not reclaimed: %v", err)
	}
}
