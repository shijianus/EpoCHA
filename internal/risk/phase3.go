package risk

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Phase 3 的风险状态：刷新惩罚、遥测摘要与无障碍闸门（ADR-026/027/029）。
//
// 这些状态都只在进程内、按 source+site 聚合，且有条目上限；它们不是防重放账本，
// 重启丢失是可接受的。任何返回值都只影响"下一次怎么出题"，不改变当前显式判定。

const (
	RefreshWindow         = 10 * time.Minute
	AccessibilityWindow   = 24 * time.Hour
	AccessibilityLimit    = 1
	MaxTrackedSources     = 4096
	MaxTelemetryEvents    = 10000
	MaxTelemetryTotalMS   = 300000
	MaxTelemetryFocusBlur = 20
)

var ErrPhase3Config = errors.New("risk: 风控状态配置不合法")

// RefreshPenalty 是刷新窗口的单调惩罚。FloorDelta/PoWDelta 只会增大，不会降低。
type RefreshPenalty struct {
	FloorDelta int
	PoWDelta   int
	Cooldown   time.Duration
	Score      int
}

type refreshState struct {
	bucket        int64
	count         int
	floorDelta    int
	powDelta      int
	score         int
	cooldownUntil time.Time
	last          time.Time
}

// RefreshTracker 按 key 统计 10 分钟窗口内的刷新次数。状态有硬上限。
type RefreshTracker struct {
	mu        sync.Mutex
	states    map[string]*refreshState
	maxStates int
	now       func() time.Time
}

func NewRefreshTracker(maxStates int, now func() time.Time) (*RefreshTracker, error) {
	if maxStates < 1 {
		return nil, fmt.Errorf("%w: maxStates=%d", ErrPhase3Config, maxStates)
	}
	if now == nil {
		now = time.Now
	}
	return &RefreshTracker{states: make(map[string]*refreshState), maxStates: maxStates, now: now}, nil
}

// Observe 必须在每次成功签发 challenge 前调用。它只记录与返回惩罚，不阻塞请求；
// 冷却由调用方按 RetryAfter 决定是否返回 429。
func (t *RefreshTracker) Observe(key string) RefreshPenalty {
	if t == nil {
		return RefreshPenalty{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now().UTC()
	bucket := now.Unix() / int64(RefreshWindow/time.Second)
	st, ok := t.states[key]
	if !ok || st.bucket != bucket {
		if !ok {
			t.ensureRoom(bucket)
		}
		st = &refreshState{bucket: bucket}
		t.states[key] = st
	}
	st.count++
	st.last = now
	switch {
	case st.count <= 2:
		// 正常浏览/换一张，不加压。
	case st.count <= 5:
		st.floorDelta = maxInt(st.floorDelta, 1)
		st.powDelta = maxInt(st.powDelta, 2)
		st.score = maxInt(st.score, 25+5*(st.count-3))
	case st.count <= 9:
		st.floorDelta = maxInt(st.floorDelta, 2)
		st.powDelta = maxInt(st.powDelta, 4)
		st.score = maxInt(st.score, 55+5*(st.count-6))
		// 只在跨过阈值时启动冷却；否则每次重试都会把冷却续期，窗口内永远无法恢复。
		if st.count == 6 {
			st.cooldownUntil = now.Add(3 * time.Second)
		}
	default:
		st.floorDelta = maxInt(st.floorDelta, 3)
		st.powDelta = maxInt(st.powDelta, 6)
		st.score = maxInt(st.score, 100)
		if st.count == 10 {
			st.cooldownUntil = now.Add(15 * time.Minute)
		}
	}
	retry := time.Duration(0)
	if now.Before(st.cooldownUntil) {
		retry = st.cooldownUntil.Sub(now)
	}
	return RefreshPenalty{
		FloorDelta: st.floorDelta, PoWDelta: st.powDelta,
		Cooldown: retry, Score: st.score,
	}
}

func (t *RefreshTracker) RetryAfter(key string) time.Duration {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.states[key]
	if !ok {
		return 0
	}
	now := t.now().UTC()
	if now.Before(st.cooldownUntil) {
		return st.cooldownUntil.Sub(now)
	}
	return 0
}

func (t *RefreshTracker) Len() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.states)
}

