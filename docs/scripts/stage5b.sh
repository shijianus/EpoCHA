#!/usr/bin/env bash
# 阶段 5B 门禁：运行互斥、恢复失败边界、部署候选清单、有限持续演练。
#
# 与 5A 的分工（刻意不重跑整套历史门禁）：
#   - 5A 已经把"镜像形状 / 密钥 fail closed / 备份恢复语义 / 反代路由"钉住了，本脚本不重做。
#   - 5B 只测这轮新增的东西：谁能在什么时候动这个数据目录（真实进程与真实容器），
#     恢复失败路径有没有明确出口，以及一份可复核的候选构建清单 + 约 5 分钟的节流持续演练。
#   - 回归跑一次 `make stage5a-check`？不。跑一次 `-race ./...` + 定向阶段脚本即可：
#     同一套 -race 跑两遍不多证明任何东西，只烧墙钟（AGENTS 的既定口径）。
#
# 三态口径与**三类归属**（两回事，都必须写清楚）：
#   状态：PASS / FAIL / BLOCKED（外部条件不足）/ NOT_RUN（本轮没执行）。禁止"PASS：某项未测"。
#   归属：
#     R 必需项——受支持的 B 路径（宿主静态编译 + named build context 装配）的部署工程验收。
#     O 可选项——未纳入第一版正式支持的构建路径与平台（A 路径容器内编译、amd64、
#               root 场景下的 UID 65534、公网 TLS/DNS）。可选项**只如实输出**。
#     S 超范围项——不属部署工程验收的产品效果项（人工可读性、抗 OCR/抗 AI、题型安全性）。
#                   S 类永不写成 PASS，也永不计入工程结论：容器化与恢复演练通过不等于题型抗 AI 通过。
#   退出码只由 R 类决定：R 有 FAIL ⇒ 1；R 有 BLOCKED/NOT_RUN ⇒ 3；否则 0。
#   O 类 NOT_RUN 不阻塞已实测通过的 B 路径交付（这是"缩小正式支持范围"，不是"把 A 改成 PASS"）。
#   可选项永远不会被计入 R_PASS；把未执行的可选项写成 PASS 的路径在结构上不存在（见 --selftest-classify）。
#
# 退出码的**包装层行为**（实测，不是推测）：直接跑本脚本拿到的是真码 0/1/3；
# `make stage5b-check` 把任何非 0 一律折叠成 **2**（make 的固有行为，配方里已把真实码打印出来）。
# 所以机器可读的结论看最后那行 RESULT 与 SCRIPT_RC，不要看 make 的码。
set -euo pipefail

# 记录器、verdict 与分类自测都在下面先定义；分派发生在任何副作用（mktemp、trap、docker）之前。
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

STEP=0
# 三类归属各自的计数。**只有 R_* 进 verdict**——可选项与超范围项再多也不改变必需项的结论，
# 反过来必需项少一条也不会被可选项的通过"补"回来。
R_PASS=0; R_FAIL=0; R_BLOCKED=0; R_NOTRUN=0
O_PASS=0; O_FAIL=0; O_BLOCKED=0; O_NOTRUN=0
S_NOTE=0
DOCKER_OK=0

log()  { printf '[stage5b] %s\n' "$*"; }
abort()  { printf '[stage5b] 中止: %s\n' "$*" >&2; exit 1; }

# --- 必需项（R）：受支持的 B 路径部署工程验收 ---
pass() { STEP=$((STEP+1)); R_PASS=$((R_PASS+1)); printf '[stage5b] PASS %d: %s\n' "$STEP" "$*"; }
fail() { STEP=$((STEP+1)); R_FAIL=$((R_FAIL+1)); printf '[stage5b] FAIL %d: %s\n' "$STEP" "$*" >&2; }
blocked() {
  STEP=$((STEP+1)); R_BLOCKED=$((R_BLOCKED+1))
  printf '[stage5b] BLOCKED %d: %s\n' "$STEP" "${1:-}"
  printf '[stage5b]   恢复执行: %s\n' "${2:-见 docs/STAGE5B.md}"
}
req_notrun() {
  STEP=$((STEP+1)); R_NOTRUN=$((R_NOTRUN+1))
  printf '[stage5b] NOT_RUN %d: %s（原因：%s）\n' "$STEP" "${1:-}" "${2:-本轮未执行}"
}

# --- 可选项（O）：第一版正式支持范围之外的构建路径与平台 ---
# opt_pass 的第二个参数是**证据**（实际跑过的那条命令/那个产物）。缺证据就拒绝：
# 结构上不存在"把没执行的可选项写成 PASS"这条路，只靠纪律提醒是防不住的。
opt_pass() {
  if [[ -z "${2:-}" ]]; then
    printf '[stage5b] 结构错误: 可选项要记 PASS 必须给出实际执行的证据（没跑就是没跑）\n' >&2
    exit 2
  fi
  STEP=$((STEP+1)); O_PASS=$((O_PASS+1))
  printf '[stage5b] OPTIONAL-PASS %d: %s｜证据: %s\n' "$STEP" "${1:-}" "$2"
}
opt_fail() {
  STEP=$((STEP+1)); O_FAIL=$((O_FAIL+1))
  printf '[stage5b] OPTIONAL-FAIL %d: %s\n' "$STEP" "${1:-}" >&2
  printf '[stage5b]   可选项失败不进必需项结论，但也不得被写成通过\n'
}
opt_blocked() {
  STEP=$((STEP+1)); O_BLOCKED=$((O_BLOCKED+1))
  printf '[stage5b] OPTIONAL-BLOCKED %d: %s\n' "$STEP" "${1:-}"
  printf '[stage5b]   恢复执行: %s\n' "${2:-另建环境执行}"
}
opt_notrun() {
  STEP=$((STEP+1)); O_NOTRUN=$((O_NOTRUN+1))
  printf '[stage5b] OPTIONAL-NOT_RUN %d: %s（原因：%s）\n' "$STEP" "${1:-}" "${2:-本轮未执行}"
  printf '[stage5b]   该类项不阻塞已实测通过的 B 路径；也不表示它已通过\n'
}

# --- 超范围项（S）：产品效果，不属部署工程验收 ---
# 这个记录器**不打印 PASS 字样**（selftest 断言这一点）：容器化/恢复演练通过 ≠ 题型抗 AI/抗 OCR 通过，
# 人工可读性与 OCR 基线有各自的门禁（make blindtest / make ocr-baseline-real），这里只声明边界。
scope_note() {
  STEP=$((STEP+1)); S_NOTE=$((S_NOTE+1))
  printf '[stage5b] OUT_OF_SCOPE %d: %s\n' "$STEP" "${1:-}"
  printf '[stage5b]   边界: %s\n' "${2:-不属本门禁的部署工程结论}"
}

# --- verdict：只看必需项 ---
verdict_rc() {
  if (( R_FAIL > 0 )); then return 1; fi
  if (( R_BLOCKED > 0 || R_NOTRUN > 0 )); then return 3; fi
  return 0
}
# 稳定机器可读行：selftest 用它比较"可选项加进去之后必需项计数有没有变"。
emit_required_line() {
  printf 'REQUIRED r_pass=%d r_fail=%d r_blocked=%d r_notrun=%d\n' \
    "$R_PASS" "$R_FAIL" "$R_BLOCKED" "$R_NOTRUN"
}

# --- 分类逻辑的场景（每条都只调用记录器与 verdict，不碰磁盘/端口/docker）---
scenario_dispatch() {
  case "${1:-}" in
    r-pass-only)     pass "a 通过"; pass "b 通过"; emit_required_line; verdict_rc ;;
    r-fail)          pass "a 通过"; fail "b 失败"; emit_required_line; verdict_rc ;;
    r-blocked)       pass "a 通过"; blocked "b 外部条件不足" "见文档"; emit_required_line; verdict_rc ;;
    r-notrun)        pass "a 通过"; req_notrun "b 本轮未执行" "受控场景"; emit_required_line; verdict_rc ;;
    r-only-optional-notrun)
      pass "a 通过"; pass "b 通过"
      opt_notrun "A 路径（容器内编译）" "需要 registry 与大磁盘"; emit_required_line; verdict_rc ;;
    r-optional-mixed)
      pass "a 通过"; pass "b 通过"
      opt_notrun "amd64 交叉构建" "本机 arm64"
      opt_blocked "UID 65534 场景" "需 root"
      scope_note "人工可读性与抗 OCR" "属产品效果验收"; emit_required_line; verdict_rc ;;
    o-pass-without-evidence) opt_pass "A 路径其实没跑" ""; ;;
    o-pass-with-evidence)    opt_pass "B 路径已执行" "make docker-image-local"; emit_required_line; verdict_rc ;;
    s-only-note)     pass "a 通过"; scope_note "抗 AI/抗 OCR 基线" "由 ocr-baseline-real 自己判定"; emit_required_line; verdict_rc ;;
    *) abort "未知场景 ${1:-（空）}" ;;
  esac
}

