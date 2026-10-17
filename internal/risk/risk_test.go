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
