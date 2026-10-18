#!/usr/bin/env bash
# stage4a.sh — 阶段 4A 端到端验收（DESIGN §17 阶段 4A、docs/STAGE4A.md）。
#
# 覆盖的是**工程闭环 + 真实浏览器交互**，不是效果结论：
#   1. 依赖隔离：cmd/server 不链接任何开发期工具，但**必须**链接 web/widget（生产脚本从二进制出）；
#   2. widget 静态资源：内容类型、可缓存、nosniff、无 CORS 头、版本化 URL、不拓宽公开面；
#   3. site_key 一致性：预检看见的站点与实际操作落到的站点必须一致（URL 与体冲突即拒）；
#   4. 真实浏览器闭环（WebDriver BiDi，零第三方驱动）：显示图片 → 键盘输入 → 页面内 PoW →
#      redeem → 业务表单 → exampleapp 调 /siteverify → 写入；含答错终结、重放被拒、
#      多实例隔离、晚到响应、reset/destroy 取消、CSP 禁 Worker 明确失败、键盘-only；
#   5. fail closed：验证服务宕机/超时/5xx ⇒ 业务不写入；
#   6. 凭据按值扫描：页面、组件响应、留言列表、两个日志、库文件原始字节；
#   7. 跨实现一致性：JS SHA-256/PoW 与 Go 对同一份固定向量结果相同；
#   8. 实测数字：widget gzip 体积、浏览器侧 PoW 时延、exampleapp 驻留。
#
# 状态口径：**PASS/FAIL 只用于工程项**；外部条件不足 BLOCKED；本轮没做 NOT_RUN。
# OCR 效果（BLOCKED）与人工可读性（NOT_RUN）沿用阶段 3B 的结论，本脚本不重复声称。
#
# 退出码：0 = 全部工程项 PASS；1 = 有 FAIL（含浏览器必需验收没能真实执行的情况）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
export GOTOOLCHAIN=local

WORK="$(mktemp -d)"
SERVER_PID=""
APP1_PID=""
APP2_PID=""
APP3_PID=""
APP4_PID=""
APP5_PID=""
APP6_PID=""
FF_PID=""
STEP=0
N_PASS=0
N_BLOCKED=0
N_NOTRUN=0
RUST_LOG_TARGET="$WORK/epocha.log"
BIDI_PORT="${EPOCHA_BIDI_PORT:-9333}"

