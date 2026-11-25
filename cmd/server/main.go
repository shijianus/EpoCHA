// Command server 是 epocha 人机验证服务的单二进制入口。
//
// 启动顺序固定为 config → store（迁移）→ 引导管理凭据 → 站点一次性导入 → signer → api/admin → http；
// 任一步失败即退出，不做"部分可用"的降级：安全关键组件缺失时必须拒绝启动（DESIGN §6、红线 12）。
//
// 两个监听器：公共 API 与**独立的管理 API**。管理路由只挂在 admin 监听器上，
// 公共监听器根本没有这些路径——即便有人从公网拿到了管理凭据，也没有那条路可走。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/shijianus/EpoCHA/internal/admin"
	"github.com/shijianus/EpoCHA/internal/api"
	"github.com/shijianus/EpoCHA/internal/config"
	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/pool"
	"github.com/shijianus/EpoCHA/internal/store"
	"github.com/shijianus/EpoCHA/internal/token"
	"github.com/shijianus/EpoCHA/internal/version"
)

const (
	shutdownTimeout   = 5 * time.Second
	purgeEvery        = 10 * time.Second
	purgeMaxBatches   = 8
	purgeBudget       = 250 * time.Millisecond
	purgeYield        = 5 * time.Millisecond
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 15 * time.Second
	idleTimeout       = 20 * time.Second
	maxHeaderBytes    = 8 << 10
	bootstrapLabel    = "bootstrap"
	// questionKeyPurpose 是题目 verifier 密钥的域分隔用途串（DESIGN §6）。
	questionKeyPurpose = "qver"
)

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout); err != nil {
		// config/store/token 的错误文案只含字段名与环境变量名，不回显任何密钥值（有测试覆盖）。
		fmt.Fprintf(os.Stderr, "epocha: %s\n", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, stdout io.Writer) error {
	cfg, err := config.Load(args, getenv)
	if err != nil {
		return err
	}
	if cfg.ShowVersion {
		fmt.Fprintf(stdout, "epocha %s (commit %s, built %s)\n", version.Version, version.Commit, version.BuildDate)
		return nil
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		// 只回显地址，不回显任何密钥或站点 secret。
		return fmt.Errorf("监听 %s 失败", cfg.Listen)
	}
	adminLn, err := net.Listen("tcp", cfg.AdminListen)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("监听管理地址 %s 失败", cfg.AdminListen)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, cfg, ln, adminLn, stdout)
}

