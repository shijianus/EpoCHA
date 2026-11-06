package main

// 阶段 5A：负载工具的口径测试。
//
// 这里证明的是**工具口径**，不是效果：分位数取的是真实样本值、失败按有限类别归并、
// 一次成功都没有时退出码非 0、以及**凭据不进任何输出**（红线 11 的按值扫描）。
// "1C1G 上实测吞吐"是另一件事，由 docs/scripts/stage5a.sh 在容器里跑并落进 docs/STAGE5A.md。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/pow"
)

func TestPercentileIsNearestRankOfRealSamples(t *testing.T) {
	ds := []time.Duration{}
	for i := 1; i <= 100; i++ {
		ds = append(ds, time.Duration(i)*time.Millisecond)
	}
	if got := pct(ds, 0.50); got != 50*time.Millisecond {
		t.Errorf("p50 = %v，期望 50ms（最近秩，不做插值）", got)
	}
	if got := pct(ds, 0.95); got != 95*time.Millisecond {
		t.Errorf("p95 = %v，期望 95ms", got)
	}
	if got := pct(ds, 1.0); got != 100*time.Millisecond {
		t.Errorf("p100 = %v，期望 100ms", got)
	}
	// 单样本：所有分位数都必须是那个样本本身，而不是插值造出的数。
	one := []time.Duration{7 * time.Millisecond}
	for _, p := range []float64{0, 0.5, 0.95, 1} {
		if got := pct(one, p); got != 7*time.Millisecond {
			t.Errorf("p%.2f(单样本) = %v", p, got)
		}
	}
	if got := pct(nil, 0.95); got != 0 {
		t.Errorf("空样本 p95 = %v，期望 0", got)
	}
	// 乱序输入不得影响结果（排序必须在函数内部完成，不能依赖调用方）。
	shuffled := []time.Duration{9 * time.Millisecond, 1 * time.Millisecond, 5 * time.Millisecond}
	if got := pct(shuffled, 0.5); got != 5*time.Millisecond {
		t.Errorf("乱序样本 p50 = %v", got)
	}
	if shuffled[0] != 9*time.Millisecond {
		t.Error("pct 改动了调用方的切片")
	}
}

func TestArgValidationRefusesUnboundedRuns(t *testing.T) {
	good := map[string]string{"EPOCHA_SITE_SECRET": "sk_" + strings.Repeat("a", 64)}
	cases := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"缺 site-key", []string{"--requests", "10"}, good},
		{"缺 secret 不许静默跑全失败", []string{"--site-key", "s", "--requests", "10"}, map[string]string{}},
		{"并发上限", []string{"--site-key", "s", "--concurrency", "17"}, good},
		{"并发下限", []string{"--site-key", "s", "--concurrency", "0"}, good},
		{"请求数上限", []string{"--site-key", "s", "--requests", "5001"}, good},
		{"请求数下限", []string{"--site-key", "s", "--requests", "0"}, good},
	}
	for _, c := range cases {
		if _, err := parseArgs(c.args, getenvOf(c.env)); err == nil {
			t.Errorf("%s: 竟然通过（应有上界校验）", c.name)
		}
	}
	o, err := parseArgs([]string{"--site-key", "s", "--requests", "10", "--concurrency", "2"}, getenvOf(good))
	if err != nil {
		t.Fatalf("合法参数被拒: %v", err)
	}
	if o.requests != 10 || o.concurrency != 2 || o.secret == "" {
		t.Errorf("解析结果不如预期: %+v", o)
	}
}

func TestRunExitsNonZeroWhenNothingSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error-codes":["challenge-unavailable"]}`))
	}))
	defer srv.Close()

	var out strings.Builder
	err := run([]string{"--base", srv.URL, "--site-key", "s", "--requests", "3", "--concurrency", "1", "--quiet"},
		getenvOf(map[string]string{"EPOCHA_SITE_SECRET": "sk_" + strings.Repeat("b", 64)}), &out)
	if err == nil {
		t.Fatal("全部失败却返回成功——那会把“服务不可用”读成“负载测试通过”")
	}
	if !strings.Contains(err.Error(), "challenge:503/challenge-unavailable") {
		t.Errorf("失败类别没有归并到 stage+状态+code：%v", err)
	}
	if !strings.Contains(out.String(), "flows=3 ok=0") {
		t.Errorf("汇总行缺 flows/ok：%s", out.String())
	}
}

