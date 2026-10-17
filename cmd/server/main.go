// Command server 是 epocha 人机验证服务的单二进制入口。
//
// 启动顺序固定为 config → store（迁移）→ signer → api → http；任一步失败即退出，
// 不做"部分可用"的降级：安全关键组件缺失时必须拒绝启动（DESIGN §6、红线 12）。
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
	"syscall"
	"time"

	"github.com/shijianus/EpoCHA/internal/api"
	"github.com/shijianus/EpoCHA/internal/config"
	"github.com/shijianus/EpoCHA/internal/store"
	"github.com/shijianus/EpoCHA/internal/token"
	"github.com/shijianus/EpoCHA/internal/version"
)

const (
	shutdownTimeout   = 5 * time.Second
	purgeEvery        = time.Minute
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 15 * time.Second
	idleTimeout       = 20 * time.Second
	maxHeaderBytes    = 8 << 10
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, cfg, ln, stdout)
}

// serve 在给定 listener 上提供服务，直到 ctx 被取消。listener 由调用方创建，
// 这样测试可以用 :0 拿到确定端口，而不必猜。
func serve(ctx context.Context, cfg *config.Config, ln net.Listener, stdout io.Writer) error {
	// 启动信息与请求日志共用一个 slog 写入器：并行 Fprintf + slog 会并发写同一个 writer，
	// 既可能交错整行，也会被 -race 判为数据竞争（cmd/server 测试实测触发过）。
	log := slog.New(slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	log.Info("启动", "version", version.Version, "config", cfg.SafeSummary())

	db, err := store.Open(ctx, cfg.DBPath, cfg.Pepper)
	if err != nil {
		return fmt.Errorf("打开数据库: %w", err)
	}
	defer func() {
		if cerr := db.Close(); cerr != nil {
			log.Error("关闭数据库", "err", cerr)
		}
	}()

	// 站点来自配置（阶段 1）；只打印公开标识。
	for _, s := range cfg.Sites {
		site, err := db.PutSite(ctx, s.Name, s.SiteKey, s.Secret, s.PowBits)
		if err != nil {
			return fmt.Errorf("登记站点 %q: %w", s.Name, err)
		}
		log.Info("站点已就绪", "name", site.Name, "site_key", site.SiteKey, "pow_bits", int(site.PowBits))
	}

	signer, err := token.NewSigner(cfg.MasterKey, cfg.PrevMaster)
	if err != nil {
		return fmt.Errorf("初始化签名器: %w", err)
	}
	handler, err := api.New(cfg, db, signer, log)
	if err != nil {
		return fmt.Errorf("初始化 API: %w", err)
	}

	srv := &http.Server{
		Addr:              ln.Addr().String(),
		Handler:           handler.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}

	httpErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- err
		}
	}()
	purgeDone := make(chan struct{})
	go purgeLoop(ctx, db, log, purgeDone)

	log.Info("已监听", "addr", ln.Addr().String())
	select {
	case <-ctx.Done():
		log.Info("收到退出信号，开始优雅关闭")
	case err := <-httpErr:
		// 不在这里 close(purgeDone)：清理 goroutine 会在 ctx 取消后自行关闭，
		// 手动关一次再被 defer 关第二次就是 close of closed channel。
		return fmt.Errorf("HTTP 服务: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("关闭 HTTP: %w", err)
	}
	<-purgeDone
	log.Info("已停止")
	return nil
}

// purgeLoop 只清理账本体积，不参与任何安全判定：过期状态一律由时间戳即时判定，
// 因此清理任务停摆也不会让已消费 token 重新可用。
func purgeLoop(ctx context.Context, db *store.DB, log *slog.Logger, done chan<- struct{}) {
	defer close(done)
	t := time.NewTicker(purgeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			r, c, err := db.Purge(ctx, now)
			if err != nil {
				log.Error("清理账本", "err", err)
				continue
			}
			if r > 0 || c > 0 {
				log.Info("清理账本", "responses", r, "challenges", c)
			}
		}
	}
}