// serve 在给定 listener 上提供服务，直到 ctx 被取消。listener 由调用方创建，
// 这样测试可以用 :0 拿到确定端口，而不必猜。adminLn 为 nil 时只跑公共面。
func serve(ctx context.Context, cfg *config.Config, ln, adminLn net.Listener, stdout io.Writer) (result error) {
	ctx, stopBackground := context.WithCancel(ctx)
	defer stopBackground()
	// 启动信息与请求日志共用一个 slog 写入器：并行 Fprintf + slog 会并发写同一个 writer，
	// 既可能交错整行，也会被 -race 判为数据竞争（阶段 1 实测触发过）。
	log := slog.New(slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("启动", "version", version.Version, "config", cfg.SafeSummary())

	// listener 由调用方创建，所以也必须由本函数保证关闭：http.Server.Shutdown 只会关闭
	// "已经被 Serve 登记过"的监听器。若在 Serve 真正接管之前 ctx 就取消（竞态测试里实测出现过），
	// 端口会保持打开——进程已退出但端口还在接受连接。
	defer func() { _ = ln.Close() }()
	if adminLn != nil {
		defer func() { _ = adminLn.Close() }()
	}

	gens, err := configuredGenerators(cfg, token.DerivePurpose(cfg.MasterKey, questionKeyPurpose))
	if err != nil {
		return err
	}
	// 运行锁必须在 store.Open（含迁移）**之前**取得，并且只能在 db.Close **之后**释放。
	// defer 是 LIFO，所以这一条必须注册在 db.Close 的 defer 之前——顺序写反了会出现
	// "锁已释放、库还开着"的窗口，那正好是离线运维能挤进来的缝。
	lock, err := store.AcquireDataLock(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if lerr := lock.Close(); lerr != nil {
			log.Error("释放运行锁", "err", lerr)
		}
	}()
	log.Info("运行锁已持有", "path", lock.Path())

	db, err := store.Open(ctx, cfg.DBPath, cfg.Pepper)
	if err != nil {
		return fmt.Errorf("打开数据库: %w", err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			log.Error("关闭数据库", "err", cerr)
		}
	}()

	if err := bootstrapAdmin(ctx, cfg, db, adminLn, log); err != nil {
		return err
	}
	if err := importSites(ctx, cfg, db, log); err != nil {
		return err
	}

	// 题池 worker：只有存在 pow_question 站点时才会真正生成题目（QuestionBuckets 为空即空转等待）。
	// 因此默认启用不是"多一个后台负载"，而是让开关只在需要时才有代价。
	var pw *pool.Worker
	if cfg.PoolEnabled {
		pc := pool.DefaultConfig()
		pc.LowWatermark, pc.HighWatermark, pc.MaxRows = cfg.PoolLow, cfg.PoolHigh, cfg.PoolMaxRows
		pc.Workers, pc.QuestionTTL, pc.PollInterval = cfg.PoolWorkers, cfg.PoolQuestionTTL, cfg.PoolPoll
		pc.ImageLowWatermark, pc.ImageHighWatermark = cfg.PoolImageLow, cfg.PoolImageHigh
		pc.ImageMaxTotalBytes, pc.ImageYieldPause = cfg.PoolImageMaxBytes, cfg.PoolImageYield
		pw, err = pool.New(db, gens, pc, log, nil)
		if err != nil {
			return fmt.Errorf("初始化题池: %w", err)
		}
		pw.Start(ctx)
		defer func() { pw.StopCancel(); pw.Wait() }()
		log.Info("题池已启用", "low", cfg.PoolLow, "high", cfg.PoolHigh,
			"max_rows", cfg.PoolMaxRows, "workers", cfg.PoolWorkers, "question_ttl", cfg.PoolQuestionTTL.String(),
			"image_low", cfg.PoolImageLow, "image_high", cfg.PoolImageHigh,
			"image_bytes", cfg.PoolImageMaxBytes, "image_yield", cfg.PoolImageYield.String())
	}

	signer, err := token.NewSigner(cfg.MasterKey, cfg.PrevMaster)
	if err != nil {
		return fmt.Errorf("初始化签名器: %w", err)
	}
	handler, err := api.New(cfg, db, signer, gens, log)
	if err != nil {
		return fmt.Errorf("初始化 API: %w", err)
	}
	requestCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	var requests sync.WaitGroup
	var requestMu sync.Mutex
	accepting := true
	track := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestMu.Lock()
			if !accepting {
				requestMu.Unlock()
				return
			}
			requests.Add(1)
			requestMu.Unlock()
			defer requests.Done()
			next.ServeHTTP(w, r)
		})
	}
	srv := newHTTPServer(ln, track(handler.Handler()))
	srv.BaseContext = func(net.Listener) context.Context { return requestCtx }

	var adminSrv *http.Server
	if adminLn != nil {
		// PoolObserver 是接口：把 typed-nil（*pool.Worker(nil)）装进去后接口 != nil，
		// handlePoolStatus 的"池禁用"分支永远走不到，Stats() 会在 nil 接收者上 panic。
		// 池禁用时必须传真正的 nil，让管理台如实报告 worker.enabled=false。
		var poolObserver admin.PoolObserver
		if pw != nil {
			poolObserver = pw
		}
		adminHandler, err := admin.New(cfg, db, log, poolObserver, gens)
		if err != nil {
			return fmt.Errorf("初始化管理 API: %w", err)
		}
		adminSrv = newHTTPServer(adminLn, track(adminHandler.Handler()))
		adminSrv.BaseContext = func(net.Listener) context.Context { return requestCtx }
		log.Info("管理面已挂载", "addr", adminLn.Addr().String())
	}

	httpErr := make(chan error, 2)
	var serving sync.WaitGroup
	startHTTP := func(server *http.Server, listener net.Listener, label string) {
		serving.Add(1)
		go func() {
			defer serving.Done()
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				httpErr <- fmt.Errorf("%s: %w", label, err)
			}
		}()
	}
	purgeDone := make(chan struct{})
	go purgeLoop(ctx, db, log, purgeDone)
	defer func() {
		stopBackground()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		var closing sync.WaitGroup
		closeErrors := make(chan error, 2)
		for _, server := range []*http.Server{srv, adminSrv} {
			if server == nil {
				continue
			}
			closing.Add(1)
			go func(server *http.Server) {
				defer closing.Done()
				if err := shutdownHTTP(shutdownCtx, server); err != nil {
					closeErrors <- err
				}
			}(server)
		}
		closing.Wait()
		requestMu.Lock()
		accepting = false
		requestMu.Unlock()
		cancelRequests()
		serving.Wait()
		requests.Wait()
		<-purgeDone
		close(closeErrors)
		for err := range closeErrors {
			result = errors.Join(result, fmt.Errorf("关闭 HTTP: %w", err))
		}
		log.Info("已停止")
	}()
	startHTTP(srv, ln, "公共 API")
	if adminSrv != nil {
		startHTTP(adminSrv, adminLn, "管理 API")
	}

	log.Info("已监听", "addr", ln.Addr().String())
	select {
	case <-ctx.Done():
		log.Info("收到退出信号，开始优雅关闭")
		return nil
	case err := <-httpErr:
		return err
	}
}

