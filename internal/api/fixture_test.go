package api

// 受控题池夹具（测试侧）。
//
// 为什么需要它：跨题绑定这类测试的前提是"两道题的答案确实不同"。用随机补题去凑这个前提，
// 要么靠概率（然后撞上相同答案时只能跳过断言），要么靠无限重抽。两者都把覆盖率交给运气。
// 夹具直接写库，答案由测试指定，前提就成了可复现的事实。
//
// 三条边界必须说清楚：
//   - **答案相同是合法现象**：题池不承诺"答案字符串唯一"，那也不是任何安全不变量（DESIGN §5.4）。
//     夹具让答案不同，只是为了让下面的断言不空转，不是为了声称"同答案的两道题是 bug"。
//   - **跨题不能复用的是**：私有 verifier、题目↔challenge 绑定、PoW seed、challenge token。
//   - 夹具**不要求**生产 API 支持"指定答案"或"返回答案"：答案出口只存在于测试进程内。

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/store"
	"github.com/shijianus/EpoCHA/internal/token"
)

// seededQuestion 是一条要直接写进题池的受控题目。
type seededQuestion struct {
	GeneratorID string // 空 = arithmetic-baseline-v1
	Payload     map[string]any
	// Answer 是**规范化之后**的答案串（与生成器 NormalizeAnswer 的输出同形）。
	Answer string
	Asset  []byte // 图片资产（阶段 3B）；nil 表示文本题
}

// answerVerifier 在测试侧**独立实现** DESIGN §5.4 的 verifier 线格式：
//
//	nonce(16B) || HMAC(K(qver), "epocha/v1|qver|" || nonce || 0x00 || 规范化答案)
//
// 刻意不复用 internal/generator 里的构造函数：判分实现被改动时这里必须一起变红，
// 否则"库里的旧 verifier 被新解释读成另一批答案"这类静默正确性事故就没人挡。
func answerVerifier(t *testing.T, master, nonce []byte, canonicalAnswer string) []byte {
	t.Helper()
	key := token.DerivePurpose(master, "qver")
	m := hmac.New(sha256.New, key)
	m.Write([]byte("epocha/v1|qver|"))
	m.Write(nonce)
	m.Write([]byte{0})
	m.Write([]byte(canonicalAnswer))
	out := make([]byte, 0, 16+sha256.Size)
	out = append(out, nonce...)
	return append(out, m.Sum(nil)...)
}

// seedQuestions 按给定顺序把题目写成 ready 行，并保证领题查询（ORDER BY expires_at）
// **按这个顺序**发出。返回每题的池行 id。
//
// nonce 由序号确定性派生（不是随机）：夹具要可复现，同时不同题的 verifier 天然不同。
func (h *harness) seedQuestions(t *testing.T, difficulty int, qs ...seededQuestion) []string {
	t.Helper()
	ids := make([]string, 0, len(qs))
	for i, q := range qs {
		genID := q.GeneratorID
		if genID == "" {
			genID = generator.ArithID
		}
		g, err := h.gens.Lookup(genID)
		if err != nil {
			t.Fatalf("夹具题型未登记 %s: %v", genID, err)
		}
		payload, err := json.Marshal(q.Payload)
		if err != nil {
			t.Fatal(err)
		}
		// 夹具自己也要过白名单：写一条 SanitizePublic 读不了的题面，等于测了一条不存在的形状。
		if _, err := g.SanitizePublic(payload); err != nil {
			t.Fatalf("夹具题面不合法: %v", err)
		}
		nonce := make([]byte, 16)
		copy(nonce, []byte("epocha-fixture-q"))
		nonce[15] = byte(i + 1)
		created := h.clock.Add(time.Duration(i) * time.Second)
		row := &store.PoolQuestion{
			ID:          "fixture" + hex.EncodeToString([]byte{byte(i), byte(len(qs))}),
			GeneratorID: genID,
			Version:     g.Version(),
			Difficulty:  difficulty,
			Status:      store.PoolReady,
			PayloadJSON: string(payload),
			Verifier:    answerVerifier(t, h.cfg.MasterKey, nonce, q.Answer),
			CreatedAt:   created,
			// 过期时间按序号递增：领取查询按 expires_at 升序取题，于是发出顺序 = 给定顺序。
			ExpiresAt: h.clock.Add(30*time.Minute + time.Duration(i)*time.Minute),
			Asset:     q.Asset,
		}
		if err := h.db.InsertQuestion(context.Background(), row,
			store.PoolLimits{MaxRows: 500, MaxImageBytes: 64 << 20}); err != nil {
			t.Fatalf("夹具写入题目: %v", err)
		}
		ids = append(ids, row.ID)
	}
	return ids
}

// arithQuestion 构造一条算术题夹具。
func arithQuestion(task, answer string) seededQuestion {
	return seededQuestion{
		Payload: map[string]any{
			"task": task, "answer_type": "integer", "max_answer_len": 14,
		},
		Answer: answer,
	}
}
