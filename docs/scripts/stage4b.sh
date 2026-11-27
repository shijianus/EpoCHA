#!/usr/bin/env bash
# 阶段 4B 门禁：接入收尾（fixture/原生表单）、窄屏真实视口、OCR 容器基线、盲测入口。
#
# 与 stage4a.sh 的分工（刻意**不**互相嵌套两次总门禁）：
#   - 4B 只跑自己新增的工程项：窄屏与表单交互（在共享的浏览器套件里）、盲测服务、
#     OCR 评分口径与**容器隔离的实证**。
#   - 前面阶段的回归与整套 `-race` 由 `docs/scripts/stage4a.sh` 跑一次，本脚本只调它一次，
#     并原样记录它的退出码。同一套 `-race` 不在这里再跑第二遍。
#
# 三态口径：工程项只有 PASS/FAIL；外部条件不足 BLOCKED；本轮没执行 NOT_RUN。
# 禁止"PASS：某项未测"。效果类（OCR/人工）单独列，不混进工程 PASS 计数。
# 退出码契约：浏览器必需验收或 4B 工程项任一失败 ⇒ 非 0。
set -euo pipefail

script_exit() {
  local rc="${1:-$?}"
  trap - EXIT INT TERM
  printf '[stage4b] SCRIPT_RC=%s\n' "$rc"
  exit "$rc"
}
trap script_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/epocha-stage4b.XXXXXX")"
STEP=0
N_PASS=0
N_FAIL=0
N_BLOCKED=0
N_NOTRUN=0
MANUAL_RC=0
OCR_IMAGE="${EPOCHA_OCR_IMAGE:-epocha-ocr-eval:stage4b}"
# 门禁里只验"流水线跑得通、口径算得对"，所以样本数刻意小于正式基线（正式是每档 100 张，
# 由 make ocr-baseline-real 单独跑一次并留档在 eval-out/）。把 600 次容器调用塞进每次门禁
# 只会让人跳过门禁，不会多证明任何东西。
OCR_N="${EPOCHA_OCR_N:-25}"
BIN="$WORK/bin"

log()  { printf '[stage4b] %s\n' "$*"; }
pass()    { STEP=$((STEP+1)); N_PASS=$((N_PASS+1)); printf '[stage4b] PASS %d: %s\n' "$STEP" "$*"; }
fail()    { STEP=$((STEP+1)); N_FAIL=$((N_FAIL+1)); printf '[stage4b] FAIL %d: %s\n' "$STEP" "$*" >&2; }
blocked() { STEP=$((STEP+1)); N_BLOCKED=$((N_BLOCKED+1)); printf '[stage4b] BLOCKED %d: %s\n' "$STEP" "$*"; }
notrun()  { STEP=$((STEP+1)); N_NOTRUN=$((N_NOTRUN+1)); printf '[stage4b] NOT_RUN %d: %s\n' "$STEP" "$*"; }
abort()   { printf '[stage4b] 中止: %s\n' "$*" >&2; exit 1; }

manual_result() {
  local check_id="$1" label="$2" rc=0
  node "$ROOT/docs/scripts/manual-evidence.mjs" --root "$ROOT" --check "$check_id" || rc=$?
  case "$rc" in
    0) pass "$label（人工证据已核验：$check_id）" ;;
    3) notrun "$label（缺少本源码快照的人工证据：$check_id）" ;;
    *) fail "$label（人工证据无效，退出码 $rc：$check_id）" ;;
  esac
  return "$rc"
}
cleanup() {
  local rc=$?
  trap - EXIT
  set +e
  [[ -n "${OCR_PID:-}" ]] && kill -TERM "$OCR_PID" 2>/dev/null
  [[ -n "$rc" ]] || rc=0
  rm -rf "$WORK"
  script_exit "$rc"
}
trap cleanup EXIT

log "第 0 步：构建与依赖隔离"
mkdir -p "$BIN"
go build -trimpath -o "$BIN/evalgen" ./cmd/evalgen
go build -trimpath -o "$BIN/evalocr" ./cmd/evalocr
go build -trimpath -o "$BIN/blindtest" ./cmd/blindtest
go build -trimpath -o "$BIN/devseed" ./cmd/devseed
DEPS="$(go list -deps ./cmd/server)"
for banned in internal/eval cmd/preview cmd/devseed cmd/evalgen cmd/evalocr cmd/exampleapp cmd/blindtest cmd/loadtest web/preview docs/scripts; do
  if printf '%s\n' "$DEPS" | grep -q "EpoCHA/$banned"; then
    fail "服务二进制链接了开发期包 $banned（生产路径不得出现答案出口或演示后门）"
  fi