func (t *RefreshTracker) ensureRoom(bucket int64) {
	if len(t.states) < t.maxStates {
		return
	}
	for k, st := range t.states {
		if st.bucket != bucket {
			delete(t.states, k)
		}
	}
	if len(t.states) < t.maxStates {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, st := range t.states {
		if oldestKey == "" || st.last.Before(oldest) {
			oldestKey, oldest = k, st.last
		}
	}
	if oldestKey != "" {
		delete(t.states, oldestKey)
	}
}

// Telemetry 是 Widget 上报的摘要，不是轨迹。字段与 ADR-027 一一对应。
type Telemetry struct {
	TotalMS       int `json:"total_ms"`
	FirstInputMS  int `json:"first_input_ms"`
	EventCount    int `json:"event_count"`
	PointerEvents int `json:"pointer_events"`
	KeyEvents     int `json:"key_events"`
	FocusBlur     int `json:"focus_blur"`
}

// Validate 只做边界校验；畸形遥测在消费 challenge 之前拒绝。
func (t Telemetry) Validate() error {
	switch {
	case t.TotalMS < 0 || t.TotalMS > MaxTelemetryTotalMS:
		return fmt.Errorf("%w: total_ms=%d", ErrPhase3Config, t.TotalMS)
	case t.FirstInputMS < 0 || t.FirstInputMS > t.TotalMS:
		return fmt.Errorf("%w: first_input_ms=%d", ErrPhase3Config, t.FirstInputMS)
	case t.EventCount < 0 || t.EventCount > MaxTelemetryEvents:
		return fmt.Errorf("%w: event_count=%d", ErrPhase3Config, t.EventCount)
	case t.PointerEvents < 0 || t.KeyEvents < 0 || t.PointerEvents+t.KeyEvents > t.EventCount:
		return fmt.Errorf("%w: pointer/key events", ErrPhase3Config)
	case t.FocusBlur < 0 || t.FocusBlur > MaxTelemetryFocusBlur:
		return fmt.Errorf("%w: focus_blur=%d", ErrPhase3Config, t.FocusBlur)
	}
	return nil
}

// AnomalyScore 是弱证据，0..20；任何单一特征都不足以拒绝正确回答。
func (t Telemetry) AnomalyScore() int {
	score := 0
	if t.TotalMS > 0 && t.TotalMS < 800 {
		score += 10
	}
	if t.FirstInputMS >= 0 && t.FirstInputMS < 80 {
		score += 5
	}
	if t.EventCount == 0 {
		score += 5
	}
	if t.PointerEvents+t.KeyEvents == 0 {
		score += 5
	}
	if t.FocusBlur > 3 {
		score += 3
	}
	if score > 20 {
		score = 20
	}
	return score
}

type sessionState struct {
	failures  int
	telemetry int
	last      time.Time
}

// Tracker 合并刷新/失败/遥测三类弱信号，返回 0..100 风险分。
type Tracker struct {
	refresh   *RefreshTracker
	mu        sync.Mutex
	states    map[string]*sessionState
	maxStates int
	now       func() time.Time
}

func NewTracker(maxStates int, now func() time.Time) (*Tracker, error) {
	if maxStates < 1 {
		return nil, fmt.Errorf("%w: maxStates=%d", ErrPhase3Config, maxStates)
	}
	if now == nil {
		now = time.Now
	}
	r, err := NewRefreshTracker(maxStates, now)
	if err != nil {
		return nil, err
	}
	return &Tracker{refresh: r, states: make(map[string]*sessionState), maxStates: maxStates, now: now}, nil
}

// Assess 在签发前调用；返回风险分与刷新惩罚。它不会改变当前题。
func (t *Tracker) Assess(key string, base int) (int, RefreshPenalty) {
	if t == nil {
		return clampScore(base), RefreshPenalty{}
	}
	p := t.refresh.Observe(key)
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.ensureState(key)
	score := clampScore(base + p.Score + st.failures*10 + st.telemetry)
	return score, p
}

// ObserveRedeem 在显式判定已经提交后调用。success 是显式答案结果，telemetry 不参与判定。
// 返回更新后的风险分，仅供下一次调度使用；当前响应形状不暴露它。
func (t *Tracker) ObserveRedeem(key string, success bool, tel *Telemetry) int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.ensureState(key)
	anomaly := 0
	if tel != nil {
		anomaly = tel.AnomalyScore()
	}
	if success {
		if anomaly > st.telemetry {
			st.telemetry = anomaly
		}
		if st.failures > 0 {
			st.failures--
		}
		if anomaly == 0 && st.telemetry > 0 {
			if st.telemetry >= 2 {
				st.telemetry -= 2
			} else {
				st.telemetry = 0
			}
		}
	} else {
		if st.failures < 5 {
			st.failures++
		}
		if anomaly > st.telemetry {
			st.telemetry = anomaly
		}
	}
	return clampScore(st.failures*10 + st.telemetry)
}