// bootstrapAdmin 处理管理引导凭据（DESIGN §6）：库里没有任何管理密钥记录时登记 env 带来的那把，
// 有记录之后这个环境变量**不再生效**——否则"我在管理 API 里撤销了它"会被下一次重启悄悄推翻。
func bootstrapAdmin(ctx context.Context, cfg *config.Config, db *store.DB, adminLn net.Listener, log *slog.Logger) error {
	if len(cfg.AdminKey) > 0 {
		key, already, err := db.BootstrapAdminKey(ctx, string(cfg.AdminKey), bootstrapLabel)
		switch {
		case errors.Is(err, store.ErrConflict):
			// 两个实例抢同一个库时可能撞标签；不影响安全性，继续以库里的记录为准。
			already = true
		case err != nil:
			return fmt.Errorf("登记引导管理凭据: %w", err)
		}
		if already {
			log.Info("管理凭据已存在于库中，忽略 EPOCHA_ADMIN_KEY（撤销不会因重启复活）")
		} else {
			log.Info("已登记引导管理凭据", "key_id", key.KeyID, "label", key.Label)
		}
	}
	keys, err := db.ListAdminKeys(ctx)
	if err != nil {
		return fmt.Errorf("检查管理凭据: %w", err)
	}
	if len(keys) == 0 {
		log.Warn("库中没有可用管理凭据：管理 API 一律拒绝（不设 EPOCHA_ADMIN_KEY 等于关闭管理面）")
	}
	if adminLn != nil && !isLoopback(adminLn.Addr()) {
		log.Warn("管理监听地址不是 loopback，请在反代/防火墙侧再加一层来源限制", "addr", adminLn.Addr().String())
	}
	return nil
}

func isLoopback(addr net.Addr) bool {
	tcpAddr, ok := addr.(*net.TCPAddr)
	return ok && tcpAddr.IP.IsLoopback()
}

// importSites 是配置文件的**一次性导入**：库里已有同一个 site_key 时什么都不做。
// 这条路径让"从阶段 1 升级"和脚本化初始化可写，但它绝不覆盖权威数据源——
// 已撤销的密钥不会被配置文件里的旧值复活，已停用的站点不会被重启重新启用。
func importSites(ctx context.Context, cfg *config.Config, db *store.DB, log *slog.Logger) error {
	for _, s := range cfg.Sites {
		// 配置文件里的 TTL/限流只在"首次导入"时成为站点策略；之后它们是库里的一行数据，
		// 改这些环境数字不会再影响已导入的站点（DESIGN §5.3）。
		policy := store.SitePolicy{
			PowBits: s.PowBits, AllowMissingOrigin: s.AllowMissingOrigin,
			ChallengeTTL: cfg.ChallengeTTL, ResponseTTL: cfg.ResponseTTL,
			RateIPRPM: cfg.RateIPRPM, RateSiteRPM: cfg.RateSiteRPM,
		}
		site, imported, err := db.ImportSite(ctx, s.Name, s.SiteKey, s.Secret, policy)
		if err != nil {
			return fmt.Errorf("导入站点 %q: %w", s.Name, err)
		}
		if !imported {
			log.Info("站点已在库中，配置条目不生效（SQLite 是权威数据源）",
				"site_key", site.SiteKey, "enabled", site.Enabled, "pow_bits", int(site.PowBits))
			continue
		}
		if len(s.Origins) > 0 {
			if _, err := db.ReplaceOrigins(ctx, site.ID, s.Origins); err != nil {
				return fmt.Errorf("导入站点 %q 的来源: %w", s.Name, err)
			}
		}
		log.Info("站点已导入", "name", site.Name, "site_key", site.SiteKey,
			"pow_bits", int(site.PowBits), "origins", len(s.Origins),
			"allow_missing_origin", site.Policy.AllowMissingOrigin)
	}
	return nil
}

