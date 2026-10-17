#!/usr/bin/env bash
# stage2.sh — 阶段 2 端到端验收（DESIGN §17 阶段 2）。
#
# 一个命令完成：随机演示密钥 → 启动临时服务（公共 + 管理两个监听器）→ 管理 API 建站点/来源白名单 →
# 浏览器来源裁决与 CORS → 停用/启用 → 密钥轮换与撤销 → 真实暴力求解 PoW 走通白名单来源 →
# 重启后确认"已撤销的密钥与已停用的站点不会因为配置文件复活"→ 审计与按值扫描。
#
# 所有密钥都是运行时随机生成、写进临时目录、退出即删；没有写死的解，也没有绕过验证的分支。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
export GOTOOLCHAIN=local

WORK="$(mktemp -d)"
SERVER_PID=""
STEP=0

log()  { printf '[stage2] %s\n' "$*"; }
pass() { STEP=$((STEP+1)); printf '[stage2] PASS %d: %s\n' "$STEP" "$*"; }
fail() {
  printf '[stage2] FAIL: %s\n' "$*" >&2
  if [[ -f "${SERVER_LOG:-}" ]]; then
    printf '[stage2] ---- 服务日志尾部 ----\n' >&2
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
  if [[ $rc -ne 0 ]]; then printf '[stage2] 已清理临时目录并停止服务（退出码 %d）\n' "$rc" >&2; fi
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
ALLOWED_ORIGIN="https://demo.example.com"
DB_PATH="$WORK/epocha.db"
SERVER_LOG="$WORK/server.log"
ADMIN_LOG="$WORK/bodies.log"   # 收集所有 HTTP 响应体，结尾统一按值扫描

# 配置文件只导入这一个站点，并且**显式**放行"无 Origin 的直接调用"：
# 演示客户端不是浏览器，阶段 2 的默认策略是拒绝无来源请求。
printf '[{"name":"imported","site_key":"%s","secret":"%s","pow_bits":12,"origins":["%s"],"allow_missing_origin":true}]\n' \
  "$IMPORT_KEY" "$IMPORT_SECRET" "$ALLOWED_ORIGIN" > "$WORK/sites.json"
chmod 600 "$WORK/sites.json"

PUB_PORT=""
ADMIN_PORT=""
BASE=""
ABASE=""

start_server() {
  : > "$SERVER_LOG"
  EPOCHA_MASTER_KEY="$MASTER_KEY" EPOCHA_PEPPER="$PEPPER" EPOCHA_ADMIN_KEY="$ADMIN_KEY" \
    "$SERVER" --sites "$WORK/sites.json" --db "$DB_PATH" \
    --listen "127.0.0.1:0" --admin-listen "127.0.0.1:0" \
    >>"$SERVER_LOG" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 200); do
    if ! kill -0 "$SERVER_PID" 2>/dev/null; then fail "服务进程启动后退出：$(tail -n 5 "$SERVER_LOG")"; fi
    # 身份凭据：本轮随机 site_key 出现在启动日志里（首次是"已导入"，重启后是"已在库中"）。
    # 若上一轮遗留进程占着某个端口，别人的服务答不上这行日志，也就拿不到端口。
    if grep -q "site_key=${IMPORT_KEY}" "$SERVER_LOG" \
       && grep -q "msg=已监听" "$SERVER_LOG" \
       && grep -q "msg=管理面已挂载" "$SERVER_LOG"; then
      PUB_PORT="$(sed -n 's/.*msg=已监听 addr=127\.0\.0\.1:\([0-9]*\).*/\1/p' "$SERVER_LOG" | head -n1)"
      ADMIN_PORT="$(sed -n 's/.*msg=管理面已挂载 addr=127\.0\.0\.1:\([0-9]*\).*/\1/p' "$SERVER_LOG" | head -n1)"
      [[ -n "$PUB_PORT" && -n "$ADMIN_PORT" ]] || fail "无法从日志解析两个监听端口"
      [[ "$PUB_PORT" != "$ADMIN_PORT" ]] || fail "公共与管理监听器落在同一个端口"
      BASE="http://127.0.0.1:${PUB_PORT}"
      ABASE="http://127.0.0.1:${ADMIN_PORT}"
      for _ in $(seq 1 50); do
        curl -fsS -m 1 "$BASE/healthz" >/dev/null 2>&1 && return 0
        sleep 0.1
      done
      fail "端口 $PUB_PORT 上健康检查不通（可能不是本次服务）"
    fi
    sleep 0.1
  done
  fail "服务在 20s 内未完成启动：$(tail -n 5 "$SERVER_LOG")"
}