# --- 负向自测：证明分类与退出码本身是对的 ---
selftest_classify() {
  local bad=0 got out
  # $1=场景 $2=期望退出码（"!" 表示只要非 0）$3=期望出现在输出里的串（可空）
  run_case() {
    got=0
    # `|| got=$?` 是必需的：简单赋值的退出码就是命令替换的退出码，而这里**故意**要观察非 0。
    out="$(bash "${BASH_SOURCE[0]}" --scenario "$1" 2>&1)" || got=$?
    if [[ "$2" == '!' ]]; then
      [[ $got -ne 0 ]] || { printf '[selftest] FAIL: %s 期望非 0，实际 %s\n' "$1" "$got" >&2; bad=$((bad+1)); }
    elif [[ "$got" != "$2" ]]; then
      printf '[selftest] FAIL: %s 期望 rc=%s，实际 rc=%s\n' "$1" "$2" "$got" >&2
      printf '%s\n' "$out" | sed 's/^/[selftest]   输出: /' >&2
      bad=$((bad+1))
    fi
    if [[ -n "${3:-}" ]] && ! printf '%s\n' "$out" | grep -qF -- "$3"; then
      printf '[selftest] FAIL: %s 的输出里没有 %s\n' "$1" "$3" >&2; bad=$((bad+1))
    fi
    printf '%s\n' "$out"
  }
  # 1. 必需项 FAIL ⇒ 非 0（1）。
  run_case r-fail 1 "REQUIRED r_pass=1 r_fail=1" >/dev/null
  # 2. 必需项 BLOCKED / NOT_RUN ⇒ 非 0（3）：**必需项没执行时不可能绿**。
  run_case r-blocked 3 "REQUIRED r_pass=1 r_fail=0 r_blocked=1" >/dev/null
  run_case r-notrun 3 "REQUIRED r_pass=1 r_fail=0 r_blocked=0 r_notrun=1" >/dev/null
  run_case r-pass-only 0 "REQUIRED r_pass=2 r_fail=0 r_blocked=0 r_notrun=0" >/dev/null
  # 3. 可选项未执行 ⇒ 必须显示 NOT_RUN，且**不**改变必需项结论（仍是 0）。
  #    与 r-pass-only 比较的是同一条 REQUIRED 计数行：可选项加进去，必需项计数必须一字不变。
  local line_opt
  line_opt="$(run_case r-only-optional-notrun 0 "OPTIONAL-NOT_RUN" | grep '^REQUIRED ' || true)"
  if [[ "$line_opt" != "REQUIRED r_pass=2 r_fail=0 r_blocked=0 r_notrun=0" ]]; then
    printf '[selftest] FAIL: 可选项的 NOT_RUN 改变了必需项计数：%s\n' "$line_opt" >&2; bad=$((bad+1))
  fi
  # 4. 可选项既不能被改写成 PASS（缺证据直接拒绝），也不能被计入必需项。
  run_case o-pass-without-evidence '!' "必须给出实际执行的证据" >/dev/null
  run_case o-pass-with-evidence 0 "OPTIONAL-PASS" >/dev/null
  local line_mixed
  line_mixed="$(run_case r-optional-mixed 0 "OPTIONAL-NOT_RUN" | grep '^REQUIRED ' || true)"
  if [[ "$line_mixed" != "REQUIRED r_pass=2 r_fail=0 r_blocked=0 r_notrun=0" ]]; then
    printf '[selftest] FAIL: 可选项/超范围项改变了必需项计数：%s\n' "$line_mixed" >&2; bad=$((bad+1))
  fi
  # 5. 超范围项永远不出 PASS 字样（"PASS：某项未测"这一类混合表达被结构上禁止）。
  local s_out
  s_out="$(run_case s-only-note 0 "OUT_OF_SCOPE")"
  if printf '%s\n' "$s_out" | grep -q 'OUT_OF_SCOPE.*PASS'; then
    printf '[selftest] FAIL: 超范围项的输出里出现了 PASS\n' >&2; bad=$((bad+1))
  fi
  if (( bad > 0 )); then
    printf '[selftest] RESULT=selftest-fail（%d 条分类断言不成立）\n' "$bad" >&2
    exit 1
  fi
  printf '[selftest] RESULT=selftest-pass（9 条场景断言成立：必需 FAIL/BLOCKED/NOT_RUN 都非 0，可选项不改判也不得 PASS，超范围项不出 PASS）\n'
  exit 0
}

# 分派必须在任何副作用之前（上面这些函数此时已定义）。这里的 `set +e` 是刻意的：
# 场景要**原样观察**"必需项 BLOCKED/NOT_RUN 时的非 0 退出码"，errexit 会在中途将码直接抛出去，
# 那样验证的就不是 verdict 而是 shell 的中途退出。
set +e
case "${1:-}" in
  --scenario) shift; scenario_dispatch "${1:-}"; exit $? ;;
  --selftest-classify) selftest_classify; exit $? ;;
esac
set -e

# Prerequisites are classified before mktemp/traps: an unsupported host must not touch Docker or old artifacts.
export GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off GOENV=off GOWORK=off
GO="${GO:-go}"
prerequisite_blocked() {
  blocked "$1" "在既定 Linux/arm64、Go 1.27.3、Node 24 与独占本地 Docker 环境重跑；不安装或改变本机"
  printf '[stage5b] 必需项(B 路径部署工程) PASS=0 FAIL=0 BLOCKED=1 NOT_RUN=0\n'
  printf '[stage5b] 可选项(A 路径/未验证平台) PASS=0 FAIL=0 BLOCKED=0 NOT_RUN=0\n'
  printf '[stage5b] RESULT=blocked（环境不足，未执行部署演练）\n[stage5b] SCRIPT_RC=3\n'
  exit 3
}
[[ "$(uname -s)" == Linux ]] || prerequisite_blocked "本机不是 Linux；交叉编译不能代替运行互斥与 Docker 实测"
for tool in docker curl jq node make ss "$GO"; do
  command -v "$tool" >/dev/null 2>&1 || prerequisite_blocked "缺少工具 $tool"
done
[[ "$("$GO" env GOVERSION)" == go1.27.3 ]] || prerequisite_blocked "需要明确安装的 Go 1.27.3（GOTOOLCHAIN=local）"
[[ "$(node -p 'process.versions.node.split(".")[0]')" == 24 ]] || prerequisite_blocked "需要 Node 24"
docker info >/dev/null 2>&1 || prerequisite_blocked "Docker daemon 不可达"

# Refuse to clean or overwrite any pre-existing resource with a name this drill could use.
# This check runs before mktemp, compose, build, traps, or any cleanup action. A stale prior
# run is still an external conflict: the operator must inspect it, never let this script guess ownership.
IMAGE="${EPOCHA_IMAGE:-epocha:stage5b-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
PROJECT="epocha5b"
for resource in epocha5b-second epocha5b-measure; do
  if docker container inspect "$resource" >/dev/null 2>&1; then
    prerequisite_blocked "Docker 容器 $resource 已存在；拒绝清理或复用同名资源"
  fi
done
for network in "${PROJECT}_default" epocha5b-recipe; do
  if docker network inspect "$network" >/dev/null 2>&1; then
    prerequisite_blocked "Docker 网络 $network 已存在；拒绝清理或复用同名资源"
  fi
done
if docker image inspect "$IMAGE" >/dev/null 2>&1; then
  prerequisite_blocked "Docker 镜像标签 $IMAGE 已存在；拒绝覆盖可变同名标签"
