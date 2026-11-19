package risk

import (
	"strings"
	"testing"
	"time"
)

func fakeClock(start time.Time) (*time.Time, func() time.Time) {
	t := start
	return &t, func() time.Time { return t }
}

func TestRefreshTrackerStateMachine(t *testing.T) {
	now, clock := fakeClock(time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	tr, err := NewRefreshTracker(4, clock)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		p := tr.Observe("k")
		switch i {
		case 1, 2:
			if p.FloorDelta != 0 || p.PoWDelta != 0 || p.Cooldown != 0 {
				t.Fatalf("第 %d 次不应加压: %+v", i, p)
			}
		case 3:
			if p.FloorDelta != 1 || p.PoWDelta != 2 || p.Cooldown != 0 || p.Score < 25 {
				t.Fatalf("第 3 次惩罚 = %+v", p)
			}
		case 6:
			if p.FloorDelta != 2 || p.PoWDelta != 4 || p.Cooldown != 3*time.Second {
				t.Fatalf("第 6 次惩罚 = %+v", p)
			}
			if got := tr.RetryAfter("k"); got != 3*time.Second {
				t.Fatalf("RetryAfter = %v", got)
			}
			*now = now.Add(3 * time.Second)
		case 7:
			// 冷却只在跨阈值时启动；否则每次重试都会续期。
			if p.Cooldown != 0 {
				t.Fatalf("第 7 次不应续期冷却: %+v", p)
			}
		case 10:
			if p.FloorDelta != 3 || p.PoWDelta != 6 || p.Cooldown != 15*time.Minute {
				t.Fatalf("第 10 次惩罚 = %+v", p)
			}
		}
		*now = now.Add(100 * time.Millisecond)
	}
	// 计数窗口到期不解除仍有效的冷却；冷却结束后新窗口才从低档开始。
	*now = now.Add(RefreshWindow)
	p := tr.Observe("k")
	if p.Cooldown <= 0 || p.FloorDelta != 3 {
		t.Fatalf("窗口切换丢失冷却 = %+v", p)
	}
	*now = now.Add(p.Cooldown)
	p = tr.Observe("k")
	if p.FloorDelta != 0 || p.Score != 0 || p.Cooldown != 0 {
		t.Fatalf("冷却结束后 = %+v", p)
	}
}

func TestRefreshTrackerBounded(t *testing.T) {
	_, clock := fakeClock(time.Now())
	tr, _ := NewRefreshTracker(2, clock)
	tr.Observe("a")
	tr.Observe("b")
	tr.Observe("c")
	if tr.Len() > 2 {
		t.Fatalf("状态数 = %d，超过上限", tr.Len())
	}
}

func TestTelemetryValidationAndAnomaly(t *testing.T) {
	good := Telemetry{TotalMS: 4200, FirstInputMS: 700, EventCount: 80, PointerEvents: 60, KeyEvents: 20, FocusBlur: 0}
	if err := good.Validate(); err != nil {
		t.Fatalf("合法遥测被拒: %v", err)
	}
	if good.AnomalyScore() != 0 {
		t.Fatalf("正常遥测异常分 = %d", good.AnomalyScore())
	}
	bad := []Telemetry{
		{TotalMS: -1},
		{TotalMS: 100, FirstInputMS: 200},
		{TotalMS: 100, EventCount: MaxTelemetryEvents + 1},
		{TotalMS: 100, EventCount: 1, PointerEvents: 1, KeyEvents: 1},
		{TotalMS: 100, EventCount: 1, FocusBlur: MaxTelemetryFocusBlur + 1},
	}
	for i, tel := range bad {
		if err := tel.Validate(); err == nil {
			t.Fatalf("畸形遥测 %d 未被拒绝: %+v", i, tel)
		}
	}
	anomalous := Telemetry{TotalMS: 200, FirstInputMS: 10, EventCount: 1, PointerEvents: 1}
	if got := anomalous.AnomalyScore(); got == 0 || got > 20 {
		t.Fatalf("异常分 = %d，应在 1..20", got)
	}
}

func TestTrackerWeakEvidenceAndRefresh(t *testing.T) {
	_, clock := fakeClock(time.Now())
	tr, _ := NewTracker(8, clock)
	if s, p := tr.Assess("k", 0); s != 0 || p.Score != 0 {
		t.Fatalf("首次 Assess = (%d,%+v)", s, p)
	}
	if score := tr.ObserveRedeem("k", false, nil); score != 10 {
		t.Fatalf("一次失败风险 = %d，期望 10", score)
	}
	if score := tr.ObserveRedeem("k", false, nil); score != 20 {
		t.Fatalf("两次失败风险 = %d，期望 20", score)
	}
	if score := tr.ObserveRedeem("k", true, nil); score >= 20 {
		t.Fatalf("成功后失败计数应衰减，got %d", score)
	}
	// 遥测异常只在显式成功后累积为弱证据。
	tel := &Telemetry{TotalMS: 100, FirstInputMS: 10, EventCount: 1, PointerEvents: 1}
	if score := tr.ObserveRedeem("tel", true, tel); score < 10 || score > 20 {
		t.Fatalf("遥测异常分 = %d，应在 10..20", score)
	}
	// 第 3 次 Assess 触发刷新惩罚；分数必须单调上升或保持。
	var last int
	for i := 0; i < 3; i++ {
		s, _ := tr.Assess("refresh", 0)
		if s < last {
			t.Fatalf("风险分回退: %d -> %d", last, s)
		}
		last = s
	}
	if last < 25 {
		t.Fatalf("第 3 次刷新后风险分 = %d，期望 >=25", last)
	}
}

func TestAccessibilityGate(t *testing.T) {
	now, clock := fakeClock(time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	g, err := NewAccessibilityGate(4, 1, 24*time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Allow("k") {
		t.Fatal("首次应允许")
	}
	if g.Allow("k") {
		t.Fatal("24h 内第二次应拒绝")
	}
	if d := g.RetryAfter("k"); d <= 0 || d > 24*time.Hour {
		t.Fatalf("RetryAfter = %v", d)
	}
	*now = now.Add(24 * time.Hour)
	if !g.Allow("k") {
		t.Fatal("窗口结束后应允许")
	}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		g.Allow(k)
	}
	if len(g.states) > 4 {
		t.Fatalf("gate 状态数 = %d，超过上限", len(g.states))
	}
	// 配置边界
	if _, err := NewAccessibilityGate(0, 1, time.Hour, clock); err == nil {
		t.Fatal("maxStates=0 应拒绝")
	}
	if _, err := NewAccessibilityGate(1, 0, time.Hour, clock); err == nil {
		t.Fatal("limit=0 应拒绝")
	}
	if _, err := NewAccessibilityGate(1, 1, 0, clock); err == nil {
		t.Fatal("window=0 应拒绝")
	}
}

func TestTelemetryDoesNotAppearInErrorText(t *testing.T) {
	if err := (Telemetry{TotalMS: -1}).Validate(); err == nil || strings.Contains(err.Error(), "SENTINEL") {
		t.Fatal("遥测错误不应包含哨兵或原始内容")
	}
}