stop_server() {
  if kill -0 "$SERVER_PID" 2>/dev/null; then
    kill -TERM "$SERVER_PID"
    for _ in $(seq 1 60); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.1; done
    kill -0 "$SERVER_PID" 2>/dev/null && fail "优雅关闭超时（5s 后仍在运行）"
  fi
  SERVER_PID=""
}

# ac <method> <path> [json] → 管理 API，带 Bearer
ac() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-s -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$ABASE$path" \
              -H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json')
  [[ -n "$body" ]] && args+=(--data "$body")
  local code
  code="$(curl "${args[@]}")"
  cat "$WORK/resp.json" >> "$ADMIN_LOG"; printf '\n' >> "$ADMIN_LOG"
  echo "$code"
}
# au <method> <path> [json] → 管理 API，**不带**凭据
au() {
  local method="$1" path="$2"
  local code
  code="$(curl -s -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$ABASE$path")"
  cat "$WORK/resp.json" >> "$ADMIN_LOG"; printf '\n' >> "$ADMIN_LOG"
  echo "$code"
}
# pb <method> <path> <json-or-empty> [extra curl args...] → 公共 API
pb() {
  local method="$1" path="$2" body="$3"; shift 3
  local args=(-s -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path")
  [[ -n "$body" ]] && args+=(--data "$body")
  args+=("$@")
  local code
  code="$(curl "${args[@]}")"
  cat "$WORK/resp.json" >> "$ADMIN_LOG"; printf '\n' >> "$ADMIN_LOG"
  echo "$code"
}
body() { cat "$WORK/resp.json"; }

# 凭据"只展示一次"必须按**展示之后的响应**来扫描：展示那一条本身当然含明文。
# 每个响应占一行，所以记下揭示发生时的行号，之后只扫该行之后的部分。
REVEAL_MARKS=()
mark_reveal() { REVEAL_MARKS+=("$(wc -l < "$ADMIN_LOG")|$1"); }
check_no_leak() {
  local label="line-scan"
  local entry ln sec
  for entry in "${REVEAL_MARKS[@]}"; do
    ln="${entry%%|*}"; sec="${entry##*|}"
    if tail -n +"$((ln+1))" "$ADMIN_LOG" 2>/dev/null | grep -qF -- "$sec"; then
      fail "凭据（长度 ${#sec}）在它被展示之后的响应里再次出现"
    fi
  done
}
jqget() { python3 -c 'import sys,json;d=json.load(sys.stdin);exec("print(d'+sys.argv[1]+")')" "$1" 2>/dev/null || true; }

log "启动临时服务"
start_server
log "公共面 $BASE，管理面 $ABASE"
curl -fsS "$BASE/healthz" | grep -q '"status":"ok"' || fail "健康检查响应不符"
pass "服务启动，公共与管理监听器分离且 /healthz 可用"

# --- 1. 管理面鉴权与监听器隔离 ---
[[ "$(au GET /api/admin/sites)" == "401" ]] || fail "未带凭据的管理请求应 401"
[[ "$(ac GET /api/admin/sites)" == "200" ]] || fail "带正确凭据应放行"
wrong="$(curl -s -o /dev/null -w '%{http_code}' -X GET "$ABASE/api/admin/sites" \
  -H "Authorization: Bearer ak_wrong_wrong_wrong_wrong_wrong_wrong_wrong")"
[[ "$wrong" == "401" ]] || fail "错误凭据应 401，实际 $wrong"
# 公共端口上管理路由**不存在**（不是"存在但鉴权失败"）
pub_admin="$(pb GET /api/admin/sites "")"
[[ "$pub_admin" == "404" ]] || fail "公共监听器不应有管理路由（期望 404，实际 $pub_admin）"
pass "管理 API 全量鉴权；公共监听器上没有管理路由"

# --- 2. 站点创建：secret 只出现一次 ---
code="$(ac POST /api/admin/sites '{"name":"e2e","site_key":"pk_e2e","pow_bits":12}')"
[[ "$code" == "201" ]] || fail "创建站点应 201，实际 $code: $(body)"
SITE_SECRET="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["secret"])' "$WORK/resp.json")"
SITE_ID="$(python3 -c 'import json,sys;print(int(json.load(open(sys.argv[1]))["site"]["id"]))' "$WORK/resp.json")"
[[ -n "$SITE_SECRET" && -n "$SITE_ID" ]] || fail "创建响应缺少 secret 或 site id"
mark_reveal "$SITE_SECRET"
ac GET "/api/admin/sites/$SITE_ID" >/dev/null
ac GET "/api/admin/sites" >/dev/null
ac GET "/api/admin/sites/$SITE_ID/secrets" >/dev/null
ac GET "/api/admin/audit?limit=50" >/dev/null
check_no_leak
pass "站点创建仅一次性返回明文 secret；详情/列表/凭据/审计均不再返回"

# --- 3. 来源白名单与 CORS ---
code="$(ac PUT "/api/admin/sites/$SITE_ID/origins" '{"origins":["https://app.example.com:443","http://localhost:8080"]}')"
[[ "$code" == "200" ]] || fail "设置来源应 200，实际 $code: $(body)"
body | grep -q '"https://app.example.com"' || fail "来源未按默认端口规范化：$(body)"

ok_hdr="$(curl -s -D - -o /dev/null -X POST "$BASE/api/challenge" \
  -H 'Content-Type: application/json' -H "Origin: https://app.example.com" \
  --data "{\"site_key\":\"pk_e2e\"}" | tr -d '\r')"
