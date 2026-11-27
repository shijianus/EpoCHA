// Command devclient 是开发与验收客户端：真实暴力求解 PoW，驱动 challenge → redeem → siteverify。
//
// 它存在的意义是让端到端验收可复现：没有写死的解、没有绕过验证的分支，
// 每次运行都重新搜索（DESIGN §17 阶段 1）。站点 secret 只从环境变量读取，不走 argv（argv 在 ps 里可见）。
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
	"strconv"
	"strings"
	"time"

	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/pow"
)

const envSecret = "EPOCHA_SITE_SECRET"

type challengeResp struct {
	Token     string `json:"token"`
	ExpiresIn int64  `json:"expires_in"`
	Pow       struct {
		Algo string `json:"algo"`
		Seed string `json:"seed"`
		Bits uint8  `json:"bits"`
	} `json:"pow"`
	// 阶段 3A：站点是 pow_question 时服务端会带公开题面。
	Mode       string         `json:"mode"`
	Question   map[string]any `json:"question"`
	ErrorCodes []string       `json:"error-codes"`
}

func (c challengeResp) codes() string { return strings.Join(c.ErrorCodes, ",") }

// answer 按题面求值。返回空串表示"这道响应里没有题"（pow_only 路径）。
// 求值器是 devclient 自己的实现：验收脚本要求"真实求解"，所以既不能读库里的答案，
// 也不能有一个硬编码的固定解。
func (c challengeResp) answer() (string, error) {
	if c.Question == nil {
		return "", nil
	}
	task, _ := c.Question["task"].(string)
	if task == "" {
		return "", errors.New("响应带 question 却没有 task 字段")
	}
	v, err := evalExpr(task)
	if err != nil {
		return "", fmt.Errorf("求解题面 %q: %w", task, err)
	}
	return strconv.FormatInt(v, 10), nil
}

type redeemResp struct {
	Response   string   `json:"response"`
	ExpiresIn  int64    `json:"expires_in"`
	ErrorCodes []string `json:"error-codes"`
}

func (r redeemResp) codes() string {
	if len(r.ErrorCodes) == 0 {
		return "-"
	}
	return strings.Join(r.ErrorCodes, ",")
}

type verifyResp struct {
	Success     bool     `json:"success"`
	ChallengeTS string   `json:"challenge_ts"`
	Hostname    string   `json:"hostname"`
	ErrorCodes  []string `json:"error-codes"`
}

func (v verifyResp) codes() string {
	if len(v.ErrorCodes) == 0 {
		return "-"
	}
	return strings.Join(v.ErrorCodes, ",")
}