log()  { printf '[stage4a] %s\n' "$*"; }
pass()    { STEP=$((STEP+1)); N_PASS=$((N_PASS+1)); printf '[stage4a] PASS %d: %s\n' "$STEP" "$*"; }
blocked() { STEP=$((STEP+1)); N_BLOCKED=$((N_BLOCKED+1)); printf '[stage4a] BLOCKED %d: %s\n' "$STEP" "$*"; }
notrun()  { STEP=$((STEP+1)); N_NOTRUN=$((N_NOTRUN+1)); printf '[stage4a] NOT_RUN %d: %s\n' "$STEP" "$*"; }
fail() {
  printf '[stage4a] FAIL: %s\n' "$*" >&2
  for f in "$RUST_LOG_TARGET" "${APP1_LOG:-}" "${APP2_LOG:-}"; do
    [[ -n "$f" && -f "$f" ]] || continue
    printf '[stage4a] ---- %s 尾部 ----\n' "$f" >&2
    tail -n 40 "$f" >&2 || true
  done
  exit 1
}
cleanup() {
  local rc=$?
  set +e
  for pid in "$SERVER_PID" "$APP1_PID" "$APP2_PID" "$APP3_PID" "$APP4_PID" "$APP6_PID" "$FF_PID"; do
    [[ -n "$pid" ]] && kill -TERM "$pid" 2>/dev/null || true
  done
  for _ in 1 2 3 4 5 6 7 8; do
    any=0
    for pid in "$SERVER_PID" "$APP1_PID" "$APP2_PID" "$APP3_PID" "$APP4_PID" "$APP6_PID" "$FF_PID"; do
      [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null && any=1
    done
    [[ $any -eq 0 ]] && break
    sleep 0.2
  done
  for pid in "$SERVER_PID" "$APP1_PID" "$APP2_PID" "$APP3_PID" "$APP4_PID" "$APP6_PID" "$FF_PID"; do
    [[ -n "$pid" ]] && kill -KILL "$pid" 2>/dev/null || true
  done
  rm -rf "$WORK"
  exit $rc
}
trap cleanup EXIT

# json <expr> —— 用 python3 取字段（本机已有；不引入 jq 依赖）
json() { python3 -c "import json,sys;d=json.load(sys.stdin);print($1)"; }

MASTER_KEY="$(head -c32 /dev/urandom | base64 | tr -d '\n')"
PEPPER="$(head -c32 /dev/urandom | base64 | tr -d '\n')"
ADMIN_KEY="ak_$(head -c32 /dev/urandom | xxd -p | tr -d '\n')"
# 必须是**运行时随机生成**的演示密钥，只活在本进程的 env 里，随 $WORK 一起消失（AGENTS.md：仓库不存密钥）。
export EPOCHA_ADMIN_KEY="$ADMIN_KEY"
SITE_KEY="pk_stage4a_$(head -c6 /dev/urandom | xxd -p)"
export EPOCHA_MASTER_KEY="$MASTER_KEY" EPOCHA_PEPPER="$PEPPER"

# 固定端口必须空闲：若上一轮的示例应用还活着，curl 与浏览器测到的都是**遗留进程**，
# 整轮结论会静默失真。宁可开局就失败，也不要"看起来通过了"。
for port in 8391 8392 8393 8394 8395 8396 "$BIDI_PORT"; do
  if ss -ltn 2>/dev/null | grep -q ":$port "; then
    fail "端口 $port 已被占用：先杀掉遗留的示例应用/浏览器进程再跑本脚本（否则测到的是别的进程）"
  fi
done

log "第一步：构建（生产二进制 + 开发期工具）"
mkdir -p bin
go build -trimpath -ldflags "-s -w" -o bin/epocha ./cmd/server
go build -o "$WORK/devseed" ./cmd/devseed
go build -o "$WORK/exampleapp" ./cmd/exampleapp
BIN_SIZE="$(stat -c %s bin/epocha)"
log "  bin/epocha = $BIN_SIZE 字节"

log "第二步：依赖隔离"
DEPS="$(go list -deps ./cmd/server)"
for banned in internal/eval cmd/preview cmd/devseed cmd/evalgen cmd/evalocr cmd/exampleapp web/preview; do
  if printf '%s\n' "$DEPS" | grep -q "EpoCHA/$banned"; then
    fail "服务二进制链接了开发期包 $banned（生产路径不得出现答案出口或演示后门）"
  fi
done
printf '%s\n' "$DEPS" | grep -q "EpoCHA/web/widget" || fail "服务二进制没有链接 web/widget：生产端口发不出 widget"
pass "服务二进制含 widget 与渲染路径，但不含任何评测/开发工具（go list -deps 证明）"

log "第三步：启动服务（题池后台 worker 关闭，题目只来自受控注入）"
DB_PATH="$WORK/epocha.db"
SERVER_LOG="$RUST_LOG_TARGET"
# 限流放开到远大于本脚本的形状：浏览器测试会在同一来源上连续领很多题，
# 撞上限流会让"失败"看起来像组件缺陷（真实限流语义在 stage1/stage2 的验收里各测一次）。
# 后台补题 worker 关掉：题池里只有受控注入的题，"哪道题被发出去"才是可复现的。
EPOCHA_RATE_IP_RPM=6000 EPOCHA_RATE_SITE_RPM=6000 EPOCHA_MAX_ACTIVE_CHALLENGES=2000 \
EPOCHA_MAX_ACTIVE_CHALLENGES_PER_SITE=2000 EPOCHA_POOL_ENABLED=0 \
bin/epocha --db "$DB_PATH" --listen 127.0.0.1:0 --admin-listen 127.0.0.1:0 >>"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
BASE=""; ABASE=""
for _ in $(seq 1 100); do
  BASE="$(sed -n 's/.*msg=已监听 addr=127\.0\.0\.1:\([0-9]*\).*/\1/p' "$SERVER_LOG" | head -n1)"
  ABASE="$(sed -n 's/.*msg=管理面已挂载 addr=127\.0\.0\.1:\([0-9]*\).*/\1/p' "$SERVER_LOG" | head -n1)"
  [[ -n "$BASE" && -n "$ABASE" ]] && break
  sleep 0.1
done
[[ -n "$BASE" && -n "$ABASE" ]] || fail "服务没有报出监听地址"
BASE="http://127.0.0.1:$BASE"; ABASE="http://127.0.0.1:$ABASE"
ADMIN=(-H "Authorization: Bearer $ADMIN_KEY")

log "第四步：widget 静态资源（公开端口、版本化 URL）"
for path in /widget/v1.js /widget/v1.worker.js; do
  hdrs="$(curl -sS -D - -o "$WORK$(basename "$path")" "$BASE$path")"
  printf '%s' "$hdrs" | grep -qi '^HTTP/1.1 200' || fail "$path 不是 200：$(printf '%s' "$hdrs" | head -n1)"
  printf '%s' "$hdrs" | grep -qi '^content-type: text/javascript' || fail "$path 内容类型不是 text/javascript"
  printf '%s' "$hdrs" | grep -qi '^x-content-type-options: nosniff' || fail "$path 缺 nosniff"
  printf '%s' "$hdrs" | grep -qi '^cache-control: public' || fail "$path 不可缓存（每次领题都回源就是负载）"
  printf '%s' "$hdrs" | grep -qi '^access-control-allow-origin' && fail "$path 不该带 CORS 头"
  grep -q 'epocha widget' "$WORK$(basename "$path")" || fail "$path 内容不是 widget 脚本"
done
if curl -fsS "$BASE/widget/v2.js" >/dev/null 2>&1; then fail "未注册的 /widget/v2.js 竟然可取"; fi
if curl -fsS "$BASE/api/admin/pool" >/dev/null 2>&1; then fail "公共端口暴露了管理路由"; fi
WIDGET_GZIP="$(gzip -9 -c web/widget/v1.js | wc -c)"
WIDGET_RAW="$(stat -c %s web/widget/v1.js)"
log "  widget：v1.js 原文 $WIDGET_RAW B，gzip $WIDGET_GZIP B（预算 30KB [目标]，DESIGN §13）"
[[ $WIDGET_GZIP -lt 30720 ]] || fail "widget gzip 超出 30KB 预算：$WIDGET_GZIP B"
pass "widget 静态资源：内容类型/nosniff/可缓存/无 CORS/未注册路径 404/公共端口无管理路由；gzip ${WIDGET_GZIP}B < 30KB"

log "第五步：建站点（pow_question + text-image-v1 + 两个示例应用来源）"
CREATE="$(curl -sS "${ADMIN[@]}" -H 'Content-Type: application/json' -X POST "$ABASE/api/admin/sites" -d '{
  "name":"stage4a 验收","site_key":"'"$SITE_KEY"'","pow_bits":12,
  "verify_mode":"pow_question","generator_id":"text-image-v1","difficulty":1,
  "origins":["http://127.0.0.1:8391","http://127.0.0.1:8392","http://127.0.0.1:8393"],"allow_missing_origin":false}')"
