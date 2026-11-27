// Command devseed 是**离线开发工具**：往题池里写若干道"答案已知"的题，并把答案与图片写到本地目录。
//
// 为什么需要它：端到端验收必须能提交正确答案，而生产 API 刻意**没有**任何答案出口
// （DESIGN §10.1、红线 5）。devclient 自己解析公开题面求值，那条路对算术题成立、
// 对图片题不成立（图片里没有可机读的文本层，反推需要 OCR，而本环境没有离线 OCR）。
// 所以脚本需要一个"从生成侧注入受控题目"的入口。
//
// 它**没有**削弱任何安全性质，理由要写清楚：
//   - 它要求主密钥与 pepper（与运维同一信任级别）。拿到主密钥的人本来就能离线枚举所有
//     verifier（DESIGN §4 已列为残余风险，也正是 pepper 必须与库分开的理由）；
//   - 它只操作本地文件里的库，不监听端口、不新增任何 HTTP 接口；
//   - 它写的是自己新生成的题，**读不出已存在题目的答案**：没有任何"答案查询"能力。
//
// 注入用的熵是一条本地随机 seed 展开的确定性流（而不是 crypto/rand）：这是为了让
// "同一 seed 两次消费"能给出配对的 (答案, 图片)。每道被注入的题因此对持有该 seed 的人可预测
// —— 这正是"它是测试夹具、不是生产题目"的定义，也是它只能作为开发工具的理由。
// 每次运行都重新随机取 seed，所以重复运行仍会得到不同的题。
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shijianus/EpoCHA/internal/eval"
	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/store"
	"github.com/shijianus/EpoCHA/internal/token"
)

const (
	envMasterKey = "EPOCHA_MASTER_KEY"
	envPepper    = "EPOCHA_PEPPER"
	// questionKeyPurpose 与 cmd/server 用的是同一个 purpose 串：
	// 派生不一致的话，写进去的题在服务端永远判假（那正是"两套真相"的样子）。
	questionKeyPurpose = "qver"
	maxCount           = 50
)

func main() {
	if err := run(os.Args[1:], os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "devseed:", err)
		os.Exit(1)
	}
}

type options struct {
	DBPath     string
	OutDir     string
	Generator  string
	Difficulty int
	Count      int
	TTL        time.Duration
	MaxRows    int
	MaxImageB  int64
}

// record 是一条被注入的题（答案表的一行）。文件只落在 --out-dir，不进仓库。
type record struct {
	PoolID     string `json:"pool_id"`
	Answer     string `json:"answer"`
	Generator  string `json:"generator"`
	Version    string `json:"version"`
	Difficulty int    `json:"difficulty"`
	PNG        string `json:"png,omitempty"`
	Bytes      int    `json:"asset_bytes,omitempty"`
	CreatedAt  string `json:"created_at_utc"`
}

