package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// questionSite 建一个可用的 pow_question 站点。
func questionSite(t *testing.T, d *DB, name string) *Site {
	t.Helper()
	site, _, err := d.CreateSite(t.Context(), name, "sk-"+name, SitePolicy{
		PowBits: 14, VerifyMode: ModePowQuestion, GeneratorID: "arithmetic-baseline-v1", Difficulty: 2,
	})
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	return site
}

// seedQuestions 直接写 n 条 ready 题（绕过 worker：本文件测的是事务与状态机，不是补题节奏）。
func seedQuestions(t *testing.T, d *DB, spec PoolSpec, n int, ttl time.Duration) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		q := &PoolQuestion{
			ID: id, GeneratorID: spec.GeneratorID, Version: spec.Version, Difficulty: spec.Difficulty,
			Status: PoolReady, PayloadJSON: `{"task":"1 + 1","answer_type":"integer","max_answer_len":14}`,
			Verifier:  []byte("verifier-bytes-" + id),
			CreatedAt: now, ExpiresAt: now.Add(ttl),
		}
		if err := d.InsertQuestion(t.Context(), q, 10000); err != nil {
			t.Fatalf("InsertQuestion: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func newChallengeRow(siteID int64, ttl time.Duration) *Challenge {
	now := time.Now().UTC()
	id, _ := NewID()
	return &Challenge{
		ID: id, SiteID: siteID, Mode: ModePowQuestion, GeneratorID: "arithmetic-baseline-v1",
		PowSeed: []byte("0123456789abcdef"), PowBits: 12,
		CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
}

var testSpec = PoolSpec{GeneratorID: "arithmetic-baseline-v1", Version: "v1", Difficulty: 2}

// TestClaimIsAtomicWithChallenge 不变量 #3：领题与 challenge 行创建同事务。
// 让 challenge 插入失败（存量上限），题池行必须**还是 ready**。
func TestClaimIsAtomicWithChallenge(t *testing.T) {
	d := newDB(t)
	site := questionSite(t, d, "alpha")
	seedQuestions(t, d, testSpec, 2, 10*time.Hour)

	caps := Caps{MaxActive: 0, MaxActivePerSite: 0} // 故意不合法，走参数校验失败路径
	if _, err := d.IssueChallengeWithQuestion(t.Context(), newChallengeRow(site.ID, time.Hour), caps, &testSpec); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("参数错误应上报 ErrBadConfig，got %v", err)
	}
	if n, _ := d.ReadyCount(t.Context(), testSpec, time.Now()); n != 2 {
		t.Fatalf("challenge 未创建时题池必须不动：ready=%d", n)
	}

	// 上限到顶（ErrCapExceeded）同样不能留下 assigned 行
	tight := Caps{MaxActive: 1, MaxActivePerSite: 1}
	if _, err := d.IssueChallengeWithQuestion(t.Context(), newChallengeRow(site.ID, time.Hour), tight, &testSpec); err != nil {
		t.Fatal(err)
	}
	before, _ := d.ReadyCount(t.Context(), testSpec, time.Now())
	if _, err := d.IssueChallengeWithQuestion(t.Context(), newChallengeRow(site.ID, time.Hour), tight, &testSpec); !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("应撞 challenge 上限，got %v", err)
	}
	after, _ := d.ReadyCount(t.Context(), testSpec, time.Now())
	if before != after {
		t.Fatalf("challenge 因上限被拒时不得消耗题目：%d → %d", before, after)
	}
}