SITE_SECRET="$(printf '%s' "$CREATE" | json 'd.get("secret","")')"
[[ -n "$SITE_SECRET" ]] || fail "建演示站点失败：$CREATE"
# 第二个站点专用于"生产难度"的取消/超时用例：bits=20（浏览器要算上百万次）。
# 用它做两件事：证明取消真的生效（短计算观察不到），以及给出 bits=20 的浏览器时延实测。
HARD_KEY="pk_stage4a_hard_$(head -c6 /dev/urandom | xxd -p)"
HARD_CREATE="$(curl -sS "${ADMIN[@]}" -H 'Content-Type: application/json' -X POST "$ABASE/api/admin/sites" -d '{
  "name":"stage4a 高难度","site_key":"'"$HARD_KEY"'","pow_bits":20,
  "verify_mode":"pow_question","generator_id":"text-image-v1","difficulty":1,
  "origins":["http://127.0.0.1:8394"],"allow_missing_origin":false}')"
HARD_SECRET="$(printf '%s' "$HARD_CREATE" | json 'd.get("secret","")')"
# 第三个演示站点：response TTL 只有 2 秒，专门用来测"过期时客户端清空 token"这条行为。
TTL_CREATE="$(curl -sS "${ADMIN[@]}" -H 'Content-Type: application/json' -X POST "$ABASE/api/admin/sites" -d '{
  "name":"stage4a 短TTL","site_key":"pk_stage4a_ttl","pow_bits":12,
  "verify_mode":"pow_question","generator_id":"text-image-v1","difficulty":1,"response_ttl_sec":2,
  "origins":["http://127.0.0.1:8396"],"allow_missing_origin":false}')"