fi
if [[ -n "$(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT")" ]]; then
  prerequisite_blocked "Compose 项目 $PROJECT 已有容器；拒绝对用户资源执行 down"
fi
if [[ -n "$(docker volume ls -q --filter "label=com.docker.compose.project=$PROJECT")" ]]; then
  prerequisite_blocked "Compose 项目 $PROJECT 已有卷；拒绝清理用户资源"
fi

cd "$ROOT"
# These are captured once, before any actual build, and passed as make command-line values.
VERSION="$(git describe --tags --always --dirty)"
COMMIT="$(git rev-parse HEAD)"
BUILDDATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
BUILD_GOOS=linux
BUILD_GOARCH="$("$GO" env GOARCH)"
BUILD_OK=0
PREBUILT_DIR=""
EVIDENCE_DIR=""
WORK="$(mktemp -d "${TMPDIR:-/tmp}/epocha-stage5b.XXXXXX")"
chmod 700 "$WORK"

IMAGE="${IMAGE:-epocha:stage5b-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
PROJECT="${PROJECT:-epocha5b}"
PUBLIC_PORT="${EPOCHA_5B_PORT:-18180}"
SECOND_PORT="${EPOCHA_5B_SECOND_PORT:-18181}"
MEASURE_PORT="${EPOCHA_5B_MEASURE_PORT:-18182}"
LOCK_HINT="epocha.lock"
RUN_UID="$(id -u)"
RUN_GID="$(id -g)"
D="$WORK/deploy"
BIN="$WORK/bin"
LOGS="$WORK/logs"
mkdir -p "$BIN" "$LOGS" "$D/data" "$D/keys" "$D/backups" "$D/isolated"

avail_kb() { df -Pk "$WORK" | awk 'NR==2 {print $4}'; }

cleanup() {
  local rc=$?
  trap - EXIT
  set +e
  if [[ -n "${SOAK_PID:-}" ]]; then
    kill -TERM "$SOAK_PID" 2>/dev/null
    wait "$SOAK_PID" 2>/dev/null
  fi
  if [[ -n "${SAMPLE_PID:-}" ]]; then
    kill -TERM "$SAMPLE_PID" 2>/dev/null
    wait "$SAMPLE_PID" 2>/dev/null
  fi
  # Only public, allowlisted evidence escapes the temporary data/credential tree.
  if [[ -n "$EVIDENCE_DIR" ]]; then
    for evidence in selftest.log locktests.log race.log static.log build.log recipe-flow.log recipe-flow2.log \
      soak.txt soak-run.json soak-samples.txt soak-sample-errors.log soak-summary.json pool-before.json pool-after.json purge.log; do
      if [[ -f "$LOGS/$evidence" ]]; then cp -n "$LOGS/$evidence" "$EVIDENCE_DIR/$evidence"; fi
    done
    printf 'script_exit=%s\n' "$rc" >"$EVIDENCE_DIR/stage5b-exit.txt"
    log "本轮无凭据/DB证据保留于 $EVIDENCE_DIR；原始构建保留于 $PREBUILT_DIR"
  fi
  # 只清理**本脚本自己**创建的容器/网络/目录；绝不 prune，也不碰别人留下的东西。
  if [[ -n "${RECIPE_COMPOSE:-}" ]]; then
    $RECIPE_COMPOSE down -v --remove-orphans >/dev/null 2>&1
  fi
  ( cd "$ROOT/deploy/docker" && docker compose --project-name "$PROJECT" --env-file "$WORK/compose.env" down -v --remove-orphans ) >/dev/null 2>&1
  for name in epocha5b-second epocha5b-measure; do
    docker rm -f "$name" >/dev/null 2>&1
  done
  rm -rf "$WORK"
  exit $rc
}
trap cleanup EXIT INT TERM

compose() {
  ( cd "$ROOT/deploy/docker" && docker compose --project-name "$PROJECT" --env-file "$WORK/compose.env" "$@" )
}
container_id() {
  local out
  out="$(compose ps -q epocha 2>/dev/null)" || return 1
  printf '%s\n' "${out%%$'\n'*}"
}
# code() 取 HTTP 状态码。
# 注意 curl 在连不上时**自己就会**用 -w 打出 000，再 `|| echo 000` 会得到 "000\n000"，
# 于是"端口是空的"被读成"端口有人在听"（本轮实测的假失败就是这么来的）。
code() {
  local out
  out="$(curl -sS -o /dev/null -w '%{http_code}' --max-time "${2:-10}" "$1" 2>/dev/null)" || true
  printf '%s' "${out:-000}"
}
wait_status() {
  local deadline=$((SECONDS + ${3:-40}))
  while [[ $SECONDS -lt $deadline ]]; do
    [[ "$(code "$1")" == "${2:-200}" ]] && return 0
    sleep 1
  done
  return 1
}
port_listening() { # $1=端口 → 0 在监听 / 1 空闲 / 2 测不出来
  local out rc=0 body
  out="$(ss -ltn "sport = :$1" 2>/dev/null)" || rc=$?
  [[ $rc -eq 0 ]] || return 2
  if [[ "$out" != *$'\n'* ]]; then return 1; fi
  body="${out#*$'\n'}"
  [[ -n "${body//[[:space:]]/}" ]]
}
sha() { # 文件 sha256（不存在时打印 -）
  if [[ -f "$1" ]]; then sha256sum "$1" | cut -d' ' -f1; else printf '%s' "-"; fi
}

# --- 第 0 步：前置条件 ---
log "第 0 步：环境、端口与磁盘"
for tool in docker curl jq; do
  command -v "$tool" >/dev/null 2>&1 || abort "缺少工具 $tool"
done
if ! docker info >/dev/null 2>&1; then
  abort "docker daemon 不可达（第 2/2.5/3/4 步都无法执行）"
fi
for p in "$PUBLIC_PORT" "$SECOND_PORT" "$MEASURE_PORT" "${EPOCHA_5B_RECIPE_PORT:-18183}" 8081; do
  rc=0
  port_listening "$p" || rc=$?
  if [[ $rc -eq 0 ]]; then
    abort "端口 $p 已被占用：演练可能连到遗留进程并以错误的原因通过"
  elif [[ $rc -ne 1 ]]; then
    abort "ss 无法判定端口 $p（rc=$rc）：不能把“测不出来”当成“空闲”"
  fi
done
# 150MB 是"能开始"的下界，不是"跑得完"的保证：镜像重建 + 两套 compose + 演练数据目录
# 都在同一条分区上。所以第 4 步之前还会**再量一次**，不够就把持续演练标成 BLOCKED（必需项），
# 而不是让一个半路 ENOSPC 的容器把整场演练变成一次无法解释的失败。
NEED_KB=$((150 * 1024))
AVAIL_KB="$(avail_kb)"
if (( AVAIL_KB >= NEED_KB )); then
  DOCKER_OK=1
  pass "工具齐备、端口空闲（$PUBLIC_PORT/$SECOND_PORT/$MEASURE_PORT/${EPOCHA_5B_RECIPE_PORT:-18183}/8081），磁盘余量 $((AVAIL_KB / 1024))MB ≥ $((NEED_KB / 1024))MB"
else
  blocked "磁盘余量 $((AVAIL_KB / 1024))MB < 容器演练所需 $((NEED_KB / 1024))MB ⇒ 第 2/3/4 步本轮不执行" \
    "释放磁盘后单独跑：bash docs/scripts/stage5b.sh（第 1 步的测试不受影响）"
fi

# --- 第 1 步：跨进程互斥的真实测试 + 一次回归 ---
log "第 1 步：先自证分类逻辑，再跑运行锁的真实子进程测试"
rc=0
bash "${BASH_SOURCE[0]}" --selftest-classify >"$LOGS/selftest.log" 2>&1 || rc=$?
if [[ $rc -ne 0 ]]; then
  tail -n 20 "$LOGS/selftest.log"
  fail "门禁自身的分类负向自测退出码 $rc（必需项没执行时必须非 0、可选项未执行必须显示 NOT_RUN）"
else
  pass "分类逻辑的 9 条负向断言成立（必需 FAIL⇒1、必需 BLOCKED/NOT_RUN⇒3、可选项不改判且缺证据不得 PASS、超范围项不出 PASS）"
fi
log "第 1 步：运行锁的测试（真实子进程）"
rc=0
"$GO" test -count=1 -race -timeout 600s ./internal/store/ ./cmd/server/ ./cmd/ops/ ./cmd/loadtest/ >"$LOGS/locktests.log" 2>&1 || rc=$?
if [[ $rc -ne 0 ]]; then
  tail -n 40 "$LOGS/locktests.log"
  fail "锁相关包的 -race 测试退出码 $rc"
else
  pass "internal/store(锁+备份恢复) / cmd/server(真实进程互斥) / cmd/ops(真实进程互斥+恢复边界) / cmd/loadtest(节流) 的 -race 测试全通过"
fi
missing_server_tests=0
for want in \
  "TestSecondServerCannotOpenSameDataDir" \
  "TestGracefulStopAllowsRestart" \
  "TestSIGKILLReleasesLock" \
  "TestStaleLockFileDoesNotBlockServer" \
  "TestStartupVersusOfflineHolderIsMutuallyExclusive" \
  "TestHolderAndServerOnSameDirConflictViaDirLock"; do
  if ! grep -q "^func $want" cmd/server/lock_test.go; then
    missing_server_tests=1
    fail "缺少必需的真实进程测试 $want"
  fi
done
if [[ $missing_server_tests -eq 0 ]]; then
  pass "服务侧 6 条真实子进程测试都在（抢同一数据目录、优雅重启、SIGKILL、陈旧锁文件、启动竞态、目录锁冲突）"
fi
missing_ops_tests=0
for want in \
  "TestAdminRecoverRefusesWhileLockHeldByAnotherProcess" \
  "TestAdminRecoverSucceedsAfterHolderExit" \
  "TestBackupDoesNotNeedTheLock" \
  "TestRestoreRefusesWhenOutDirIsInUse" \
  "TestTwoAdminRecoverProcessesRace"; do
  if ! grep -q "^func $want" cmd/ops/main_test.go; then
    missing_ops_tests=1
    fail "缺少必需的运维侧测试 $want"
  fi
done
if [[ $missing_ops_tests -eq 0 ]]; then
  pass "运维侧 5 条测试都在：服务在跑时改不了库、退出后可重取、在线备份不需要停服、就地恢复被拒、两进程抢恢复恰好一个成功"
fi

log "第 1b 步：一次全量回归（-race ./...）"
rc=0
"$GO" test -race -count=1 -timeout 1200s ./... >"$LOGS/race.log" 2>&1 || rc=$?
if [[ $rc -ne 0 ]]; then
  tail -n 30 "$LOGS/race.log"
  fail "go test -race ./... 退出码 $rc"
else
  pass "go test -race -count=1 ./... 退出码 0"
fi
rc=0
make GO="$GO" fmt-check vet size >"$LOGS/static.log" 2>&1 || rc=$?
if [[ $rc -ne 0 ]]; then
  tail -n 20 "$LOGS/static.log"
  fail "fmt-check/vet/size 退出码 $rc"
else
  pass "fmt-check / vet / size 全通过"
fi
DEPS="$("$GO" list -deps ./cmd/server)"
for banned in internal/eval cmd/preview cmd/devseed cmd/evalgen cmd/evalocr cmd/exampleapp cmd/blindtest cmd/loadtest web/preview; do
  if printf '%s\n' "$DEPS" | grep -q "EpoCHA/$banned"; then
    fail "服务二进制链接了开发期包 $banned"
  fi
done
pass "cmd/server 仍不链接任何开发期包（go list -deps 证明）"

# --- 第 2 步：容器与宿主共享同一把锁 ---
log "第 2 步：真实容器里的互斥（宿主 ↔ 容器，两个容器）"
if [[ $DOCKER_OK -eq 1 ]]; then
  mkdir -p "$ROOT/deploy/docker/.prebuilt" "$ROOT/release-artifacts"
  BUILD_RUN="$(mktemp -d "$ROOT/deploy/docker/.prebuilt/run-XXXXXX")"
  PREBUILT_DIR="$BUILD_RUN/build"
  EVIDENCE_DIR="$(mktemp -d "$ROOT/release-artifacts/stage5b-XXXXXX")"
  rc=0
  make docker-image-local VERSION="$VERSION" COMMIT="$COMMIT" BUILDDATE="$BUILDDATE" PREBUILT_DIR="$PREBUILT_DIR" DOCKER_IMAGE="$IMAGE" GO="$GO" BUILD_GOOS="$BUILD_GOOS" BUILD_GOARCH="$BUILD_GOARCH" >"$LOGS/build.log" 2>&1 || rc=$?
  if [[ $rc -ne 0 ]]; then
    fail "实际构建或镜像字节/版本对账失败（make rc=$rc）；停止演练，不复用同名旧标签。证据 $LOGS/build.log"
    printf '[stage5b] 必需项(B 路径部署工程) PASS=%d FAIL=%d BLOCKED=%d NOT_RUN=%d\n' "$R_PASS" "$((R_FAIL))" "$R_BLOCKED" "$R_NOTRUN"
    printf '[stage5b] 可选项(A 路径/未验证平台) PASS=%d FAIL=%d BLOCKED=%d NOT_RUN=%d\n' "$O_PASS" "$O_FAIL" "$O_BLOCKED" "$O_NOTRUN"
    printf '[stage5b] 超范围项(产品效果，本门禁不判定) 登记=%d\n' "$S_NOTE"
    printf '[stage5b] RESULT=fail（实际构建失败，未使用旧产物）\n[stage5b] SCRIPT_RC=1\n'
    exit 1
  else
    BUILD_OK=1
    # All drills use the verified immutable local image ID, never re-resolve a mutable tag.
    IMAGE_TAG="$IMAGE"
    IMAGE="$(node -e 'const fs=require("node:fs"); process.stdout.write(JSON.parse(fs.readFileSync(process.argv[1],"utf8")).image_id)' "$PREBUILT_DIR/image.json")"
    pass "新镜像实际两份字节与版本完全对账（local image ID=$IMAGE，mutable tag=$IMAGE_TAG；均不称 registry digest）"
  fi

  gen_key() { head -c32 /dev/urandom | base64 >"$1"; chmod 600 "$1"; }
  gen_key "$D/keys/master"; gen_key "$D/keys/pepper"
  ADMIN0="ak_$(head -c32 /dev/urandom | base64 | tr -d '/+=' | head -c48)"
  printf '%s' "$ADMIN0" >"$D/keys/admin"; chmod 600 "$D/keys/admin"
  cat >"$WORK/compose.env" <<EOF
EPOCHA_IMAGE=$IMAGE
EPOCHA_VERSION=$VERSION
EPOCHA_COMMIT=$COMMIT
EPOCHA_BUILD_DATE=$BUILDDATE
EPOCHA_PUBLIC_BIND=127.0.0.1
EPOCHA_PUBLIC_PORT=$PUBLIC_PORT
EPOCHA_DATA_DIR=$D/data
EPOCHA_KEY_DIR=$D/keys
EPOCHA_BACKUP_DIR=$D/backups
EPOCHA_UID=$RUN_UID
EPOCHA_GID=$RUN_GID
EPOCHA_ADMIN_KEY_FILE=/etc/epocha/keys/admin
EPOCHA_NETWORK=${PROJECT}_default
EPOCHA_SUBNET=172.30.0.0/24
EOF
  # compose 里的容器内路径常量（不是宿主路径）。
  # 上面的 ${...} 在 heredoc 里就地展开，宿主路径与容器路径各归各位：
  # 宿主 $D/keys → 容器内 /etc/epocha/keys（同一个文件，所以锁与密钥都按同一份走）。
  chmod 600 "$WORK/compose.env"

  rc=0
  compose up -d --wait --wait-timeout 60 >"$LOGS/up.log" 2>&1 || rc=$?
  CID="$(container_id)"
  if [[ $rc -ne 0 || -z "$CID" ]]; then
    tail -n 20 "$LOGS/up.log"
    fail "服务容器没起来（rc=$rc）"
  else
    pass "服务容器 healthy 且 /readyz 通（$PUBLIC_PORT）"

    # 锁文件必须出现在**宿主能看到的数据目录**里——这是"同一 inode"的前提。
    if [[ -f "$D/data/epocha.lock" ]]; then
      pass "数据目录里有 epocha.lock（宿主可见）：容器与宿主 bind 的是同一个文件，flock 才可能互通"
    else
      fail "数据目录里没有 epocha.lock——服务没在数据目录里留锁，跨命名空间的互斥无从谈起"
    fi
    DB_SHA_BEFORE="$(sha "$D/data/epocha.db")"

    # (a) 服务在跑时，**宿主**上的 admin-recover 必须被拒。
    cp "$PREBUILT_DIR/out/epocha-ops" "$BIN/epocha-ops"
    rc=0
    EPOCHA_PEPPER_FILE="$D/keys/pepper" "$BIN/epocha-ops" admin-recover \
      --db "$D/data/epocha.db" --key-file "$D/recovered-key" >"$LOGS/recover-host.log" 2>&1 || rc=$?
    if [[ $rc -eq 0 ]]; then
      fail "服务容器在跑，宿主上的 admin-recover 却成功了——锁没有跨命名空间生效"
    elif ! grep -q "已被另一个进程占用" "$LOGS/recover-host.log"; then
      fail "宿主 admin-recover 被拒了，但原因不是运行锁：$(tail -c 220 "$LOGS/recover-host.log")"
    else
      pass "宿主进程无法在服务容器运行时改库（拒绝原因 = 数据目录已被另一个进程占用）"
    fi
    if [[ -e "$D/recovered-key" ]]; then
      fail "被拒的宿主 admin-recover 留下了凭据文件"
    fi

    # (b) 第二个服务实例（另一个容器、同一数据目录、另一个端口）必须起不来。
    rc=0
    docker run -d --name epocha5b-second \
      --user "$RUN_UID:$RUN_GID" --read-only \
      --cap-drop ALL --security-opt no-new-privileges \
      --memory 128m --memory-swap 128m --cpus 1 --pids-limit 128 \
      -v "$D/data:/var/lib/epocha" -v "$D/keys:/etc/epocha/keys:ro" \
      -p "127.0.0.1:$SECOND_PORT:8080" \
      -e EPOCHA_LISTEN=0.0.0.0:8080 -e EPOCHA_DB=/var/lib/epocha/epocha.db \
      -e EPOCHA_MASTER_KEY_FILE=/etc/epocha/keys/master -e EPOCHA_PEPPER_FILE=/etc/epocha/keys/pepper \
      "$IMAGE" >/dev/null 2>&1 || rc=$?
    # docker wait 没有超时参数，所以自己轮询状态：把"命令写错"和"第二个实例真的活着"
    # 区分开很重要——后者的处置是修锁，前者的处置是修脚本。
    SECOND_RC="timeout"
    if [[ $rc -ne 0 ]]; then
      SECOND_RC="create-failed"
    else
      for _ in $(seq 1 30); do
        ST2="$(docker inspect -f '{{.State.Status}}' epocha5b-second 2>/dev/null || echo gone)"
        if [[ "$ST2" == "exited" || "$ST2" == "dead" || "$ST2" == "gone" ]]; then
          SECOND_RC="$(docker inspect -f '{{.State.ExitCode}}' epocha5b-second 2>/dev/null || echo '?')"
          break
        fi
        sleep 1
      done
    fi
    SECOND_LOG="$(docker logs epocha5b-second 2>&1 | tail -c 400 || true)"
    docker rm -f epocha5b-second >/dev/null 2>&1 || true
    if [[ "$SECOND_RC" == "timeout" || "$SECOND_RC" == "0" || "$SECOND_RC" == "create-failed" ]]; then
      fail "第二个服务容器没在 30s 内退出（状态/退出码=$SECOND_RC）——两个写者可能正在共享同一个数据目录"
    elif ! printf '%s' "$SECOND_LOG" | grep -q "已被另一个进程占用"; then
      fail "第二个服务容器退出了，但原因不是运行锁：$SECOND_LOG"
    else
      pass "第二个服务容器立即拒绝启动（同一个数据目录上只允许一个写者），并给出锁冲突原因"
    fi
    # 输家不得改库、不得创建自己的库文件。
    if [[ "$(sha "$D/data/epocha.db")" != "$DB_SHA_BEFORE" ]]; then
      fail "被拒的第二个实例仍然改了工作库"
    fi
    if [[ -e "$D/data/epocha.db-wal" && ! -s "$D/data/epocha.db-wal" ]]; then
      log "  （数据目录里有空的 -wal，这是 WAL 模式的正常形状）"
    fi
    # 赢家不受影响。
    if [[ "$(code "http://127.0.0.1:$PUBLIC_PORT/readyz")" != "200" ]]; then
      fail "争用之后赢家不再就绪"
    else
      pass "争用与两次拒绝之后，运行中的服务仍然就绪（赢家没被扰动）"
    fi

    # (c) 停服后同一把锁必须立即可取，不需要任何清理动作；锁文件不许被删。
    compose stop >/dev/null 2>&1 || true
    rc=0
    EPOCHA_PEPPER_FILE="$D/keys/pepper" "$BIN/epocha-ops" admin-recover \
      --db "$D/data/epocha.db" --key-file "$D/recovered-key" >"$LOGS/recover-ok.log" 2>&1 || rc=$?
    if [[ $rc -ne 0 ]]; then
      fail "停服后 admin-recover 仍失败（rc=$rc）：$(tail -c 220 "$LOGS/recover-ok.log")"
    else
      ADMIN1="$(awk '/key_id=/{sub(/^.*key_id=/,"");sub(/ .*/,"");print;exit}' "$LOGS/recover-ok.log")"
      if [[ ! -s "$D/recovered-key" || "$(stat -c '%a' "$D/recovered-key")" != "600" ]]; then
        fail "恢复出的凭据文件缺失或权限不对"
      elif grep -qF "$(cat "$D/recovered-key")" "$LOGS/recover-ok.log"; then
        fail "admin-recover 的输出把新凭据值带了出来"
      else
        pass "停服后同一把锁立即可取（锁文件仍在、未被删除），admin-recover 成功并只打印 key_id=$ADMIN1"
      fi
      # 换入新凭据文件并重启：管理访问应当只认这一把。
      cp "$D/recovered-key" "$D/keys/admin"; chmod 600 "$D/keys/admin"; rm -f "$D/recovered-key"
      compose up -d --wait --wait-timeout 60 >/dev/null 2>&1 || true
      CID="$(container_id)"
      rc=0
      if compose exec -T epocha /usr/local/bin/epocha-ops admin GET /api/admin/whoami </dev/null >"$LOGS/whoami.raw" 2>&1; then
        sed -n '2,$p' "$LOGS/whoami.raw" >"$LOGS/whoami.json"
        WHO="$(jq -r '.key_id // ""' "$LOGS/whoami.json" 2>/dev/null || echo "")"
        if [[ "$WHO" == "$ADMIN1" ]]; then
          pass "重启后管理面认的正是恢复时装入的那把新凭据（whoami=$WHO）"
        else
          fail "whoami=$WHO 与 admin-recover 报出的 key_id=$ADMIN1 不一致"
        fi
      else
        fail "重启后管理面不可用：$(tail -c 200 "$LOGS/whoami.raw")"
      fi
    fi

    # (c2) 就地恢复的判据要一个**当场**的基线：上面的 admin-recover  legitimately 改过库，
    #      拿更早的快照来比会把"合法的修改"读成"被拒的操作改了库"（本轮实测撞过）。
    DB_SHA_LIVE="$(sha "$D/data/epocha.db")"

    # (d) 服务在跑时，在线备份必须**照常成功**（不要求停服）。
    rc=0
    compose exec -T epocha /usr/local/bin/epocha-ops backup \
      --db /var/lib/epocha/epocha.db --dest /var/backups/epocha >"$LOGS/backup-live.log" 2>&1 || rc=$?
    if [[ $rc -ne 0 ]]; then
      fail "服务运行时在线备份被拒（rc=$rc）：$(tail -c 200 "$LOGS/backup-live.log")——过度加锁会把备份变成没人肯做的事"
    else
      BK="$(awk '/^备份完成/{sub(/^name=/,"",$2);print $2;exit}' "$LOGS/backup-live.log")"
      if [[ -n "$BK" && -f "$D/backups/$BK.db" && -f "$D/backups/$BK.manifest.json" ]]; then
        pass "在线备份在服务持锁期间成功（产物 $BK.db + manifest，权限 $(stat -c '%a' "$D/backups/$BK.db")）"
      else
        fail "在线备份说成功，但产物不齐"
      fi
    fi

    # (e) 就地恢复（out-dir = 正在服务的数据目录）必须被拒；隔离目录可以。
    OLDKEY_ADMIN="$(printf '%s' "$ADMIN1")"
    rc=0
    EPOCHA_PEPPER_FILE="$D/keys/pepper" EPOCHA_ADMIN_KEY="$OLDKEY_ADMIN" \
      "$BIN/epocha-ops" restore --backup "$D/backups/$BK.db" --manifest "$D/backups/$BK.manifest.json" \
      --out-dir "$D/data" >"$LOGS/restore-live.log" 2>&1 || rc=$?
    if [[ $rc -eq 0 ]]; then
      fail "恢复到正在服务的数据目录竟然成功了（这就是“就地恢复”手滑的落点）"
    elif ! grep -q "正被某个服务用作数据目录" "$LOGS/restore-live.log"; then
      fail "就地恢复被拒，但原因不对：$(tail -c 220 "$LOGS/restore-live.log")"
    else
      pass "就地恢复（out-dir 正被服务使用）被拒，并点名风险；工作库未被改动"
    fi
    if [[ "$(sha "$D/data/epocha.db")" != "$DB_SHA_LIVE" ]]; then
      fail "被拒的就地恢复改了工作库"
    else
      pass "被拒的就地恢复对工作库逐字节无副作用"
    fi
    rc=0
    EPOCHA_PEPPER_FILE="$D/keys/pepper" EPOCHA_ADMIN_KEY="ak_$(head -c32 /dev/urandom | base64 | tr -d '/+=' | head -c48)" \
      "$BIN/epocha-ops" restore --backup "$D/backups/$BK.db" --manifest "$D/backups/$BK.manifest.json" \
      --out-dir "$D/isolated" >"$LOGS/restore-iso.log" 2>&1 || rc=$?
    if [[ $rc -ne 0 ]]; then
      fail "恢复到隔离目录失败（rc=$rc）：$(tail -c 220 "$LOGS/restore-iso.log")——锁不该挡这条路"
    else
      pass "同一份备份恢复到隔离目录成功（锁只挡“就地恢复”，不挡隔离恢复）"
    fi
    # 错误 pepper 配对：拒绝且不产生任何文件。
    head -c32 /dev/urandom | base64 >"$D/foreign-pepper.b64"; chmod 600 "$D/foreign-pepper.b64"
    mkdir -p "$D/isolated2"
    rc=0
    EPOCHA_PEPPER_FILE="$D/foreign-pepper.b64" EPOCHA_ADMIN_KEY="ak_$(head -c32 /dev/urandom | base64 | tr -d '/+=' | head -c48)" \
      "$BIN/epocha-ops" restore --backup "$D/backups/$BK.db" --manifest "$D/backups/$BK.manifest.json" \
      --out-dir "$D/isolated2" >/dev/null 2>&1 || rc=$?
    if [[ $rc -eq 0 ]]; then
      fail "pepper 不配对的恢复竟然成功"
    elif compgen -G "$D/isolated2/*" >/dev/null 2>&1 && \
         [[ -n "$(find "$D/isolated2" -mindepth 1 ! -name "$LOCK_HINT" -print -quit 2>/dev/null)" ]]; then
      # 取锁会在目标目录留下 epocha.lock，那是 flock 的正常形状、不是半成品；
      # 半成品指的是 .db / .partial / -wal / -shm 这些"看起来像一份库"的东西。
      fail "pepper 不配对的恢复留下了库形态的产物：$(find "$D/isolated2" -mindepth 1 ! -name "$LOCK_HINT" -printf '%f ' 2>/dev/null)"
    else
      pass "pepper 与备份不配对 ⇒ 拒绝且不留下任何文件"
    fi
  fi
else
  blocked "容器与宿主共享锁、就地恢复拒绝、在线备份等演练：磁盘不足" "释放磁盘后：bash docs/scripts/stage5b.sh"
fi

# --- 第 2.5 步：DEPLOYMENT §11 的启动配方（照文档的形状真跑一遍）---
# 这一步的存在理由：文档里的命令只有"复制出去能不能起得来"这一个判据，而 shell 语法检查
# 证明不了六件事：端口绑定形状、uid 与目录属主、变量名漂移、重跑不覆盖密钥、真实闭环的一次性
# 消费、以及重建容器后这些状态是否还在。所以按 §11 的调用形状（仓库根 + -f/--project-directory，
# 不 cd）在**隔离部署根**里真跑一次，全程只清理本步自己创建的那一套。
log "第 2.5 步：启动配方演练（隔离部署根 $WORK/recipe）"
RECIPE_PORT="${EPOCHA_5B_RECIPE_PORT:-18183}"
EPR="$WORK/recipe"
RECIPE_COMPOSE=""
RC0=0
port_listening "$RECIPE_PORT" || RC0=$?
if [[ $RC0 -ne 1 ]]; then
  fail "配方演练要求端口 $RECIPE_PORT 空闲（判定结果 rc=$RC0）：连到遗留进程会比失败更危险"
elif [[ $DOCKER_OK -ne 1 ]]; then
  req_notrun "启动配方演练（DEPLOYMENT §11）" "磁盘不足 ⇒ 这是必需项，不是可选项"
else
  mkdir -p "$EPR"
  # 变量名双向对账：打错一个名字的表现不是报错，而是"用默认值静默启动"——
  # 文档说的和实际跑的就成了两件事。这一条不需要 docker，纯文本判。
  # 失败方向必须闭合：两个列表**取不到东西**（文件不在、pattern 不对）也算 FAIL。
  # 空输入 ⇒ 无漂移 ⇒ PASS 就是"判据写反"，演练台上真的撞出来过一次。
  drift=""
  RC0=0
  CVAR="$(grep -o '\${[A-Z][A-Z0-9_]*' deploy/docker/compose.yml | sed 's/[$]{//' | sort -u)" || RC0=$?
  if [[ $RC0 -ne 0 || -z "$CVAR" ]]; then
    fail "从 compose.yml 取不到任何 \${VAR} 引用（rc=$RC0）⇒ 双向对账没法做，不能把空输入读成通过"
  fi
  RC0=0
  EVAR="$(grep -oE '^[A-Z][A-Z0-9_]*=' deploy/docker/.env.example | tr -d '=' | sort -u)" || RC0=$?
  if [[ $RC0 -ne 0 || -z "$EVAR" ]]; then
    fail "从 .env.example 取不到任何键（rc=$RC0）⇒ 双向对账没法做"
  fi
  for v in $CVAR; do
    # 样例里两种存在形式都算"这个名字确实在"：`KEY=` 与被注释掉的 `# KEY=`（可选项）。
    if ! grep -qE "^(# ?)?$v=" deploy/docker/.env.example; then drift="$drift ${v}(compose 读它但样例没给)"; fi
  done
  for v in $EVAR; do
    if ! grep -q "\${$v" deploy/docker/compose.yml; then drift="$drift ${v}(样例里有但 compose 不读)"; fi
  done
  if [[ -n "$drift" ]]; then
    fail "compose 与 .env.example 的变量名漂移:$drift"
  else
    pass "变量名双向对账通过（compose 引用 $(printf '%s\n' "$CVAR" | wc -l) 个变量，样例给出 $(printf '%s\n' "$EVAR" | wc -l) 个键，两边一一对应）"
  fi

  cp deploy/docker/compose.yml deploy/docker/.env.example "$EPR/"
  # 与 §11 逐行同形状（`[[ -e ]] ||` 幂等 + set -C 兜底）。这里测的就是"重跑一遍这一段会不会毁密钥"。
  # 函数体写成 `(...)`：bash 里 set -C 不会随函数返回而撤销，只有子 shell 才把它关在这里。
  keys_section() (
    set -C
    mkdir -p "$EPR/data" "$EPR/keys" "$EPR/backups"
    chmod 700 "$EPR/data" "$EPR/keys" "$EPR/backups"
    for k in master pepper; do
      [[ -e "$EPR/keys/$k" ]] || head -c32 /dev/urandom | base64 > "$EPR/keys/$k"
      chmod 600 "$EPR/keys/$k"
    done
    [[ -e "$EPR/keys/admin" ]] || printf 'ak_%s' "$(head -c32 /dev/urandom | base64 | tr -d '/+=' | head -c48)" > "$EPR/keys/admin"
    chmod 600 "$EPR/keys/admin"
  )
  RC0=0
  keys_section || RC0=$?
  if [[ $RC0 -ne 0 ]]; then
    fail "§11 的密钥准备段第一次就跑不通（rc=$RC0）"
  elif [[ ! -s "$EPR/keys/master" || ! -s "$EPR/keys/pepper" || ! -s "$EPR/keys/admin" ]]; then
    fail "三把密钥文件不齐（master/pepper/admin）"
  elif [[ "$(stat -c '%a' "$EPR/keys/master")$(stat -c '%a' "$EPR/keys/admin")" != "600600" ]]; then
    fail "密钥文件权限不是 0600"
  else
    M1="$(sha "$EPR/keys/master")"
    # set -C 到底拦不拦得住：对已存在文件做 `>` 必须失败。这是 §11 那句"重跑不毁密钥"的机制本身。
    RC0=0
    ( set -C; head -c32 /dev/urandom | base64 > "$EPR/keys/master" ) || RC0=$?
    if [[ $RC0 -eq 0 ]]; then
      fail "set -C 下仍然覆盖了已有 master——文档承诺的重跑保护不成立"
    else
      RC1=0
      keys_section || RC1=$?      # 原样重跑整段
      if [[ "$M1" != "$(sha "$EPR/keys/master")" ]]; then
        fail "重跑密钥段改了 master（内容变了）"
      elif [[ $RC1 -ne 0 ]]; then
        fail "§11 的密钥段不能安全重跑（第二次 rc=$RC1，期望幂等）"
      else
        pass "密钥段幂等且带保护：set -C 拒绝覆盖已存在密钥（rc=$RC0 即那次拒绝），整段重跑后 master 未变且退出码 0"
      fi
    fi
    cp "$EPR/.env.example" "$EPR/.env"
    sed -i "s|^EPOCHA_UID=.*|EPOCHA_UID=$RUN_UID|;s|^EPOCHA_GID=.*|EPOCHA_GID=$RUN_GID|" "$EPR/.env"
    sed -i "s|^# EPOCHA_ADMIN_KEY_FILE=.*|EPOCHA_ADMIN_KEY_FILE=/etc/epocha/keys/admin|" "$EPR/.env"
    sed -i "s|^EPOCHA_PUBLIC_PORT=.*|EPOCHA_PUBLIC_PORT=$RECIPE_PORT|" "$EPR/.env"
    sed -i "s|^EPOCHA_NETWORK=.*|EPOCHA_NETWORK=epocha5b-recipe|" "$EPR/.env"
    sed -i "s|^EPOCHA_SUBNET=.*|EPOCHA_SUBNET=172.31.0.0/24|" "$EPR/.env"
    sed -i "s|^EPOCHA_IMAGE=.*|EPOCHA_IMAGE=$IMAGE|" "$EPR/.env"
    chmod 600 "$EPR/.env"
    RECIPE_COMPOSE="docker compose -p epocha5brecipe -f $EPR/compose.yml --project-directory $EPR --env-file $EPR/.env"

    RC0=0
    $RECIPE_COMPOSE config >"$LOGS/recipe-config.yml" 2>&1 || RC0=$?
    if [[ $RC0 -ne 0 ]]; then
      tail -n 15 "$LOGS/recipe-config.yml"
      fail "§11 的调用形状（仓库根、不 cd、-f/--project-directory/--env-file）渲染失败（rc=$RC0）"
    else
      # 渲染后的端口是**映射形式**（host_ip / target / published），不是 "8080:8080" 字符串：
      # 断言必须照着真实渲染形状写，否则"省略宿主 IP"这类检查会因为找不到那个串而永远通过。
      PORTBLK="$(awk '/^    ports:/{f=1;next} f && /^    [a-z]/{f=0} f' "$LOGS/recipe-config.yml")"
      if ! printf '%s\n' "$PORTBLK" | grep -q "published: \"$RECIPE_PORT\""; then
        fail "渲染结果里没有公共端口 $RECIPE_PORT 的发布项：$(printf '%s' "$PORTBLK" | tr '\n' ' ')"
      elif ! printf '%s\n' "$PORTBLK" | grep -q "host_ip: 127.0.0.1"; then
        fail "公共端口的发布项缺 host_ip（省略宿主 IP 会绑 0.0.0.0，等于把验证服务放到公网网卡）"
      elif printf '%s\n' "$PORTBLK" | grep -q 'published: "8081"'; then
        fail "管理端口 8081 被发布了"
      else
        pass "§11 调用形状合法：公共端口显式带 host_ip 127.0.0.1，管理端口 8081 没有任何发布项"
      fi
      if grep -q "source: $EPR/data" "$LOGS/recipe-config.yml"; then
        pass "compose 里的 ./data 按**项目目录**解析到隔离部署根（$EPR/data），不会写进仓库目录"
      else
        fail "绑定挂载没解析到 $EPR/data：$(grep -A1 'type: bind' "$LOGS/recipe-config.yml" | grep source | tr '\n' ' ')"
      fi
    fi

    RC0=0
    $RECIPE_COMPOSE up -d --wait --wait-timeout 90 >"$LOGS/recipe-up.log" 2>&1 || RC0=$?
    RU="http://127.0.0.1:$RECIPE_PORT"
    if [[ $RC0 -ne 0 || "$(code "$RU/readyz")" != "200" ]]; then
      tail -n 20 "$LOGS/recipe-up.log"
      fail "按 §11 起不来或没就绪（rc=$RC0，readyz=$(code "$RU/readyz")）"
    else
      pass "① 文档命令真起得来：--wait 依 /readyz 判定可接流量（宿主 $RECIPE_PORT，容器 uid=$RUN_UID 与目录属主一致）"

      # ② 容器内受控管理命令：引导凭据（EPOCHA_ADMIN_KEY_FILE）生效的唯一直接观察点。
      RC0=0
      $RECIPE_COMPOSE exec -T epocha /usr/local/bin/epocha-ops admin GET /api/admin/whoami \
        </dev/null >"$LOGS/recipe-whoami.raw" 2>&1 || RC0=$?
      sed -n '2,$p' "$LOGS/recipe-whoami.raw" >"$LOGS/recipe-whoami.json"   # 第一行是 ops 的 status=NNN
      RWHO="$(jq -r '.key_id // ""' "$LOGS/recipe-whoami.json" 2>/dev/null || echo "")"
      if [[ $RC0 -ne 0 || -z "$RWHO" ]]; then
        fail "容器内管理命令失败（rc=$RC0）：$(tail -c 220 "$LOGS/recipe-whoami.raw")"
      else
        pass "② 容器内 epocha-ops admin 可用，EPOCHA_ADMIN_KEY_FILE 引导出的凭据通过鉴权（whoami=$RWHO）"
      fi
      RC0=0
      port_listening 8081 || RC0=$?
      if [[ $RC0 -eq 0 ]]; then
        fail "宿主上出现了 8081 监听——管理面被发布了"
      elif [[ $RC0 -ne 1 ]]; then
        fail "ss 判不出 8081（rc=$RC0）：不能把测不出来当成没发布"
      else
        pass "管理端口 8081 在宿主上不可达（只有容器内 127.0.0.1:8081）"
      fi

      "$GO" build -trimpath -o "$BIN/devclient" ./cmd/devclient
      admin_r() { # $1=METHOD $2=PATH $3=OUT(可选第 4 个参数=请求体)
        local rc2=0
        if [[ -n "${4:-}" ]]; then
          printf '%s' "$4" | $RECIPE_COMPOSE exec -T epocha /usr/local/bin/epocha-ops admin "$1" "$2" \
            >"$3.raw" 2>"$3.err" || rc2=$?
        else
          $RECIPE_COMPOSE exec -T epocha /usr/local/bin/epocha-ops admin "$1" "$2" </dev/null \
            >"$3.raw" 2>"$3.err" || rc2=$?
        fi
        [[ $rc2 -eq 0 ]] || return $rc2
        sed -n '2,$p' "$3.raw" >"$3"
      }

      # ③ 测试站点 + 一次真实闭环（challenge → 真解 PoW → redeem → /siteverify）。
      #    限流抬高只写在这个测量站点上（§10.5：默认 60 IP-RPM 会先于容量起作用），不进 compose。
      RSITE=""; RSECRET=""
      RC0=0
      admin_r POST /api/admin/sites "$LOGS/recipe-site.json" \
        '{"name":"recipe-drill","verify_mode":"pow_only","allow_missing_origin":true,"pow_bits":12,"challenge_ttl_sec":180,"response_ttl_sec":120,"rate_ip_rpm":600,"rate_site_rpm":600}' || RC0=$?
      RSITE="$(jq -r '.site.site_key // ""' "$LOGS/recipe-site.json" 2>/dev/null || echo "")"
      RSECRET="$(jq -r '.secret // ""' "$LOGS/recipe-site.json" 2>/dev/null || echo "")"
      RC0=0
      admin_r POST /api/admin/sites "$LOGS/recipe-site2.json" \
        '{"name":"recipe-drill-2","verify_mode":"pow_only","allow_missing_origin":true,"pow_bits":12,"rate_ip_rpm":600,"rate_site_rpm":600}' || RC0=$?
      SITE2="$(jq -r '.site.site_key // ""' "$LOGS/recipe-site2.json" 2>/dev/null || echo "")"
      SECRET2="$(jq -r '.secret // ""' "$LOGS/recipe-site2.json" 2>/dev/null || echo "")"
      if [[ -z "$RSITE" || -z "$RSECRET" || -z "$SITE2" || -z "$SECRET2" ]]; then
        # pow_bits 的下界是 12（internal/pow.MinBits）——填 10 会得到 400，而 400 的正文只在 stdout 里。
        fail "建不出测试站点（site1=${RSITE:-空} site2=${SITE2:-空}）：stdout=$(tail -c 200 "$LOGS/recipe-site.raw" 2>/dev/null) stderr=$(tail -c 160 "$LOGS/recipe-site.err" 2>/dev/null)"
      else
        RC0=0
        EPOCHA_SITE_SECRET="$RSECRET" "$BIN/devclient" --base "$RU" --site-key "$RSITE" --mode full \
          >"$LOGS/recipe-flow.log" 2>&1 || RC0=$?
        if [[ $RC0 -eq 0 ]] && grep -q "step=siteverify success=true" "$LOGS/recipe-flow.log"; then
          pass "③ 真实闭环走通（pow_bits=12 由工具真解，四端点：challenge/redeem/siteverify 全过）"
        else
          fail "闭环没走通（rc=$RC0）：$(tail -c 220 "$LOGS/recipe-flow.log")"
        fi

        # ④ 同一个 token 第二次验证必须被拒（一次性消费）。
        RC0=0
        EPOCHA_SITE_SECRET="$SECRET2" "$BIN/devclient" --base "$RU" --site-key "$SITE2" \
          --mode redeem-only --response-file "$WORK/resp-once" >"$LOGS/recipe-acq.log" 2>&1 || RC0=$?
        if [[ $RC0 -ne 0 ]]; then
          fail "拿不到未消费的 response：$(tail -c 200 "$LOGS/recipe-acq.log")"
        else
          FIRST="$(curl -sS --max-time 10 -X POST -d "secret=$SECRET2&response=$(cat "$WORK/resp-once")" "$RU/siteverify" 2>/dev/null || echo '')"
          SECOND="$(curl -sS --max-time 10 -X POST -d "secret=$SECRET2&response=$(cat "$WORK/resp-once")" "$RU/siteverify" 2>/dev/null || echo '')"
          if ! printf '%s' "$FIRST" | grep -q '"success":true'; then
            fail "首次 siteverify 没过：$(printf '%s' "$FIRST" | tail -c 160)"
          elif printf '%s' "$SECOND" | grep -q '"success":true'; then
            fail "同一个 token 第二次验证仍然 success:true ⇒ 一次性消费没生效"
          elif ! printf '%s' "$SECOND" | grep -q 'timeout-or-duplicate'; then
            fail "重放被拒但不是 reCAPTCHA 族的 timeout-or-duplicate：$(printf '%s' "$SECOND" | tail -c 160)"
          else
            pass "④ 首次验证通过、同 token 重放被拒（timeout-or-duplicate，不透露第几层失败）"
          fi

          # ⑤ 重建容器：站点/凭据/消费状态都必须活下来（消费是权威账本，不是缓存）。
          RC0=0
          $RECIPE_COMPOSE up -d --force-recreate --wait --wait-timeout 90 >"$LOGS/recipe-recreate.log" 2>&1 || RC0=$?
          if [[ $RC0 -ne 0 || "$(code "$RU/readyz")" != "200" ]]; then
            fail "重建容器后没起来或没就绪（rc=$RC0）：$(tail -c 220 "$LOGS/recipe-recreate.log")"
          else
            RC0=0
            EPOCHA_SITE_SECRET="$RSECRET" "$BIN/devclient" --base "$RU" --site-key "$RSITE" --mode full \
              >"$LOGS/recipe-flow2.log" 2>&1 || RC0=$?
            AFTER="$(curl -sS --max-time 10 -X POST -d "secret=$SECRET2&response=$(cat "$WORK/resp-once")" "$RU/siteverify" 2>/dev/null || echo '')"
            if [[ $RC0 -ne 0 ]] || ! grep -q "step=siteverify success=true" "$LOGS/recipe-flow2.log"; then
              fail "重建后同一个站点/凭据完不成闭环（rc=$RC0）：$(tail -c 200 "$LOGS/recipe-flow2.log")"
            elif ! printf '%s' "$AFTER" | grep -q 'timeout-or-duplicate'; then
              fail "重建后已消费的 token 又可验证 ⇒ 防重放账本没活过重启（响应=$(printf '%s' "$AFTER" | tail -c 160)）"
            elif [[ ! -f "$EPR/data/epocha.lock" ]]; then
              fail "重建后数据目录里没有 epocha.lock"
            else
              pass "⑤ 重建容器后：站点仍在用、已消费 token 仍被拒、运行锁仍在同一数据目录（权威状态在卷里，不在容器里）"
            fi
          fi

          # ⑥ 停止后端口必须真释放（不是"容器还在但不响应"）。
          RC0=0
          $RECIPE_COMPOSE stop >"$LOGS/recipe-stop.log" 2>&1 || RC0=$?
          RC1=0
          port_listening "$RECIPE_PORT" || RC1=$?
          if [[ $RC0 -ne 0 ]]; then
            fail "compose stop 失败（rc=$RC0）：$(tail -c 200 "$LOGS/recipe-stop.log")"
          elif [[ "$(code "$RU/readyz" 3)" != "000" || $RC1 -ne 1 ]]; then
            fail "停止后端口 $RECIPE_PORT 仍可应答或仍在监听（http=$(code "$RU/readyz" 3) ss_rc=$RC1）"
          else
            pass "⑥ 停止后端口释放（HTTP 连不上且 ss 查不到监听）；SIGTERM 走优雅关闭"
          fi

          # ⑦ 定点清理：只删本步自己创建的那一套（项目名 + 网络 + 隔离目录）。
          RC0=0
          $RECIPE_COMPOSE down -v --remove-orphans >"$LOGS/recipe-down.log" 2>&1 || RC0=$?
          if [[ $RC0 -ne 0 ]]; then
            fail "清理配方项目失败（rc=$RC0）：$(tail -c 200 "$LOGS/recipe-down.log")"
          elif docker network inspect epocha5b-recipe >/dev/null 2>&1; then
            fail "配方网络 epocha5b-recipe 还在——down -v 没清干净"
          elif [[ -n "$($RECIPE_COMPOSE ps -q 2>/dev/null)" ]]; then
            fail "配方项目下仍有容器"
          elif [[ -n "${CID:-}" ]] && [[ "$(code "http://127.0.0.1:$PUBLIC_PORT/readyz" 3)" != "200" ]]; then
            fail "配方项目的定点清理波及了第 2 步那套（$PUBLIC_PORT 不再就绪）——down 只该作用于 -p epocha5brecipe"
          else
            OTHER="（第 2 步那套本轮未起，无从波及）"
            [[ -n "${CID:-}" ]] && OTHER="（第 2 步那套仍在 $PUBLIC_PORT 就绪）"
            pass "⑦ 本步资源已定点清理（容器/网络/卷都没剩）；$OTHER"
          fi
        fi
      fi
    fi
  fi
fi

# --- 第 3 步：候选构建清单（受支持范围＝B 路径）---
log "第 3 步：部署候选清单（必需＝B 路径；A 路径与未验证平台归为可选项）"
# The receipt was written by the actual build. Verification never invokes make or rebuilds the binaries.
if [[ "$BUILD_OK" -eq 1 ]]; then
  rc=0
  node docs/scripts/build-receipt.mjs verify --out "$PREBUILT_DIR" --go "$GO" >"$LOGS/manifest.txt" 2>&1 || rc=$?
  if [[ $rc -ne 0 ]]; then
    fail "构建收据、原始字节、实际镜像版本或静态 ELF 复核不一致；见 $LOGS/manifest.txt"
  elif [[ -z "${BK:-}" || ! -f "$D/backups/$BK.manifest.json" ]]; then
    fail "当次备份 manifest 缺失；不能把 schema/app/commit 对账记成通过"
  else
    rc=0
    node docs/scripts/build-receipt.mjs candidate --root "$ROOT" --out "$PREBUILT_DIR" --go "$GO" \
      --backup-manifest "$D/backups/$BK.manifest.json" --uid "$RUN_UID" --gid "$RUN_GID" >>"$LOGS/manifest.txt" 2>&1 || rc=$?
    if [[ $rc -ne 0 ]]; then
      fail "候选身份复核失败：需要构建时/验收后同一干净源码、linux/arm64 与实测 UID:GID 1000:1000；备份 app_version/完整 commit 任一不一致也 FAIL（$LOGS/manifest.txt）"
    else
      pass "当次候选身份完全对账；原始 server/ops、镜像拷出字节、实际命令与版本、schema 已封存于 $PREBUILT_DIR"
    fi
  fi
else
  req_notrun "候选清单的二进制与镜像部分" "未完成本轮真实构建；不读取旧 .prebuilt/out 或同名标签"
fi

# --- 可选项：第一版正式支持范围之外的构建路径与平台（只如实登记，不决定退出码）---
opt_notrun "A 路径（make docker-image，容器内 go build）" \
  "未纳入第一版正式支持；执行需要 registry 可达 + ≥2GB 空闲磁盘（本轮起始 $(( $(avail_kb) / 1024 ))MB）。复现命令：make docker-image && bash docs/scripts/stage5b.sh"
opt_notrun "amd64 交叉构建的镜像" "本机 linux/arm64 且无 amd64 验证环境；不引入交叉构建体系（5A 的同一决定）"
opt_blocked "UID/GID 65534（镜像默认 USER）下的数据目录属主与 chown 动作" "需要 root：按 DEPLOYMENT §2/§5.5 执行后复测第 2 步"
scope_note "题型的人工可读性盲测、抗 OCR/抗 AI 基线、生产默认档位" \
  "属产品效果验收，分别由 make blindtest 与 make ocr-baseline-real 判定；容器化与恢复演练通过**不**构成它们的结论"
if [[ $BUILD_OK -eq 1 ]]; then
  pass "B 路径本轮实际构建并验证通过（原始字节与镜像拷出字节相等、PT_INTERP 不存在、实际版本元组一致）"
else
  req_notrun "B 路径（受支持的构建路径）与镜像装配" "本轮构建未完成 ⇒ 这是必需项，不是可选项"
fi
if [[ -f "$LOGS/manifest.txt" ]]; then
  while IFS= read -r line; do printf '[stage5b]   清单: %s\n' "$line"; done <"$LOGS/manifest.txt"
fi

# --- 第 4 步：约 5 分钟的节流持续演练 ---
log "第 4 步：持续演练（1 CPU / 128MiB，节流约 1.5 闭环/秒，5 分钟硬上限）"
# 磁盘余量在这里**再量一次**：前面几步已经用过一轮空间，而"5 分钟窗口跑到一半 ENOSPC"
# 比"根本没开始"更难归因。不足 ⇒ BLOCKED（必需项）⇒ 退出码 3 ⇒ 不打完成标签。
AVAIL_NOW="$(avail_kb)"
if [[ $BUILD_OK -eq 1 && -n "${CID:-}" ]] && (( AVAIL_NOW >= 100 * 1024 )); then
  "$GO" build -trimpath -o "$BIN/loadtest" ./cmd/loadtest
  "$GO" build -trimpath -o "$BIN/devclient" ./cmd/devclient
  compose down >/dev/null 2>&1 || true
  CID=""
  # 测量实例：资源限额与生产一致，只把**全局限流**抬高（默认 60 IP-RPM 会先于容量起作用）。
  # 这份抬高**不写进** compose 文件——部署配置里不许混入测试高限流。
  rc=0
  docker run -d --name epocha5b-measure \
    --user "$RUN_UID:$RUN_GID" --read-only \
    --cap-drop ALL --security-opt no-new-privileges \
    --memory 128m --memory-swap 128m --cpus 1 --pids-limit 128 \
    -v "$D/data:/var/lib/epocha" -v "$D/keys:/etc/epocha/keys:ro" -v "$D/backups:/var/backups/epocha" \
    -p "127.0.0.1:$MEASURE_PORT:8080" \
    -e EPOCHA_LISTEN=0.0.0.0:8080 -e EPOCHA_DB=/var/lib/epocha/epocha.db \
    -e EPOCHA_MASTER_KEY_FILE=/etc/epocha/keys/master -e EPOCHA_PEPPER_FILE=/etc/epocha/keys/pepper \
    -e EPOCHA_ADMIN_KEY_FILE=/etc/epocha/keys/admin \
    -e EPOCHA_RATE_IP_RPM=6000 -e EPOCHA_RATE_SITE_RPM=6000 \
    -e GOMEMLIMIT=56MiB -e GOMAXPROCS=1 \
    "$IMAGE" >/dev/null 2>&1 || rc=$?
  MEAS="epocha5b-measure"
  MU="http://127.0.0.1:$MEASURE_PORT"
  if [[ $rc -ne 0 ]] || ! wait_status "$MU/readyz" 200 40; then
    fail "测量实例起不来或没就绪：$(docker logs "$MEAS" 2>&1 | tail -c 240)"
  else
    pass "测量实例就绪（1 CPU / 128MiB / 只读根 / 非 root；限流抬高仅用于测量）"

    OPS='/usr/local/bin/epocha-ops'
    admin_in() { # $1=METHOD $2=PATH $3=OUT $4=BODY
      local rc=0
      if [[ -n "${4:-}" ]]; then
        printf '%s' "$4" | docker exec -i "$MEAS" "$OPS" admin "$1" "$2" >"$3.raw" 2>"$3.err" || rc=$?
      else
        docker exec -i "$MEAS" "$OPS" admin "$1" "$2" </dev/null >"$3.raw" 2>"$3.err" || rc=$?
      fi
      [[ $rc -eq 0 ]] || return $rc
      sed -n '2,$p' "$3.raw" >"$3"
    }
    SOAK_POW=""
    SOAK_SECRET=""
    # 站点级配额在这里显式抬高，理由与 ADR-016 有关：窗口修好之后 rate_ip_rpm=60 真的是"每分钟 60 次"，
    # 450 次闭环（约 1.5/秒）会在中途开始吃 429 —— 那测的又是限流器而不是稳态资源（第一次重跑就撞上了）。
    # 抬高只作用于这个测量站点，**不写进 compose**（部署配置里不留测试高限流）。
    rc=0
    admin_in POST /api/admin/sites "$LOGS/site-soak.json" '{"name":"stage5b-soak","verify_mode":"pow_only","allow_missing_origin":true,"pow_bits":12,"challenge_ttl_sec":180,"response_ttl_sec":120,"rate_ip_rpm":6000,"rate_site_rpm":6000}' || rc=$?
    if [[ $rc -eq 0 ]]; then
      SOAK_POW="$(jq -r '.site.site_key // ""' "$LOGS/site-soak.json")"
      SOAK_SECRET="$(jq -r '.secret // ""' "$LOGS/site-soak.json")"
    fi
    # 图片题站点：让后台补池在整个窗口里真的有活可干。
    rc=0
    admin_in POST /api/admin/sites "$LOGS/site-img.json" \
      '{"name":"stage5b-img","verify_mode":"pow_question","generator_id":"text-image-v1","difficulty":5,"allow_missing_origin":true}' || rc=$?
    IMAGE_SITE_OK=0
    if node docs/scripts/soak-evidence.mjs image-site --rc "$rc" --file "$LOGS/site-img.json"; then IMAGE_SITE_OK=1; fi
    # 短 TTL 站点 + 只领题不作答：这是"过期清理是否真的触发"的唯一可观察形状。
    rc=0
    admin_in POST /api/admin/sites "$LOGS/site-exp.json" '{"name":"stage5b-expire","verify_mode":"pow_only","allow_missing_origin":true,"pow_bits":12,"challenge_ttl_sec":5,"rate_ip_rpm":600,"rate_site_rpm":600}' || rc=$?
    EXP_KEY="$(jq -r '.site.site_key // ""' "$LOGS/site-exp.json" 2>/dev/null || echo "")"
    if [[ -z "$SOAK_POW" || -z "$SOAK_SECRET" || -z "$EXP_KEY" || $IMAGE_SITE_OK -ne 1 || $rc -ne 0 ]]; then
      fail "持续演练需要的三个真实站点没建齐（pow=$SOAK_POW exp=$EXP_KEY image_ok=$IMAGE_SITE_OK）"
    else
      pass "三个演练站点就绪：pow_only 流量站、实际 text-image 补池站（高档 5）、challenge_ttl=5s 过期站"
      POOL_READY=0
      POOL_DEADLINE=$((SECONDS + 45))
      while [[ $SECONDS -lt $POOL_DEADLINE ]]; do
        if admin_in GET /api/admin/pool "$LOGS/pool-before.json" && \
           node docs/scripts/soak-evidence.mjs pool --file "$LOGS/pool-before.json" >/dev/null 2>&1; then
          POOL_READY=1; break
        fi
        sleep 1
      done
      if [[ $POOL_READY -ne 1 ]]; then
        fail "45s 内没有实际高档图片 ready>0 且 ready_asset_bytes/global asset_bytes>0；不把空池或别的桶计入"
      fi
      READY0="$(jq -r '[.buckets[]?|.ready]|add//0' "$LOGS/pool-before.json")"

      # 过期样本：领 20 道题后放着不管。
      for _ in $(seq 1 20); do
        curl -sS -o /dev/null --max-time 8 -X POST -H 'Content-Type: application/json' \
          -d "{\"site_key\":\"$EXP_KEY\"}" "$MU/api/challenge" 2>/dev/null || true
      done

      DB0="$(stat -c '%s' "$D/data/epocha.db" 2>/dev/null || echo 0)"
      WAL0="$(stat -c '%s' "$D/data/epocha.db-wal" 2>/dev/null || echo 0)"
      SOAK_START=$SECONDS
      # The Node watchdog owns the actual child and enforces 330s independent of HTTP/PoW timeouts.
      EPOCHA_SITE_SECRET="$SOAK_SECRET" node docs/scripts/soak-evidence.mjs run --out "$LOGS/soak-run.json" --log "$LOGS/soak.txt" -- \
        "$BIN/loadtest" --base "$MU" --site-key "$SOAK_POW" --concurrency 1 --requests 450 --pace 660ms --timeout 10s &
      SOAK_PID=$!
      SAMPLES="$LOGS/soak-samples.txt"
      : >"$SAMPLES"
      SAMPLE_PID=""
      SAMPLE_STARTED=-100
      SAMPLE_FAILED=0
      while kill -0 "$SOAK_PID" 2>/dev/null; do
        NOW=$((SECONDS - SOAK_START))
        if [[ -n "$SAMPLE_PID" ]] && ! kill -0 "$SAMPLE_PID" 2>/dev/null; then
          wait "$SAMPLE_PID" || SAMPLE_FAILED=1
          SAMPLE_PID=""
        fi
        if [[ -z "$SAMPLE_PID" ]] && (( NOW - SAMPLE_STARTED >= 30 )); then
          SAMPLE_STARTED=$NOW
          node docs/scripts/soak-evidence.mjs sample --container "$MEAS" --base "$MU" --db "$D/data/epocha.db" --elapsed "$NOW" \
            >>"$SAMPLES" 2>>"$LOGS/soak-sample-errors.log" &
          SAMPLE_PID=$!
        fi
        sleep 1
      done
      SOAK_RC=0
      wait "$SOAK_PID" || SOAK_RC=$?
      SOAK_PID=""
      if [[ -n "$SAMPLE_PID" ]]; then
        wait "$SAMPLE_PID" || SAMPLE_FAILED=1
        SAMPLE_PID=""
      fi
      SOAK_T=$((SECONDS - SOAK_START))
      POOL_AFTER_OK=0
      if admin_in GET /api/admin/pool "$LOGS/pool-after.json" && \
         node docs/scripts/soak-evidence.mjs pool --file "$LOGS/pool-after.json" >/dev/null; then POOL_AFTER_OK=1; fi
      VERIFY_RC=0
      node docs/scripts/soak-evidence.mjs verify --run "$LOGS/soak-run.json" --log "$LOGS/soak.txt" --samples "$SAMPLES" \
        --pool-before "$LOGS/pool-before.json" --pool-after "$LOGS/pool-after.json" --out "$LOGS/soak-summary.json" || VERIFY_RC=$?
      if [[ $SOAK_RC -eq 0 && $SAMPLE_FAILED -eq 0 && $POOL_READY -eq 1 && $POOL_AFTER_OK -eq 1 && $VERIFY_RC -eq 0 ]]; then
        pass "持续演练实际 450/450 闭环成功、无失败、330s 看门狗内结束；高档图片 ready/字节与连续可解析资源采样均有实证（${SOAK_T}s）"
      else
        fail "持续演练不满足完整证据（load_rc=$SOAK_RC sample_failed=$SAMPLE_FAILED pool_before=$POOL_READY pool_after=$POOL_AFTER_OK verify_rc=$VERIFY_RC）；缺失或未知数据不能视为通过"
      fi

      # 过期清理有没有真的触发（不是"我们有个 purge 循环"，是日志里有没有非零计数）
      docker logs "$MEAS" 2>&1 | grep "清理" >"$LOGS/purge.log" || true
      if grep -qE "challenges=[1-9]" "$LOGS/purge.log"; then
        pass "过期清理实际触发过：$(tail -n 1 "$LOGS/purge.log")"
      else
        fail "5 分钟窗口内没有观察到任何非零的过期清理计数（挑战 ttl=5s 的 20 道题应被清掉）"
      fi

      POOL1="$(docker exec -i "$MEAS" "$OPS" admin GET /api/admin/pool </dev/null 2>/dev/null | sed -n '2,$p')"
      printf '%s\n' "$POOL1" >"$LOGS/pool-after.json"
      node docs/scripts/soak-evidence.mjs pool --file "$LOGS/pool-after.json" >/dev/null 2>&1 || true
      READY1="$(printf '%s' "$POOL1" | jq -r '[.buckets[]?|.ready]|add//0')"
      IMGB="$(printf '%s' "$POOL1" | jq -r '.image.asset_bytes // 0')"
      DB1="$(stat -c '%s' "$D/data/epocha.db" 2>/dev/null || echo 0)"
      WAL1="$(stat -c '%s' "$D/data/epocha.db-wal" 2>/dev/null || echo 0)"
      log "  题池 ready $READY0→$READY1，图片字节 $IMGB；库 $DB0→$DB1，WAL $WAL0→$WAL1"
      OOMS="$(docker inspect -f '{{.State.OOMKilled}}/{{.RestartCount}}' "$MEAS" 2>/dev/null || echo '?/?')"
      if [[ "$OOMS" == "false/0" ]]; then
        pass "窗口内无 OOM、无重启（OOMKilled/RestartCount=$OOMS）"
      else
        fail "出现 OOM 或重启（$OOMS）"
      fi
      MEMFINAL="$(docker stats --no-stream --format '{{.MemUsage}}' "$MEAS" 2>/dev/null || echo '?')"
      log "  结束时容器内存：$MEMFINAL"
      rc=0
      ss -ltnp >"$LOGS/ss-after.txt" 2>/dev/null || rc=$?
      if [[ $rc -ne 0 ]]; then
        fail "ss 取不到监听表（rc=$rc），无法判定端口边界"
      elif [[ "$(code "http://127.0.0.1:$PUBLIC_PORT/readyz" 3)" == "000" ]] && \
           ! grep -q ":$PUBLIC_PORT " "$LOGS/ss-after.txt"; then
        pass "测量期间宿主上没有多出意外监听（编排服务已停，只有 $MEASURE_PORT）"
      else
        fail "宿主上出现了预期之外的服务监听（$PUBLIC_PORT 仍在监听）：$(grep ":$PUBLIC_PORT " "$LOGS/ss-after.txt" | head -2 | tr '\n' ' ')"
      fi
    fi
    docker rm -f "$MEAS" >/dev/null 2>&1 || true
  fi
else
  blocked "持续演练：磁盘余量 $((AVAIL_NOW / 1024))MB（下限 100MB）或第 2 步的服务容器不可用" \
    "释放磁盘后单独跑：bash docs/scripts/stage5b.sh"
fi

# --- 汇总 ---
log "汇总（结论只由必需项决定；可选项与超范围项如实列出）"
printf '[stage5b] 必需项(B 路径部署工程) PASS=%d FAIL=%d BLOCKED=%d NOT_RUN=%d\n' \
  "$R_PASS" "$R_FAIL" "$R_BLOCKED" "$R_NOTRUN"
printf '[stage5b] 可选项(A 路径/未验证平台) PASS=%d FAIL=%d BLOCKED=%d NOT_RUN=%d\n' \
  "$O_PASS" "$O_FAIL" "$O_BLOCKED" "$O_NOTRUN"
printf '[stage5b] 超范围项(产品效果，本门禁不判定) 登记=%d\n' "$S_NOTE"
if [[ -f "$LOGS/manifest.txt" ]]; then
  printf '[stage5b] 候选清单：deploy/docker/.prebuilt/candidate-manifest.txt（忽略目录里的构建产物，不入库以免自引用）\n'
fi
RC=0
verdict_rc || RC=$?
case $RC in
  1) printf '[stage5b] RESULT=fail（必需项 %d 条工程失败）\n' "$R_FAIL" ;;
  3) printf '[stage5b] RESULT=blocked（必需项无失败，但 BLOCKED=%d NOT_RUN=%d ⇒ 不打完成标签）\n' \
    "$R_BLOCKED" "$R_NOTRUN" ;;
  *) printf '[stage5b] RESULT=pass（必需项 %d 条全部通过；可选项 NOT_RUN=%d 不阻塞交付，也未被写成通过）\n' \
    "$R_PASS" "$O_NOTRUN" ;;
esac
# 直接跑脚本时这就是**真实退出码**；经 make 时会被折叠成 2（配方会把这里的数字打印出来）。
printf '[stage5b] SCRIPT_RC=%d\n' "$RC"
exit "$RC"