func main() {
	if err := run(os.Args[1:], os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "devclient: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string) error {
	fs := flag.NewFlagSet("devclient", flag.ContinueOnError)
	base := fs.String("base", "http://127.0.0.1:8080", "服务地址")
	siteKey := fs.String("site-key", "", "站点 site_key（公开）")
	mode := fs.String("mode", "full", "full|double-redeem|double-verify|wrong-secret|acquire|redeem-only|wrong-answer|verify-only")
	file := fs.String("response-file", "", "acquire 写入 / verify 读取的 response 文件")
	hostname := fs.String("hostname", "", "siteverify 自报 hostname（阶段 2 起服务端不采信，仅用于验证它不会被回显）")
	origin := fs.String("origin", "", "浏览器 Origin 头（阶段 2 来源策略；留空表示无来源的直接调用）")
	maxIter := fs.Int64("max-iterations", 50_000_000, "PoW 搜索迭代上限")
	answerFile := fs.String("answer-file", "", "答案文件（图片题专用：devseed 注入的题才有配对的已知答案）")
	timeout := fs.Duration("timeout", 30*time.Second, "单次 HTTP 超时")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *siteKey == "" {
		return errors.New("缺少 --site-key")
	}
	c := &client{base: strings.TrimRight(*base, "/"), origin: *origin, http: &http.Client{Timeout: *timeout}}
	if *answerFile != "" {
		raw, err := os.ReadFile(*answerFile)
		if err != nil {
			return fmt.Errorf("读答案文件: %w", err)
		}
		v := strings.TrimSpace(string(raw))
		if v == "" || len(v) > 64 {
			return errors.New("答案文件内容为空或过长")
		}
		c.answerOverride = v
	}

	switch *mode {
	case "full":
		return c.full(*siteKey, getenv, *maxIter, *hostname)
	case "double-redeem":
		return c.doubleRedeem(*siteKey, *maxIter)
	case "double-verify":
		return c.doubleVerify(*siteKey, getenv, *maxIter)
	case "wrong-secret":
		return c.wrongSecret(*siteKey, getenv, *maxIter)
	case "acquire":
		return c.acquire(*siteKey, getenv, *maxIter, *file)
	case "redeem-only":
		return c.redeemOnly(*siteKey, *maxIter, *file)
	case "wrong-answer":
		return c.wrongAnswer(*siteKey, *maxIter)
	case "verify-only":
		return c.verifyOnly(getenv, *file, "failure")
	default:
		return fmt.Errorf("未知 mode %q", *mode)
	}
}

type client struct {
	base   string
	origin string // 非空时每个请求都带上该 Origin，用于演示白名单放行路径
	http   *http.Client
	// answerOverride 只服务于"图片题的验收"：图片里没有可机读的文本层，
	// 客户端**不可能**独立求出答案（那需要 OCR）。所以答案来自 devseed 注入时
	// 写下的本地文件——它配的是自己刚生成的那道题，不是"读出已有题的答案"。
	// 算术题路径不受它影响：能独立求值的一律独立求值（DESIGN §17 阶段 3A 的要求）。
	answerOverride string
}

func (c *client) challenge(siteKey string) (challengeResp, error) {
	var out challengeResp
	body, _ := json.Marshal(map[string]string{"site_key": siteKey})
	status, err := c.post("/api/challenge", body, "application/json", &out)
	if err != nil {
		return out, err
	}
	// 不检查状态码就会把"领题失败"变成一句看不懂的"未知 PoW 算法"：
	// 空字段是失败的结果，不是响应形状。
	if status != http.StatusOK || out.Token == "" || out.Pow.Algo == "" {
		return out, fmt.Errorf("领题失败：status=%d codes=%s mode=%s", status, out.codes(), out.Mode)
	}
	return out, nil
}

func (c *client) redeem(tok, nonce, answer string) (redeemResp, int, error) {
	var out redeemResp
	payload := map[string]string{"token": tok, "pow_nonce": nonce}
	if answer != "" {
		payload["answer"] = answer
	}
	body, _ := json.Marshal(payload)
	status, err := c.post("/api/redeem", body, "application/json", &out)
	return out, status, err
}

func (c *client) verify(secret, response, hostname string) (verifyResp, error) {
	var out verifyResp
	form := url.Values{"secret": {secret}, "response": {response}}
	if hostname != "" {
		form.Set("hostname", hostname)
	}
	_, err := c.post("/siteverify", []byte(form.Encode()), "application/x-www-form-urlencoded", &out)
	return out, err
}

func (c *client) postJSON(path string, body []byte, dst any) error {
	_, err := c.post(path, body, "application/json", dst)
	return err
}

func (c *client) post(path string, body []byte, contentType string, dst any) (int, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", contentType)
	if c.origin != "" {
		req.Header.Set("Origin", c.origin)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("请求 %s: %w", path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return res.StatusCode, err
	}
	if dst != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, dst); err != nil {
			return res.StatusCode, fmt.Errorf("解析 %s 响应: %w", path, err)
		}
	}
	return res.StatusCode, nil
}

// --- 各模式 ---

func (c *client) full(siteKey string, getenv func(string) string, maxIter int64, hostname string) error {
	ch, iterations, err := c.solve(siteKey, maxIter)
	if err != nil {
		return err
	}
	fmt.Printf("RESULT step=challenge status=ok bits=%d iterations=%d\n", ch.Pow.Bits, iterations)
	r, status, err := c.redeem(ch.Token, ch.nonce, ch.answer)
	if err != nil {
		return err
	}
	if status != http.StatusOK || r.Response == "" {
		return fmt.Errorf("redeem 未成功：status=%d codes=%s", status, r.codes())
	}
	v, err := c.verify(getenv(envSecret), r.Response, hostname)
	if err != nil {
		return err
	}
	fmt.Printf("RESULT step=siteverify success=%v codes=%s ts=%s\n", v.Success, v.codes(), v.ChallengeTS)
	if !v.Success {
		return fmt.Errorf("siteverify 期望成功，实际 codes=%s", v.codes())
	}
	return nil
}

func (c *client) doubleRedeem(siteKey string, maxIter int64) error {
	ch, iterations, err := c.solve(siteKey, maxIter)
	if err != nil {
		return err
	}
	fmt.Printf("RESULT step=challenge status=ok bits=%d iterations=%d\n", ch.Pow.Bits, iterations)
	_, status1, err := c.redeem(ch.Token, ch.nonce, ch.answer)
	if err != nil {
		return err
	}
	if status1 != http.StatusOK {
		return fmt.Errorf("首次 redeem 应成功，status=%d", status1)
	}
	fmt.Println("RESULT step=redeem-1 status=ok")
	_, status2, err := c.redeem(ch.Token, ch.nonce, ch.answer)
	if err != nil {
		return err
	}
	if status2 == http.StatusOK {
		return errors.New("第二次 redeem 竟然成功：一次性保证失效")
	}
	fmt.Printf("RESULT step=redeem-2 status=%d rejected=1\n", status2)
	return nil
}