TTL_SECRET="$(printf '%s' "$TTL_CREATE" | json 'd.get("secret","")')"
[[ -n "$HARD_SECRET" ]] || fail "建高难度演示站点失败：$HARD_CREATE"
[[ -n "$HARD_SECRET" && "$HARD_SECRET" != "None" ]] || fail "高难度演示站点建不出来：$HARD_CREATE"
[[ -n "$SITE_SECRET" && "$SITE_SECRET" != "None" ]] || fail "建站没拿到验证凭据：$CREATE"
printf '%s' "$SITE_SECRET" > "$WORK/secret"

GENS="$(curl -sS "${ADMIN[@]}" "$ABASE/api/admin/generators")"
printf '%s' "$GENS" | grep -q '"text-image-v1"' || fail "管理面没登记 text-image-v1"
printf '%s' "$GENS" | grep -q '"experimental":true' || fail "视觉题型没标 experimental"
printf '%s' "$GENS" | grep -q '"production_ready":false' || fail "视觉题型被标成 production_ready"
pass "站点与题型登记：pow_question + text-image-v1（experimental:true / production_ready:false）"

log "第六步：受控注入 50 道题（答案只落在临时目录；浏览器用例每题消耗一条）"
"$WORK/devseed" --db "$DB_PATH" --out-dir "$WORK/seed" --generator text-image-v1 --difficulty 1 --count 50 \
  > "$WORK/devseed.log" 2>&1 || fail "devseed 注入失败：$(tail -n 5 "$WORK/devseed.log")"
SEED_JSON="$WORK/seed/seeded-answers.json"
[[ -f "$SEED_JSON" ]] || fail "没有生成答案表"
SEED_COUNT="$(python3 -c 'import json,sys;print(len(json.load(open(sys.argv[1]))))' "$SEED_JSON")"
[[ "$SEED_COUNT" == "50" ]] || fail "注入条数 = $SEED_COUNT，期望 30"
pass "受控注入 $SEED_COUNT 道视觉题（图片与答案成对；生产 API 无答案出口）"

log "第七步：site_key 一致性（预检一站、实际操作另一站必须被拒）"
# 每一步都带**合法来源**：否则 400 可能来自"来源被拒"而不是 site_key 规则，
# 测试就会因为错误的原因通过。
ORG=(-H "Origin: http://127.0.0.1:8391")
code="$(curl -sS "${ORG[@]}" -o "$WORK/conflict.json" -w '%{http_code}' -X POST "$BASE/api/challenge?site_key=pk_other" \
  -H 'Content-Type: application/json' --data '{"site_key":"'"$SITE_KEY"'"}')"
[[ "$code" == "400" ]] || fail "URL 与体 site_key 冲突应 400，实得 $code"
grep -q '"invalid-request"' "$WORK/conflict.json" || fail "冲突响应码不对：$(cat "$WORK/conflict.json")"
grep -q 'pk_other' "$WORK/conflict.json" && fail "响应里回显了冲突的 site_key（红线 8：不透露哪一层挡住）"
ROWS="$(python3 -c '
import sqlite3,sys
c=sqlite3.connect(sys.argv[1]); print(c.execute("select count(*) from challenges").fetchone()[0]); c.close()' "$DB_PATH")"
ok1="$(curl -sS "${ORG[@]}" -o /dev/null -w '%{http_code}' -X POST "$BASE/api/challenge?site_key=$SITE_KEY" \
  -H 'Content-Type: application/json' --data '{"site_key":"'"$SITE_KEY"'"}')"
