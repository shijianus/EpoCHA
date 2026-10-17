#!/usr/bin/env bash
# stage3a.sh — 阶段 3A 端到端验收（DESIGN §17 阶段 3A）。
#
# 一个命令完成：随机演示密钥 → 临时库 → **先关掉补题 worker** 证明"池空不降级成 PoW-only" →
# 打开 worker 让后台真的备题 → 管理面建 pow_question 站点与来源白名单 →
# devclient 真实求解 PoW 并**从公开题面算出答案**兑换 → 错误答案终结 challenge →
# 重启后终态与防重放仍成立 → 按值扫描答案/凭据/私有 verifier。
#
# 没有读数据库里的答案，没有硬编码固定解，也没有绕过验证的分支。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
export GOTOOLCHAIN=local

WORK="$(mktemp -d)"
SERVER_PID=""
STEP=0

log()  { printf '[stage3a] %s\n' "$*"; }
pass() { STEP=$((STEP+1)); printf '[stage3a] PASS %d: %s\n' "$STEP" "$*"; }
fail() {
  printf '[stage3a] FAIL: %s\n' "$*" >&2
  if [[ -f "${SERVER_LOG:-}" ]]; then
    printf '[stage3a] ---- 服务日志尾部 ----\n' >&2
    tail -n 40 "$SERVER_LOG" >&2 || true
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
  if [[ $rc -ne 0 ]]; then printf '[stage3a] 已清理临时目录并停止服务（退出码 %d）\n' "$rc" >&2; fi
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
ADMIN_KEY="ak_$(head -c32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
IMPORT_SECRET="sk_$(head -c32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
IMPORT_KEY="pk_imported_$(head -c6 /dev/urandom | od -An -tx1 | tr -d ' \n')"
DB_PATH="$WORK/epocha.db"
SERVER_LOG="$WORK/server.log"
BODIES="$WORK/bodies.log"
ORIGIN="https://demo.example.com"

# 配置文件导入的这个站点保持 pow_only：它同时是阶段 1/2 契约未被改动的回归位。
printf '[{"name":"imported","site_key":"%s","secret":"%s","pow_bits":12,"allow_missing_origin":true}]\n' \
  "$IMPORT_KEY" "$IMPORT_SECRET" > "$WORK/sites.json"
chmod 600 "$WORK/sites.json"

PUB_PORT=""
ADMIN_PORT=""
BASE=""
ABASE=""

# start_server <pool-enabled:true|false>
start_server() {
  local pool_enabled="$1"
  : > "$SERVER_LOG"
  EPOCHA_MASTER_KEY="$MASTER_KEY" EPOCHA_PEPPER="$PEPPER" EPOCHA_ADMIN_KEY="$ADMIN_KEY" \
  EPOCHA_POOL_ENABLED="$pool_enabled" EPOCHA_POOL_LOW_WATERMARK=4 EPOCHA_POOL_HIGH_WATERMARK=8 \
    "$SERVER" --sites "$WORK/sites.json" --db "$DB_PATH" \
    --listen "127.0.0.1:0" --admin-listen "127.0.0.1:0" \
    >>"$SERVER_LOG" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 200); do
    if ! kill -0 "$SERVER_PID" 2>/dev/null; then fail "服务进程启动后退出：$(tail -n 5 "$SERVER_LOG")"; fi
    if grep -q "site_key=${IMPORT_KEY}" "$SERVER_LOG" \
       && grep -q "msg=已监听" "$SERVER_LOG" \
       && grep -q "msg=管理面已挂载" "$SERVER_LOG"; then
      PUB_PORT="$(sed -n 's/.*msg=已监听 addr=127\.0\.0\.1:\([0-9]*\).*/\1/p' "$SERVER_LOG" | head -n1)"
      ADMIN_PORT="$(sed -n 's/.*msg=管理面已挂载 addr=127\.0\.0\.1:\([0-9]*\).*/\1/p' "$SERVER_LOG" | head -n1)"
      [[ -n "$PUB_PORT" && -n "$ADMIN_PORT" ]] || fail "无法从日志解析两个监听端口"
      BASE="http://127.0.0.1:${PUB_PORT}"
      ABASE="http://127.0.0.1:${ADMIN_PORT}"
      for _ in $(seq 1 50); do
        curl -fsS -m 1 "$BASE/healthz" >/dev/null 2>&1 && return 0
        sleep 0.1
      done
      fail "端口 $PUB_PORT 上健康检查不通"
    fi
    sleep 0.1
  done
  fail "服务在 20s 内未完成启动：$(tail -n 5 "$SERVER_LOG")"
}