func (c *client) doubleVerify(siteKey string, getenv func(string) string, maxIter int64) error {
	ch, _, err := c.solve(siteKey, maxIter)
	if err != nil {
		return err
	}
	r, status, err := c.redeem(ch.Token, ch.nonce, ch.answer)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("redeem 失败：status=%d codes=%s err=%v", status, r.codes(), err)
	}
	first, err := c.verify(getenv(envSecret), r.Response, "")
	if err != nil {
		return err
	}
	if !first.Success {
		return fmt.Errorf("首次 siteverify 应成功，codes=%s", first.codes())
	}
	fmt.Println("RESULT step=siteverify-1 success=true")
	second, err := c.verify(getenv(envSecret), r.Response, "")
	if err != nil {
		return err
	}
	if second.Success {
		return errors.New("同一 response 二次验证竟然成功：防重放失效")
	}
	fmt.Printf("RESULT step=siteverify-2 success=false codes=%s\n", second.codes())
	if len(second.ErrorCodes) != 1 || second.ErrorCodes[0] != "timeout-or-duplicate" {
		return fmt.Errorf("二次验证错误码应为 timeout-or-duplicate，实际 %s", second.codes())
	}
	return nil
}

// wrongSecret 证明"错误 secret 不消费合法 token"：先用错 secret 打一次，再用对的 secret 必须成功。
func (c *client) wrongSecret(siteKey string, getenv func(string) string, maxIter int64) error {
	ch, _, err := c.solve(siteKey, maxIter)
	if err != nil {
		return err
	}
	r, status, err := c.redeem(ch.Token, ch.nonce, ch.answer)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("redeem 失败：status=%d codes=%s err=%v", status, r.codes(), err)
	}
	bad, err := c.verify(getenv(envSecret)+"-tampered", r.Response, "")
	if err != nil {
		return err
	}
	if bad.Success || len(bad.ErrorCodes) != 1 || bad.ErrorCodes[0] != "invalid-input-secret" {
		return fmt.Errorf("错误 secret 应返回 invalid-input-secret，实际 success=%v codes=%s", bad.Success, bad.codes())
	}
	fmt.Printf("RESULT step=siteverify-badsecret success=false codes=%s\n", bad.codes())
	good, err := c.verify(getenv(envSecret), r.Response, "")
	if err != nil {
		return err
	}
	if !good.Success {
		return fmt.Errorf("错误 secret 之后合法 token 竟被消费：codes=%s", good.codes())
	}
	fmt.Println("RESULT step=siteverify-goodsecret success=true not-consumed=1")
	return nil
}

// acquire 走到 redeem 并**当场消费一次**（siteverify 成功），再把 response 存文件。
// 必须先消费，否则重启后的"重放检查"验的是一个从未使用过的 token —— 那时成功是正确行为，
// 测试就失去了意义（本脚本曾经因此写出一个假失败）。
func (c *client) acquire(siteKey string, getenv func(string) string, maxIter int64, path string) error {
	ch, _, err := c.solve(siteKey, maxIter)
	if err != nil {
		return err
	}
	r, status, err := c.redeem(ch.Token, ch.nonce, ch.answer)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("redeem 失败：status=%d codes=%s err=%v", status, r.codes(), err)
	}
	if path == "" {
		return errors.New("acquire 需要 --response-file")
	}
	first, err := c.verify(getenv(envSecret), r.Response, "")
	if err != nil {
		return err
	}
	if !first.Success {
		return fmt.Errorf("acquire 内的首次验证应成功，codes=%s", first.codes())
	}
	if err := os.WriteFile(path, []byte(r.Response), 0o600); err != nil {
		return err
	}
	fmt.Println("RESULT step=acquire status=ok consumed=1")
	return nil
}

// redeemOnly 走到 redeem 后把 response 存文件，**不做任何验证**。
// 阶段 2 的管理动作（停用站点、撤销凭据）要用"尚未消费的 token"来演示，
// 而 acquire 会当场消费掉它——两者不能混用。
func (c *client) redeemOnly(siteKey string, maxIter int64, path string) error {
	ch, _, err := c.solve(siteKey, maxIter)
	if err != nil {
		return err
	}
	r, status, err := c.redeem(ch.Token, ch.nonce, ch.answer)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("redeem 失败：status=%d codes=%s err=%v", status, r.codes(), err)
	}
	if path == "" {
		return errors.New("redeem-only 需要 --response-file")
	}
	if err := os.WriteFile(path, []byte(r.Response), 0o600); err != nil {
		return err
	}
	fmt.Println("RESULT step=redeem status=ok consumed=0")
	return nil
}

