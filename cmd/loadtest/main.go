// Command loadtest 是阶段 5A 的**有界**负载工具：在指定并发与请求数下跑完整验证闭环，
// 分段记录 /api/challenge、/api/redeem、/siteverify 的时延与失败类别。
//
// 它存在的理由：DESIGN §13/§17 要求阶段 5 给出 `[实测]` 吞吐与时延，而"实测"必须是一个
// 别人能原样重跑的东西。它不是压测平台：并发与请求数都有硬上限，跑完就退出——
// 1 vCPU / 1GB 的目标机器上，无界压测只会把被测量对象 itself 打死，数据随之失真。
//
// 两条不得越过的表述边界（AGENTS 红线 14、DESIGN §13）：
//   - PoW 在真实部署里由**访问者浏览器**求解。本工具代算 PoW 只是为了把服务端三段 HTTP
//     的时延单独量出来；它测的是**服务端吞吐形状**，不能据此推断真人解题耗时。
//   - 本工具是开发期代码，`cmd/server` 不得链接它（stage4b.sh 的依赖隔离检查钉住）。
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/shijianus/EpoCHA/internal/pow"
)

const (
	envSecret = "EPOCHA_SITE_SECRET"

	// 硬上限：在 1 vCPU / 1GB 的机器上"再多一点"不是更精确，是让测量对象先崩。
	maxConcurrency = 16
	maxRequests    = 5000

	nonceCeil = 1_000_000_000_000
	powBudget = 30 * time.Second
)

type options struct {
	base        string
	siteKey     string
	secret      string
	concurrency int
	requests    int
	timeout     time.Duration
	quiet       bool
}

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "loadtest: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, stdout io.Writer) error {
	o, err := parseArgs(args, getenv)
	if err != nil {
		return err
	}
	c := &client{base: o.base, http: &http.Client{Timeout: o.timeout}}
	s := drive(context.Background(), c, o, time.Now)
	s.print(stdout, o)
	// 退出码契约：一次成功闭环都没有 ⇒ 非 0。"跑完了但全失败"不能等于"负载测试通过"。
	if s.OK == 0 {
		return fmt.Errorf("没有任何一次闭环成功（失败类别：%s）", s.errClasses())
	}
	return nil
}

func parseArgs(args []string, getenv func(string) string) (*options, error) {
	fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := &options{}
	fs.StringVar(&o.base, "base", "http://127.0.0.1:8080", "服务地址")
	fs.StringVar(&o.siteKey, "site-key", "", "站点 site_key（公开）")
	fs.IntVar(&o.concurrency, "concurrency", 1, "并发闭环数（上限 16）")
	fs.IntVar(&o.requests, "requests", 50, "总闭环数（上限 5000）")
	fs.DurationVar(&o.timeout, "timeout", 10*time.Second, "单次 HTTP 超时")
	fs.BoolVar(&o.quiet, "quiet", false, "只打印一行汇总")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if o.siteKey == "" {
		return nil, errors.New("缺少 --site-key")
	}
	o.secret = getenv(envSecret)
	if o.secret == "" {
		// 缺凭据时不静默跑几百次注定失败的 siteverify：那会把"配置没配对"伪装成"服务很慢"。
		return nil, fmt.Errorf("缺少 %s（siteverify 需要站点 secret）", envSecret)
	}
	if o.concurrency < 1 || o.concurrency > maxConcurrency {
		return nil, fmt.Errorf("--concurrency 必须在 [1,%d]", maxConcurrency)
	}
	if o.requests < 1 || o.requests > maxRequests {
		return nil, fmt.Errorf("--requests 必须在 [1,%d]", maxRequests)
	}
	return o, nil
}

// --- 客户端 ---

type challengeResp struct {
	Token      string          `json:"token"`
	Mode       string          `json:"mode"`
	ExpiresIn  int64           `json:"expires_in"`
	Pow        powTask         `json:"pow"`
	Question   json.RawMessage `json:"question"`
	ErrorCodes []string        `json:"error-codes"`
}

type powTask struct {
	Algo string `json:"algo"`
	Bits uint8  `json:"bits"`
	Seed string `json:"seed"`
}

type redeemResp struct {
	Response   string   `json:"response"`
	ErrorCodes []string `json:"error-codes"`
}

type verifyResp struct {
	Success    bool     `json:"success"`
	ErrorCodes []string `json:"error-codes"`
}

type client struct {
	base string
	http *http.Client
}

