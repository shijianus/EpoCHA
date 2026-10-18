#!/usr/bin/env bash
# 离线 OCR 基线（阶段 4B）：在**隔离容器**里用 Tesseract 测 text-image-v1 的机器通过率。
#
# 用法：
#   docs/scripts/ocr/run-baseline.sh tune     # 只在 dev 集上比候选参数
#   docs/scripts/ocr/run-baseline.sh final    # 用固定参数跑 dev + holdout（holdout 只看一次）
#   docs/scripts/ocr/run-baseline.sh all      # tune 然后 final
#
# 四条口径纪律（违反任何一条，测出来的数字就没有意思）：
#  1. **调参只看 dev**。holdout 的答案表在 final 之前连看都不看，固定参数写死在下面的常量里。
#  2. OCR 进程**只读图片**：ground-truth.jsonl 从不挂进容器，也不出现在 ocr1.sh 的 argv 里。
#     暂存目录里只有 PNG，且脚本会拒绝挂载点之外的路径。
#  3. 分母是**全部样本**：工具失败、缺测、畸形输出都算不通过。
#     "有效样本通过率"只是附带信息，不能拿它当基线结论。
#  4. 结论只允许写成"在该固定工具/参数/样本集/机器下的基线表现"，
#     **不得**泛化成"抗 OCR / 抗多模态 / 生产可用"（ADR-002、红线 14）。M4（每次尝试成本）
#     仍然只是这台开发机上的单张耗时，不是目标机上的攻击成本。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
OUT="${EPOCHA_OCR_OUT:-$ROOT/eval-out/ocr-$(date -u +%Y%m%dT%H%M%SZ)}"
MODE="${1:-all}"
IMAGE="epocha-ocr-eval:stage4b"
CHARSET="ABCDEFGHJKLMNPQRSTUVWXYZ23456789"   # 与 generator.TextImageCharset 同一套字符
PER_SET="${EPOCHA_OCR_N:-100}"               # 正式基线每档样本数（要求 ≥100）
PER_TUNE="${EPOCHA_OCR_TUNE_N:-40}"          # 调参每档样本数：候选参数要跑 4 组，不必都吃满 100
DIFFS=(1 3 5)                                 # 低 / 中 / 高三个渲染档位
BIN="$OUT/bin"
DISK_MIN_MB="${EPOCHA_OCR_DISK_MIN_MB:-700}"  # 磁盘预算：低于此值先停下来问，不要写满 /

mkdir -p "$OUT" "$BIN"
log() { printf '[ocr-baseline] %s\n' "$*"; }
fail() { printf '[ocr-baseline] FAIL: %s\n' "$*" >&2; exit 1; }

# ---- 前置条件：容器与磁盘 ----
disk_free_mb() { df -Pm / | awk 'NR==2{print $4}'; }
FREE_MB="$(disk_free_mb)"
[[ "$FREE_MB" =~ ^[0-9]+$ ]] || fail "读不到 / 的剩余空间（df -Pm 输出变了？）"
(( FREE_MB >= DISK_MIN_MB )) || fail "磁盘只剩 ${FREE_MB}MB（阈值 ${DISK_MIN_MB}MB）：先清理或换目录，不要在这里硬写"
log "  / 可用 ${FREE_MB}MB（阈值 ${DISK_MIN_MB}MB）"
docker image inspect "$IMAGE" >/dev/null 2>&1 || fail "找不到镜像 $IMAGE：先构建 docs/scripts/ocr/Dockerfile"

# ---- 工具指纹：谁的版本、哪个模型、什么参数，全部落盘 ----
docker run --rm --entrypoint cat --network none "$IMAGE" /TOOL_FINGERPRINT.txt > "$OUT/tool-fingerprint.txt"
docker image inspect --format '{{index .RepoDigests 0}} | {{.Id}}' "$IMAGE" >> "$OUT/tool-fingerprint.txt" 2>/dev/null || true
log "  工具指纹：$(head -n 4 "$OUT/tool-fingerprint.txt" | tr '\n' ' ' | cut -c1-120)…"

