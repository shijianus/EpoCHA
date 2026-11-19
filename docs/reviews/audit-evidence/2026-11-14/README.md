# 2026-11-14 专案审计证据包

主报告：`../../PROJECT-STATUS-AUDIT-2026-11-14.md`。

## 先读

1. `RESULTS-final.json`：最终65份命令收据汇总，早期`RESULTS.json`保留但非最终版本。
2. `isolated-release-gate/summary.json`：正式full入口本次的正确聚合结果为 **INCOMPLETE / 3 / release_ready:false**。
3. `m4-replication/m4-summary.json`及`m4-results.csv`：新的7000题，6824题接受，7型效果FAIL；工具rc0仅指测量完成。
4. `risk-cooldown-reproduction.stdout.log`：隔离副本补充回归FAIL，15分钟冷却跨10分钟bucket后只过2秒就解除。
5. `source-initial.json`与`source-final.json`：284个被审计文件hash完全一致，生产源码与用户原有未提交M4开发未被本次修改。

## 离线核验

在仓库根使用已有Python执行（不联网、不安装、不修改文件）：

```text
python -I docs/reviews/audit-evidence/2026-11-14/verify_evidence.py
```

核验器读取`SHA256SUMS.json`，验证交付文件、收据输出、源码起止一致、M4逐题索引／分母／成功率／Wilson区间。`verification:PASS`只表示这份证据包内部完整性成立，**不表示项目测试／发布／CAPTCHA效果通过**。Hash没有签名，不提供第三方来源认证。

## 阅读失败日志时的分类

- `go-test-full`／`go-vet-full`：原工作区Windows flock阻塞＋ignored Go工装污染，真实非零，不是全量通过。
- `release-full`：原树源码清单正确拒绝；其source-stability另受本轮并行写证据影响，不应算产品缺陷。
- `build-receipt-integration`：原树因审计文件同时变化触发source changed；同源隔离复跑`isolated-build-receipt-integration` rc0。
- `go-test-compile-linux-project`：一次多包输出名冲突；后`linux-test-compile-*`逐包唯一输出25包全部编译成功，未执行。
- `stage5b-classifier`：第一次解析到WSL的bash shim，无发行版，失败；`stage5b-classifier-gitbash`明确调用Git Bash后9条分类自测rc0，后者不是实际部署验收。
- `go-race-full`：缺cgo；`go-race-with-cgo`显式开cgo后确认缺gcc；race未完成。
- `make-capability`：FileNotFoundError，raw_exit_code为null，不是伪造进程退出码。
- WSL输出为Windows UTF-16LE，原始字节保留，不强行转换后覆盖。
- `risk-cooldown-reproduction`：故意添加“冷却必须保留”的新回归，FAIL是实际缺陷证据，不是变异生产源码。

## 产物位置与限制

`*.receipt.json`记录cwd、argv、原始码、时间与输出hash。`audit_runner.py`自身在成功记录一个非零子命令后可以返回0；必须读receipt的`raw_exit_code`。

大体积Linux二进制和测试可执行文件位于根目录ignored的`release-artifacts/audit-2026-11-14-cross/`，不复制进小型证据包。其hash见`cross-build-manifest.json`。M4实际测量exe的hash见`m4-measurement-binary.json`。

本地临时隔离clone的路径与构造命令见`isolated-source-manifest.json`。full／build收据验证是在添加冷却补充测试之前执行；添加后clone不再是没有附加测试的原始副本。原工作区产品源码未改。

新证据目录只含审计脚本、报告、日志、收据与公开测量统计，不含真实secret、token、verifier、私有seed、DB或答案导出。日志中Go测试可能含有限的测试夹具值，不能把它当真实部署凭据。

核验脚本带机器特定工具链路径的是采集脚本；独立核验`verify_evidence.py`不需要该Go路径。不要盲目重跑采集脚本覆写既有结果；多数输出使用独占新建并会拒绝重用。