echo "$ok_hdr" | grep -qi '^HTTP/1.1 200' || fail "白名单内来源应可签发：$ok_hdr"
echo "$ok_hdr" | grep -qi '^access-control-allow-origin: https://app\.example\.com$' \
  || fail "缺少精确回显的 CORS 头：$(echo "$ok_hdr" | head -n 12)"
echo "$ok_hdr" | grep -qi '^vary: Origin' || fail "缺少 Vary: Origin：$ok_hdr"

for bad_origin in "https://evil-example.com" "https://app.example.com.evil.net" "https://sub.app.example.com" "http://app.example.com" "null"; do
  c="$(pb POST /api/challenge "{\"site_key\":\"pk_e2e\"}" -H "Origin: $bad_origin")"
  [[ "$c" == "400" ]] || fail "来源 $bad_origin 应被拒绝（400），实际 $c"
done
# 无 Origin 的直接调用：该站点没放行 → 400
[[ "$(pb POST /api/challenge '{"site_key":"pk_e2e"}')" == "400" ]] \
  || fail "未放行缺失 Origin 的站点应拒绝无来源请求"
# 预检：204、不签发任何东西；被拒来源不给宽松头
pre_h="$(curl -s -D - -o "$WORK/pre.txt" -X OPTIONS "$BASE/api/challenge?site_key=pk_e2e" \
  -H "Origin: https://app.example.com" -H 'Access-Control-Request-Method: POST' | tr -d '\r')"
echo "$pre_h" | grep -qi '^HTTP/1.1 204' || fail "合法预检应 204：$pre_h"
[[ ! -s "$WORK/pre.txt" ]] || fail "预检返回了响应体（不得签发任何东西）：$(cat "$WORK/pre.txt")"
pre_bad="$(curl -s -D - -o /dev/null -X OPTIONS "$BASE/api/challenge?site_key=pk_e2e" \
  -H "Origin: https://evil-example.com" -H 'Access-Control-Request-Method: POST' | tr -d '\r')"