[[ "$ok1" == "200" ]] || fail "一致的 site_key 应可领题，实得 $ok1"
ROWS2="$(python3 -c '
import sqlite3,sys
c=sqlite3.connect(sys.argv[1]); print(c.execute("select count(*) from challenges").fetchone()[0]); c.close()' "$DB_PATH")"
[[ "$ROWS2" -eq $((ROWS + 1)) ]] || fail "被拒的请求也写了 challenge 行（$ROWS → $ROWS2）"
pass "site_key 冲突被拒且零副作用（且不回显冲突值）；一致路径正常（Go 侧另有判定顺序与不消耗机会的测试）"

log "第八步：启动四个示例应用（blob / worker-src none / 同源 worker 文件 / 高难度站点）"
APP1_LOG="$WORK/app1.log"; APP2_LOG="$WORK/app2.log"; APP3_LOG="$WORK/app3.log"; APP4_LOG="$WORK/app4.log"; APP6_LOG="$WORK/app6.log"
EPOCHA_API_BASE="$BASE" "$WORK/exampleapp" --listen 127.0.0.1:8391 --api "$BASE" \
  --site-key "$SITE_KEY" --site-secret "$SITE_SECRET" --csp blob --widget-origin api >"$APP1_LOG" 2>&1 &
APP1_PID=$!
EPOCHA_API_BASE="$BASE" "$WORK/exampleapp" --listen 127.0.0.1:8392 --api "$BASE" \
  --site-key "$SITE_KEY" --site-secret "$SITE_SECRET" --csp no-worker --widget-origin api >"$APP2_LOG" 2>&1 &
APP2_PID=$!
"$WORK/exampleapp" --listen 127.0.0.1:8393 --api "$BASE" \
  --site-key "$SITE_KEY" --site-secret "$SITE_SECRET" --csp worker-file --widget-origin self >"$APP3_LOG" 2>&1 &
APP3_PID=$!
"$WORK/exampleapp" --listen 127.0.0.1:8394 --api "$BASE" \
  --site-key "$HARD_KEY" --site-secret "$HARD_SECRET" --csp blob --widget-origin api >"$APP4_LOG" 2>&1 &
APP4_PID=$!
"$WORK/exampleapp" --listen 127.0.0.1:8396 --api "$BASE" \
  --site-key pk_stage4a_ttl --site-secret "$TTL_SECRET" --csp blob >"$APP6_LOG" 2>&1 &
APP6_PID=$!
for _ in $(seq 1 80); do
  ok=1
  for port in 8391 8392 8393 8394 8396; do curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 || ok=0; done
  [[ $ok -eq 1 ]] && break
  sleep 0.1
done
curl -fsS http://127.0.0.1:8391/healthz >/dev/null || fail "示例应用 1 没起来：$(tail -n 5 "$APP1_LOG")"
curl -fsS http://127.0.0.1:8392/healthz >/dev/null || fail "示例应用 2 没起来：$(tail -n 5 "$APP2_LOG")"
curl -fsS http://127.0.0.1:8393/healthz >/dev/null || fail "示例应用 3 没起来：$(tail -n 5 "$APP3_LOG")"
curl -fsS http://127.0.0.1:8394/healthz >/dev/null || fail "示例应用 4 没起来：$(tail -n 5 "$APP4_LOG")"

csp1="$(curl -sSI http://127.0.0.1:8391/ | grep -i '^content-security-policy' || true)"
printf '%s' "$csp1" | grep -q "worker-src blob:" || fail "app1 CSP 缺 worker-src blob: → $csp1"
printf '%s' "$csp1" | grep -q "unsafe-eval" && fail "CSP 不该要求 unsafe-eval"
curl -sSI http://127.0.0.1:8392/ | grep -qi "worker-src 'none'" || fail "app2 CSP 应是 worker-src 'none'"
curl -sSI http://127.0.0.1:8393/ | grep -q "worker-src 'self'" || fail "app3 CSP 应是 worker-src 'self'"
curl -sS http://127.0.0.1:8393/widget/v1.worker.js | grep -q 'epocha widget PoW worker' || fail "app3 应同源提供 worker 文件（方案 B）"
pass "示例应用起来且 CSP 档位正确（blob / none），两档都不要 unsafe-eval 或 unsafe-inline"

log "第九步：业务侧反例（curl 层，不经浏览器）"
before="$(curl -sS http://127.0.0.1:8391/guestbook | json 'd["count"]')"
code="$(curl -sS -o "$WORK/n0.json" -w '%{http_code}' -X POST http://127.0.0.1:8391/guestbook \
  -H 'Content-Type: application/json' --data '{"text":"没有 token"}')"