func run(args []string, getenv func(string) string) error {
	fs := flag.NewFlagSet("devseed", flag.ContinueOnError)
	var o options
	fs.StringVar(&o.DBPath, "db", getenv("EPOCHA_DB"), "SQLite 库路径（必填）")
	fs.StringVar(&o.OutDir, "out-dir", "", "答案与图片的输出目录（必填，本地开发目录）")
	fs.StringVar(&o.Generator, "generator", generator.TextImageID, "题型：text-image-v1/v2/v3、shape-grid-v1、shape-count-v1、parallax-code-v1、bank-shot-v1、accessible-choice-v1、energy-window-v1、pipe-connect-v1、intersection-point-v1、tile-rotation-v1、symbol-path-v1、crowd-find-v1、visual-rank-v1、progressive-target-v1、shell-track-v1、silhouette-assembly-v1 或 arithmetic-baseline-v1")
	fs.IntVar(&o.Difficulty, "difficulty", 2, "难度档 1..5，必须与站点配置一致，否则领不到")
	fs.IntVar(&o.Count, "count", 1, "注入条数 1..50")
	fs.DurationVar(&o.TTL, "ttl", 70*time.Minute, "题目寿命（必须 > 站点 challenge TTL）")
	fs.IntVar(&o.MaxRows, "max-rows", 20000, "题池行数上限（与服务端配置一致）")
	fs.Int64Var(&o.MaxImageB, "max-image-bytes", 4<<20, "全局图片字节预算（与服务端配置一致）")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("命令行参数: %w", err)
	}
	if o.DBPath == "" || o.OutDir == "" {
		return errors.New("必须给 --db 与 --out-dir")
	}
	if o.Count < 1 || o.Count > maxCount {
		return fmt.Errorf("注入条数须在 1..%d", maxCount)
	}
	if o.Difficulty < 1 || o.Difficulty > generator.MaxDifficulties {
		return fmt.Errorf("难度档须在 1..%d", generator.MaxDifficulties)
	}
	if o.TTL <= time.Minute {
		return errors.New("题目寿命过短：领题条件要求题目比 challenge 晚过期")
	}
	master, err := readKey(getenv, envMasterKey)
	if err != nil {
		return err
	}
	pepper, err := readKey(getenv, envPepper)
	if err != nil {
		return err
	}
	qKey := token.DerivePurpose(master, questionKeyPurpose)
	if len(qKey) < 32 {
		return errors.New("题目判分派生密钥长度不足")
	}

	g, err := buildGenerator(o.Generator, qKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.OutDir, 0o700); err != nil {
		return fmt.Errorf("创建输出目录: %w", err)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, o.DBPath, pepper)
	if err != nil {
		return fmt.Errorf("打开库: %w", err)
	}
	defer func() { _ = db.Close() }()

	now := time.Now().UTC()
	recs := make([]record, 0, o.Count)
	for i := 0; i < o.Count; i++ {
		rec, err := seedOne(ctx, db, g, o, i, now)
		if err != nil {
			return err
		}
		recs = append(recs, *rec)
	}
	raw, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	outPath := filepath.Join(o.OutDir, "seeded-answers.json")
	if err := os.WriteFile(outPath, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("已注入 %d 道题（题型 %s 难度 %d），答案表：%s\n", len(recs), g.ID(), o.Difficulty, outPath)
	for _, r := range recs {
		fmt.Printf("  %s answer=%s png=%s(%d B)\n", r.PoolID, r.Answer, r.PNG, r.Bytes)
	}
	return nil
}

func buildGenerator(id string, qKey []byte) (generator.Generator, error) {
	switch id {
	case generator.TextImageID:
		g, err := generator.NewTextImage(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造视觉题型: %w", err)
		}
		return g, nil
	case generator.TextImageV2ID:
		g, err := generator.NewTextImageV2(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造视觉题型 v2: %w", err)
		}
		return g, nil
	case generator.TextImageV3ID:
		g, err := generator.NewTextImageV3(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造抽象文字题型 v3: %w", err)
		}
		return g, nil
	case generator.ShapeGridID:
		g, err := generator.NewShapeGrid(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造图形点选题型: %w", err)
		}
		return g, nil
	case generator.ShapeCountID:
		g, err := generator.NewShapeCount(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造图形计数题型: %w", err)
		}
		return g, nil
	case generator.ParallaxID:
		g, err := generator.NewParallax(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造时序视差题型: %w", err)
		}
		return g, nil
	case generator.BankShotID:
		g, err := generator.NewBankShot(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造物理反弹题型: %w", err)
		}
		return g, nil
	case generator.AccessibleChoiceID:
		g, err := generator.NewAccessibleChoice(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造无障碍文本题型: %w", err)
		}
		return g, nil
	case generator.EnergyWindowID:
		g, err := generator.NewEnergyWindow(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造瞬态时机题型: %w", err)
		}
		return g, nil
	case generator.PipeConnectID:
		g, err := generator.NewPipeConnect(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造拓扑连通题型: %w", err)
		}
		return g, nil
	case generator.IntersectionPointID:
		g, err := generator.NewIntersectionPoint(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造交点外推题型: %w", err)
		}
		return g, nil
	case generator.TileRotationID:
		g, err := generator.NewTileRotation(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造路径块旋转题型: %w", err)
		}
		return g, nil
	case generator.SymbolPathID:
		g, err := generator.NewSymbolPath(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造符号路径题型: %w", err)
		}
		return g, nil
	case generator.CrowdFindID:
		g, err := generator.NewCrowdFind(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造找相同角色题型: %w", err)
		}
		return g, nil
	case generator.VisualRankID:
		g, err := generator.NewVisualRank(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造视觉排序题型: %w", err)
		}
		return g, nil
	case generator.ProgressiveTargetID:
		g, err := generator.NewProgressiveTarget(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造渐进定位题型: %w", err)
		}
		return g, nil
	case generator.ShellTrackID:
		g, err := generator.NewShellTrack(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造容器跟踪题型: %w", err)
		}
		return g, nil
	case generator.SilhouetteAssemblyID:
		g, err := generator.NewSilhouetteAssembly(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造轮廓装配题型: %w", err)
		}
		return g, nil
	case generator.ArithID:
		g, err := generator.NewArithmeticBaseline(qKey)
		if err != nil {
			return nil, fmt.Errorf("构造算术题型: %w", err)
		}
		return g, nil
	default:
		return nil, fmt.Errorf("未知题型 %q", id)
	}
}