// httpStatusError 保留响应体，因为失败**类别**要从 error-codes 里取，
// 而类别是有限集合：把整段响应文本塞进统计等于把请求内容抄进日志（红线 11）。
type httpStatusError struct {
	status int
	body   []byte
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.status) }

func (c *client) post(ctx context.Context, path string, body []byte, contentType string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return &httpStatusError{status: res.StatusCode, body: raw}
	}
	if dst != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("解析 %s 响应: %w", path, err)
		}
	}
	return nil
}

// --- 一次闭环 ---

type stageTimings struct {
	challenge, pow, redeem, verify time.Duration
}

// flow 跑一次 challenge→PoW→redeem→siteverify。返回值 stage 标记失败发生在哪一段。
func (c *client) flow(ctx context.Context, siteKey, secret string) (stageTimings, string, error) {
	var t stageTimings

	t0 := time.Now()
	var ch challengeResp
	body, _ := json.Marshal(map[string]string{"site_key": siteKey})
	if err := c.post(ctx, "/api/challenge", body, "application/json", &ch); err != nil {
		return t, "challenge", err
	}
	t.challenge = time.Since(t0)
	if ch.Token == "" || ch.Pow.Algo == "" {
		return t, "challenge", errors.New("challenge 响应缺少 token/pow")
	}
	if len(ch.Question) > 0 && string(ch.Question) != "null" {
		// 题目负载需要答案（图片题还要 OCR），不属于"服务端三段时延"的测量范围。
		return t, "challenge", errors.New("该站点是 pow_question：本工具只测 pow_only 形状负载")
	}
	if ch.Pow.Algo != pow.Algo {
		return t, "challenge", fmt.Errorf("未知 PoW 算法 %q", ch.Pow.Algo)
	}
	seed, err := base64.RawURLEncoding.DecodeString(ch.Pow.Seed)
	if err != nil || len(seed) == 0 {
		return t, "challenge", errors.New("pow seed 解码失败")
	}

	p0 := time.Now()
	nonce, err := searchNonce(ctx, pow.Params{Algo: ch.Pow.Algo, Bits: ch.Pow.Bits, Seed: seed})
	t.pow = time.Since(p0)
	if err != nil {
		return t, "pow", err
	}

	t1 := time.Now()
	var rr redeemResp
	rbody, _ := json.Marshal(map[string]string{"token": ch.Token, "pow_nonce": nonce})
	if err := c.post(ctx, "/api/redeem", rbody, "application/json", &rr); err != nil {
		return t, "redeem", err
	}
	t.redeem = time.Since(t1)
	if rr.Response == "" {
		return t, "redeem", errors.New("redeem 未返回 response")
	}

	t2 := time.Now()
	var vr verifyResp
	form := []byte("secret=" + url.QueryEscape(secret) + "&response=" + url.QueryEscape(rr.Response))
	if err := c.post(ctx, "/siteverify", form, "application/x-www-form-urlencoded", &vr); err != nil {
		return t, "verify", err
	}
	t.verify = time.Since(t2)
	if !vr.Success {
		return t, "verify", fmt.Errorf("siteverify 未通过 codes=%s", first(vr.ErrorCodes, "-"))
	}
	return t, "", nil
}

// searchNonce 起点随机、线性前进，判定复用 internal/pow 的 Verify：
// 客户端绝不与服务端各写一套哈希规则（与 cmd/devclient 同构）。
func searchNonce(ctx context.Context, p pow.Params) (string, error) {
	start, err := randStart()
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(powBudget)
	for i := int64(0); i < 1<<24; i++ {
		n := start + i
		if n > nonceCeil {
			n -= nonceCeil + 1
		}
		nonce := strconv.FormatInt(n, 10)
		ok, err := p.Verify(nonce)
		if err != nil {
			return "", fmt.Errorf("候选解 %q 被判非法: %w", nonce, err)
		}
		if ok {
			return nonce, nil
		}
		if i%20_000 == 0 {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if time.Now().After(deadline) {
				return "", errors.New("PoW 求解超时")
			}
		}
	}
	return "", errors.New("PoW 搜索超出预算")
}

// randStart 用 crypto/rand.Int 而不是"读字节转 int64 再取模"：
// 后者在最高位为 1 时得到负数，负起点会提交 "-123" 这类服务端判非法的 nonce。
func randStart() (int64, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(nonceCeil))
	if err != nil {
		return 0, err
	}
	return n.Int64(), nil
}

// --- 编排与统计 ---

type summary struct {
	Flows       int
	OK          int
	StageErrors map[string]int // "stage:HTTP状态/error-code" 或 "stage" → 次数
	Challenge   []time.Duration
	Redeem      []time.Duration
	Verify      []time.Duration
	Pow         []time.Duration

	wall         time.Duration
	throughputPS float64
}

