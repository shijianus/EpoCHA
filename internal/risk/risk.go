// Package risk 提供阶段 1 的滥用防护：有界限流与来源地址裁决（DESIGN §9、§14-8/9）。
//
// 风险打分不在本阶段：它的阈值依赖尚未测量的时延分布（ADR-006），实现只能凭猜。
package risk

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 限流窗口固定为 1 分钟，语义与配置项 RPM 一致。
const windowSize = time.Minute

var ErrBadConfig = errors.New("risk: 配置不合法")

// Limiter 是按 key 的固定窗口计数器，状态条目数有硬上限。
//
// 上限是生存性问题而非优化：1GB 内存的机器上，攻击者用随机源地址制造 key，
// 无上限的 map 会让进程先于业务被击穿（DESIGN §14-9）。
type Limiter struct {
	mu        sync.Mutex
	states    map[string]*counter
	rpm       int
	maxStates int
	now       func() time.Time
	evictions int
}

type counter struct {
	bucket int64
	count  int
}

// NewLimiter 构造限流器。rpm 与 maxStates 必须为正。
func NewLimiter(rpm, maxStates int) (*Limiter, error) {
	if rpm <= 0 {
		return nil, fmt.Errorf("%w: rpm=%d", ErrBadConfig, rpm)
	}
	if maxStates <= 0 {
		return nil, fmt.Errorf("%w: max_states=%d", ErrBadConfig, maxStates)
	}
	return &Limiter{
		states:    make(map[string]*counter, 64),
		rpm:       rpm,
		maxStates: maxStates,
		now:       time.Now,
	}, nil
}

// Allow 用构造时的默认速率计数。
func (l *Limiter) Allow(key string) bool { return l.AllowN(key, l.rpm) }

// AllowN 按调用方给定的速率计数——阶段 2 的 rpm 是**站点级**配置，
// 而限流器只需要关心"状态条目有界"这一件事，两者因此分开。
// rpm<=0 视为配置错误：宁可拒绝放行也不能变成"不限流"。
func (l *Limiter) AllowN(key string, rpm int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if rpm <= 0 {
		return false
	}
	cur := l.bucket()
	c, ok := l.states[key]
	if !ok {
		l.ensureRoom(cur)
		c = &counter{bucket: cur}
		l.states[key] = c
	}
	if c.bucket != cur {
		c.bucket, c.count = cur, 0
	}
	c.count++
	return c.count <= rpm
}

// RetryAfter 返回该 key 建议等待时长；未超限也返回窗口剩余时间，便于调用方设置表头。
func (l *Limiter) RetryAfter(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.states[key]
	if !ok {
		return 0
	}
	// 用注入的时钟而非 time.Since：限流的窗口判定必须与 Allow 同一时间源。
	elapsed := l.now().Sub(bucketStart(c.bucket))
	if elapsed >= windowSize {
		return 0
	}
	return windowSize - elapsed
}

// Len 与 Evictions 供测试断言状态确实有界。
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.states)
}

func (l *Limiter) Evictions() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.evictions
}

// bucket 返回**按窗口对齐**的窗口序号，不是墙上时钟的秒数。
//
// 这是一次真实缺陷的修复：这里曾经是 `now().Unix()`，而 windowSize 是一分钟——
// 于是计数器每秒就换窗口，"60 RPM"实际允许 60 次/秒（≈3600 次/分钟），
// 而配置名、RetryAfter 与 DESIGN §9 说的都是一分钟窗口。限流这类控件最怕的就是
// "代码里的语义比文档松一个数量级"，而且它只会体现在真实负载下，单测推进整个窗口时看不出来。
func (l *Limiter) bucket() int64 {
	return l.now().Unix() / int64(windowSize/time.Second)
}

// bucketStart 把一个窗口序号换回它的起始时刻（RetryAfter 需要的是"这个窗口什么时候开始"）。
// 少了这一步、直接把序号当 Unix 秒用，窗口就会变成 1970 年附近的一个时间点。
func bucketStart(bucket int64) time.Time {
	return time.Unix(bucket*int64(windowSize/time.Second), 0)
}

// ensureRoom 在插入新 key 前腾出空间：先清掉过期窗口，仍满则淘汰最旧窗口。
// 被淘汰的 key 只是重新计数，不会造成"攻击者占满状态就永久封禁合法用户"的反向效果。
func (l *Limiter) ensureRoom(cur int64) {
	if len(l.states) < l.maxStates {
		return
	}
	for k, c := range l.states {
		if c.bucket != cur {
			delete(l.states, k)
			l.evictions++
		}
	}
	if len(l.states) < l.maxStates {
		return
	}
	var (
		oldestKey string
		oldest    int64 = -1
	)
	for k, c := range l.states {
		if oldest < 0 || c.bucket < oldest {
			oldest, oldestKey = c.bucket, k
		}
	}
	if oldestKey != "" {
		delete(l.states, oldestKey)
		l.evictions++
	}
}

// Source 裁决"这个请求的真实来源地址是什么"。
//
// 默认只信 TCP 连接地址。X-Forwarded-For / CF-Connecting-IP 是客户端可自写的头，
// 只有当连接确实来自配置的受信代理网段时才采信（DESIGN §14-8）。
type Source struct {
	trustProxy bool
	nets       []*net.IPNet
}

// NewSource 构造裁决器。cidrs 为空表示"不信任任何代理"（即便 trustProxy 为真也只用自己的 RemoteAddr）。
func NewSource(trustProxy bool, cidrs []string) (*Source, error) {
	s := &Source{trustProxy: trustProxy}
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		_, network, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("risk: 受信代理网段 %q: %w", c, err)
		}
		s.nets = append(s.nets, network)
	}
	if trustProxy && len(s.nets) == 0 {
		return nil, fmt.Errorf("%w: trust_proxy 开启但未配置受信代理网段", ErrBadConfig)
	}
	return s, nil
}

// ClientIP 返回可用于限流与审计的来源地址。
func (s *Source) ClientIP(r *http.Request) net.IP {
	remote := ipFromAddr(r.RemoteAddr)
	if remote == nil {
		return nil
	}
	if !s.trustProxy || !s.isTrusted(remote) {
		return remote
	}
	if ip := parseIPHeader(r.Header.Get("CF-Connecting-IP")); ip != nil {
		return ip
	}
	// XFF 是逗号链表，链上最左值由最初客户端控制；这里取最右一个由可信代理追加的值。
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if ip := parseIPHeader(parts[len(parts)-1]); ip != nil {
			return ip
		}
	}
	return remote
}

// Key 把地址规范成限流 key（IPv4 按 /24、IPv6 按 /64 聚合，避免单个 NAT 后用户被逐个封禁）。
func Key(ip net.IP) string {
	if ip == nil {
		return "unknown"
	}
	if v4 := ip.To4(); v4 != nil {
		return fmt.Sprintf("v4:%d.%d.%d.0", v4[0], v4[1], v4[2])
	}
	if v6 := ip.To16(); v6 != nil {
		return fmt.Sprintf("v6:%x", v6[:8]) // /64 前缀，后 8 字节丢弃
	}
	return "ip:" + ip.String()
}

func (s *Source) isTrusted(ip net.IP) bool {
	for _, n := range s.nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func parseIPHeader(v string) net.IP {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return net.ParseIP(v)
}

func ipFromAddr(addr string) net.IP {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return net.ParseIP(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")))
}