func newHTTPServer(ln net.Listener, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              ln.Addr().String(),
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
}

func shutdownHTTP(ctx context.Context, srv *http.Server) error {
	if err := srv.Shutdown(ctx); err != nil {
		return errors.Join(err, srv.Close())
	}
	return nil
}

// purgeLoop 只清理账本与审计体积，不参与任何安全判定：过期状态一律由时间戳即时判定，
// 因此清理任务停摆也不会让已消费 token 重新可用（DESIGN §5.1）。
func purgeLoop(ctx context.Context, db *store.DB, log *slog.Logger, done chan<- struct{}) {
	defer close(done)
	run := func(now time.Time) {
		if db == nil || ctx.Err() != nil {
			return
		}
		roundCtx, cancel := context.WithTimeout(ctx, purgeBudget)
		c, err := purgeLedgerRoundDB(roundCtx, db, now)
		cancel()
		if err != nil {
			log.Error("清理账本", "err", errKindOf(err))
			return
		}
		a, err := db.PurgeAudit(ctx, now)
		if err != nil {
			log.Error("清理审计", "err", errKindOf(err))
		}
		if c.Responses > 0 || c.Challenges > 0 || c.Pool > 0 || a > 0 {
			log.Info("清理", "responses", c.Responses, "pool", c.Pool,
				"challenges", c.Challenges, "audit", a)
		}
	}
	// Run once at startup so a restart does not leave an already-known backlog
	// untouched for a full scheduler interval.
	run(time.Now().UTC())
	t := time.NewTicker(purgeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			run(now)
		}
	}
}

func purgeLedgerRoundDB(ctx context.Context, db *store.DB, now time.Time) (store.PurgeCounts, error) {
	return purgeLedgerRound(ctx, now, db.Purge)
}

func purgeLedgerRound(ctx context.Context, now time.Time, purge func(context.Context, time.Time) (store.PurgeCounts, error)) (store.PurgeCounts, error) {
	deadline, cancel := context.WithTimeout(ctx, purgeBudget)
	defer cancel()
	var total store.PurgeCounts
	for batch := 0; batch < purgeMaxBatches; batch++ {
		if err := deadline.Err(); err != nil {
			return total, err
		}
		counts, err := purge(deadline, now)
		if err != nil {
			return total, err
		}
		total.Responses += counts.Responses
		total.Pool += counts.Pool
		total.Challenges += counts.Challenges
		if counts.Responses == 0 && counts.Pool == 0 && counts.Challenges == 0 {
			return total, nil
		}
		if batch+1 < purgeMaxBatches {
			timer := time.NewTimer(purgeYield)
			select {
			case <-deadline.Done():
				timer.Stop()
				return total, deadline.Err()
			case <-timer.C:
			}
		}
	}
	return total, nil
}

// errKindOf 只保留错误类别：驱动/SQL 文本会带表名与约束名，不该进日志。
func errKindOf(err error) string {
	if err == nil {
		return "nil"
	}
	return "unspecified"
}

// knownGenerators 列出**本程序支持**的生成器（不受白名单影响）。
//
// 视觉生成器的构造可能因为内嵌字体缺字形而失败——那必须让服务拒绝启动，
// 而不是带着一个"题面永远答不对"的题型上线（DESIGN §17 阶段 3B）。
func knownGenerators(qKey []byte) ([]generator.Generator, error) {
	return selectGenerators(generatorFactories(qKey), nil, false, generator.Lifecycle)
}

type generatorFactory struct {
	id  string
	new func() (generator.Generator, error)
}