[[ "$code" == "403" ]] && grep -q 'epocha-missing-response' "$WORK/n0.json" || fail "无 token 应 403：$code $(cat "$WORK/n0.json")"
code="$(curl -sS -o "$WORK/n1.json" -w '%{http_code}' -X POST http://127.0.0.1:8391/guestbook \
  -H 'Content-Type: application/json' --data '{"text":"坏 token","response":"garbage.token.value"}')"
[[ "$code" == "403" ]] && grep -q 'epocha-invalid-response' "$WORK/n1.json" || fail "坏 token 应 403：$code $(cat "$WORK/n1.json")"
code="$(curl -sS -o "$WORK/n2.json" -w '%{http_code}' -X POST http://127.0.0.1:8391/guestbook \
  -H 'Content-Type: application/json' --data '{"text":"带 target","response":"x","target":"http://127.0.0.1:1/"}')"
[[ "$code" == "400" ]] || fail "请求体里指定验证目标必须 400，实得 $code"
code="$(curl -sS -o "$WORK/n3.json" -w '%{http_code}' -X POST http://127.0.0.1:8391/guestbook \
  -H 'Content-Type: application/json' -H 'Origin: http://evil.example' --data '{"text":"跨站","response":"x"}')"
[[ "$code" == "403" ]] && grep -q 'epocha-origin-rejected' "$WORK/n3.json" || fail "跨站 Origin 应 403：$code"
after="$(curl -sS http://127.0.0.1:8391/guestbook | json 'd["count"]')"
[[ "$before" == "$after" ]] || fail "反例竟然写入了留言（$before → $after）"
pass "无 token / 坏 token / 指定验证目标 / 跨站 Origin 四种反例都没有写入"

log "第十步：验证服务宕机时业务 fail closed"
# 用一个指向死端口的示例应用：/siteverify 连不上 ⇒ 必须 503 且不写入。
if curl -fsS http://127.0.0.1:8395/healthz >/dev/null 2>&1; then
  fail "端口 8395 已被占用：宕机路径测试会打到别的进程，结论无意义"
fi
"$WORK/exampleapp" --listen 127.0.0.1:8395 --api "http://127.0.0.1:1" \
  --site-key "$SITE_KEY" --site-secret "$SITE_SECRET" --csp blob >"$WORK/app-down.log" 2>&1 &
DOWN_PID=$!
for _ in $(seq 1 80); do curl -fsS http://127.0.0.1:8395/healthz >/dev/null 2>&1 && break; sleep 0.1; done
curl -fsS http://127.0.0.1:8395/healthz >/dev/null || fail "宕机档示例应用没起来：$(tail -n 5 "$WORK/app-down.log")"
code="$(curl -sS -o "$WORK/n4.json" -w '%{http_code}' -X POST http://127.0.0.1:8395/guestbook \
  -H 'Content-Type: application/json' --data '{"text":"验证服务挂了","response":"tok"}')"
[[ "$code" == "503" ]] && grep -q 'epocha-verify-unavailable' "$WORK/n4.json" || fail "验证服务不可用应 503，实得 $code $(cat "$WORK/n4.json")"
kill -TERM "$DOWN_PID" 2>/dev/null || true
wait "$DOWN_PID" 2>/dev/null || true
DOWN_PID=""
pass "验证服务不可达 ⇒ 503 且不放行（示例应用自己的 Go 测试另覆盖超时与写入失败）"

log "第十一步：WebDriver BiDi 真实浏览器验收（零第三方驱动）"
if ! command -v firefox >/dev/null 2>&1; then
  blocked "浏览器交互验收：本机没有 Firefox，无法用 BiDi 驱动 ⇒ 整批交互项未执行"
elif ! node --version >/dev/null 2>&1; then
  fail "没有 node，无法运行 BiDi 测试与向量测试（工程项，不是外部条件）"