// TestPoolEmptyDoesNotCreateChallenge 不变量 #5：池空时既不建 challenge，也绝不"改成不发题"。
func TestPoolEmptyDoesNotCreateChallenge(t *testing.T) {
	d := newDB(t)
	site := questionSite(t, d, "alpha")
	// 桶里根本没有对应难度的题
	if _, err := d.IssueChallengeWithQuestion(t.Context(), newChallengeRow(site.ID, time.Hour),
		Caps{MaxActive: 100, MaxActivePerSite: 100}, &testSpec); !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("池空应 ErrPoolEmpty，got %v", err)
	}
	if n, _ := d.CountChallenges(t.Context(), statusIssued); n != 0 {
		t.Fatalf("池空却留下了 challenge 行：%d", n)
	}
	// 题目比 challenge 早过期 → 同样算池空（不能发一道会在挑战期内过期的题）
	seedQuestions(t, d, testSpec, 1, 10*time.Second)
	if _, err := d.IssueChallengeWithQuestion(t.Context(), newChallengeRow(site.ID, time.Hour),
		Caps{MaxActive: 100, MaxActivePerSite: 100}, &testSpec); !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("题目寿命短于 challenge 时应拒绝领题，got %v", err)
	}
	if n, _ := d.CountChallenges(t.Context(), statusIssued); n != 0 {
		t.Fatalf("不该建 challenge：%d", n)
	}
}