func TestFlowAgainstStubRecordsAllThreeStages(t *testing.T) {
	// 桩服务用 internal/pow 真出题，工具真求解：这样"闭环成功"不是靠桩返回假 token。
	var hits [4]int32
	srv := stubVerifyServer(t, &hits, pow.MinBits)
	defer srv.Close()

	secret := "sk_" + strings.Repeat("c", 64)
	o := &options{base: srv.URL, siteKey: "s", secret: secret, concurrency: 3, requests: 9, timeout: 5 * time.Second}
	c := &client{base: o.base, http: &http.Client{Timeout: o.timeout}}
	s := drive(context.Background(), c, o, time.Now)
	if s.Flows != 9 || s.OK != 9 {
		t.Fatalf("flows=%d ok=%d，期望 9/9（错误类别：%s）", s.Flows, s.OK, s.errClasses())
	}
	if len(s.Challenge) != 9 || len(s.Redeem) != 9 || len(s.Verify) != 9 {
		t.Errorf("三段样本数不齐：%d/%d/%d", len(s.Challenge), len(s.Redeem), len(s.Verify))
	}
	for _, n := range []string{"challenge", "redeem", "siteverify"} {
		switch n {
		case "challenge":
			if atomic.LoadInt32(&hits[0]) != 9 {
				t.Errorf("服务端收到 %d 次 challenge，期望 9", hits[0])
			}
		case "redeem":
			if atomic.LoadInt32(&hits[1]) != 9 {
				t.Errorf("服务端收到 %d 次 redeem，期望 9", hits[1])
			}
		case "siteverify":
			if atomic.LoadInt32(&hits[2]) != 9 {
				t.Errorf("服务端收到 %d 次 siteverify，期望 9", hits[2])
			}
		}
	}
	var out strings.Builder
	s.print(&out, o)
	// 按值扫描：站点 secret 与 token 都不得出现在输出里（红线 11）。
	if strings.Contains(out.String(), secret) {
		t.Error("输出泄露了站点 secret")
	}
	if strings.Contains(out.String(), "chal.") || strings.Contains(out.String(), "resp.") {
		t.Errorf("输出泄露了 token 片段: %s", out.String())
	}
}

func TestFlowClassifiesServerRejectionAtVerify(t *testing.T) {
	// redeem 成功但 siteverify 判失败：类别必须落在 verify，而不是笼统的"请求失败"。
	var verifyFails int32 = 1
	srv := stubServerWithFailure(t, &verifyFails, pow.MinBits)
	defer srv.Close()
	o := &options{base: srv.URL, siteKey: "s", secret: "sk_" + strings.Repeat("d", 64),
		concurrency: 1, requests: 2, timeout: 5 * time.Second}
	c := &client{base: o.base, http: &http.Client{Timeout: o.timeout}}
	s := drive(context.Background(), c, o, time.Now)
	if s.OK != 0 || s.Flows != 2 {
		t.Fatalf("flows=%d ok=%d，期望 2/0", s.Flows, s.OK)
	}
	if s.StageErrors["verify:400/invalid-input-response"] != 2 {
		t.Errorf("失败类别 = %v，期望归到 verify:400/invalid-input-response", s.StageErrors)
	}
}

func TestFlowRefusesQuestionSites(t *testing.T) {
	// pow_question 需要答案（图片题还要 OCR）。工具必须明确拒绝，而不是"答案留空照样提交"。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"chal.x","mode":"pow_question","question":{"task":"1+2"},"pow":{"algo":"sha256-prefix","bits":4,"seed":"AAAAAAAAAAAAAAAAAAAAAA"}}`))
	}))
	defer srv.Close()
	c := &client{base: srv.URL, http: &http.Client{Timeout: 5 * time.Second}}
	_, stage, err := c.flow(context.Background(), "s", "sec")
	if err == nil || stage != "challenge" {
		t.Fatalf("题目站点应被拒绝，得到 stage=%q err=%v", stage, err)
	}
	if !strings.Contains(err.Error(), "pow_question") {
		t.Errorf("错误文案没有说明原因: %v", err)
	}
}

// --- 测试桩 ---

func getenvOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// stubVerifyServer 是一个最小可信服务端：真出题（crypto/rand + internal/pow），
// redeem 与 siteverify 只校验"确实解过"，不重复实现服务端的账本语义。
func stubVerifyServer(t *testing.T, hits *[4]int32, bits uint8) *httptest.Server {
	verifyFails := int32(0)
	return stubServer(t, hits, &verifyFails, bits)
}

func stubServerWithFailure(t *testing.T, verifyFails *int32, bits uint8) *httptest.Server {
	var hits [4]int32
	return stubServer(t, &hits, verifyFails, bits)
}