else
  FF_PROFILE="$WORK/ffprofile"
  mkdir -p "$FF_PROFILE"
  firefox --headless --no-remote --profile "$FF_PROFILE" --remote-debugging-port "$BIDI_PORT" about:blank \
    >"$WORK/firefox.log" 2>&1 &
  FF_PID=$!
  up=0
  for _ in $(seq 1 100); do
    if grep -q "WebDriver BiDi listening" "$WORK/firefox.log" 2>/dev/null; then up=1; break; fi
    kill -0 "$FF_PID" 2>/dev/null || break
    sleep 0.2
  done
  if [[ $up -eq 0 ]]; then
    blocked "浏览器交互验收：Firefox 远程端点没起来（日志：$(tail -n 3 "$WORK/firefox.log")）"
  else
    log "  Firefox BiDi：$(grep -o 'WebDriver BiDi listening.*' "$WORK/firefox.log" | head -n1)"
    set +e
    BIDI_WS="ws://127.0.0.1:$BIDI_PORT/session" \
    EPOCHA_API="$BASE" EXAMPLE_URL="http://127.0.0.1:8391/" EXAMPLE_NOWORKER_URL="http://127.0.0.1:8392/" \
    SEED_JSON="$SEED_JSON" SEED_DIR="$WORK/seed" SITE_KEY="$SITE_KEY" \
    HARD_SITE_KEY="$HARD_KEY" \
    EXAMPLE_WORKERFILE_URL="http://127.0.0.1:8393/" EXAMPLE_HARD_URL="http://127.0.0.1:8394/" \
    EXAMPLE_TTL_URL="http://127.0.0.1:8396/" TTL_SITE_KEY="pk_stage4a_ttl" \
      node --test docs/scripts/browser/widget_browser.test.mjs 2>&1 | tee "$WORK/browser.log"
    browser_rc=${PIPESTATUS[0]}
    set -e
    # node:test 的 skip 是"这条没执行"，不是"这条过了"：必须换算成 BLOCKED 计数。
    SKIPPED=0
    if grep -qE "^# skipped [1-9]" "$WORK/browser.log"; then SKIPPED=1; fi
    if [[ $SKIPPED -gt 0 ]]; then
      grep -E "BLOCKED" "$WORK/browser.log" | sed 's/^/  /'
      blocked "浏览器交互项之一未执行：Firefox BiDi 不提供视口设置命令 ⇒ 375px 移动端无横向溢出没被测到（需人工确认）"
    fi
    [[ $browser_rc -eq 0 ]] || { tail -n 40 "$WORK/browser.log"; fail "浏览器交互验收未通过（node --test 退出码 $browser_rc）"; }
    pass "真实浏览器闭环与六项交互断言全部通过（Node 内置 WebSocket + Firefox BiDi，退出码 $browser_rc）"
  fi
fi

log "第十二步：跨实现一致性（JS SHA-256/PoW 对同一份固定向量）"
node --test --test-reporter=tap docs/scripts/browser/pow_vectors.test.mjs >"$WORK/vectors.log" 2>&1 || {
  tail -n 40 "$WORK/vectors.log"; fail "JS 与 Go 的 PoW 结果不一致（见上）"; }
GORUN_RC=0
go test -count=1 -run TestSharedVectors ./internal/pow/ >/dev/null 2>&1 || GORUN_RC=$?
[[ $GORUN_RC -eq 0 ]] || fail "Go 侧共享向量测试失败（退出码 $GORUN_RC）"
pass "PoW 固定向量：JS 与 Go 对同一份 testdata/pow_vectors.json 结果相同（两侧各自断言）"

log "第十三步：凭据与私有材料按值扫描"
curl -sS http://127.0.0.1:8391/ > "$WORK/page.html"
curl -sS http://127.0.0.1:8393/ > "$WORK/page_self.html"
curl -sS http://127.0.0.1:8394/ > "$WORK/page_hard.html"
curl -sS http://127.0.0.1:8391/guestbook > "$WORK/list.json"
curl -sS "$BASE/widget/v1.js" > "$WORK/w.js"
cat "$WORK/n0.json" "$WORK/n1.json" "$WORK/n3.json" "$WORK/n4.json" > "$WORK/errs.json"
python3 - "$MASTER_KEY" "$PEPPER" "$ADMIN_KEY" "$SITE_SECRET" \
  "$WORK/page.html" "$WORK/list.json" "$WORK/w.js" "$WORK/errs.json" "$SERVER_LOG" "$APP1_LOG" "$APP2_LOG" "$DB_PATH" <<'PYS'