// TestAssignedNeverReturnsToReady 不变量 #2：领取后无论答案对错都不回池。
func TestAssignedNeverReturnsToReady(t *testing.T) {
	d := newDB(t)
	site := questionSite(t, d, "alpha")
	seedQuestions(t, d, testSpec, 3, 10*time.Hour)
	caps := Caps{MaxActive: 100, MaxActivePerSite: 100}

	c1 := newChallengeRow(site.ID, time.Hour)
	q1, err := d.IssueChallengeWithQuestion(t.Context(), c1, caps, &testSpec)
	if err != nil {
		t.Fatal(err)
	}
	// 错误答案终结
	if _, err := d.RedeemAttempt(t.Context(), c1.ID, time.Now(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if n, _ := d.ReadyCount(t.Context(), testSpec, time.Now()); n != 2 {
		t.Fatalf("错误答案后 ready 必须仍是 2（题不回池）：%d", n)
	}
	got, err := d.QuestionForChallenge(t.Context(), c1.ID)
	if err != nil || got.Status != PoolTerminal {
		t.Fatalf("题目应随 challenge 终结而 terminal：%+v err %v", got, err)
	}
	if got.ID != q1.ID {
		t.Fatal("题池绑定读回不符")
	}
	// 正确答案终结：同样 terminal，且 ready 数继续下降
	c2 := newChallengeRow(site.ID, time.Hour)
	if _, err := d.IssueChallengeWithQuestion(t.Context(), c2, caps, &testSpec); err != nil {
		t.Fatal(err)
	}
	res, err := d.RedeemAttempt(t.Context(), c2.ID, time.Now(), time.Minute, true)
	if err != nil || res.Outcome != OutcomeIssued || res.Response == nil {
		t.Fatalf("正确兑换应签发 response: %+v err %v", res, err)
	}
	if n, _ := d.ReadyCount(t.Context(), testSpec, time.Now()); n != 1 {
		t.Fatalf("ready 应为 1，got %d", n)
	}
	// 同一个 challenge 再试一次：不可兑换，且不会二次消耗题目
	if _, err := d.RedeemAttempt(t.Context(), c2.ID, time.Now(), time.Minute, true); !errors.Is(err, ErrNotRedeemable) {
		t.Fatalf("重复兑换应 ErrNotRedeemable，got %v", err)
	}
	if n, _ := d.ReadyCount(t.Context(), testSpec, time.Now()); n != 1 {
		t.Fatalf("重复兑换消耗了题目：%d", n)
	}
}

// TestOneQuestionPerChallengeTwoHandles 不变量 #1/#6：
// 两个独立 *sql.DB 句柄争抢同一个 SQLite 文件时，每道题只有一个领取者。
func TestOneQuestionPerChallengeTwoHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epocha.db")
	a, _ := newDBAt(t, path)
	site := questionSite(t, a, "alpha")
	const questions = 6
	seedQuestions(t, a, testSpec, questions, 10*time.Hour)

	b, err := Open(t.Context(), path, testPepper())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	handles := []*DB{a, b}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, fails := 0, 0
	unexpected := []string{}
	start := make(chan struct{})
	// 6 个 challenge 争 6 道题：每个都该成功（题数够），但每道题只会被用一次
	challenges := make([]*Challenge, questions+4)
	for i := range challenges {
		challenges[i] = newChallengeRow(site.ID, time.Hour)
	}
	for i := range challenges {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			d := handles[i%2]
			_, err := d.IssueChallengeWithQuestion(t.Context(), challenges[i],
				Caps{MaxActive: 1000, MaxActivePerSite: 1000}, &testSpec)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrPoolEmpty):
				fails++
			default:
				unexpected = append(unexpected, err.Error())
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if len(unexpected) > 0 {
		t.Fatalf("出现意外错误（前 3 条）：%v", unexpected[:min(3, len(unexpected))])
	}
	if wins != questions {
		t.Fatalf("成功领题 %d 次，题目只有 %d 道", wins, questions)
	}
	if fails != len(challenges)-questions {
		t.Fatalf("池耗尽后的请求应全部 ErrPoolEmpty：%d", fails)
	}
	// 绑定基数必须与成功次数**逐一相等**：
	// "count(*) GROUP BY challenge_id HAVING >1" 只挡得住"一个 challenge 绑多题"（partial UNIQUE 已经保证），
	// 挡不住"同一题行被第二个事务改绑到别的 challenge"——后者会让 bound 行数照常等于 wins，
	// 但 distinct challenge_id 少于 wins。两个方向都要断言，否则不变量 #1 只测了一半。
	var bound, distinct int
	if err := a.h.QueryRowContext(t.Context(),
		`SELECT count(*), count(DISTINCT challenge_id) FROM pool WHERE challenge_id IS NOT NULL`).
		Scan(&bound, &distinct); err != nil {
		t.Fatal(err)
	}
	if bound != wins {
		t.Fatalf("带绑定的题行数 %d 与成功次数 %d 不符", bound, wins)
	}
	if distinct != wins {
		t.Fatalf("只有 %d 个不同的 challenge 拥有题目，却有 %d 次成功领取：存在同一题行被改绑", distinct, wins)
	}
	if n, _ := a.ReadyCount(t.Context(), testSpec, time.Now()); n != 0 {
		t.Fatalf("ready 未清空：%d", n)
	}
}

// TestStaleSnapshotClaimFailsClosed 跨句柄竞争下的失败模式必须是"整笔回滚"，
// 而不是"用过期快照覆盖已提交的绑定"。
//
// 手工编排顺序（并发测试测不到这个具体交错）：
//  1. 句柄 a 开事务并**先做一次读**（在 WAL 里取得快照），此刻还没写；
//  2. 句柄 b 在同一库文件上领走那一行并提交；
//  3. a 再执行领取用的条件更新。
//
// a 的写法只有两种可接受结果：报错（SQLite 的 "database is locked"）或影响 0 行。
// 若 a 真的写入成功，就等于 b 的绑定被静默改写——那是不变量 #1 的实质破坏。
func TestStaleSnapshotClaimFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epocha.db")
	a, _ := newDBAt(t, path)
	seedQuestions(t, a, testSpec, 1, 10*time.Hour)

	tx, err := a.h.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	var qid string
	if err := tx.QueryRowContext(t.Context(),
		`SELECT id FROM pool WHERE status=? ORDER BY expires_at LIMIT 1`, PoolReady).Scan(&qid); err != nil {
		t.Fatal(err)
	}

	b, err := Open(t.Context(), path, testPepper())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	site := questionSite(t, b, "alpha")
	bCh := newChallengeRow(site.ID, time.Hour)
	bound, err := b.IssueChallengeWithQuestion(t.Context(), bCh,
		Caps{MaxActive: 100, MaxActivePerSite: 100}, &testSpec)
	if err != nil {
		t.Fatalf("b 领题失败：%v", err)
	}

	res, err := tx.ExecContext(t.Context(), `
UPDATE pool SET status=?, challenge_id=?, assigned_at=?
WHERE id=? AND status=?`,
		PoolAssigned, "STALE-CID", formatTime(time.Now().UTC()), qid, PoolReady)
	// 期望的失败形态是**报错**（SQLITE_BUSY），不是"影响 0 行"：两者都可接受，
	// 真正不可接受的是"写入成功并改写了别人已提交的绑定"——那由下面的 owner 断言兜住。
	if err == nil {
		if n, _ := res.RowsAffected(); n == 1 {
			var owner, st string
			if err := b.h.QueryRowContext(t.Context(),
				`SELECT challenge_id, status FROM pool WHERE id=?`, qid).Scan(&owner, &st); err != nil {
				t.Fatal(err)
			}
			t.Fatalf("过期快照写成功了：题行现在属于 %s（期望 %s），status=%s", owner, bound.ChallengeID, st)
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("回滚失败：%v", err)
	}
	// 失败方不得留下任何痕迹：既没有 STALE-CID 的绑定，被抢的题仍只属于 b 的 challenge
	if n := countPoolByChallenge(t, a, "STALE-CID"); n != 0 {
		t.Fatalf("失败事务留下了 %d 行孤儿绑定", n)
	}
	if owner, _ := ownerOfQuestion(t, a, qid); owner != bCh.ID {
		t.Fatalf("题目绑定被改写：owner=%s 期望=%s", owner, bCh.ID)
	}
}

func countPoolByChallenge(t *testing.T, d *DB, cid string) int {
	t.Helper()
	var n int
	if err := d.h.QueryRowContext(t.Context(), `SELECT count(*) FROM pool WHERE challenge_id=?`, cid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func ownerOfQuestion(t *testing.T, d *DB, qid string) (string, int) {
	t.Helper()
	var owner string
	var n int
	err := d.h.QueryRowContext(t.Context(),
		`SELECT coalesce(challenge_id,''), count(*) FROM pool WHERE id=? GROUP BY challenge_id`, qid).Scan(&owner, &n)
	if err != nil {
		t.Fatal(err)
	}
	return owner, n
}

// TestQuestionBindingSurvivesReopen 不变量 #6：重启后 assigned/terminal 题不会被再次发出。
func TestQuestionBindingSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epocha.db")
	d, _ := newDBAt(t, path)
	site := questionSite(t, d, "alpha")
	seedQuestions(t, d, testSpec, 2, 10*time.Hour)
	c1 := newChallengeRow(site.ID, time.Hour)
	q, err := d.IssueChallengeWithQuestion(t.Context(), c1, Caps{MaxActive: 100, MaxActivePerSite: 100}, &testSpec)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d2, _ := newDBAt(t, path)
	if n, _ := d2.ReadyCount(t.Context(), testSpec, time.Now()); n != 1 {
		t.Fatalf("重启后 ready 应为 1，got %d", n)
	}
	got, err := d2.QuestionForChallenge(t.Context(), c1.ID)
	if err != nil || got.ID != q.ID || got.Status != PoolAssigned {
		t.Fatalf("重启后题目绑定丢失：%+v err %v", got, err)
	}
	// 重启后仍能凭绑定完成终结（说明 assigned 行是可用的权威状态）
	if _, err := d2.RedeemAttempt(t.Context(), c1.ID, time.Now(), time.Minute, true); err != nil {
		t.Fatal(err)
	}
	if again, _ := d2.QuestionForChallenge(t.Context(), c1.ID); again.Status != PoolTerminal {
		t.Fatalf("重启后终态化失败：%+v", again)
	}
}

// TestPurgeKeepsLiveBindings 清理不会破坏仍然有效的绑定与防重放记录。
func TestPurgeKeepsLiveBindings(t *testing.T) {
	d := newDB(t)
	site := questionSite(t, d, "alpha")
	ids := seedQuestions(t, d, testSpec, 3, 10*time.Hour)
	caps := Caps{MaxActive: 100, MaxActivePerSite: 100}

	// A：已领取、challenge 仍有效（题必须留着）
	cA := newChallengeRow(site.ID, time.Hour)
	if _, err := d.IssueChallengeWithQuestion(t.Context(), cA, caps, &testSpec); err != nil {
		t.Fatal(err)
	}
	// B：已领取且已终结（题可清理，连带 challenge 也可清理）
	cB := newChallengeRow(site.ID, time.Hour)
	qB, err := d.IssueChallengeWithQuestion(t.Context(), cB, caps, &testSpec)
	if err != nil {
		t.Fatal(err)
	}
	rB, err := d.RedeemAttempt(t.Context(), cB.ID, time.Now(), time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	// 兑换成功即刻终结题目（清理之前它必然是 terminal 状态）
	if got, err := d.QuestionForChallenge(t.Context(), cB.ID); err != nil || got.ID != qB.ID || got.Status != PoolTerminal {
		t.Fatalf("兑换后题目应终态: %+v err %v", got, err)
	}
	// C：未领取但已过期的题（可清理）
	d.h.ExecContext(t.Context(), `UPDATE pool SET expires_at=? WHERE id=?`,
		formatTime(time.Now().Add(-time.Hour).UTC()), ids[2])

	c, err := d.Purge(t.Context(), time.Now().Add(10*time.Minute))
	if err != nil {
		t.Fatalf("Purge 失败: %v", err)
	}
	if c.Pool < 1 {
		t.Fatalf("应清掉过期/终态题目行，got %+v", c)
	}
	// A 的绑定还在：challenge 未过期，所以题不能跟着 B 的清理一起消失
	if got, err := d.QuestionForChallenge(t.Context(), cA.ID); err != nil || got.Status != PoolAssigned {
		t.Fatalf("有效绑定被清理破坏了：%+v err %v", got, err)
	}
	// 已消费的 response 仍然不可重放
	if _, err := d.ConsumeResponse(t.Context(), rB.Response.ID, site.ID, time.Now().Add(time.Hour)); !errors.Is(err, ErrConsumed) {
		t.Fatalf("清理后重放应失败，got %v", err)
	}
}

// TestPurgeRemovesExpiredIssuedRows 记录一条容易被忽略的事实：
// 被放弃的 issued challenge（永不调用 redeem）也必须能被回收，否则账本单调增长。
func TestPurgeRemovesExpiredIssuedRows(t *testing.T) {
	d := newDB(t)
	site := mustSite(t, d, "alpha")
	abandoned := newChallenge(t, d, site.ID, time.Minute)
	live := newChallenge(t, d, site.ID, time.Hour)
	if _, err := d.Purge(t.Context(), time.Now().Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Challenge(t.Context(), abandoned.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("过期未用的 challenge 应被回收，got %v", err)
	}
	if _, err := d.Challenge(t.Context(), live.ID); err != nil {
		t.Fatalf("未过期的 challenge 不该被清理: %v", err)
	}
	// 删除过期行不削弱防重放：已消费的 response 依旧不可重放
	c := newChallenge(t, d, site.ID, time.Minute)
	r, err := d.Redeem(t.Context(), c.ID, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ConsumeResponse(t.Context(), r.ID, site.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Purge(t.Context(), time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ConsumeResponse(t.Context(), r.ID, site.ID, time.Now().Add(3*time.Hour)); !errors.Is(err, ErrConsumed) {
		t.Fatalf("行被删除后重放必须仍失败（fail closed），got %v", err)
	}
}

// TestRedeemAttemptWrongAnswerCommits 是 ADR-011 #2 的落点：
// 错误答案必须**提交**为终态。这条如果被"返回 error 触发回滚"的实现替代，本文件其它测试会连带失败，
// 而这里给出最直接的断言。
func TestRedeemAttemptWrongAnswerCommits(t *testing.T) {
	d := newDB(t)
	site := questionSite(t, d, "alpha")
	seedQuestions(t, d, testSpec, 1, 10*time.Hour)
	c := newChallengeRow(site.ID, time.Hour)
	if _, err := d.IssueChallengeWithQuestion(t.Context(), c, Caps{MaxActive: 100, MaxActivePerSite: 100}, &testSpec); err != nil {
		t.Fatal(err)
	}
	res, err := d.RedeemAttempt(t.Context(), c.ID, time.Now(), time.Minute, false)
	if err != nil {
		t.Fatalf("错误答案分支绝不能返回 error（那会让事务回滚、重试机会被退回）: %v", err)
	}
	if res.Outcome != OutcomeWrongAnswer || res.Response != nil {
		t.Fatalf("结果不符：%+v", res)
	}
	got, err := d.Challenge(t.Context(), c.ID)
	if err != nil || got.Status != statusInvalid {
		t.Fatalf("challenge 未进入终态：%+v err %v", got, err)
	}
	// 再试正确答案：必须已经不可兑换
	if _, err := d.RedeemAttempt(t.Context(), c.ID, time.Now(), time.Minute, true); !errors.Is(err, ErrNotRedeemable) {
		t.Fatalf("错误答案后仍允许重试，got %v", err)
	}
}

// TestConcurrentRedeemMixedAnswers 正确与错误答案并发争抢同一 challenge：
// 最多一个终态、最多一张 response。
func TestConcurrentRedeemMixedAnswers(t *testing.T) {
	d := newDB(t)
	site := questionSite(t, d, "alpha")
	seedQuestions(t, d, testSpec, 1, 10*time.Hour)
	c := newChallengeRow(site.ID, time.Hour)
	if _, err := d.IssueChallengeWithQuestion(t.Context(), c, Caps{MaxActive: 100, MaxActivePerSite: 100}, &testSpec); err != nil {
		t.Fatal(err)
	}
	const n = 10
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	issued, wrong, errs, losers := 0, 0, 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := d.RedeemAttempt(t.Context(), c.ID, time.Now(), time.Minute, i%2 == 0)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, ErrNotRedeemable):
				losers++ // 已被别人终结：这正是"最多一个终态"的证据
			case err != nil:
				errs++
			case res.Outcome == OutcomeIssued:
				issued++
			default:
				wrong++
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if errs != 0 {
		t.Fatalf("并发里出现了非受控错误：%d", errs)
	}
	if issued > 1 {
		t.Fatalf("签发 %d 张 response，最多 1", issued)
	}
	if issued+wrong != 1 {
		t.Fatalf("终结次数 = %d（issued %d + wrong %d），必须恰好 1 次终态", issued+wrong, issued, wrong)
	}
	if losers != n-1 {
		t.Fatalf("其余 %d 路都该拿到 ErrNotRedeemable，实际 %d", n-1, losers)
	}
}

// TestInsertQuestionBounds 单题体积与总行数上限都是硬边界。
func TestInsertQuestionBounds(t *testing.T) {
	d := newDB(t)
	questionSite(t, d, "alpha")
	base := func() *PoolQuestion {
		now := time.Now().UTC()
		return &PoolQuestion{
			ID: "q-" + randSuffix(), GeneratorID: testSpec.GeneratorID, Version: testSpec.Version,
			Difficulty: testSpec.Difficulty, Status: PoolReady,
			PayloadJSON: `{"task":"1 + 1","answer_type":"integer","max_answer_len":14}`,
			Verifier:    []byte("v"), CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		}
	}
	if err := d.InsertQuestion(t.Context(), base(), 10); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertQuestion(t.Context(), base(), 1); !errors.Is(err, ErrPoolFull) {
		t.Fatalf("行数上限应生效，got %v", err)
	}
	tooBig := base()
	tooBig.PayloadJSON = pad(tooBig.PayloadJSON, MaxPayloadBytes+1)
	if err := d.InsertQuestion(t.Context(), tooBig, 100); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("超大题面应拒绝，got %v", err)
	}
	tooBigVerifier := base()
	tooBigVerifier.ID = "q-" + randSuffix()
	tooBigVerifier.Verifier = []byte(pad("v", MaxVerifierBytes+1))
	if err := d.InsertQuestion(t.Context(), tooBigVerifier, 100); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("超大 verifier 应拒绝，got %v", err)
	}
	// 分桶键必须完整：缺 version 无法判分
	noVer := base()
	noVer.ID = "q-" + randSuffix()
	noVer.Version = ""
	if err := d.InsertQuestion(t.Context(), noVer, 100); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("缺 version 应拒绝，got %v", err)
	}
	// 未知模式与越界难度
	badDiff := base()
	badDiff.ID = "q-" + randSuffix()
	badDiff.Difficulty = 99
	if err := d.InsertQuestion(t.Context(), badDiff, 100); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("难度越界应拒绝，got %v", err)
	}
	// QuestionBuckets 只报告在用站点的显式配置
	buckets, err := d.QuestionBuckets(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range buckets {
		if b.GeneratorID == testSpec.GeneratorID && b.Difficulty == testSpec.Difficulty {
			found = true
		}
	}
	if !found {
		t.Fatalf("备题分桶缺失: %+v", buckets)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func randSuffix() string {
	id, _ := NewID()
	return id[:8]
}

func pad(s string, n int) string {
	for len(s) < n {
		s += s
	}
	return s[:n]
}

// TestPowOnlySiteNeedsNoQuestions：pow_only 站点的签发完全不碰池。
func TestPowOnlySiteNeedsNoQuestions(t *testing.T) {
	d := newDB(t)
	site := mustSite(t, d, "alpha") // 默认 pow_only
	c := newChallengeRow(site.ID, time.Hour)
	c.Mode = ModePowOnly
	c.GeneratorID = ""
	q, err := d.IssueChallengeWithQuestion(t.Context(), c, Caps{MaxActive: 10, MaxActivePerSite: 10}, nil)
	if err != nil || q != nil {
		t.Fatalf("pow_only 不该领题: %+v err %v", q, err)
	}
	if n, _ := d.PoolCount(t.Context()); n != 0 {
		t.Fatalf("池被触碰：%d", n)
	}
	// 模式写进库行，供 redeem 判定
	got, err := d.Challenge(t.Context(), c.ID)
	if err != nil || got.Mode != ModePowOnly {
		t.Fatalf("模式快照未写入: %+v err %v", got, err)
	}
}

// TestIssueRejectsUnknownMode：签发时模式与 spec 的组合必须自洽，
// 不允许"库里写成 pow_question 但没带 spec"这种半成品状态被写进去。
func TestIssueRejectsUnknownMode(t *testing.T) {
	d := newDB(t)
	site := mustSite(t, d, "alpha")
	c := newChallengeRow(site.ID, time.Hour)
	if _, err := d.IssueChallengeWithQuestion(t.Context(), c, Caps{MaxActive: 10, MaxActivePerSite: 10}, nil); err != nil {
		t.Fatal(err)
	}
	// 直接走内部写入路径（模拟接线错误）：未知模式必须被拒而不是当成 pow_only 放行
	tx, err := d.h.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertChallengeTx(t.Context(), tx, c, Caps{MaxActive: 10, MaxActivePerSite: 10}, "question-only", ""); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("未知模式应拒绝，got %v", err)
	}
}