func (t *Tracker) RetryAfter(key string) time.Duration {
	if t == nil {
		return 0
	}
	return t.refresh.RetryAfter(key)
}

func (t *Tracker) ensureState(key string) *sessionState {
	if st, ok := t.states[key]; ok {
		st.last = t.now().UTC()
		return st
	}
	if len(t.states) >= t.maxStates {
		var oldestKey string
		var oldest time.Time
		for k, st := range t.states {
			if oldestKey == "" || st.last.Before(oldest) {
				oldestKey, oldest = k, st.last
			}
		}
		if oldestKey != "" {
			delete(t.states, oldestKey)
		}
	}
	st := &sessionState{last: t.now().UTC()}
	t.states[key] = st
	return st
}

// AccessibilityGate 是无障碍通道的 24h 限额；一次签发即占用一个名额。
type AccessibilityGate struct {
	mu        sync.Mutex
	states    map[string]accessState
	maxStates int
	limit     int
	window    time.Duration
	now       func() time.Time
}

type accessState struct {
	start time.Time
	count int
}

func NewAccessibilityGate(maxStates, limit int, window time.Duration, now func() time.Time) (*AccessibilityGate, error) {
	if maxStates < 1 || limit < 1 || window <= 0 {
		return nil, fmt.Errorf("%w: gate config", ErrPhase3Config)
	}
	if now == nil {
		now = time.Now
	}
	return &AccessibilityGate{states: make(map[string]accessState), maxStates: maxStates, limit: limit, window: window, now: now}, nil
}

// Allow 占用一次无障碍签发名额；false 表示本次不得签发（调用方返回 429）。
func (g *AccessibilityGate) Allow(key string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now().UTC()
	st, ok := g.states[key]
	if !ok {
		g.ensureRoom(now)
		st = accessState{start: now}
	}
	if now.Sub(st.start) >= g.window {
		st = accessState{start: now}
	}
	if st.count >= g.limit {
		g.states[key] = st
		return false
	}
	st.count++
	g.states[key] = st
	return true
}

func (g *AccessibilityGate) RetryAfter(key string) time.Duration {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.states[key]
	if !ok {
		return 0
	}
	if d := g.window - g.now().UTC().Sub(st.start); d > 0 && st.count >= g.limit {
		return d
	}
	return 0
}

func (g *AccessibilityGate) ensureRoom(now time.Time) {
	if len(g.states) < g.maxStates {
		return
	}
	for k, st := range g.states {
		if now.Sub(st.start) >= g.window {
			delete(g.states, k)
		}
	}
	if len(g.states) < g.maxStates {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, st := range g.states {
		if oldestKey == "" || st.start.Before(oldest) {
			oldestKey, oldest = k, st.start
		}
	}
	if oldestKey != "" {
		delete(g.states, oldestKey)
	}
}

func clampScore(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