func stubServer(t *testing.T, hits *[4]int32, verifyFails *int32, bits uint8) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/challenge", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits[0], 1)
		p, err := pow.NewParams(bits)
		if err != nil {
			t.Errorf("出题失败: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("chal.stub.%d", time.Now().UnixNano()),
			"mode":       "pow_only",
			"expires_in": 180,
			"pow": map[string]any{
				"algo": p.Algo, "bits": p.Bits,
				"seed": base64.RawURLEncoding.EncodeToString(p.Seed),
			},
		})
	})
	mux.HandleFunc("POST /api/redeem", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits[1], 1)
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			Token    string `json:"token"`
			PowNonce string `json:"pow_nonce"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Token == "" || req.PowNonce == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error-codes":["invalid-request"]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response": fmt.Sprintf("resp.stub.%d", time.Now().UnixNano()), "expires_in": 120,
		})
	})
	mux.HandleFunc("POST /siteverify", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits[2], 1)
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		vals, _ := url.ParseQuery(string(body))
		if atomic.LoadInt32(verifyFails) != 0 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
			return
		}
		if vals.Get("secret") == "" || !strings.HasPrefix(vals.Get("response"), "resp.stub.") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":false,"error-codes":["missing-input-response"]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"challenge_ts":"2026-10-18T00:00:00Z"}`))
	})
	return httptest.NewServer(mux)
}

// 节流是给"持续演练"用的：没有它，150 次闭环会在 1 秒内打完，
// 那样量到的峰值既不代表 1C1G 上的稳态，也观察不到内存/WAL/题池随时间的变化。
func TestPaceThrottlesWorkers(t *testing.T) {
	srv := stubVerifyServer(t, &[4]int32{}, pow.MinBits)
	defer srv.Close()

	// 直接观察节流等待的调用次数，而不是拿墙钟做阈值：-race 或并发构建
	// 会把 4/8 次真实闭环的耗时推过任何固定毫秒数（本轮在 stage5b 上实测撞过）。
	var waits atomic.Int32
	oldWait := paceWait
	paceWait = func(context.Context, time.Duration) bool {
		waits.Add(1)
		return true
	}
	defer func() { paceWait = oldWait }()

	o := &options{base: srv.URL, siteKey: "s", secret: "sk_" + strings.Repeat("e", 64),
		concurrency: 1, requests: 4, timeout: 5 * time.Second, pace: 120 * time.Millisecond}
	c := &client{base: o.base, http: &http.Client{Timeout: o.timeout}}
	s := drive(context.Background(), c, o, time.Now)
	if s.OK != 4 {
		t.Fatalf("ok = %d，期望 4（错误：%s）", s.OK, s.errClasses())
	}
	// 4 次闭环、每次之后睡 120ms ⇒ 4 次等待（最后一次在结果交付之后，仍会调用节流）。
	if got := waits.Load(); got != 4 {
		t.Fatalf("节流等待次数 = %d，期望 4", got)
	}

	// pace=0 必须仍然是"尽可能快"（5A 的容量测量依赖这个行为）。
	waits.Store(0)
	o2 := &options{base: srv.URL, siteKey: "s", secret: o.secret, concurrency: 2, requests: 8, timeout: 5 * time.Second}
	s2 := drive(context.Background(), c, o2, time.Now)
	if s2.OK != 8 {
		t.Fatalf("pace=0 时 ok = %d，期望 8（错误：%s）", s2.OK, s2.errClasses())
	}
	if got := waits.Load(); got != 0 {
		t.Fatalf("pace=0 仍发生了 %d 次节流等待", got)
	}
}

func TestPaceArgValidationAndReporting(t *testing.T) {
	good := map[string]string{"EPOCHA_SITE_SECRET": "sk_" + strings.Repeat("f", 64)}
	if _, err := parseArgs([]string{"--site-key", "s", "--pace", "-1s"}, getenvOf(good)); err == nil {
		t.Error("负 pace 应被拒绝")
	}
	o, err := parseArgs([]string{"--site-key", "s", "--pace", "660ms", "--requests", "450"}, getenvOf(good))
	if err != nil {
		t.Fatalf("合法 pace 被拒: %v", err)
	}
	if o.pace != 660*time.Millisecond || o.requests != 450 {
		t.Errorf("解析结果不如预期: pace=%v requests=%d", o.pace, o.requests)
	}
	var out strings.Builder
	s := newSummary()
	s.Flows, s.OK = 450, 450
	s.print(&out, o)
	if !strings.Contains(out.String(), "节流 pace=660ms") {
		t.Errorf("输出没有如实记录节流与目标速率:\n%s", out.String())
	}
}