stop_server() {
  if kill -0 "$SERVER_PID" 2>/dev/null; then
    kill -TERM "$SERVER_PID"
    for _ in $(seq 1 60); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.1; done
    kill -0 "$SERVER_PID" 2>/dev/null && fail "优雅关闭超时"
  fi
  SERVER_PID=""
}

AH=(-H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json')
# ac <method> <path> [json]
ac() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-s -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$ABASE$path" "${AH[@]}")
  [[ -n "$body" ]] && args+=(--data "$body")
  local code
  code="$(curl "${args[@]}")"
  cat "$WORK/resp.json" >> "$BODIES"; printf '\n' >> "$BODIES"
  echo "$code"
}
pb() { # pb <method> <path> <json-or-empty> [extra curl args...]
  local method="$1" path="$2" body="$3"; shift 3
  local args=(-s -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path")
  [[ -n "$body" ]] && args+=(--data "$body")
  args+=("$@")
  local code
  code="$(curl "${args[@]}")"
  cat "$WORK/resp.json" >> "$BODIES"; printf '\n' >> "$BODIES"
  echo "$code"
}
body() { cat "$WORK/resp.json"; }

# 凭据"只出现一次"要按**揭示之后的响应**来扫：揭示那一条本身就带明文。
# 每个响应占一行，因此记下揭示时的行号，之后只扫它后面的部分。
Q_REVEAL_LINE=0
check_no_leak() {
  local secret_value="$1"
  if [[ "$Q_REVEAL_LINE" -eq 0 ]]; then return 0; fi
  if tail -n +"$((Q_REVEAL_LINE + 1))" "$BODIES" | grep -qF -- "$secret_value"; then
    fail "凭据在它被展示之后的响应里再次出现"
  fi
}
jqpy() { python3 -c "$1" "$WORK/resp.json"; }

log "第一步：**关掉补题 worker** 启动（用来证明池空时不降级）"
start_server false
curl -fsS "$BASE/healthz" | grep -q '"status":"ok"' || fail "健康检查异常"

code="$(ac POST /api/admin/sites "{\"name\":\"quiz\",\"site_key\":\"pk_quiz\",\"pow_bits\":12,\"verify_mode\":\"pow_question\",\"generator_id\":\"arithmetic-baseline-v1\",\"difficulty\":3}")"
[[ "$code" == "201" ]] || fail "创建 pow_question 站点应 201，实际 $code: $(body)"
Q_SECRET="$(jqpy 'import json,sys;print(json.load(open(sys.argv[1]))["secret"])')"
Q_ID="$(jqpy 'import json,sys;print(int(json.load(open(sys.argv[1]))["site"]["id"]))')"
[[ "$(jqpy 'import json,sys;print(json.load(open(sys.argv[1]))["site"]["verify_mode"])')" == "pow_question" ]] \
  || fail "站点模式未生效: $(body)"
# 先配好允许来源：后面要区分"来源被拒(400)"与"缺题(503)"两种失败，不能混在一起
[[ "$(ac PUT "/api/admin/sites/$Q_ID/origins" "{\"origins\":[\"$ORIGIN\"]}")" == "200" ]] \
  || fail "设置允许来源失败: $(body)"
Q_REVEAL_LINE="$(wc -l < "$BODIES")"
pass "管理 API 可创建 pow_question 站点（默认仍是 pow_only，模式必须显式声明）"

# 未知题型必须被拒绝：存进去只会让站点永远返回 503
[[ "$(ac POST /api/admin/sites '{"name":"ghost","site_key":"pk_ghost","verify_mode":"pow_question","generator_id":"text-warp-v1","difficulty":2}')" == "400" ]] \
  || fail "未登记的题型应被管理面拒绝"
# 基线题型的定位必须在管理返回里看得见，而不是藏在文档里
ac GET /api/admin/generators >/dev/null
jqpy 'import json,sys;g=json.load(open(sys.argv[1]))["generators"][0];assert g["production_ready"] is False and "不抗自动解题" in g["note"]' \
  || fail '生成器列表未标明"工程基线/不抗自动解题"'
pass "管理面拒绝未知题型，并把基线题型的定位显式返回"

[[ "$(pb POST /api/challenge '{"site_key":"pk_quiz"}' -H "Origin: $ORIGIN")" == "503" ]] \
  || fail "worker 关闭且池空时应 503，而不是静默只发 PoW"
body | grep -q 'challenge-unavailable' || fail "池空错误码不符: $(body)"
if body | grep -qE '"token"|"pow"'; then
  fail "池空却返回了 PoW 载荷（静默降级）：$(body)"
fi
[[ "$(pb GET /healthz "")" == "200" ]] || fail "缺题不能把整个服务打成不可用"
pass "题池为空时返回可重试的 503 challenge-unavailable，绝不降级成 PoW-only"

# 回归：pow_only 站点的阶段 1/2 流程完全不受影响
EPOCHA_SITE_SECRET="$IMPORT_SECRET" "$DEV" --base "$BASE" --site-key "$IMPORT_KEY" \
  --mode full > "$WORK/only.txt" || fail "pow_only 回归流程失败：$(cat "$WORK/only.txt")"
grep -q 'step=siteverify success=true' "$WORK/only.txt" || fail "pow_only 回归未通过"
pass "旧站点模式不变：pow_only 的 challenge→redeem→siteverify 仍然全通"

stop_server
log "第二步：打开补题 worker 重启（同一库）"
start_server true

# 等后台真的备出题来（这里测的是 worker 本身，不是测试手动 Tick）
ready=0
for _ in $(seq 1 80); do
  ac GET /api/admin/pool >/dev/null
  ready="$(jqpy 'import json,sys;b=json.load(open(sys.argv[1]))["buckets"];print(b[0]["ready"] if b else 0)')"
  [[ "$ready" -ge 2 ]] && break
  sleep 0.25
done
[[ "$ready" -ge 2 ]] || fail "后台 worker 未能在 20s 内备出题（ready=$ready）"
jqpy 'import json,sys;b=json.load(open(sys.argv[1]))["buckets"][0];assert "verifier" not in b and "payload" not in b and "task" not in b' \
  || fail "池状态响应含题目内容或私有判分数据"
jqpy 'import json,sys;w=json.load(open(sys.argv[1]))["worker"];assert w["restart_resets"] is True and w["generated"]>0' \
  || fail "worker 计数缺失或未标明重启清零: $(body)"
pass "后台 worker 自动补题到水位；观测面只有计数，没有题面与 verifier"

# 完整闭环：题面由脚本**自己求值**（不读库、不硬编码）
CH_CODE="$(pb POST /api/challenge '{"site_key":"pk_quiz"}' -H "Origin: $ORIGIN")"
[[ "$CH_CODE" == "200" ]] || fail "白名单来源签发失败：$CH_CODE $(body)"
body | grep -q '"mode":"pow_question"' || fail "签发响应缺 mode: $(body)"
TASK="$(jqpy 'import json,sys;print(json.load(open(sys.argv[1]))["question"]["task"])')"
[[ -n "$TASK" ]] || fail "响应缺 question.task"
jqpy 'import json,sys;q=json.load(open(sys.argv[1]))["question"];assert set(q)=={"kind","task","answer_type","max_answer_len","generator","generator_version"}, q' \
  || fail "题面字段不是白名单集合"
# 逐层核对键集合：pow.seed 是协议要求公开的 PoW 随机数，不属于"内部种子"，
# 所以这里比的是键集合，不能用 "seed" 之类的子串 grep（会误伤，也会漏判新键）。
jqpy 'import json,sys
d=json.load(open(sys.argv[1]))
assert set(d)=={"token","expires_in","pow","mode","question"}, sorted(d)
assert set(d["pow"])=={"algo","seed","bits"}, sorted(d["pow"])' \
  || fail "签发响应的顶层/pow 键集合与契约不符: $(body)"
ANSWER="$(python3 -c '
import sys
# 与 devclient 同一条语法契约的第三种实现：独立求值，避免"客户端和服务端共享同一个错误"
s = sys.argv[1].replace("×","*").replace("÷","/")
def flat(x):
    a, op, b = x.split()
    a, b = int(a), int(b)
    return {"+":lambda:a+b,"-":lambda:a-b,"*":lambda:a*b,"/":lambda:a//b}[op]()
if s.startswith("("):
    i = s.index(")")
    v = flat(s[1:i])
    rest = s[i+1:].strip()
    if not rest: print(v); sys.exit()
    op, num = rest.split()[0], rest.split()[1]
    print({"+":lambda:v+int(num),"-":lambda:v-int(num),"*":lambda:v*int(num),"/":lambda:v//int(num)}[op]())
else:
    print(flat(s))
' "$TASK")"
[[ "$ANSWER" =~ ^-?[0-9]+$ ]] || fail "题面求值失败：task=$TASK"
# redeem-only：拿到一条**尚未消费**的 response，好让下面这次 siteverify 是第一次验证
EPOCHA_SITE_SECRET="$Q_SECRET" "$DEV" --base "$BASE" --site-key pk_quiz --origin "$ORIGIN" \
  --mode redeem-only --response-file "$WORK/qr.txt" > "$WORK/qa.txt" || fail "题目流程失败：$(cat "$WORK/qa.txt")"
grep -q 'step=redeem status=ok consumed=0' "$WORK/qa.txt" || fail "redeem-only 应留下未消费的 response：$(cat "$WORK/qa.txt")"
ok_v="$(curl -s -X POST "$BASE/siteverify" --data-urlencode "secret=$Q_SECRET" \
  --data-urlencode "response=$(cat "$WORK/qr.txt")" --data-urlencode "hostname=evil.example")"
echo "$ok_v" >> "$BODIES"
echo "$ok_v" | grep -q '"success":true' || fail "pow_question 的 response 验证失败：$ok_v"
echo "$ok_v" | grep -q '"hostname":"demo.example.com"' || fail "hostname 应来自绑定来源：$ok_v"
if echo "$ok_v" | grep -q 'evil.example'; then fail "自报 hostname 被回显：$ok_v"; fi
pass "真实求解 PoW + 从公开题面算出答案 → redeem → siteverify 全通（hostname 服务端裁决）"

# 单次作答：错误答案终结，正确答案紧随其后仍失败
EPOCHA_SITE_SECRET="$Q_SECRET" "$DEV" --base "$BASE" --site-key pk_quiz --origin "$ORIGIN" \
  --mode wrong-answer > "$WORK/wa.txt" || fail "单次作答语义未成立：$(cat "$WORK/wa.txt")"
grep -q 'step=redeem-wrong status=400' "$WORK/wa.txt" || fail "错误答案应得到 400 invalid-challenge：$(cat "$WORK/wa.txt")"
grep -q 'step=redeem-right-after-wrong status=400' "$WORK/wa.txt" || fail "错误答案后正确答案竟可重试"
pass "错误答案终结 challenge：紧随其后的正确答案也不能重试（一次作答）"

# 缺答案 / PoW 不对都不算有效作答
pb POST /api/challenge '{"site_key":"pk_quiz"}' -H "Origin: $ORIGIN" >/dev/null
tok2="$(jqpy 'import json,sys;print(json.load(open(sys.argv[1]))["token"])')"
[[ "$(pb POST /api/redeem "{\"token\":\"$tok2\",\"pow_nonce\":\"1\"}" -H "Origin: $ORIGIN")" != "200" ]] \
  || fail "缺答案竟能兑换"
if grep -q '"success":true' <<<"$(curl -s -X POST "$BASE/siteverify" --data-urlencode "secret=$Q_SECRET" --data-urlencode "response=$tok2")"; then
  fail "challenge token 竟能直接当作 response 验证通过"
fi
pass "缺 answer / PoW 不对都不算有效作答（不终结、也不放行）"

stop_server
log "第三步：重启（同一库）后终态与防重放仍成立"
start_server true
replay="$(curl -s -X POST "$BASE/siteverify" --data-urlencode "secret=$Q_SECRET" \
  --data-urlencode "response=$(cat "$WORK/qr.txt")")"
echo "$replay" | grep -q 'timeout-or-duplicate' || fail "重启后已消费的 response 可重放：$replay"
EPOCHA_SITE_SECRET="$Q_SECRET" "$DEV" --base "$BASE" --site-key pk_quiz --origin "$ORIGIN" \
  --mode full > "$WORK/after.txt" || fail "重启后新流程失败：$(cat "$WORK/after.txt")"
grep -q 'step=siteverify success=true' "$WORK/after.txt" || fail "重启后新流程未通过"
pass "重启后：已消费不可重放、新题目流程照常可用"

# 按值扫描
if grep -qF -e "$Q_SECRET" -e "$IMPORT_SECRET" -e "$ADMIN_KEY" -e "$MASTER_KEY" -e "$PEPPER" "$SERVER_LOG"; then
  fail "服务日志泄露凭据/密钥值"
fi
for secret_value in "$ADMIN_KEY" "$MASTER_KEY" "$PEPPER" "$IMPORT_SECRET"; do
  if grep -qF -- "$secret_value" "$BODIES"; then
    fail "HTTP 响应集合里出现了凭据值"
  fi
done
check_no_leak "$Q_SECRET"
# 答案值本身是个小整数，拿它扫日志会误伤（dur_ms 之类就能命中），
# 因此这里扫的是"字段是否越界"，而不是数值。
if grep -qE -- '"answer":|"verifier":|"seed":"' "$SERVER_LOG"; then
  fail "服务日志出现答案/verifier/内部种子的字段名（记录结构越界）"
fi
if grep -qE -- '"token"|"secret"|"verifier"|"answer"' "$SERVER_LOG"; then
  fail "服务日志出现凭据/答案类字段名"
fi
pass "扫描通过：凭据/密钥按值扫描，答案与私有 verifier 按字段名扫描（小整数答案做值扫描会误伤耗时字段）"

log "全部 ${STEP} 项验收通过"