if echo "$pre_bad" | grep -qi '^access-control-allow-origin'; then
  fail "被拒来源的预检出现了 CORS 头：$pre_bad"
fi
pass "来源精确匹配（后缀/子域/端口/协议/null 全拒）；CORS 与预检行为正确"

# --- 4. 白名单来源下走通完整验证流程（真实暴力求解 PoW） ---
EPOCHA_SITE_SECRET="$SITE_SECRET" "$DEV" --base "$BASE" --site-key pk_e2e \
  --origin https://app.example.com --mode full > "$WORK/full.txt" || fail "白名单流程失败：$(cat "$WORK/full.txt")"
grep -q 'step=siteverify success=true' "$WORK/full.txt" || fail "siteverify 未成功"
grep -q 'iterations=[1-9]' "$WORK/full.txt" || fail "PoW 未真实求解"

# hostname 由服务端绑定的来源裁决：取一条未消费的 response，自报 evil 值再验证
EPOCHA_SITE_SECRET="$SITE_SECRET" "$DEV" --base "$BASE" --site-key pk_e2e \
  --origin https://app.example.com --mode redeem-only --response-file "$WORK/host.txt" >/dev/null \
  || fail "取得带来源的 response 失败"
hostcheck="$(curl -s -X POST "$BASE/siteverify" --data-urlencode "secret=$SITE_SECRET" \
  --data-urlencode "response=$(cat "$WORK/host.txt")" --data-urlencode "hostname=evil.example")"
echo "$hostcheck" | grep -q '"success":true' || fail "带来源的验证应成功：$hostcheck"
echo "$hostcheck" | grep -q '"hostname":"app.example.com"' || fail "hostname 应来自服务端绑定的来源：$hostcheck"
if echo "$hostcheck" | grep -q 'evil.example'; then fail "调用方自报的 hostname 被回显了：$hostcheck"; fi
echo "$hostcheck" >> "$ADMIN_LOG"
pass "challenge → 真实求解 PoW → redeem → siteverify 在白名单来源上全通，hostname 由服务端裁决"

# --- 5. 停用站点 → 未消费 token 失效；重新启用不复活 ---
# A 是一条**尚未消费**的 response（redeem-only 取的），用来观察管理动作对它的影响
EPOCHA_SITE_SECRET="$SITE_SECRET" "$DEV" --base "$BASE" --site-key pk_e2e \
  --origin https://app.example.com --mode redeem-only --response-file "$WORK/a.txt" >/dev/null \
  || fail "取得未消费 response 失败"