# ---- 构建评测二进制（先 build 再跑：包装层会折叠退出码，见 docs/scripts/ocr-baseline.sh）----
(cd "$ROOT" && go build -trimpath -o "$BIN/evalgen" ./cmd/evalgen && go build -trimpath -o "$BIN/evalocr" ./cmd/evalocr)
log "  已构建 evalgen / evalocr"

# gen <set> <difficulty> <n> → 把图片复制进只含图片的暂存目录，返回两行（GT 路径 / 暂存目录）
declare -A GT STAGE
gen_set() {
  local set="$1" diff="$2" n="$3" dir
  "$BIN/evalgen" --out "$OUT/sets" --set "$set" --difficulty "$diff" --count "$n" >"$OUT/evalgen-$set-d$diff.log" 2>&1 \
    || fail "evalgen($set d$diff) 失败：$(cat "$OUT/evalgen-$set-d$diff.log")"
  # evalgen 的目录名带时间戳，按 set+difficulty+count 精确找回唯一目录
  local -a cands
  mapfile -t cands < <(find "$OUT/sets" -maxdepth 1 -type d -name "$set-d$diff-n$n-*")
  [[ ${#cands[@]} -eq 1 ]] || fail "定位样本目录不唯一（找到 ${#cands[@]} 个）：$set d$diff n$n"
  dir="${cands[0]}"
  local stage="$OUT/stage/$set-d$diff"
  mkdir -p "$stage"
  # 复制而不是硬链接：容器以 root 运行但 --cap-drop ALL 拿掉了 CAP_DAC_OVERRIDE，
  # 读不了 0600 的文件；把评测图片放成 0644 副本，既解决问题又不动源目录权限。
  # 只复制 PNG —— ground-truth.jsonl / manifest.json **从不**进入挂载点。
  local f
  for f in "$dir"/sample-*.png; do
    [[ -f "$f" ]] || fail "样本目录里没有 PNG：$dir"
    install -m 0644 "$f" "$stage/"
  done
  GT["$set-d$diff"]="$dir/ground-truth.jsonl"
  STAGE["$set-d$diff"]="$stage"
}

# run_eval <label> <psm> <oem> <whitelist> <set> <difficulty>；结果追加到 $OUT/results.txt
run_eval() {
  local label="$1" psm="$2" oem="$3" wl="$4" set="$5" diff="$6" want="${7:-$PER_SET}"
  local key="$set-d$diff"
  [[ -n "${GT[$key]:-}" && -n "${STAGE[$key]:-}" ]] || fail "样本集 $key 还没生成"
  local nf
  nf=$(find "${STAGE[$key]}" -name 'sample-*.png' | wc -l)
  (( nf == want )) || fail "暂存目录 $key 里有 $nf 张，期望 $want 张"
  log "  ▶ $label / $key（$nf 张，psm=$psm oem=$oem whitelist=${#wl} 字符）"
  local rc=0
  EPOCHA_OCR_STAGE="${STAGE[$key]}" EPOCHA_OCR_PSM="$psm" EPOCHA_OCR_OEM="$oem" EPOCHA_OCR_WHITELIST="$wl" \
    "$BIN/evalocr" \
      --groundtruth "${GT[$key]}" \
      --images "${STAGE[$key]}" \
      --ocr-cmd "$ROOT/docs/scripts/ocr/ocr1.sh" \
      --ocr-arg '{image}' \
      --timeout-sec 30 \
      --normalize textimage \
      --out "$OUT/preds-$label-$key.jsonl" \
      >"$OUT/report-$label-$key.txt" 2>&1 || rc=$?
  # 0=跑完，1=出错，3=未执行（工具缺失）。这里 3 不可能出现（挂载已验证），其它非 0 都要炸。
  (( rc == 0 )) || { cat "$OUT/report-$label-$key.txt"; fail "evalocr($label/$key) 退出码 $rc"; }
  {
    echo "== $label / $key =="
    grep -E "样本量|工具失败|整串正确|严格通过率|有效样本通过率|字符级正确率|单张耗时" "$OUT/report-$label-$key.txt"
  } >>"$OUT/results.txt"
}

# ---- 候选参数：只在 dev 上比较 ----
# psm=7（单行文本）与 psm=6（自动版面块）是这一类"一整行 6 字符"图形的两种主流切法；
# psm=8 是单字符，需要逐字切分——我们没有做逐字裁剪，所以列出来只做对照，不当候选。
tune() {
  : >"$OUT/results.txt"
  for d in "${DIFFS[@]}"; do gen_set dev "$d" "$PER_TUNE"; done
  log "第一步：在 dev 集上比候选参数（holdout 此阶段完全不参与）"
  for cfg in "psm7-oem3-wl:7:3:$CHARSET" "psm6-oem3-wl:6:3:$CHARSET" "psm7-oem1-wl:7:1:$CHARSET" "psm7-oem1-nowl:7:1:"; do
    IFS=: read -r name psm oem wl <<<"$cfg"
    for d in "${DIFFS[@]}"; do run_eval "$name" "$psm" "$oem" "$wl" dev "$d" "$PER_TUNE"; done
  done
  log "候选参数比较结果（完整见 $OUT/results.txt）："
  cat "$OUT/results.txt"
}

# ---- 固定参数跑正式基线 ----
# FINAL_* 是**只看 dev**（每档 40 张，见 tune 阶段）比较后定下来的，比较表：
#   配置              d1      d3      d5
#   psm7-oem3-wl      97.50%  27.50%   0.00%
#   psm6-oem3-wl      97.50%  37.50%   0.00%   ← 选它（dev 上最强）
#   psm7-oem1-wl      97.50%  27.50%   0.00%
#   psm7-oem1-nowl    95.00%   7.50%   0.00%
# 为什么必须选**最强**的一组而不是随便一组：基线要回答的是"一台 commodity OCR 最多能读成什么样"。
# 故意留一个弱配置会让数字好看，那正是 §五 禁止的方向。
# 注意白名单是**帮忙**的（把输出空间限制到题目字符集），所以带白名单反而更强；
# 不用白名单那一档 d3 只有 7.5%，不能拿它当"OCR 读不出来"的证据。
# 改这三个常量必须同时改 docs/STAGE4B.md 里的那句"选定于 dev"，否则"holdout 只看一次"就是空话。
FINAL_LABEL="${EPOCHA_OCR_FINAL_LABEL:-psm6-oem3-wl}"
FINAL_PSM="${EPOCHA_OCR_FINAL_PSM:-6}"
FINAL_OEM="${EPOCHA_OCR_FINAL_OEM:-3}"
FINAL_WL="${EPOCHA_OCR_FINAL_WL:-$CHARSET}"

final() {
  [[ -s "$OUT/results.txt" ]] || : >"$OUT/results.txt"
  log "第二步：固定参数 dev + holdout（label=$FINAL_LABEL psm=$FINAL_PSM oem=$FINAL_OEM）"
  for d in "${DIFFS[@]}"; do gen_set dev "$d" "$PER_SET"; done
  for d in "${DIFFS[@]}"; do gen_set holdout "$d" "$PER_SET"; done
  for d in "${DIFFS[@]}"; do run_eval "$FINAL_LABEL-devfixed" "$FINAL_PSM" "$FINAL_OEM" "$FINAL_WL" dev "$d" "$PER_SET"; done
  for d in "${DIFFS[@]}"; do run_eval "$FINAL_LABEL-holdout" "$FINAL_PSM" "$FINAL_OEM" "$FINAL_WL" holdout "$d" "$PER_SET"; done
  log "正式基线结果："
  grep -A 7 -E "^== $FINAL_LABEL-(fixed|final)" "$OUT/results.txt" || cat "$OUT/results.txt"
}

case "$MODE" in
  tune) tune ;;
  final) final ;;
  all) tune; final ;;
  *) fail "用法: $0 tune|final|all（实得 $MODE）" ;;
esac
log "完成。产物目录：$OUT（不入库；ground-truth 与预测都在里面）"
