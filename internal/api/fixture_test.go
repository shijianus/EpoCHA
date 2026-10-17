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
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/shijianus/EpoCHA/internal/eval"
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
	for _, q := range qs {
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
		idx := h.seedCount
		h.seedCount++
		nonce := make([]byte, 16)
		copy(nonce, []byte("epocha-fixture-q"))
		nonce[15] = byte(idx + 1)
		created := h.clock.Add(time.Duration(idx) * time.Second)
		row := &store.PoolQuestion{
			ID:          "fixture" + hex.EncodeToString([]byte{byte(idx >> 8), byte(idx), byte(len(qs))}),
			GeneratorID: genID,
			Version:     g.Version(),
			Difficulty:  difficulty,
			Status:      store.PoolReady,
			PayloadJSON: string(payload),
			Verifier:    answerVerifier(t, h.cfg.MasterKey, nonce, q.Answer),
			CreatedAt:   created,
			// 过期时间按序号递增：领取查询按 expires_at 升序取题，于是发出顺序 = 给定顺序。
			ExpiresAt: h.clock.Add(30*time.Minute + time.Duration(idx)*time.Minute),
			Asset:     q.Asset,
			AssetMime: assetMimeFor(q.Asset),
		}
		if err := h.db.InsertQuestion(context.Background(), row,
			store.PoolLimits{MaxRows: 500, MaxImageBytes: 64 << 20}); err != nil {
			t.Fatalf("夹具写入题目: %v", err)
		}
		ids = append(ids, row.ID)
	}
	return ids
}

// hasSuccess 读 /siteverify 的 success 字段（缺省即 false）。
func (r reply) hasSuccess() bool {
	v, ok := r.got["success"].(bool)
	return ok && v
}

// tokenQuestionKey 取与生产同一条派生路径的题目判分密钥（K(qver)）。
func tokenQuestionKey(t *testing.T) []byte {
	t.Helper()
	return token.DerivePurpose(testMasterKey, "qver")
}

// imageSiteWithSecret 建一个用视觉题型的站点并返回它的验证凭据（演示后端持有的那把 secret）。
func (h *harness) imageSiteWithSecret(t *testing.T, name, siteKey string, difficulty int) (*store.Site, string) {
	t.Helper()
	site, secret, err := h.db.CreateSite(context.Background(), name, siteKey, store.SitePolicy{
		PowBits: 12, VerifyMode: store.ModePowQuestion,
		GeneratorID: generator.TextImageID, Difficulty: difficulty,
		AllowMissingOrigin: true,
	})
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	return site, secret
}

// restartWithRegistry 模拟"改过生成器白名单之后重启"：
// 库与主密钥不变（token 与账本仍然有效），只有**可判分的题型集合**变了。
// 这就是运维用 EPOCHA_GENERATORS 关掉某个题型后的真实形状。
func (h *harness) restartWithRegistry(t *testing.T, gens *generator.Registry) {
	t.Helper()
	for _, f := range h.cleanup {
		f()
	}
	h.cleanup = nil
	reopened, err := store.Open(context.Background(), h.dbPath, testPepper)
	if err != nil {
		t.Fatalf("重开库: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	h.db = reopened
	signer, err := token.NewSigner(testMasterKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(h.cfg, reopened, signer, gens, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return h.clock }
	h.srv, h.gens = srv, gens
}

// rawSQLite 用**第二个独立句柄**打开同一个库文件跑维护性 SQL（测试里改资产列用）。
// 不给 store.DB 开"任意 SQL 出口"：那等于在生产对象上挂一个后门。
func rawSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	h, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// evalStream 是测试/评测用的确定性熵流。**只**在测试里出现：
// 服务的生成路径由 pool 传 crypto/rand，没有任何固定种子回落。
func evalStream(t *testing.T, seed string) io.Reader {
	t.Helper()
	r, err := eval.NewDeterministic([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// imageAnswerFromSeed 取"某个 seed 下生产路径会用的答案"（与 imageFixture 同源）。
func imageAnswerFromSeed(t *testing.T, seed string) string {
	t.Helper()
	a, err := generator.TextImageAnswerFrom(evalStream(t, "epocha-api-"+seed))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// assetMimeFor 与生成器的声明一致：有资产就是 PNG，没有就是空串（store 侧要求成对）。
func assetMimeFor(asset []byte) string {
	if len(asset) == 0 {
		return ""
	}
	return store.MimeTypePNG
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