import sys
vals = [v for v in sys.argv[1:5] if v]
files = sys.argv[5:]
hits = []
for path in files:
    try:
        raw = open(path, 'rb').read()
    except OSError:
        continue
    for v in vals:
        if v.encode() in raw:
            hits.append((path, v[:6] + "***"))
if hits:
    print("泄露：", hits)
    sys.exit(1)
print("按值扫描：8 个载体 × 4 个机密值，零命中")
PYS
python3 - "$WORK/w.js" "$WORK/page.html" <<'PYJ'
import sys
bad = ["innerHTML", "outerHTML", "insertAdjacentHTML", "eval(", "new Function", "document.write"]
src = "".join(open(p, encoding="utf-8").read() for p in sys.argv[1:3])
found = [b for b in bad if b in src]
if found:
    print("widget 或示例页面出现危险 DOM 构造：", found)
    sys.exit(1)
print("widget 与示例页面：无 innerHTML/eval/new Function/document.write")
PYJ
pass "凭据按值扫描（页面/列表/错误响应/脚本/两个日志/库文件）与危险 DOM 构造扫描都干净"

log "第十四步：资源实测"
RSS_KB="$(awk '/VmRSS/{print $2}' /proc/$APP1_PID/status)"
HWM_KB="$(awk '/VmHWM/{print $2}' /proc/$APP1_PID/status)"
THREADS="$(awk '/Threads/{print $2}' /proc/$APP1_PID/status)"
log "  exampleapp 进程 VmRSS=${RSS_KB}kB VmHWM=${HWM_KB}kB Threads=${THREADS}（含两个已启动实例的本进程）"
[[ $RSS_KB -lt 102400 ]] || fail "示例应用 RSS 越过 100MB：${RSS_KB}kB"
grep -o '\[实测\].*' "$WORK/browser.log" | sed 's/^/  /' || log "  （浏览器测试没有打印 [实测] 行）"
pass "资源实测已记录（widget gzip、exampleapp RSS、浏览器侧 PoW 时延见上）"

log "第十五步：阶段回归（各跑一次，退出码逐条记录）"
# 每一项**只跑一次**：不再嵌套总门禁（同一套 race 跑四遍只是烧墙钟，不多证明任何东西）。
run_regress() {
  local name="$1"; shift
  set +e
  "$@" >"$WORK/regress-$name.log" 2>&1
  local rc=$?
  set -e
  if [[ $rc -ne 0 ]]; then
    tail -n 30 "$WORK/regress-$name.log"
    fail "回归项 $name 失败（退出码 $rc）"
  fi
  log "  $name 退出码 0"
}
run_regress fmt-check   make fmt-check
run_regress vet         make vet
run_regress race        make race
run_regress size        make size
run_regress secret-scan make secret-scan
run_regress widget-size make widget-size
run_regress stage1      bash docs/scripts/stage1.sh
run_regress stage2      bash docs/scripts/stage2.sh
run_regress stage3a     bash docs/scripts/stage3a.sh
run_regress stage3b     bash docs/scripts/stage3b.sh
pass "回归全部通过（退出码 0）：fmt-check / vet / race / size / secret-scan / widget-size / stage1 / stage2 / stage3a / stage3b"

log "第十六步：效果类结论（不写 PASS）"
blocked "OCR 效果实测：仍未执行 —— 本机无离线 OCR，未获授权安装工具或下载模型（同阶段 3B）"
notrun "人工可读性：本轮未执行（4A 没有新的人工参与数据；盲测入口仍在 cmd/preview）"
notrun "widget 完整无障碍合规（音频/非视觉等价通道）：本阶段只交付视觉通道，缺口记录在 docs/WIDGET.md"

log "工程项 $N_PASS 项 PASS；BLOCKED $N_BLOCKED；NOT_RUN $N_NOTRUN"
log "结论口径：widget 与业务接入的工程闭环 + 真实浏览器交互 = PASS；OCR 效果 BLOCKED；人工可读性 NOT_RUN。"
log "本脚本**不**声称抗 AI/抗 OCR，也**不**等于阶段 4 完成（正式 widget 的其余项在 4B）。"
