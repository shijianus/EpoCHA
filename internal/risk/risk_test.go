package risk

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

func newTestLimiter(t *testing.T, rpm, maxStates int) *Limiter {
	t.Helper()
	l, err := NewLimiter(rpm, maxStates)
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}
	return l
}

func TestLimiterBasicWindows(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newTestLimiter(t, 3, 100)
	l.now = func() time.Time { return now }

	for i := 1; i <= 3; i++ {
		if !l.Allow("k") {
			t.Fatalf("第 %d 次应放行", i)
		}
	}
	if l.Allow("k") {
		t.Fatal("第 4 次应被限流")
	}
	if ra := l.RetryAfter("k"); ra <= 0 || ra > windowSize {
		t.Fatalf("RetryAfter = %v，应在 (0, 1min]", ra)
	}
	// 窗口滚动后恢复
	now = now.Add(windowSize)
	if !l.Allow("k") {
		t.Fatal("新窗口应恢复放行")
	}
	// 不同 key 互不影响
	for range 3 {
		if !l.Allow("other") {
			t.Fatal("other 不应被 k 的配额影响")
		}
	}
	if l.Allow("other") {
		t.Fatal("other 自身配额应已用尽")
	}
	if l.RetryAfter("never-seen") != 0 {
		t.Fatal("未见过的 key 无需等待")
	}
}

// 随机源地址不能把状态表撑大（1GB 机器的生存条件）。
func TestLimiterStateIsBounded(t *testing.T) {
	const maxStates = 128
	l := newTestLimiter(t, 1000, maxStates)
	for i := range 50_000 {
		l.Allow(fmt.Sprintf("v4:10.0.%d.%d", i/254, i%254))
	}
	if l.Len() > maxStates {
		t.Fatalf("状态数 %d 超过上限 %d", l.Len(), maxStates)
	}
	if l.Evictions() == 0 {
		t.Fatal("未发生淘汰，上限形同虚设")
	}
}

// 淘汰必须让合法用户重新获得配额，而不是留下永久封禁。
func TestEvictionRestoresQuota(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newTestLimiter(t, 2, 4)
	l.now = func() time.Time { return now }
	for i := range 4 {
		l.Allow(fmt.Sprintf("attacker-%d", i))
	}
	// 全在同一窗口：再插入新 key 会挤掉最旧窗口的条目；此处都是当前窗口，走"淘汰任一"路径
	if !l.Allow("victim") {
		t.Fatal("victim 应被放行（配额 2）")
	}
	before := l.Len()
	if before > 4 {
		t.Fatalf("状态数 %d 超过上限", before)
	}
	// 窗口滚动：过期条目应被回收
	l.now = func() time.Time { return now.Add(windowSize) }
	l.Allow("victim")
	for i := range 10 {
		l.Allow(fmt.Sprintf("new-%d", i))
	}
	if l.Len() > 4 {
		t.Fatalf("窗口滚动后状态数 %d 超过上限", l.Len())
	}
}

func TestLimiterRejectsBadConfig(t *testing.T) {
	if _, err := NewLimiter(0, 10); !errors.Is(err, ErrBadConfig) {
		t.Errorf("rpm=0 应拒绝，got %v", err)
	}
	if _, err := NewLimiter(10, 0); !errors.Is(err, ErrBadConfig) {
		t.Errorf("max_states=0 应拒绝，got %v", err)
	}
	if _, err := NewLimiter(-1, 5); !errors.Is(err, ErrBadConfig) {
		t.Errorf("负 rpm 应拒绝，got %v", err)
	}
}