func newSummary() *summary { return &summary{StageErrors: map[string]int{}} }

// drive 用固定数量 worker 消费一个已装满的任务队列。now 注入是为了让测试可判定。
func drive(ctx context.Context, c *client, o *options, now func() time.Time) *summary {
	s := newSummary()
	start := now()
	jobs := make(chan int, o.requests)
	for i := 0; i < o.requests; i++ {
		jobs <- i
	}
	close(jobs)

	out := make(chan flowResult, o.requests)
	for range o.concurrency {
		go func() {
			for range jobs {
				out <- c.runOnce(ctx, o)
			}
		}()
	}
	for range o.requests {
		r := <-out
		s.Flows++
		if r.err != nil {
			s.StageErrors[r.classify()]++
			continue
		}
		s.OK++
		s.Challenge = append(s.Challenge, r.t.challenge)
		s.Redeem = append(s.Redeem, r.t.redeem)
		s.Verify = append(s.Verify, r.t.verify)
		s.Pow = append(s.Pow, r.t.pow)
	}
	s.wall = now().Sub(start)
	if s.wall > 0 {
		s.throughputPS = float64(s.Flows) / s.wall.Seconds()
	}
	return s
}

type flowResult struct {
	t     stageTimings
	stage string
	err   error
}

// classify 把失败归成有限类别：HTTP 状态 + 服务端 error-code。不含响应正文。
func (r flowResult) classify() string {
	var he *httpStatusError
	if errors.As(r.err, &he) {
		return fmt.Sprintf("%s:%d/%s", r.stage, he.status, codesOf(he.body))
	}
	return r.stage
}

func (c *client) runOnce(ctx context.Context, o *options) flowResult {
	t, stage, err := c.flow(ctx, o.siteKey, o.secret)
	return flowResult{t: t, stage: stage, err: err}
}

func codesOf(body []byte) string {
	var probe struct {
		ErrorCodes []string `json:"error-codes"`
	}
	if json.Unmarshal(body, &probe) == nil && len(probe.ErrorCodes) > 0 {
		return probe.ErrorCodes[0]
	}
	return "-"
}

// pct 用最近秩（nearest-rank）：不做插值，报告的每个分位数都是真实样本里的值。
func pct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	idx := int(p*float64(len(s)) + 0.999999)
	if idx < 1 {
		idx = 1
	}
	if idx > len(s) {
		idx = len(s)
	}
	return s[idx-1]
}

func maxOf(ds []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range ds {
		if d > m {
			m = d
		}
	}
	return m
}

func (s *summary) errClasses() string {
	if len(s.StageErrors) == 0 {
		return "无"
	}
	keys := make([]string, 0, len(s.StageErrors))
	for k := range s.StageErrors {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += " "
		}
		out += fmt.Sprintf("%s=%d", k, s.StageErrors[k])
	}
	return out
}

func (s *summary) print(w io.Writer, o *options) {
	if o.quiet {
		fmt.Fprintf(w, "flows=%d ok=%d wall=%s rps=%.2f err=%s\n",
			s.Flows, s.OK, s.wall.Round(time.Millisecond), s.throughputPS, s.errClasses())
		return
	}
	line := func(name string, ds []time.Duration) {
		fmt.Fprintf(w, "%-10s n=%d p50=%s p95=%s max=%s\n", name, len(ds),
			pct(ds, 0.50).Round(time.Microsecond), pct(ds, 0.95).Round(time.Microsecond),
			maxOf(ds).Round(time.Microsecond))
	}
	fmt.Fprintf(w, "负载形状 concurrency=%d requests=%d base=%s（凭据不回显）\n", o.concurrency, o.requests, o.base)
	fmt.Fprintf(w, "wall=%s flows=%d ok=%d 吞吐=%.2f 闭环/秒\n",
		s.wall.Round(time.Millisecond), s.Flows, s.OK, s.throughputPS)
	line("challenge", s.Challenge)
	line("redeem", s.Redeem)
	line("siteverify", s.Verify)
	// pow-tool 是**本工具进程内**代算 PoW 的耗时，不是浏览器里真人的解题时间（DESIGN §13 的边界）。
	line("pow-tool", s.Pow)
	if len(s.StageErrors) > 0 {
		fmt.Fprintf(w, "失败类别：%s\n", s.errClasses())
	}
}

func first(s []string, def string) string {
	if len(s) > 0 {
		return s[0]
	}
	return def
}