// seedOne 生成一题、自检"答案与图片配对"、写库、落盘。
//
// 配对自检不是可选项：如果答案与图不匹配，脚本提交"正确答案"会失败，
// 而那次失败看起来像"判分坏了"——所以宁可在这里就报错。
func seedOne(ctx context.Context, db *store.DB, g generator.Generator, o options, i int, now time.Time) (*record, error) {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("取注入种子: %w", err)
	}
	s1, err := eval.NewDeterministic(seed)
	if err != nil {
		return nil, err
	}
	s2, err := eval.NewDeterministic(seed)
	if err != nil {
		return nil, err
	}
	params := generator.Params{Difficulty: o.Difficulty}
	answer, err := answerFromStream(g, s1, params)
	if err != nil {
		return nil, fmt.Errorf("取答案: %w", err)
	}
	q, err := g.Generate(ctx, params, s2)
	if err != nil {
		return nil, fmt.Errorf("生成题目: %w", err)
	}
	if !g.Verify(q.Verifier, answer) {
		return nil, errors.New("答案与题面不匹配（熵流位置不一致），拒绝注入")
	}
	payload, err := json.Marshal(q.Payload)
	if err != nil {
		return nil, err
	}
	if _, err := g.SanitizePublic(payload); err != nil {
		return nil, fmt.Errorf("题面不合法: %w", err)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	poolID := "devseed" + fmt.Sprintf("%x", id)
	row := &store.PoolQuestion{
		ID: poolID, GeneratorID: g.ID(), Version: g.Version(), Difficulty: o.Difficulty,
		Status: store.PoolReady, PayloadJSON: string(payload), Verifier: []byte(q.Verifier),
		Asset: q.Asset, AssetMime: q.MimeType,
		CreatedAt: now, ExpiresAt: now.Add(o.TTL),
	}
	if err := db.InsertQuestion(ctx, row, store.PoolLimits{
		MaxRows: o.MaxRows, MaxImageBytes: o.MaxImageB,
	}); err != nil {
		return nil, fmt.Errorf("写入题池: %w", err)
	}
	rec := &record{
		PoolID: poolID, Answer: answer, Generator: g.ID(), Version: g.Version(),
		Difficulty: o.Difficulty, CreatedAt: now.Format(time.RFC3339),
	}
	if len(q.Asset) > 0 {
		if err := os.MkdirAll(o.OutDir, 0o700); err != nil {
			return nil, fmt.Errorf("创建输出目录: %w", err)
		}
		name := "question-" + strconv.Itoa(i+1) + ".png"
		if err := os.WriteFile(filepath.Join(o.OutDir, name), q.Asset, 0o600); err != nil {
			return nil, err
		}
		rec.PNG = name
		rec.Bytes = len(q.Asset)
	}
	return rec, nil
}

