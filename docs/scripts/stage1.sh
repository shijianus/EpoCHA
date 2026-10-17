#!/usr/bin/env bash
# stage1.sh — 阶段 1 端到端验收（DESIGN §17 阶段 1）。
#
# 阶段 2 之后本脚本仍然有效，只有两处随契约更新（都不降低验收力度）：
#   1. 站点配置显式写上 "allow_missing_origin": true —— 阶段 2 的默认策略是拒绝无 Origin 的
#      直接调用，devclient 不是浏览器，所以放行必须是配置里的明确决定；
#   2. 启动日志的站点就绪标记改为阶段 2 的实际文案，并给管理监听器也分配随机端口，
#      避免与本机 8081 上的别的进程相撞。
#
# 一个命令完成：随机生成演示密钥 → 启动临时服务 → 领 challenge → 真实暴力求解 PoW →
# redeem → siteverify，并明确演示"第二次兑换失败""第二次验证失败""重启后重放失败"。
# 没有任何写死的解，也没有绕过验证的分支；密钥全部来自运行时随机数，临时目录用完即删。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
export GOTOOLCHAIN=local

WORK="$(mktemp -d)"
SERVER_PID=""
STEP=0

log()  { printf '[stage1] %s\n' "$*"; }
pass() { STEP=$((STEP+1)); printf '[stage1] PASS %d: %s\n' "$STEP" "$*"; }
fail() {
  printf '[stage1] FAIL: %s\n' "$*" >&2
  if [[ -f "${SERVER_LOG:-}" ]]; then
    printf '[stage1] ---- 服务日志尾部 ----\n' >&2
    tail -n 30 "$SERVER_LOG" >&2 || true
  fi
  exit 1
}

cleanup() {
  local rc=$?
  set +e
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill -TERM "$SERVER_PID" 2>/dev/null
    for _ in $(seq 1 50); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.1; done
    kill -KILL "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$WORK"
  if [[ $rc -ne 0 ]]; then printf '[stage1] 已清理临时目录并停止服务（退出码 %d）\n' "$rc" >&2; fi
  exit $rc
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

DEV="$WORK/devclient"
SERVER="$WORK/epocha-server"

log "构建 server 与 devclient"
go build -trimpath -o "$SERVER" ./cmd/server
go build -trimpath -o "$DEV" ./cmd/devclient

log "生成运行时演示密钥（不落仓库、退出即删）"
MASTER_KEY="$(head -c32 /dev/urandom | base64 | tr -d '\n')"
PEPPER="$(head -c32 /dev/urandom | base64 | tr -d '\n')"
SITE_SECRET="$(head -c24 /dev/urandom | base64 | tr -d '\n')"
SITE_KEY="sk-e2e-$(head -c6 /dev/urandom | od -An -tx1 | tr -d ' \n')"
printf '[{"name":"e2e","site_key":"%s","secret":"%s","pow_bits":12,"allow_missing_origin":true}]\n' \
  "$SITE_KEY" "$SITE_SECRET" > "$WORK/sites.json"
chmod 600 "$WORK/sites.json"

DB_PATH="$WORK/epocha.db"
SERVER_LOG="$WORK/server.log"
PORT=""
BASE=""

# 端口由内核分配（--listen 127.0.0.1:0），再从日志里读回。
# 同时以"本轮随机 site_key 的就绪日志"作为身份凭据：若上一轮遗留的进程还占着某个端口，
# 健康检查会被别人的服务答上，那种假通过必须避免（本专案踩过这个坑）。
start_server() {
  : > "$SERVER_LOG"
  EPOCHA_MASTER_KEY="$MASTER_KEY" EPOCHA_PEPPER="$PEPPER" \
    "$SERVER" --sites "$WORK/sites.json" --db "$DB_PATH" \
    --listen "127.0.0.1:0" --admin-listen "127.0.0.1:0" \
    >>"$SERVER_LOG" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 150); do
    if ! kill -0 "$SERVER_PID" 2>/dev/null; then fail "服务进程启动后退出：$(tail -n 3 "$SERVER_LOG")"; fi
    if grep -q "site_key=${SITE_KEY}" "$SERVER_LOG" \
       && grep -q "msg=已监听" "$SERVER_LOG"; then
      # 只取公共监听器的端口（阶段 2 起日志里有两个 addr= 行）
      PORT="$(sed -n 's/.*msg=已监听 addr=127\.0\.0\.1:\([0-9]*\).*/\1/p' "$SERVER_LOG" | head -n1)"
      [[ -n "$PORT" ]] || fail "无法从服务日志解析监听端口"
      BASE="http://127.0.0.1:${PORT}"
      for _ in $(seq 1 50); do
        curl -fsS -m 1 "$BASE/healthz" >/dev/null 2>&1 && return 0
        sleep 0.1
      done
      fail "端口 $PORT 上健康检查不通（可能不是本次服务）"
    fi
    sleep 0.1
  done
  fail "服务在 15s 内未完成启动：$(tail -n 5 "$SERVER_LOG")"
}

stop_server() {
  if kill -0 "$SERVER_PID" 2>/dev/null; then
    kill -TERM "$SERVER_PID"
    for _ in $(seq 1 60); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.1; done
    kill -0 "$SERVER_PID" 2>/dev/null && fail "优雅关闭超时（5s 后仍在运行）"
  fi
  SERVER_PID=""
}

run_client() { # mode [extra args...]
  local mode="$1"; shift
  EPOCHA_SITE_SECRET="$SITE_SECRET" "$DEV" --base "$BASE" --site-key "$SITE_KEY" --mode "$mode" "$@"
}

log "启动临时服务（--listen 127.0.0.1:0，端口由内核分配，从日志识别本次服务）"
start_server
log "本次服务地址 $BASE"
pass "服务启动且 /healthz 可用"

