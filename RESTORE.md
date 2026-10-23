# EpoCHA (epocha) — 加密数据包

分支：`backup/vps-replica-20261023` · 打包 2026-10-23
代码与历史**不需要**这个包：`origin/main` 的父提交 + `origin/dev-emergency-backup` = 本机 HEAD `f1cf3fb`
（含 5C-M1 全部改动与 20MB `epocha-offline-assets.tar.gz`），阶段标签 `stage-0 … stage-5b` 均已在远端。
本包只装**被 .gitignore 排除、且新机上无法凭仓库内容重新得到**的东西。

## 包内容

| 路径 | 为什么不可再生 |
|---|---|
| `epocha/eval-out/` | stage 3B/4B 的离线样本集与 OCR 评分原始产出（含 ground-truth 答案表）。AGENTS.md 红线禁止明文入库 ⇒ 这里只有密文。重跑需要固定配置的 tesseract 容器 + 时间，且 `--network none` 的评测_once_记录的数字（低 96% / 中 31% / 高 3%）就是靠这些素材支撑的 |
| `epocha/bin/` | `epocha` / `epocha-ops` / `devclient` / `evalocr` 的 linux-arm64 静态二进制（构建产物，可重建但对应具体提交） |
| `epocha/devclient` | 同上，仓库根的独立二进制 |
| `epocha/deploy/docker/.prebuilt/` | candidate-manifest.txt 与 candidate-manifest-993a792.txt（构建收据）+ `out/` 里的候选二进制 |
| `extra/git-metadata/epocha/` | local config、refs、reflogs、status、hooks、ls-remote 快照 |
| `extra/recovered-git-objects/epocha/` | 一个不可达的 tag 对象（早期 `stage-0-done`，指向 `a5cdf2e`），只在本机对象库里 |

## 还原

```bash
git clone -b dev-emergency-backup git@github.com:shijianus/EpoCHA.git   # 这是最全的那条线
cd EpoCHA
git fetch origin backup/vps-replica-20261023
git checkout backup/vps-replica-20261023 -- volumes MANIFEST
cat volumes/*.bin | gpg --decrypt 2>/dev/null | tar -xzp -C ../    # 解出 epocha/eval-out 等，路径自带 epocha/ 前缀
```

新机跑测试：`GOTOOLCHAIN=local`（`make` 已写入）、`CGO_ENABLED=0`、go1.24.5 / linux-arm64。
**密钥一个都不在本机**：`deploy/docker/{keys,data,backups}` 与 `.env` 在这台机器上不存在，
所以没有"丢失的密钥材料"需要搬。新机按 `docs/DEPLOYMENT.md` §11 的启动配方生成**新的**
master / pepper / admin key 即可；按 ADR 的口径，旧快照本来也不得保持消费/撤销状态。