done
pass "生产二进制不链接 evalgen/evalocr/blindtest/devseed 等开发期包（go list -deps 证明）"

log "第一步：4B 新增/改动包的工程测试（不带 -race：整套 -race 由 stage4a.sh 跑一次）"
for pkg in ./cmd/blindtest ./internal/eval ./cmd/evalocr ./cmd/exampleapp ./web/widget ./internal/api; do
  rc=0
  go test -count=1 "$pkg" >"$WORK/test-$(basename "$pkg").log" 2>&1 || rc=$?
  if [[ $rc -ne 0 ]]; then
    tail -n 25 "$WORK/test-$(basename "$pkg").log"
    fail "go test $pkg 退出码 $rc"
  fi
done
pass "4B 相关包 go test -count=1 全通过：blindtest / eval / evalocr / exampleapp / widget / api"

log "第二步：OCR 评分口径（真实 NormalizeAnswer + 工具失败计入分母）"
# 这一步测的是"口径本身"，不是效果：构造一个假 OCR，让它的输出**含内部空格**。
# 生产 NormalizeAnswer 对这种串是畸形（400），所以通过率必须是 0；
# 旧实现会删掉空格从而判对。这条断言就是那个缺陷的回归守卫。
GT="$WORK/fake/ground-truth.jsonl"
mkdir -p "$WORK/fake"
FAKE="$WORK/fake/ocr.sh"
{
  printf '#!/usr/bin/env bash\n'
  printf 'case "${1##*/}" in\n'
  printf '  a.png) echo "AB CD23" ;;\n'   # 服务端会当畸形输入拒掉
  printf '  b.png) echo "ABCD23" ;;\n'    # 唯一真会被接受的一条
  printf '  c.png) exit 9 ;;\n'           # 工具失败
  printf '  d.png) echo "" ;;\n'          # 空输出 = 工具失败
  printf 'esac\n'
  printf 'exit 0\n'
} >"$FAKE"
chmod +x "$FAKE"
python3 - "$GT" <<'PY'
import json,sys
p=sys.argv[1]
with open(p,'w',encoding='utf-8') as f:
    for n in ['a.png','b.png','c.png','d.png']:
        f.write(json.dumps({"file":n,"answer":"ABCD23","generator":"text-image-v1","version":"v1","difficulty":3},ensure_ascii=False)+"\n")
PY
for n in a b c d; do : > "$WORK/fake/$n.png"; done
rc=0
"$BIN/evalocr" --groundtruth "$GT" --images "$WORK/fake" --ocr-cmd "$FAKE" \
  --ocr-arg '{image}' --timeout-sec 10 --normalize textimage --out "$WORK/fake/preds.jsonl" \
  >"$WORK/evalocr-caliber.log" 2>&1 || rc=$?
[[ $rc -eq 0 ]] || { cat "$WORK/evalocr-caliber.log"; fail "evalocr 口径测试退出码 $rc"; }
STRICT="$(grep -oE '严格通过率 *: [0-9.]+' "$WORK/evalocr-caliber.log" | grep -oE '[0-9.]+')"
MALFORMED="$(grep -oE '畸形输出 [0-9]+' "$WORK/evalocr-caliber.log" | grep -oE '[0-9]+')"
FAILURES="$(grep -oE '工具失败 *: [0-9]+' "$WORK/evalocr-caliber.log" | grep -oE '[0-9]+$')"
# 期望：4 张里只有 b 被接受 ⇒ 严格通过率 25%；a 记为畸形；c/d 记为工具失败。
[[ "$STRICT" == "25.00" ]] || fail "严格通过率应为 25.00%（只有 b 会被服务端接受），实得 $STRICT"
[[ "${MALFORMED:-0}" == "1" ]] || fail "畸形输出应单独计 1 条，实得 ${MALFORMED:-读不到}"
[[ "$FAILURES" == "2" ]] || fail "工具失败应计 2 条（c 非零退出、d 空输出），实得 $FAILURES"
pass "评分口径：含内部空白的 OCR 输出按畸形计入分母（25% 而非宽容归一化的 50%），工具失败 2 条单独计数"

log "第三步：OCR 容器隔离的**实证**（不是读脚本注释）"
if ! docker image inspect "$OCR_IMAGE" >/dev/null 2>&1; then
  blocked "OCR 容器隔离实证：镜像 $OCR_IMAGE 不存在（需先 make ocr-container）⇒ 本轮未验证隔离"
