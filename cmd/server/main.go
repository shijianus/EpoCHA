// Command server 是 epocha 人机验证服务的单二进制入口。
//
// 阶段 0 只提供可构建的骨架与配置解析入口：加载配置 → 打开 SQLite → 启动 HTTP
// 的实现在阶段 1 落地（见 docs/DESIGN.md §17 阶段 1）。
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"epocha/internal/version"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "epocha:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(out)
	showVersion := fs.Bool("version", false, "打印版本信息后退出")
	dbPath := fs.String("db", "", "SQLite 数据库文件路径（必需）")
	listen := fs.String("listen", ":8080", "HTTP 监听地址")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Fprintf(out, "epocha %s (commit %s, built %s)\n", version.Version, version.Commit, version.BuildDate)
		return nil
	}
	if *dbPath == "" {
		return errors.New("缺少 --db")
	}
	_ = *listen
	return errors.New("服务尚未实现：请按 docs/DESIGN.md §17 阶段 1 完成 PoW + token + /siteverify 闭环")
}