ac POST "/api/admin/sites/$SITE_ID/disable" '{}' >/dev/null
[[ "$(pb POST /api/challenge '{"site_key":"pk_e2e"}' -H "Origin: https://app.example.com")" == "400" ]] \
  || fail "停用站点后仍允许签发"
[[ "$(pb POST /api/challenge '{"site_key":"pk_e2e"}' -H "Origin: https://app.example.com")" != "200" ]] \
  || fail "停用站点后仍签发成功"
disabled="$(curl -s -X POST "$BASE/siteverify" --data-urlencode "secret=$SITE_SECRET" \
  --data-urlencode "response=$(cat "$WORK/a.txt")")"
echo "$disabled" | grep -q '"success":true' && fail "停用站点后旧 response 仍验证成功：$disabled"
echo "$disabled" >> "$ADMIN_LOG"

ac POST "/api/admin/sites/$SITE_ID/enable" '{}' >/dev/null
reenabled="$(curl -s -X POST "$BASE/siteverify" --data-urlencode "secret=$SITE_SECRET" \
  --data-urlencode "response=$(cat "$WORK/a.txt")")"
echo "$reenabled" | grep -q '"success":true' && fail "重新启用后停用前的 response 复活了：$reenabled"
echo "$reenabled" >> "$ADMIN_LOG"
# 重新启用后新流程照常可用
EPOCHA_SITE_SECRET="$SITE_SECRET" "$DEV" --base "$BASE" --site-key pk_e2e \
  --origin https://app.example.com --mode full >/dev/null || fail "重新启用后新流程不可用"
pass "停用站点不签发、未消费 token 立即失效；重新启用不会让它复活，新流程照常可用"

# --- 6. 密钥轮换：立即撤销 + 过渡期；撤销不得消费合法 response ---
# 上一节把站点停用又启用，版本两次提升，所以那条 token 已按设计永久失效。
# 这里重新取一条**未消费**的 B 作为观察对象。
rot="$(ac POST "/api/admin/sites/$SITE_ID/secrets/rotate" '{"transition_sec":0}')"
[[ "$rot" == "200" ]] || fail "轮换应 200，实际 $rot: $(body)"
S2="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["secret"])' "$WORK/resp.json")"
[[ -n "$S2" && "$S2" != "$SITE_SECRET" ]] || fail "轮换未返回新 secret"
mark_reveal "$S2"
[[ "$(body | python3 -c 'import json,sys;print(len(json.load(sys.stdin)["retired"]))')" == "1" ]] \
  || fail "立即撤销应撤下 1 把：$(body)"

# 取一条未消费的 B（用当前有效凭据 S2 签发）
EPOCHA_SITE_SECRET="$S2" "$DEV" --base "$BASE" --site-key pk_e2e \
  --origin https://app.example.com --mode redeem-only --response-file "$WORK/b.txt" >/dev/null \
  || fail "取得未消费 response B 失败"

# 被撤销的旧凭据 + 有效的 B：必须被拒，而且不能消费掉 B
bad="$(curl -s -X POST "$BASE/siteverify" --data-urlencode "secret=$SITE_SECRET" \
  --data-urlencode "response=$(cat "$WORK/b.txt")")"
echo "$bad" | grep -q 'invalid-input-secret' || fail "被撤销的凭据应归到 invalid-input-secret：$bad"
echo "$bad" >> "$ADMIN_LOG"

# 过渡期轮换：S2 进入窗口，S3 生效
rot2="$(ac POST "/api/admin/sites/$SITE_ID/secrets/rotate" '{"transition_sec":3600}')"
[[ "$rot2" == "200" ]] || fail "过渡期轮换应 200，实际 $rot2"
S3="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["secret"])' "$WORK/resp.json")"
mark_reveal "$S3"
body | grep -q '"active":true' || fail "过渡期内的旧密钥应仍标记为有效：$(body)"

# 过渡期内的 S2 仍可完成验证——这同时证明上一步"旧凭据被拒"没有消费掉 B
ok2="$(curl -s -X POST "$BASE/siteverify" --data-urlencode "secret=$S2" \
  --data-urlencode "response=$(cat "$WORK/b.txt")")"
echo "$ok2" | grep -q '"success":true' || fail "过渡期内的凭据应能验证未消费的 response：$ok2"
echo "$ok2" >> "$ADMIN_LOG"
# B 现在才真正被消费：任何有效凭据再验都必须是 timeout-or-duplicate
dup="$(curl -s -X POST "$BASE/siteverify" --data-urlencode "secret=$S3" \
  --data-urlencode "response=$(cat "$WORK/b.txt")")"
echo "$dup" | grep -q 'timeout-or-duplicate' || fail "同一 response 第二次验证未被拒：$dup"
echo "$dup" >> "$ADMIN_LOG"
# 过渡期上限
over="$(ac POST "/api/admin/sites/$SITE_ID/secrets/rotate" '{"transition_sec":900000}')"
[[ "$over" == "400" ]] || fail "过渡期超上限应 400，实际 $over"
pass "两类轮换都生效：立即撤销不消费合法 response，过渡期有上限且有窗口"

# --- 7. 重启后配置不得覆盖权威数据源 ---
stop_server
log "重启服务（同一 DB 文件 + 同一份配置文件）"
start_server
# SITE_SECRET 已被**立即撤销**：重启后必须仍不可用（撤销是库里的行，不是内存标记）
revoked_still="$(curl -s -X POST "$BASE/siteverify" --data-urlencode "secret=$SITE_SECRET" \
  --data-urlencode "response=$(cat "$WORK/b.txt")")"
echo "$revoked_still" | grep -q 'invalid-input-secret' || fail "重启后已撤销的凭据又可用了：$revoked_still"
# B 已在旧进程里被真正消费过一次，所以这里的"重放被拒"才是防重放持久化的证据
replay="$(curl -s -X POST "$BASE/siteverify" --data-urlencode "secret=$S3" \
  --data-urlencode "response=$(cat "$WORK/b.txt")")"
echo "$replay" | grep -q 'timeout-or-duplicate' || fail "重启后已消费的 response 可重放：$replay"
# 站点状态与版本都在库里：重启后新流程照常可用
EPOCHA_SITE_SECRET="$S3" "$DEV" --base "$BASE" --site-key pk_e2e \
  --origin https://app.example.com --mode full >/dev/null || fail "重启后新流程不可用"
# 导入站点仍可用（它的凭据从未被撤销）
EPOCHA_SITE_SECRET="$IMPORT_SECRET" "$DEV" --base "$BASE" --site-key "$IMPORT_KEY" \
  --mode full > "$WORK/imported.txt" || fail "重启后导入站点流程应照常可用"
grep -q 'step=siteverify success=true' "$WORK/imported.txt" || fail "导入站点验证未成功"
pass "重启后：已撤销凭据仍失效、已消费状态仍不可重放、未撤销站点仍可用（库是权威数据源）"

# --- 8. 审计 ---
# 分页边界先单独确认：limit=5 最多返回 5 行
[[ "$(ac GET '/api/admin/audit?limit=5')" == "200" ]] || fail "审计查询应 200"
small_n="$(body | python3 -c 'import json,sys;print(len(json.load(sys.stdin)["audit"]))')"
[[ "$small_n" -le 5 ]] || fail "limit=5 却返回 $small_n 行"
code="$(ac GET '/api/admin/audit?limit=50')"
[[ "$code" == "200" ]] || fail "审计查询应 200，实际 $code"
body | grep -q '"site.create"' || fail "审计未覆盖 site.create：$(body)"
body | grep -q '"secret.rotate"' || fail "审计未覆盖 secret.rotate：$(body)"
body | grep -q '"site.disable"' || fail "审计未覆盖 site.disable：$(body)"
[[ "$(ac GET '/api/admin/audit?limit=501')" == "400" ]] || fail "审计分页上限未生效"
[[ "$(ac GET '/api/admin/sites/999999')" == "404" ]] || fail "未知站点应 404"
pass "审计覆盖写操作、分页有上限、查询需鉴权"

# --- 9. 按值扫描：日志与全部响应都不含密钥材料 ---
if grep -qF -e "$SITE_SECRET" -e "$S2" -e "$S3" -e "$IMPORT_SECRET" -e "$ADMIN_KEY" \
     -e "$MASTER_KEY" -e "$PEPPER" "$SERVER_LOG"; then
  fail "服务日志泄露了密钥材料的值"
fi
check_no_leak
# 响应集合：管理凭据/主密钥/pepper 绝不该出现；导入站点的 secret 也从没被任何接口返回过。
# 被创建/轮换"展示一次"的那些 secret 由 check_no_leak 按各自揭示之后的响应逐条扫描。
if grep -qF -e "$ADMIN_KEY" -e "$MASTER_KEY" -e "$PEPPER" -e "$IMPORT_SECRET" "$ADMIN_LOG"; then
  fail "HTTP 响应集合里出现了管理凭据/主密钥/pepper/导入 secret"
fi
if grep -qE -- '"token"|"secret"|"authorization"|"answer"' "$SERVER_LOG"; then
  fail "服务日志出现凭据类字段名（记录结构越界）"
fi
pass "按值扫描通过：日志与响应均不含密钥/凭据值"

log "全部 ${STEP} 项验收通过"