// answerFromStream 按**与生产 Generate 相同的抽样顺序**从熵流里取出答案。
// 算术题型走另一条路：它的答案可由公开题面独立求值（不消费熵流），
// 所以这里对它返回的是"求值结果"，配对由下面的 Verify 复核。
func answerFromStream(g generator.Generator, stream io.Reader, params generator.Params) (string, error) {
	switch g.ID() {
	case generator.TextImageID:
		return generator.TextImageAnswerFrom(stream)
	case generator.TextImageV2ID:
		return generator.TextImageV2AnswerFrom(stream)
	case generator.TextImageV3ID:
		return generator.TextImageV3AnswerFrom(stream)
	case generator.ShapeGridID:
		return generator.ShapeGridAnswerFrom(stream)
	case generator.ShapeCountID:
		return generator.ShapeCountAnswerFrom(stream)
	case generator.ParallaxID:
		return generator.ParallaxAnswerFrom(stream, params.Difficulty)
	case generator.BankShotID:
		return generator.BankShotAnswerFrom(stream, params.Difficulty)
	case generator.AccessibleChoiceID:
		return generator.AccessibleChoiceAnswerFrom(stream, params.Difficulty)
	case generator.EnergyWindowID:
		return generator.EnergyWindowAnswerFrom(stream, params.Difficulty)
	case generator.PipeConnectID:
		return generator.PipeConnectAnswerFrom(stream, params.Difficulty)
	case generator.IntersectionPointID:
		return generator.IntersectionPointAnswerFrom(stream, params.Difficulty)
	case generator.ArithID:
		// 算术题：先按同一条流（同一难度）生成一次拿到题面，再独立求值
		q, err := g.Generate(context.Background(), params, stream)
		if err != nil {
			return "", err
		}
		v, err := evalExpr(q.Payload.(generator.ArithPublic).Task)
		if err != nil {
			return "", err
		}
		return strconv.FormatInt(v, 10), nil
	}
	return "", fmt.Errorf("题型 %s 不支持受控注入", g.ID())
}

func readKey(getenv func(string) string, name string) ([]byte, error) {
	v := getenv(name)
	if v == "" {
		return nil, fmt.Errorf("缺少环境变量 %s", name)
	}
	k, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, fmt.Errorf("%s 不是合法 base64", name)
	}
	if len(k) < 32 {
		return nil, fmt.Errorf("%s 解码后短于 32 字节", name)
	}
	return k, nil
}

// evalExpr 是 devclient 的独立表达式求值（同一份逻辑：不读库就能算出答案）。
func evalExpr(task string) (int64, error) {
	tokens := strings.Fields(task)
	apply := func(a int64, op string, b int64) (int64, error) {
		switch op {
		case "+":
			return a + b, nil
		case "-":
			return a - b, nil
		case "×":
			return a * b, nil
		case "÷":
			if b == 0 || a%b != 0 {
				return 0, fmt.Errorf("非整除：%d ÷ %d", a, b)
			}
			return a / b, nil
		}
		return 0, fmt.Errorf("未知运算符 %q", op)
	}
	num := func(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }
	if (len(tokens) == 5 || len(tokens) == 7) && strings.HasPrefix(tokens[0], "(") && strings.HasSuffix(tokens[2], ")") {
		a, err := num(strings.TrimPrefix(tokens[0], "("))
		if err != nil {
			return 0, err
		}
		b, err := num(strings.TrimSuffix(tokens[2], ")"))
		if err != nil {
			return 0, err
		}
		inner, err := apply(a, tokens[1], b)
		if err != nil {
			return 0, err
		}
		if len(tokens) == 7 {
			if !strings.HasPrefix(tokens[4], "(") || !strings.HasSuffix(tokens[6], ")") {
				return 0, fmt.Errorf("第二组括号不完整：%q", task)
			}
			c, err := num(strings.TrimPrefix(tokens[4], "("))
			if err != nil {
				return 0, err
			}
			d, err := num(strings.TrimSuffix(tokens[6], ")"))
			if err != nil {
				return 0, err
			}
			right, err := apply(c, tokens[5], d)
			if err != nil {
				return 0, err
			}
			return apply(inner, tokens[3], right)
		}
		c, err := num(tokens[4])
		if err != nil {
			return 0, err
		}
		return apply(inner, tokens[3], c)
	}
	if len(tokens) != 3 {
		return 0, fmt.Errorf("题面形态不认识：%q", task)
	}
	a, err := num(tokens[0])
	if err != nil {
		return 0, err
	}
	b, err := num(tokens[2])
	if err != nil {
		return 0, err
	}
	return apply(a, tokens[1], b)
}