// wrongAnswer 演示阶段 3A 的单次作答语义：
// 错误答案使 challenge 进入终态，紧随其后的正确答案也不能重试成功。
func (c *client) wrongAnswer(siteKey string, maxIter int64) error {
	ch, _, err := c.solve(siteKey, maxIter)
	if err != nil {
		return err
	}
	if ch.answer == "" {
		return errors.New("wrong-answer 需要一个 pow_question 站点（响应里没有题面）")
	}
	wrong, err := bump(ch.answer)
	if err != nil {
		return err
	}
	r1, s1, err := c.redeem(ch.Token, ch.nonce, wrong)
	if err != nil {
		return err
	}
	// 只认"契约里的 400 invalid-challenge"：500 说明服务端把错误答案当成了内部故障
	// （典型错实现：判错分支返回 error ⇒ 事务回滚、重试机会被退回）。
	if s1 != http.StatusBadRequest || !strings.Contains(r1.codes(), "invalid-challenge") {
		return fmt.Errorf("错误答案应返回 400 invalid-challenge，got %d %s", s1, r1.codes())
	}
	fmt.Printf("RESULT step=redeem-wrong status=%d rejected=1 codes=%s\n", s1, r1.codes())
	r2, s2, err := c.redeem(ch.Token, ch.nonce, ch.answer)
	if err != nil {
		return err
	}
	if s2 != http.StatusBadRequest || !strings.Contains(r2.codes(), "invalid-challenge") {
		return fmt.Errorf("错误答案之后正确答案应仍被拒（400 invalid-challenge），got %d %s", s2, r2.codes())
	}
	fmt.Printf("RESULT step=redeem-right-after-wrong status=%d rejected=1 codes=%s\n", s2, r2.codes())
	return nil
}

// bump 造一个"文法正确但值不对"的答案。
//
// 必须是**文法正确**：畸形答案不消耗作答机会（DESIGN §10.1），拿它去测"答错即终结"
// 会测到一个假通过。数字答案 +1；视觉题的字符序列换掉首字符（字符集取自生成器，
// 不在这里另抄一份）。
func bump(s string) (string, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return strconv.FormatInt(n+1, 10), nil
	}
	cs := generator.TextImageCharset
	b := []byte(s)
	if len(b) > 0 {
		if i := strings.IndexByte(cs, b[0]); i >= 0 {
			b[0] = cs[(i+1)%len(cs)]
			return string(b), nil
		}
	}
	return "", fmt.Errorf("不认识的答案形态 %q", s)
}

