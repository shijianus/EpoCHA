package risk

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func triggerLongCooldown(t *testing.T, tr *RefreshTracker, now *time.Time, key string) time.Time {
	t.Helper()
	for i := 1; i <= 10; i++ {
		p := tr.Observe(key)
		if i == 6 {
			*now = now.Add(3 * time.Second)
		}
		if i == 10 && p.Cooldown != 15*time.Minute {
			t.Fatalf("threshold cooldown=%v", p.Cooldown)
		}
	}
	return now.Add(15 * time.Minute)
}

func TestRefreshCooldownSurvivesWindowAndExpiresExactly(t *testing.T) {
	now, clock := fakeClock(time.Unix(596, 0))
	tr, _ := NewRefreshTracker(4, clock)
	deadline := triggerLongCooldown(t, tr, now, "k") // threshold at Unix 599
	for _, at := range []time.Time{time.Unix(601, 0), deadline.Add(-time.Nanosecond)} {
		*now = at
		p := tr.Observe("k")
		if p.Cooldown != deadline.Sub(at) || tr.RetryAfter("k") != p.Cooldown {
			t.Fatalf("at %v: cooldown=%v retry=%v want=%v", at, p.Cooldown, tr.RetryAfter("k"), deadline.Sub(at))
		}
		if p.FloorDelta != 3 || p.PoWDelta != 6 || p.Score != 100 {
			t.Fatalf("active cooldown lost penalty: %+v", p)
		}
	}
	*now = deadline
	if tr.RetryAfter("k") != 0 {
		t.Fatal("cooldown must release at deadline")
	}
	if p := tr.Observe("k"); p != (RefreshPenalty{}) {
		t.Fatalf("expired cooldown should start a new window: %+v", p)
	}
	*now = now.Add(time.Nanosecond)
	if p := tr.Observe("k"); p.Cooldown != 0 {
		t.Fatalf("post-expiry retry=%+v", p)
	}
}

func TestRefreshCooldownRetriesDoNotCountOrRenew(t *testing.T) {
	now, clock := fakeClock(time.Unix(1200, 0))
	tr, _ := NewRefreshTracker(2, clock)
	for i := 0; i < 6; i++ {
		tr.Observe("k")
	}
	deadline := now.Add(3 * time.Second)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p := tr.Observe("k"); p.Cooldown != 3*time.Second {
				t.Errorf("concurrent retry changed cooldown: %+v", p)
			}
		}()
	}
	wg.Wait()
	*now = deadline
	p := tr.Observe("k")
	if p.Cooldown != 0 || p.FloorDelta != 2 || tr.states["k"].count != 7 {
		t.Fatalf("blocked retries counted or renewed: %+v count=%d", p, tr.states["k"].count)
	}
}

func TestRefreshCapacityNeverEvictsActiveCooldown(t *testing.T) {
	now, clock := fakeClock(time.Unix(596, 0))
	tr, _ := NewRefreshTracker(2, clock)
	deadline := triggerLongCooldown(t, tr, now, "cooling")
	*now = time.Unix(601, 0)
	for i := 0; i < 20; i++ {
		tr.Observe(fmt.Sprintf("other-%d", i))
	}
	if got := tr.RetryAfter("cooling"); got != deadline.Sub(*now) {
		t.Fatalf("capacity evicted active cooldown: %v want %v", got, deadline.Sub(*now))
	}
	if tr.Len() > 2 {
		t.Fatal("state capacity exceeded")
	}
}

func TestRefreshAllCoolingCapacityFailsClosedUntilExpiry(t *testing.T) {
	now, clock := fakeClock(time.Unix(1200, 0))
	tr, _ := NewRefreshTracker(1, clock)
	deadline := triggerLongCooldown(t, tr, now, "a")
	p := tr.Observe("b")
	if p.Cooldown != deadline.Sub(*now) || tr.Len() != 1 || tr.RetryAfter("a") != p.Cooldown {
		t.Fatalf("full cooling state must reject new key: %+v len=%d retry=%v", p, tr.Len(), tr.RetryAfter("a"))
	}
	*now = deadline
	if p := tr.Observe("b"); p.Cooldown != 0 || tr.Len() != 1 {
		t.Fatalf("expired slot not reusable: %+v len=%d", p, tr.Len())
	}
}