func generatorFactories(qKey []byte) []generatorFactory {
	return []generatorFactory{
		{generator.ArithID, func() (generator.Generator, error) { return generator.NewArithmeticBaseline(qKey) }},
		{generator.TextImageID, func() (generator.Generator, error) { return generator.NewTextImage(qKey) }},
		{generator.TextImageV2ID, func() (generator.Generator, error) { return generator.NewTextImageV2(qKey) }},
		{generator.TextImageV3ID, func() (generator.Generator, error) { return generator.NewTextImageV3(qKey) }},
		{generator.ShapeGridID, func() (generator.Generator, error) { return generator.NewShapeGrid(qKey) }},
		{generator.ShapeCountID, func() (generator.Generator, error) { return generator.NewShapeCount(qKey) }},
		{generator.ParallaxID, func() (generator.Generator, error) { return generator.NewParallax(qKey) }},
		{generator.BankShotID, func() (generator.Generator, error) { return generator.NewBankShot(qKey) }},
		{generator.AccessibleChoiceID, func() (generator.Generator, error) { return generator.NewAccessibleChoice(qKey) }},
		{generator.EnergyWindowID, func() (generator.Generator, error) { return generator.NewEnergyWindow(qKey) }},
		{generator.PipeConnectID, func() (generator.Generator, error) { return generator.NewPipeConnect(qKey) }},
		{generator.IntersectionPointID, func() (generator.Generator, error) { return generator.NewIntersectionPoint(qKey) }},
		{generator.TileRotationID, func() (generator.Generator, error) { return generator.NewTileRotation(qKey) }},
		{generator.SymbolPathID, func() (generator.Generator, error) { return generator.NewSymbolPath(qKey) }},
		{generator.CrowdFindID, func() (generator.Generator, error) { return generator.NewCrowdFind(qKey) }},
		{generator.VisualRankID, func() (generator.Generator, error) { return generator.NewVisualRank(qKey) }},
		{generator.ProgressiveTargetID, func() (generator.Generator, error) { return generator.NewProgressiveTarget(qKey) }},
		{generator.ShellTrackID, func() (generator.Generator, error) { return generator.NewShellTrack(qKey) }},
		{generator.SilhouetteAssemblyID, func() (generator.Generator, error) { return generator.NewSilhouetteAssembly(qKey) }},
	}
}

func selectGenerators(factories []generatorFactory, allow []string, production bool, lifecycle func(string) (bool, bool)) ([]generator.Generator, error) {
	eligible := func(id string) bool {
		experimental, ready := lifecycle(id)
		return !experimental && ready
	}
	for _, id := range allow {
		found := false
		for _, f := range factories {
			if f.id == id {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("初始化生成器: EPOCHA_GENERATORS 含未知题型名 %q", id)
		}
		if production && !eligible(id) {
			return nil, generator.ErrProductionProfile
		}
	}
	var out []generator.Generator
	for _, f := range factories {
		if !generatorEnabled(allow, f.id) || (production && !eligible(f.id)) {
			continue
		}
		g, err := f.new()
		if err != nil {
			return nil, fmt.Errorf("初始化生成器 %s: %w", f.id, err)
		}
		if g == nil || g.ID() != f.id {
			return nil, errors.New("初始化生成器: 构造器标识不一致")
		}
		out = append(out, g)
	}
	if len(out) == 0 && production {
		return nil, generator.ErrProductionProfile
	}
	return out, nil
}

func configuredGenerators(cfg *config.Config, qKey []byte) (*generator.Registry, error) {
	known, err := selectGenerators(generatorFactories(qKey), cfg.Generators, cfg.ProductionProfile, generator.Lifecycle)
	if err != nil {
		return nil, err
	}
	reg := generator.NewRegistry()
	for _, g := range known {
		if err := reg.Register(g); err != nil {
			return nil, err
		}
	}
	if len(reg.IDs()) == 0 {
		return nil, errors.New("初始化生成器: 没有可用题型")
	}
	if cfg.ProductionProfile {
		if err := reg.RequireProduction(); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// generatorEnabled：空白名单 = 全部启用。
func generatorEnabled(allow []string, id string) bool {
	if len(allow) == 0 {
		return true
	}
	for _, a := range allow {
		if a == id {
			return true
		}
	}
	return false
}

func isKnownGeneratorID(known []generator.Generator, id string) bool {
	for _, g := range known {
		if g.ID() == id {
			return true
		}
	}
	return false
}

func generatorIDs(known []generator.Generator) []string {
	out := make([]string, 0, len(known))
	for _, g := range known {
		out = append(out, g.ID())
	}
	return out
}