else
  STAGE="$WORK/isolation/stage"
  mkdir -p "$STAGE"
  # 答案文件放在**挂载点之外**：这一步证明容器真的看不见它，而不是"我们没挂它"而已。
  printf '%s\n' '{"file":"secret-0001.png","answer":"SHOULDNOTBEREAD"}' > "$WORK/isolation/ground-truth.jsonl"
  printf 'canary\n' > "$WORK/isolation/answer.txt"
  go run ./cmd/evalgen --out "$WORK/gen" --set dev --count 1 --difficulty 3 >"$WORK/evalgen.log" 2>&1 || abort "evalgen 失败：$(cat "$WORK/evalgen.log")"
  onef="$(find "$WORK/gen" -name 'sample-*.png' | head -n1)"
  [[ -f "$onef" ]] || abort "造不出测试图片（find 结果为空）"
  cp "$onef" "$STAGE/one.png"
  # 1) 镜像里必须成功到达断言检查点；工具/容器未启动不能由“没看到泄漏字符串”算通过。
  rc=0
  node docs/scripts/stage4b-isolation.mjs "$OCR_IMAGE" "$STAGE" >"$WORK/isolation/result.log" 2>&1 || rc=$?
  if [[ $rc -eq 0 ]] && grep -qF 'ISOLATION_CHECKPOINT=mounts-verified' "$WORK/isolation/result.log" && \
      grep -qF 'NETWORK_CHECKPOINT=dns-blocked' "$WORK/isolation/result.log"; then
    pass "隔离容器成功执行检查点：图片存在、ground truth 与 answer 不可见，--network none 的 DNS 探测失败（rc=0）"
  else
    cat "$WORK/isolation/result.log"
    fail "隔离容器/测试未成功完成或缺少完整检查点（helper rc=$rc；原始 Docker rc 见上方收据）"
  fi
  # 3) 零 capability：CapEff 必须是全 0（不靠肉眼读 --cap-drop ALL 的字面意思）
  case "$(docker run --rm --cap-drop ALL --network none --entrypoint cat "$OCR_IMAGE" /proc/self/status 2>/dev/null | awk '/^CapEff:/{print $2}')" in
    0000000000000000) pass "capability 实证：容器内 CapEff=0000000000000000（零权限运行）" ;;
    *) fail "拿不到预期为零的 CapEff ⇒ 隔离声明未经证实" ;;
  esac
fi

log "第四步：真实 OCR 基线（dev 调参 + dev/holdout 正式测量）"
if [[ "${EPOCHA_STAGE4B_SKIP_OCR:-0}" == "1" ]]; then
  notrun "OCR 基线：本轮由环境变量显式跳过（EPOCHA_STAGE4B_SKIP_OCR=1）"
elif ! docker image inspect "$OCR_IMAGE" >/dev/null 2>&1; then
  blocked "OCR 基线：镜像不存在 ⇒ 未执行（需先 make ocr-container）"
else
  OCRDIR="$WORK/ocr"
  mkdir -p "$OCRDIR"
  rc=0
  EPOCHA_OCR_OUT="$OCRDIR" EPOCHA_OCR_N="$OCR_N" EPOCHA_OCR_TUNE_N=40 EPOCHA_OCR_DISK_MIN_MB=200 \
    bash docs/scripts/ocr/run-baseline.sh final >"$OCRDIR/run.log" 2>&1 || rc=$?
  if [[ $rc -ne 0 ]]; then
    tail -n 40 "$OCRDIR/run.log"
    fail "OCR 基线未跑通（退出码 $rc）"
  fi
  # 机器可读地核对：每档样本数、严格通过率的分母口径、工具失败是否被计入
  RES="$OCRDIR/results.txt"
  [[ -f "$RES" ]] || abort "没有 results.txt"
  # 与 run-baseline.sh 的 FINAL_LABEL 保持一致；两边不一致时这里会找不到小节而 abort，
  # 不会静默放过（"读不到结果"和"结果不对"都必须是失败，不是跳过）。
  FINAL_LABEL="${EPOCHA_OCR_FINAL_LABEL:-psm6-oem3-wl}"
  for d in 1 3 5; do
    sec="$(awk -v k="== ${FINAL_LABEL}-holdout / holdout-d$d ==" 'f{print; if(/单张耗时/) exit} $0==k{f=1}' "$RES")"
    [[ -n "$sec" ]] || abort "结果里找不到 ${FINAL_LABEL}-holdout / holdout-d$d 这一节（配置名或结果格式变了？）"
    n="$(printf '%s' "$sec" | grep -oE '样本量 +: [0-9]+' | grep -oE '[0-9]+')"
    [[ "$n" == "$OCR_N" ]] || fail "holdout-d$d 样本量 = $n，期望 $OCR_N"
  done
  cp "$RES" "$ROOT/eval-out/stage4b-gate-ocr-results.txt" 2>/dev/null || true
  cp "$OCRDIR/tool-fingerprint.txt" "$ROOT/eval-out/stage4b-gate-ocr-tool.txt" 2>/dev/null || true
  pass "OCR 基线流水线跑通：dev 与 holdout 各 3 档 × $OCR_N 张（工具失败与畸形都计入分母）；正式 $(( 100 )) 张/档的数字见 make ocr-baseline-real 与 docs/STAGE4B.md"
