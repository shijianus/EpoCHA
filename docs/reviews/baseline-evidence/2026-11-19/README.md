# 基线恢复原始证据（2026-11-19）

接受检查的干净源码 OID 为 `3ed3a156f48f02dafa51a7f7b392e6d3b4d8753c`；每个历史红绿对照另有独立 OID，不把最终 OID 倒填成旧运行身份。环境是 Linux amd64 / Go 1.27.3 / Node 24.22.3 / UID:GID 1000:1000；它不替代正式 Linux arm64 验收。

`report.json` 汇总测试、命令、环境、CPU 架构、原始退出码、PASS/FAIL/BLOCKED/NOT_RUN、日志路径与 SHA-256、缺陷与修复关联。最终 full 为 INCOMPLETE / 3、release_ready:false；没有发布批准。

原始日志由 Linux 经 SCP 回收后在 Windows 打包。日志扩展名改为 `.txt`；内容不改。两次 full 格式失败、初始 Go/Wave/图片/浏览器失败、后续真实成功与资源阻断均保留。`*.receipt.txt` 是历史原始 shell 收据；`*.receipt.json` 由 `baseline-run.mjs` 运行时自动生成。full 各子命令另在 `full-publish-final-summary.json` 中有 command、raw_exit_code 和日志哈希，静默成功的空日志不独立充作非空证据。

已发现宿主根文件系统满载，故后续 SQLite 测试使用本轮独占的 SQLITE_TMPDIR；未清理任何未知资源。Go race 的两个未来阶段占位 skip 单列，不计入通过测试。

`SHA256SUMS.json` 覆盖本目录除它自身外的所有文件；SHA-256 是字节一致性，不是数字签名或来源认证。报告全文见上一层 `BASELINE-RECOVERY-2026-11-19.md`。