curl -fsS "$BASE/healthz" | grep -q '"status":"ok"' || fail "健康检查响应不符"

# 未鉴权的 site_key 必须被拒绝，且不能创建 challenge
bad_status="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/challenge" \
  -H 'Content-Type: application/json' --data '{"site_key":"sk-does-not-exist"}')"
[[ "$bad_status" == "400" ]] || fail "未知 site_key 应返回 400，实际 $bad_status"
[[ "$(curl -s -X POST "$BASE/api/challenge" -H 'Content-Type: application/json' --data '{}' )" == *invalid-request* ]] \
  || fail "空请求体应返回 invalid-request"
pass "未鉴权/畸形请求被拒绝"

run_client full > "$WORK/out-full.txt" || fail "完整流程失败：$(cat "$WORK/out-full.txt")"
grep -q 'step=siteverify success=true' "$WORK/out-full.txt" || fail "siteverify 未成功"
grep -q 'iterations=[1-9]' "$WORK/out-full.txt" || fail "PoW 未真实求解（iterations=0）"
pass "challenge → 真实求解 PoW → redeem → siteverify 全部成功"

run_client double-redeem > "$WORK/out-dr.txt" || fail "double-redeem 失败：$(cat "$WORK/out-dr.txt")"
grep -q 'step=redeem-1 status=ok' "$WORK/out-dr.txt" || fail "首次 redeem 应成功"
grep -q 'step=redeem-2 status=4.*rejected=1' "$WORK/out-dr.txt" || fail "第二次 redeem 未失败"
pass "同一 challenge 第二次兑换被拒绝"

run_client double-verify > "$WORK/out-dv.txt" || fail "double-verify 失败：$(cat "$WORK/out-dv.txt")"
grep -q 'step=siteverify-2 success=false codes=timeout-or-duplicate' "$WORK/out-dv.txt" || fail "二次验证应为 timeout-or-duplicate"
pass "同一 response token 第二次验证被拒绝"

run_client wrong-secret > "$WORK/out-ws.txt" || fail "wrong-secret 失败：$(cat "$WORK/out-ws.txt")"
grep -q 'step=siteverify-badsecret success=false codes=invalid-input-secret' "$WORK/out-ws.txt" || fail "错误 secret 应返回 invalid-input-secret"
grep -q 'step=siteverify-goodsecret success=true not-consumed=1' "$WORK/out-ws.txt" || fail "错误 secret 竟消费了合法 token"
pass "错误 secret 不消费合法 token"

# 篡改 PoW 参数：直接改 token 里的 payload 段必然验签失败
tampered="$(curl -s -X POST "$BASE/api/challenge" -H 'Content-Type: application/json' --data "{\"site_key\":\"$SITE_KEY\"}" | python3 -c '
import sys, json
d = json.load(sys.stdin)
t = d["token"].split(".")
t[2] = t[2][:-4] + "AAAA"
print(".".join(t))')"
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/redeem" -H 'Content-Type: application/json' \
  --data "{\"token\":\"$tampered\",\"pow_nonce\":\"1\"}")"
[[ "$code" == "400" ]] || fail "篡改 token 后 redeem 应 400，实际 $code"
pass "篡改签名 token 被拒绝"

# 按值扫描：服务日志里不得出现本轮生成的任何密钥材料
for secret_value in "$SITE_SECRET" "$MASTER_KEY" "$PEPPER"; do
  if grep -qF -- "$secret_value" "$SERVER_LOG"; then
    fail "服务日志泄露了密钥材料的值"
  fi
done
if grep -qE -- '"token"|"response"|"secret"' "$SERVER_LOG"; then
  fail "服务日志出现 token/secret 字段名（提示记录结构越界）"
fi
pass "服务日志不含本轮密钥值（按值扫描）"

# 重启后重放：token 必须在旧进程里被真正消费过一次，重启后再验才叫"重放"。
# （若只走到 redeem 就重启，验证成功是正确行为，检查会失去意义。）
run_client acquire --response-file "$WORK/resp.txt" > "$WORK/out-acq.txt" || fail "acquire 失败"
grep -q 'step=acquire status=ok consumed=1' "$WORK/out-acq.txt" || fail "acquire 未在旧进程内消费 token"
[[ -s "$WORK/resp.txt" ]] || fail "未写出 response token"
BEFORE_RESPONSE="$(cat "$WORK/resp.txt")"
stop_server
log "重启服务（同一数据库文件）"
start_server
# devclient verify-only 在"重放被拒绝"时退出码为 0（那才是期望结果）；非零表示服务行为或工具本身异常。
if ! run_client verify-only --response-file "$WORK/resp.txt" > "$WORK/out-replay.txt" 2>"$WORK/out-replay.err"; then
  fail "重启后重放检查异常退出：$(cat "$WORK/out-replay.txt" "$WORK/out-replay.err")"
fi
grep -q 'step=siteverify-replay success=false codes=timeout-or-duplicate' "$WORK/out-replay.txt" \
  || fail "重启后重放未被拒绝（防重放未持久化）：$(cat "$WORK/out-replay.txt")"
[[ "$(cat "$WORK/resp.txt")" == "$BEFORE_RESPONSE" ]] || fail "response 文件内容意外变化"
pass "进程重启后旧 response 不可重放（状态在 SQLite）"

# 重启后新流程仍可用（证明不是"全部拒绝"式的假安全）
run_client full > "$WORK/out-after-restart.txt" || fail "重启后完整流程失败"
grep -q 'step=siteverify success=true' "$WORK/out-after-restart.txt" || fail "重启后新验证未通过"
pass "重启后新 challenge 流程照常可用"

log "全部 ${STEP} 项验收通过"