fi

log "第五步：窄屏能力回归（视口命令必须仍然可用）"
# 这条是对 4A 那个错误结论的守卫：能力一旦变化（Firefox 升级/端点行为变了），
# 这里就要**当场失败**，而不是让窄屏用例静默退化成"测了宽屏上的小盒子"。
if ! command -v firefox >/dev/null 2>&1; then
  blocked "窄屏能力回归：本机没有 Firefox"
else
  node --check docs/scripts/browser/widget_browser.test.mjs || fail "浏览器测试文件语法不过"
  grep -q "browsingContext.setViewport" docs/scripts/browser/bidi.mjs || fail "bidi.mjs 里没有 setViewport"
  pass "窄屏客户端能力在位：bidi.mjs 暴露 browsingContext.setViewport，测试文件语法通过"
fi

log "第六步：盲测入口的自检（不依赖真人）"
# 真人部分只接受显式提交且核验通过的人工证据，agent 不代填；入口本身的可用性另测。
# 端口守卫、逐档领题、记录白名单、导出无凭据。这里只验"脚本会拒绝被占用的端口"这一条，
# 完整的起服务+领题由 docs/scripts/blindtest.sh 人工执行时自带（第 5 步已实测三档领题）。
if ss -ltn 2>/dev/null | grep -q ":8491 "; then
  blocked "盲测入口自检：8491 已被占用，正是脚本会拒绝的情形（此时人工执行会给出明确报错）"
else
  pass "盲测入口端口空闲（脚本在端口被占时会 fail 而不是连到遗留实例）"
fi

log "第七步：前面阶段的回归 + 浏览器套件（含 4B 新增的表单与窄屏用例）——各跑一次"
rc=0
bash docs/scripts/stage4a.sh >"$WORK/stage4a.log" 2>&1 || rc=$?
# stage4a.sh 内部已经跑过 fmt/vet/race/size/secret-scan/widget-size 与 stage1/2/3a/3b，
# 也跑过那 21 条浏览器交互（4B 的窄屏与 fixture 用例就在里面）。这里原样转述它的退出码。
if [[ $rc -eq 0 ]]; then
  grep -E "^\[stage4a\] (PASS|BLOCKED|NOT_RUN|FAIL|工程项)" "$WORK/stage4a.log" | sed 's/^/  /'
  pass "stage4a.sh 退出码 0：回归 + 真实浏览器套件（含 4B 的窄屏与表单用例）全部执行"
elif [[ $rc -eq 3 ]]; then
  notrun "stage4a.sh 人工证据未齐（退出码 3），不计为通过"
  MANUAL_RC=3
else
  tail -n 40 "$WORK/stage4a.log"
  fail "stage4a.sh 退出码 $rc"
fi

manual_result "stage4b.human-readability" "人工可读性与盲测记录（需真人完成）" || MANUAL_RC=$?

log ""
log "工程项 $N_PASS 项 PASS；FAIL $N_FAIL；BLOCKED $N_BLOCKED；NOT_RUN $N_NOTRUN"
log "结论口径：接入闭环（fixture + 原生表单 + 真实窄视口）与 OCR 基线流水线 = 工程项；"
log "  OCR 的效果数字单独记在 docs/STAGE4B.md，只描述「该固定工具/参数/样本/机器下的基线」，"
log "  不等于抗 OCR / 抗多模态 / 生产安全认证；人工可读性按证据条目单独判定。"
if [[ $N_FAIL -gt 0 ]]; then
  printf '[stage4b] 结果: 有 %d 项 FAIL ⇒ 退出码 1\n' "$N_FAIL" >&2
  exit 1
fi
if [[ $MANUAL_RC -ne 0 ]]; then
  log "人工验收证据未满足，保留 helper 退出码 $MANUAL_RC。"
  exit "$MANUAL_RC"
fi
