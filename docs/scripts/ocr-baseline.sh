#!/usr/bin/env bash
# ocr-baseline.sh — 跑离线 OCR 基线，并把**退出码分层**如实打印出来。
#
# 为什么单独成脚本而不是写在 Makefile 里：make 的配方里嵌套引号与 `$$` 转义极易出错，
# 而这条路径上的退出码正是本阶段的验收对象之一。
#
# 本机实测（go1.24.5 / linux-arm64，2026-10-18）：
#   evalocr 二进制     → 3（"缺工具 = 未测"的机器可读信号）
#   go run ./cmd/evalocr → 1（go run 把任何非 0 **折叠成 1**，真实码只留在 stderr 的 "exit status 3"）
#   make ocr-baseline    → 2（make 把任何配方失败折叠成 2）
# ⇒ 包装层不会原样透传 3。要区分"未测 / 失败 / 通过"，只能读二进制的码。
#
# 退出码约定：0 = 已实测并给出通过率；3 = BLOCKED（本机无离线 OCR）；1 = 真的失败。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
export GOTOOLCHAIN=local

shopt -s nullglob
GT_CANDIDATES=("$ROOT"/eval-out/holdout-*/ground-truth.jsonl)
if [[ ${#GT_CANDIDATES[@]} -eq 0 ]]; then
  echo "找不到 holdout 集的 ground-truth.jsonl：先跑 make eval-samples" >&2
  exit 1
fi
GT="${GT_CANDIDATES[0]}"

BIN="$ROOT/bin/evalocr"
mkdir -p "$ROOT/bin"
if ! go build -o "$BIN" ./cmd/evalocr; then
  echo "构建 evalocr 失败" >&2
  exit 1
fi

echo "ground truth：$GT"
"$BIN" --groundtruth "$GT"
RC_BIN=$?

# 只有二进制这条路径给真实码；下面两行是把"折叠"这件事变成可见证据，而不是让人误以为
# `go run`/`make` 也能报 3。
GO_OUT="$(go run ./cmd/evalocr --groundtruth "$GT" 2>&1 >/dev/null)"
RC_GORUN=$?
echo "退出码分层：evalocr 二进制=$RC_BIN；go run=$RC_GORUN（stderr: $(printf '%s' "$GO_OUT" | tail -n1)）；make 自身只会给 0 或 2"

case $RC_BIN in
  0) echo "OCR 实测：PASS（通过率与工具版本见上）" ;;
  3) echo "OCR 实测：BLOCKED —— 本机没有离线 OCR 工具，未获授权安装或下载模型。这不是通过。" ;;
  *) echo "OCR 实测：FAIL —— 工具存在但执行失败（见上面的评分器输出）" ;;
esac
exit $RC_BIN