// verifyOnly 用于重启后的重放检查。expect=failure 时，成功返回才是错误。
func (c *client) verifyOnly(getenv func(string) string, path, expect string) error {
	if path == "" {
		return errors.New("verify 需要 --response-file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	v, err := c.verify(getenv(envSecret), string(raw), "")
	if err != nil {
		return err
	}
	fmt.Printf("RESULT step=siteverify-replay success=%v codes=%s\n", v.Success, v.codes())
	if expect == "failure" && v.Success {
		return errors.New("重启后重放竟然成功：状态未持久化")
	}
	if expect == "success" && !v.Success {
		return fmt.Errorf("重放验证应成功，codes=%s", v.codes())
	}
	return nil
}

// --- PoW 求解 ---

type solved struct {
	challengeResp
	nonce  string
	answer string // pow_only 时为空
}

// solveRaw 领取 challenge 并真实搜索 PoW 解。
// 判定复用 internal/pow.Params.Verify：客户端绝不与服务端各写一套哈希规则。
func (c *client) solveRaw(siteKey string, maxIter int64) (challengeResp, string, int64, error) {
	ch, err := c.challenge(siteKey)
	if err != nil {
		return challengeResp{}, "", 0, err
	}
	if ch.Pow.Algo != pow.Algo {
		return ch, "", 0, fmt.Errorf("未知 PoW 算法 %q", ch.Pow.Algo)
	}
	seed, err := base64.RawURLEncoding.DecodeString(ch.Pow.Seed)
	if err != nil {
		return ch, "", 0, fmt.Errorf("seed 解码: %w", err)
	}
	params := pow.Params{Algo: ch.Pow.Algo, Bits: ch.Pow.Bits, Seed: seed}
	nonce, iterations, err := searchNonce(params, maxIter)
	if err != nil {
		return ch, "", iterations, err
	}
	return ch, nonce, iterations, nil
}

func (c *client) solve(siteKey string, maxIter int64) (solved, int64, error) {
	ch, nonce, iterations, err := c.solveRaw(siteKey, maxIter)
	if err != nil {
		return solved{}, iterations, err
	}
	if nonce == "" {
		return solved{}, iterations, errors.New("未找到解")
	}
	answer, err := ch.answer()
	if err != nil {
		if c.answerOverride == "" {
			return solved{}, iterations, fmt.Errorf("题面无法独立求值（图片题需要 --answer-file）: %w", err)
		}
		answer = c.answerOverride
		fmt.Println("RESULT step=answer source=injected-file（图片题：答案来自注入侧，PoW 仍由本客户端真实求解）")
	}
	return solved{challengeResp: ch, nonce: nonce, answer: answer}, iterations, nil
}

// searchNonce 从随机起点线性搜索，用服务端同一套 Verify 判定。
// 单独成函数是为了可测试：候选 nonce 的合法性由 pow 保证，不依赖任何 HTTP 状态。
func searchNonce(params pow.Params, maxIter int64) (string, int64, error) {
	start, err := randStart()
	if err != nil {
		return "", 0, err
	}
	deadline := time.Now().Add(25 * time.Second)
	for i := int64(0); i < maxIter; i++ {
		n := start + i
		if n > nonceCeil {
			n -= nonceCeil + 1
		}
		nonce := strconv.FormatInt(n, 10)
		ok, err := params.Verify(nonce)
		if err != nil {
			return "", i, fmt.Errorf("候选解 %q 被服务端校验规则判非法：%w", nonce, err)
		}
		if ok {
			return nonce, i + 1, nil
		}
		if i%50_000 == 0 && time.Now().After(deadline) {
			return "", i, errors.New("PoW 求解超时（25s）")
		}
	}
	return "", maxIter, errors.New("PoW 求解超出迭代上限")
}

// nonceCeil 与 internal/pow 的上限一致：搜索空间 [0, 1e12]。
const nonceCeil = 1_000_000_000_000

// randStart 返回 [0, nonceCeil] 内的非负起点。
// 用 crypto/rand.Int 而不是"读字节转 int64 再取模"：后者在最高位为 1 时得到负数，
// 负起点会让求解器提交 "-123" 这类服务端判非法的 nonce（本工具曾因此偶发失败）。
func randStart() (int64, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(nonceCeil))
	if err != nil {
		return 0, err
	}
	return n.Int64(), nil
}

// evalExpr 求 arithmetic-baseline 题面的值。它是 devclient 自己的实现，
// 同时支持旧题面和两组括号题面，运算符 + - × ÷，÷ 保证整除。
// 验收脚本用它来"真实答题"：既没有读数据库里的答案，也没有硬编码固定解。
func evalExpr(task string) (int64, error) {
	inner := task
	if strings.HasPrefix(task, "(") {
		end := strings.Index(task, ")")
		if end < 0 {
			return 0, errors.New("括号未闭合")
		}
		v, err := evalFlat(task[1:end])
		if err != nil {
			return 0, err
		}
		rest := strings.TrimSpace(task[end+1:])
		if rest == "" {
			return v, nil
		}
		op, tail, found := strings.Cut(rest, " ")
		if !found {
			return 0, errors.New("括号后缺少运算符")
		}
		tail = strings.TrimSpace(tail)
		var n int64
		if strings.HasPrefix(tail, "(") && strings.HasSuffix(tail, ")") {
			n, err = evalFlat(tail[1 : len(tail)-1])
		} else {
			n, err = strconv.ParseInt(tail, 10, 64)
		}
		if err != nil {
			return 0, err
		}
		return applyOp(v, op, n)
	}
	return evalFlat(inner)
}

func evalFlat(s string) (int64, error) {
	f := strings.Fields(s)
	if len(f) != 3 {
		return 0, errors.New("表达式不是 a op b 形状")
	}
	a, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return 0, err
	}
	b, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return 0, err
	}
	return applyOp(a, f[1], b)
}

func applyOp(a int64, op string, b int64) (int64, error) {
	switch op {
	case "+":
		return a + b, nil
	case "-":
		return a - b, nil
	case "×":
		return a * b, nil
	case "÷":
		if b == 0 || a%b != 0 {
			return 0, errors.New("非整除除法")
		}
		return a / b, nil
	}
	return 0, errors.New("未知运算符 " + op)
}