func request(remoteAddr string, headers map[string]string) *http.Request {
	r := &http.Request{Method: http.MethodPost, RemoteAddr: remoteAddr, Header: http.Header{}}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestClientIPIgnoysSpoofedHeadersByDefault(t *testing.T) {
	s, err := NewSource(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := request("203.0.113.9:5555", map[string]string{
		"CF-Connecting-IP": "198.51.100.7",
		"X-Forwarded-For":  "192.0.2.1, 192.0.2.2",
	})
	if got := s.ClientIP(r); got.String() != "203.0.113.9" {
		t.Fatalf("默认应只信连接地址，got %s", got)
	}
}

func TestClientIPTrustedProxyOnly(t *testing.T) {
	s, err := NewSource(true, []string{"198.51.100.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	// 来自受信代理：采信 CF-Connecting-IP
	fromProxy := request("198.51.100.20:443", map[string]string{"CF-Connecting-IP": "203.0.113.5"})
	if got := s.ClientIP(fromProxy); got.String() != "203.0.113.5" {
		t.Fatalf("受信代理应采信 CF-Connecting-IP，got %s", got)
	}
	// 来自受信代理：CF 头缺失时取 XFF 最右（由该代理追加的一跳）
	xff := request("198.51.100.20:443", map[string]string{"X-Forwarded-For": "192.0.2.1, 203.0.113.6"})
	if got := s.ClientIP(xff); got.String() != "203.0.113.6" {
		t.Fatalf("应取 XFF 最右值，got %s", got)
	}
	// 来自未受信地址：伪造头一律忽略
	liar := request("203.0.113.99:5555", map[string]string{"CF-Connecting-IP": "198.51.100.20"})
	if got := s.ClientIP(liar); got.String() != "203.0.113.99" {
		t.Fatalf("未受信来源应忽略伪造头，got %s", got)
	}
	// 头里是垃圾：回落连接地址
	junk := request("198.51.100.20:443", map[string]string{"CF-Connecting-IP": "not-an-ip", "X-Forwarded-For": "also-junk"})
	if got := s.ClientIP(junk); got.String() != "198.51.100.20" {
		t.Fatalf("非法头应回落连接地址，got %s", got)
	}
	// 空 / 缺失 RemoteAddr
	if got := s.ClientIP(request("", nil)); got != nil {
		t.Fatalf("空 RemoteAddr 应返回 nil，got %s", got)
	}
}

func TestSourceConfigValidation(t *testing.T) {
	if _, err := NewSource(true, nil); !errors.Is(err, ErrBadConfig) {
		t.Error("trust_proxy 开启但无受信网段应拒绝")
	}
	if _, err := NewSource(true, []string{"not-a-cidr"}); err == nil {
		t.Error("非法 CIDR 应报错")
	}
	s, err := NewSource(false, []string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.ClientIP(request("10.0.0.1:1", map[string]string{"X-Forwarded-For": "8.8.8.8"})); got.String() != "10.0.0.1" {
		t.Error("trust_proxy=false 时即便配了网段也不采信头")
	}
}

func TestKeyAggregation(t *testing.T) {
	v4a := Key(net.ParseIP("203.0.113.7"))
	v4b := Key(net.ParseIP("203.0.113.200"))
	if v4a != v4b {
		t.Errorf("同 /24 应聚合成同一 key：%s vs %s", v4a, v4b)
	}
	if Key(net.ParseIP("203.0.112.7")) == v4a {
		t.Error("不同 /24 不应共用 key")
	}
	v6a := Key(net.ParseIP("2001:db8:1234:5678::1"))
	v6b := Key(net.ParseIP("2001:db8:1234:5678::ffff"))
	if v6a != v6b {
		t.Errorf("同 /64 应聚合：%s vs %s", v6a, v6b)
	}
	if Key(net.ParseIP("2001:db8:1234:5679::1")) == v6a {
		t.Error("不同 /64 不应共用 key")
	}
	if Key(nil) != "unknown" {
		t.Error("nil 地址应有确定 key")
	}
}

// 并发命中同一 key：计数不得丢失或超发（-race 下的基本健全性）。
func TestLimiterConcurrentAccess(t *testing.T) {
	l := newTestLimiter(t, 100, 1000)
	done := make(chan struct{})
	go func() {
		for range 200 {
			l.Allow("shared")
		}
		close(done)
	}()
	for range 200 {
		l.Allow("shared")
		_ = l.RetryAfter("shared")
	}
	<-done
	if l.Len() != 1 {
		t.Fatalf("状态数 = %d，应为 1", l.Len())
	}
}

// TestLimiterWindowIsNotPerSecond 是阶段 5B 抓到的真实缺陷的回归测试。
//
// 配置项叫 EPOCHA_RATE_IP_RPM / rate_site_rpm，`RetryAfter` 也按 windowSize(=1 分钟) 报剩余时间，
// 但窗口分桶一度是 `now().Unix()`（**每秒**一个桶）。后果不是"计数误差"，而是限流比声称的松 60 倍：
// 60 RPM 实际允许 60 次/秒 ≈ 3600 次/分钟。
//
// 旧的 TestLimiterBasicWindows 检测不到它：它把时钟一次推进整个 windowSize，
// 那一秒当然也变了桶。这里必须推进"1 秒但不足一个窗口"，才分得开两种语义。
func TestLimiterWindowIsNotPerSecond(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newTestLimiter(t, 3, 100)
	l.now = func() time.Time { return now }

	for i := 1; i <= 3; i++ {
		if !l.Allow("k") {
			t.Fatalf("第 %d 次应放行", i)
		}
	}
	if l.Allow("k") {
		t.Fatal("前置不成立：第 4 次竟然被放行")
	}
	// 推进 1 秒：分钟窗口**没走完**，配额不得恢复。
	now = now.Add(time.Second)
	if l.Allow("k") {
		t.Errorf("推进 1 秒后配额被重置——窗口其实是 1 秒，而配置与 RetryAfter 都按 1 分钟声称（rpm=%d 实际等于 %.0f 次/分钟）",
			l.rpm, float64(l.rpm)*60)
	}
	// 走到窗口边界才允许恢复。
	now = now.Add(windowSize)
	if !l.Allow("k") {
		t.Error("跨过一个完整窗口后应恢复放行")
	}
}

// TestLimiterBucketBoundaryIsStable 钉住"同一个窗口内不论跨几秒都算同一窗口"：
// 分桶必须按窗口对齐，不能按墙上时钟的秒数对齐（后者还会让窗口在 59 秒与 1 秒之间抖动）。
func TestLimiterBucketBoundaryIsStable(t *testing.T) {
	l := newTestLimiter(t, 2, 100)
	// 从一个窗口的中后段开始，跨过秒边界但仍处于同一分钟窗口。
	start := time.Unix(1_700_000_059, 0)
	now := start
	l.now = func() time.Time { return now }
	if !l.Allow("k") || !l.Allow("k") {
		t.Fatal("前两次应在配额内")
	}
	if l.Allow("k") {
		t.Error("第 3 次应被限流")
	}
	now = start.Add(30 * time.Second) // 仍在同一窗口内（该窗口从 :00 起算）
	if l.Allow("k") {
		t.Error("同一窗口内的 30 秒之后配额不应恢复")
	}
	now = start.Add(windowSize) // 整窗口滚动
	if !l.Allow("k") {
		t.Error("整窗口滚动后应恢复")
	}
}
