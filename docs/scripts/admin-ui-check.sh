#!/usr/bin/env bash
# 管理控制台 M1 的门禁：Go 层挂载边界与安全头 + 真实浏览器交互验收。
#
# 三态口径（AGENTS.md）：工程项只有 PASS/FAIL；外部条件不足 BLOCKED；本轮没执行 NOT_RUN。
# 退出码：必需项任一 FAIL ⇒ 1；无 FAIL 但有 BLOCKED/NOT_RUN ⇒ 3；全 PASS ⇒ 0。
# 注意 `make` 会把任何非 0 折叠成 2、`go run` 折叠成 1：要拿真码就直接跑本脚本。
#
# 磁盘纪律：构建产物、临时库、演示凭据一律放临时目录（根文件系统空间不足时搬到 tmpfs），
# 不重跑整套 ./... 的 -race，也不跑 OCR 与持续演练——那些不属于本里程碑。
set -euo pipefail

script_exit() {
  local rc="${1:-$?}"
  trap - EXIT INT TERM
  printf '[adminui] SCRIPT_RC=%s\n' "$rc"
  exit "$rc"
}
trap script_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

PUBLIC_PORT="${EPOCHA_ADMINUI_PUBLIC_PORT:-8411}"
ADMIN_PORT="${EPOCHA_ADMINUI_ADMIN_PORT:-8412}"
BIDI_PORT="${EPOCHA_BIDI_PORT:-9344}"

STEP=0
N_PASS=0
N_FAIL=0
N_BLOCKED=0
N_NOTRUN=0
FF_PID=""
SERVER_PID=""
WORK=""
TMPFS_SCRATCH=()

log()     { printf '[adminui] %s\n' "$*"; }
pass()    { STEP=$((STEP + 1)); N_PASS=$((N_PASS + 1)); printf '[adminui] PASS %d: %s\n' "$STEP" "$*"; }
fail()    { STEP=$((STEP + 1)); N_FAIL=$((N_FAIL + 1)); printf '[adminui] FAIL %d: %s\n' "$STEP" "$*" >&2; }
blocked() { STEP=$((STEP + 1)); N_BLOCKED=$((N_BLOCKED + 1)); printf '[adminui] BLOCKED %d: %s\n' "$STEP" "$*"; }
notrun()  { STEP=$((STEP + 1)); N_NOTRUN=$((N_NOTRUN + 1)); printf '[adminui] NOT_RUN %d: %s\n' "$STEP" "$*"; }
abort()   { printf '[adminui] 中止: %s\n' "$*" >&2; exit 1; }

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
cleanup_child() {
  local pid="$1" i
  [[ -n "$pid" ]] || return 0
  if kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null || true
    for i in $(seq 1 50); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
    kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null || true
  fi
  wait "$pid" 2>/dev/null || true
}
start_admin_service() {
  local up=0 c1="000" c2="000" i
  cleanup_child "$SERVER_PID"
  SERVER_PID=""
  EPOCHA_DB="$WORK/data/epocha.db" \
  EPOCHA_LISTEN="127.0.0.1:$PUBLIC_PORT" \
  EPOCHA_ADMIN_LISTEN="127.0.0.1:$ADMIN_PORT" \
  EPOCHA_MASTER_KEY_FILE="$WORK/keys/master" \
  EPOCHA_PEPPER_FILE="$WORK/keys/pepper" \
  EPOCHA_ADMIN_KEY_FILE="$WORK/keys/admin" \
  EPOCHA_POOL_ENABLED=false \
    "$WORK/bin/epocha" >>"$WORK/server.log" 2>&1 &
  SERVER_PID=$!
  for i in $(seq 1 30); do
    kill -0 "$SERVER_PID" 2>/dev/null || break
    c1="$(code_of "http://127.0.0.1:$PUBLIC_PORT/readyz" "$WORK/http/readyz")"
    c2="$(code_of "http://127.0.0.1:$ADMIN_PORT/admin" "$WORK/http/admin")"
    if [[ "$c1" == "200" && "$c2" == "200" ]]; then up=1; break; fi
    sleep 0.2
  done
  if [[ $up -eq 1 ]]; then
    log "本轮服务已就绪（pid=$SERVER_PID readyz=$c1 admin=$c2）"
    return 0
  fi
  cleanup_child "$SERVER_PID"
  SERVER_PID=""
  log "服务启动/恢复未就绪（readyz=$c1 admin=$c2）；日志尾部：$(tail -n 10 "$WORK/server.log" 2>/dev/null)" >&2
  return 1
}
assert_admin_service() {
  local when="${1:-}"
  local ready_rc admin_rc
  ready_rc="$(code_of "http://127.0.0.1:$PUBLIC_PORT/readyz" "$WORK/http/owner-readyz")"
  admin_rc="$(code_of "http://127.0.0.1:$ADMIN_PORT/admin" "$WORK/http/owner-admin")"
  if [[ "$ready_rc" != "200" || "$admin_rc" != "200" ]] || ! kill -0 "$SERVER_PID" 2>/dev/null; then
    fail "${when:-服务归属检查}：本轮服务不存活或响应不对（pid=$SERVER_PID readyz=$ready_rc admin=$admin_rc）；日志尾部：$(tail -n 10 "$WORK/server.log" 2>/dev/null)"
    log "服务归属检查失败已记 FAIL，尝试用本轮原数据库与密钥恢复"
    if start_admin_service; then
      log "服务恢复完成；保留既有 FAIL 结论并继续后续诊断（不重放管理写入）"
    else
      fail "服务恢复失败；后续诊断仍将以有界 HTTP 请求继续"
      SERVER_PID=""
    fi
  fi
}
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  cleanup_child "$FF_PID"
  cleanup_child "$SERVER_PID"
  for path in "${TMPFS_SCRATCH[@]}"; do [[ -n "$path" ]] && rm -rf -- "$path"; done
  [[ -n "$WORK" && -d "$WORK" ]] && rm -rf "$WORK"
  script_exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

curl_status() {
  local outfile="$1" rc=0 code
  shift
  code="$(curl --connect-timeout 2 --max-time 5 -s -o "$outfile" -w '%{http_code}' "$@" 2>/dev/null)" || rc=$?
  printf '%s' "${code:-000}"
  return 0
}
code_of() { curl_status "$2" "$1"; }
# acode_of：带管理凭据的读取；可传第三参数覆盖凭据用于 401 负例。
acode_of() {
  local key="${3:-$ADMIN_KEY_VAL}"
  curl_status "$2" -H "Authorization: Bearer $key" "$1"
}
hdr_of() { curl --connect-timeout 2 --max-time 5 -s -D "$2" -o /dev/null "$1" 2>/dev/null; }

log "第 0 步：端口空闲检查（连到遗留进程会让整轮结论静默失真）"
for port in "$PUBLIC_PORT" "$ADMIN_PORT" "$BIDI_PORT"; do
  if ss -ltn 2>/dev/null | grep -q ":$port "; then
    fail "端口 $port 已被占用：先停掉遗留进程再跑（本脚本用固定端口，占用时测到的不是本轮的服务）"
  fi
done

CACHE_FS="$(df --output=target "$(go env GOCACHE)" 2>/dev/null | tail -n1 | tr -d ' ')"
FREE_KB="$(df --output=avail "$CACHE_FS" 2>/dev/null | tail -n1 | tr -d ' ')"
if [[ -n "$FREE_KB" && "$FREE_KB" -lt 400000 && -w /dev/shm ]]; then
  log "  构建缓存文件系统（$CACHE_FS）只剩 $((FREE_KB / 1024)) MB ⇒ 本轮缓存在 tmpfs 构建"
  export GOCACHE="/dev/shm/epocha-adminui-gocache.$$"
  export TMPDIR="/dev/shm/epocha-adminui-tmp.$$"
  mkdir -p "$GOCACHE" "$TMPDIR"
  TMPFS_SCRATCH=("$GOCACHE" "$TMPDIR")
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/epocha-adminui.XXXXXX")"
chmod 700 "$WORK"
mkdir -p "$WORK/bin" "$WORK/data" "$WORK/keys" "$WORK/http"
chmod 700 "$WORK/keys"

log "第 1 步：构建"
go build -trimpath -ldflags "-s -w" -o "$WORK/bin/epocha" ./cmd/server || abort "服务二进制构建失败"
[[ -s "$WORK/bin/epocha" ]] || abort "二进制为空"
pass "服务二进制构建（$(stat -c %s "$WORK/bin/epocha") 字节，临时目录内）"

log "第 2 步：管理面与页面包的单元测试（-race，只跑本轮涉及的三个包）"
set +e
go test -race -count=1 ./web/adminui ./internal/admin ./cmd/server >"$WORK/gotest.log" 2>&1
gotest_rc=$?
set -e
if [[ $gotest_rc -eq 0 ]]; then
  pass "go test -race ./web/adminui ./internal/admin ./cmd/server"
else
  tail -n 40 "$WORK/gotest.log"
  fail "管理面 Go 测试未通过（退出码 $gotest_rc）"
fi

log "第 3 步：起一套隔离实例（空库、运行时随机凭据、两个 loopback 监听器）"
printf '%s' "$(head -c32 /dev/urandom | base64 | tr -d '\n')" > "$WORK/keys/master"
printf '%s' "$(head -c32 /dev/urandom | base64 | tr -d '\n')" > "$WORK/keys/pepper"
printf '%s' "ak_$(head -c32 /dev/urandom | od -An -tx1 | tr -d ' \n')" > "$WORK/keys/admin"
printf '%s' "ak_$(head -c32 /dev/urandom | od -An -tx1 | tr -d ' \n')" > "$WORK/keys/wrong"
for f in master pepper admin wrong; do
  chmod 600 "$WORK/keys/$f"
  [[ -s "$WORK/keys/$f" ]] || abort "演示凭据文件为空：$WORK/keys/$f"
done
start_admin_service || abort "实例未就绪；日志尾部：$(tail -n 10 "$WORK/server.log")"
ADMIN_KEY_VAL="$(cat "$WORK/keys/admin")"
BAD_KEY_VAL="$(cat "$WORK/keys/wrong")"

log "第 4 步：挂载边界（公共监听器上不得有管理页面与管理 API 的路由）"
pub_bad=0
for path in /admin /admin/ /admin/app.js /admin/app.css /api/admin/sites /api/admin/whoami /api/admin/audit; do
  rc="$(code_of "http://127.0.0.1:$PUBLIC_PORT$path" "$WORK/http/pub")"
  if [[ "$rc" == "200" || "$rc" == "401" ]]; then
    fail "公共端口 $path = $rc（200 表示暴露，401 表示路由挂上了只差鉴权；两者都不接受）"
    pub_bad=1
  else
    log "  公共端口 $path → $rc"
  fi
done
[[ $pub_bad -eq 0 ]] && pass "公共监听器不提供管理页面与管理 API"

log "第 5 步：管理监听器上的静态资源与安全响应头"
head_bad=0
hdr_of "http://127.0.0.1:$ADMIN_PORT/admin" "$WORK/http/h-index" || abort "取不到 /admin 响应头"
hdr_of "http://127.0.0.1:$ADMIN_PORT/admin/app.js" "$WORK/http/h-js" || abort "取不到 /admin/app.js 响应头"
hdr_of "http://127.0.0.1:$ADMIN_PORT/admin/app.css" "$WORK/http/h-css" || abort "取不到 /admin/app.css 响应头"
for h in "$WORK/http/h-index" "$WORK/http/h-js" "$WORK/http/h-css"; do
  grep -qi '^Content-Security-Policy:' "$h" || { fail "$(basename "$h") 没有 CSP 头"; head_bad=1; }
  grep -qi "frame-ancestors 'none'" "$h" || { fail "$(basename "$h") CSP 缺 frame-ancestors 'none'"; head_bad=1; }
  if grep -qi 'unsafe-inline\|unsafe-eval' "$h"; then fail "$(basename "$h") CSP 含 unsafe-inline/unsafe-eval"; head_bad=1; fi
  grep -qi '^Cache-Control: no-store' "$h" || { fail "$(basename "$h") 缺 Cache-Control: no-store"; head_bad=1; }
  grep -qi '^X-Content-Type-Options: nosniff' "$h" || { fail "$(basename "$h") 缺 nosniff"; head_bad=1; }
  grep -qi '^X-Frame-Options: DENY' "$h" || { fail "$(basename "$h") 缺 X-Frame-Options: DENY"; head_bad=1; }
  if grep -qi '^Access-Control-Allow' "$h"; then fail "$(basename "$h") 出现了 CORS 头（管理面不加 CORS）"; head_bad=1; fi
done
[[ $head_bad -eq 0 ]] && pass "管理页面的 CSP/禁缓存/nosniff/DENY/无 CORS 全部就位"

ct="$(tr -d '\r' <"$WORK/http/h-index" | awk 'BEGIN{IGNORECASE=1}/^Content-Type:/{print $2}')"
[[ "$ct" == "text/html;"* ]] && pass "页面 Content-Type=$ct" || fail "页面 Content-Type=$ct"
ctjs="$(tr -d '\r' <"$WORK/http/h-js" | awk 'BEGIN{IGNORECASE=1}/^Content-Type:/{print $2}')"
[[ "$ctjs" == "text/javascript;"* ]] && pass "脚本 Content-Type=$ctjs（配 nosniff 可执行）" || fail "脚本 Content-Type=$ctjs"

log "第 6 步：页面壳未登录可取，管理 API 仍逐个要凭据"
shell_rc="$(code_of "http://127.0.0.1:$ADMIN_PORT/admin" "$WORK/http/shell")"
[[ "$shell_rc" == "200" ]] && pass "未鉴权可取页面壳（$shell_rc，纯静态、不含数据）" || fail "未鉴权取页面壳得到 $shell_rc"
api_rc="$(curl_status "$WORK/http/noauth" "http://127.0.0.1:$ADMIN_PORT/api/admin/whoami")"
[[ "$api_rc" == "401" ]] && pass "未鉴权 /api/admin/whoami = 401（挂页面没有变成鉴权旁路）" || fail "未鉴权 whoami 得到 $api_rc"
bad_rc="$(curl_status "$WORK/http/badkey" -H "Authorization: Bearer $BAD_KEY_VAL" "http://127.0.0.1:$ADMIN_PORT/api/admin/sites")"
[[ "$bad_rc" == "401" ]] && pass "错误凭据 /api/admin/sites = 401" || fail "错误凭据得到 $bad_rc"
if grep -q "$ADMIN_KEY_VAL" "$WORK/http/badkey" "$WORK/http/noauth"; then
  fail "401 响应里出现了凭据片段"
else
  pass "401 响应不回显凭据"
fi

log "第 7 步：静态资源里不得有凭据形状（空输入不算通过：先确认取到了字节）"
for f in shell; do
  sz=$(stat -c %s "$WORK/http/$f")
  [[ $sz -gt 200 ]] || fail "http/$f 只有 $sz 字节，无法据此判断"
done
leak=0
for u in /admin /admin/app.js /admin/app.css; do
  out="$WORK/http/asset-$(echo "$u" | tr '/.' '__')"
  code_of "http://127.0.0.1:$ADMIN_PORT$u" "$out" >/dev/null
  sz="$(stat -c %s "$out")"
  [[ "$sz" -gt 200 ]] || { fail "$u 取回 $sz 字节（过小即视为取失败，不能据此宣布没泄露）"; leak=1; continue; }
  if grep -Eq 'ak_[0-9A-Za-z_-]{20,}|sk_[0-9a-fA-F]{24,}' "$out"; then
    fail "$u 的源代码里含高熵凭据形状"
    leak=1
  fi
done
[[ $leak -eq 0 ]] && pass "三个静态资源的源代码里没有管理密钥/站点 secret 形状（且都非空）"

# 浏览器套件断言"初始空列表"（库里还没有站点），所以必须跑在 API 写入轮**之前**：
# 两者共用同一个库，写入轮先建站会让空状态断言必败（f1cf3fb 引入时的顺序缺陷）。
log "第 8 步：真实浏览器交互验收（Firefox BiDi + node --test，零第三方依赖）"
if ! command -v firefox >/dev/null 2>&1; then
  blocked "浏览器交互验收：本机没有 Firefox ⇒ 十项交互用例整批未执行（本轮不算验收通过）"
elif ! node --version >/dev/null 2>&1; then
  fail "没有 node，无法运行 BiDi 测试（工程项，不是外部条件）"
else
    assert_admin_service "浏览器验收前"
    FF_PROFILE="$WORK/ffprofile"

  mkdir -p "$FF_PROFILE"
  firefox --headless --no-remote --profile "$FF_PROFILE" --remote-debugging-port "$BIDI_PORT" about:blank \
    >"$WORK/firefox.log" 2>&1 &
  FF_PID=$!
  bidi_up=0
  for _ in $(seq 1 100); do
    if grep -q "WebDriver BiDi listening" "$WORK/firefox.log" 2>/dev/null; then bidi_up=1; break; fi
    kill -0 "$FF_PID" 2>/dev/null || break
    sleep 0.2
  done
  if [[ $bidi_up -eq 0 ]]; then
    blocked "浏览器交互验收：Firefox 远程端点没起来（日志：$(tail -n 3 "$WORK/firefox.log")）⇒ 本轮不算验收通过"
  else
    log "  Firefox BiDi：$(grep -o 'WebDriver BiDi listening.*' "$WORK/firefox.log" | head -n1)"
    set +e
    BIDI_WS="ws://127.0.0.1:$BIDI_PORT/session" \
    ADMIN_UI_URL="http://127.0.0.1:$ADMIN_PORT/admin" \
    ADMIN_API="http://127.0.0.1:$ADMIN_PORT" \
    ADMIN_KEY_FILE="$WORK/keys/admin" \
    BAD_ADMIN_KEY="$BAD_KEY_VAL" \
      node docs/scripts/node-tests.mjs --test-reporter=tap docs/scripts/browser/adminui_browser.test.mjs 2>&1 | tee "$WORK/browser.log"
    browser_rc=${PIPESTATUS[0]}
    set -e
    [[ $browser_rc -eq 0 ]] || { tail -n 60 "$WORK/browser.log"; fail "浏览器交互验收未通过（node --test 退出码 $browser_rc）"; }

    assert_admin_service "浏览器验收后"

    manifest_rc=0
    node docs/scripts/release-check.mjs --check-browser-manifest docs/scripts/browser/cases.mjs docs/scripts/browser/adminui_browser.test.mjs "$WORK/browser.log" || manifest_rc=$?
    case "$manifest_rc" in
      0) [[ $browser_rc -eq 0 ]] && pass "管理台真实浏览器用例全部通过，TAP 与统一 manifest 逐项一致" ;;
      3) blocked "管理台浏览器必需用例包含 SKIP/TODO/cancelled，本轮未完整执行" ;;
      *) fail "管理台浏览器 manifest 对账失败（退出码 $manifest_rc）" ;;
    esac
  fi
fi

log "第 9 步：真实写入一轮，确认只读接口不回显 secret"
created="$(curl_status "$WORK/http/create.json" -X POST \
  -H "Authorization: Bearer $ADMIN_KEY_VAL" -H 'Content-Type: application/json' \
  -d '{"name":"门禁站点","verify_mode":"pow_question","generator_id":"arithmetic-baseline-v1","difficulty":3}' \
  "http://127.0.0.1:$ADMIN_PORT/api/admin/sites")"
[[ "$created" == "201" ]] || fail "创建站点应 201，实得 $created：$(cat "$WORK/http/create.json" 2>/dev/null)"
# 提取失败（例如创建就没成功）会得到空串：这里刻意 `|| true` 让流程继续走到
# "前提不成立 ⇒ FAIL"的显式分支，而不是被 set -e 掐死在第 9 步中间。
SECRET_VAL="$(tr -d '\n' <"$WORK/http/create.json" | grep -o '"secret":"[^"]*"' | sed 's/^"secret":"//; s/"$//' || true)"
SITE_ID="$(tr -d '\n' <"$WORK/http/create.json" | sed -n 's/.*"site":{\("id":[0-9]*\).*/\1/p' | cut -d: -f2 || true)"
if [[ -z "$SECRET_VAL" || -z "$SITE_ID" ]]; then
  fail "没能从创建响应里取出 secret 与站点 id（判据的前提不成立）"
else
  scanned=0
  for p in "/api/admin/sites/$SITE_ID" "/api/admin/sites" "/api/admin/sites/$SITE_ID/secrets" \
           "/api/admin/sites/$SITE_ID/origins" "/api/admin/audit?limit=200" "/api/admin/admin-keys"; do
    out="$WORK/http/scan-$(printf '%s' "$p" | tr '/?=&' '____')"
    rc="$(acode_of "http://127.0.0.1:$ADMIN_PORT$p" "$out")"
    if [[ "$rc" != "200" ]]; then
      fail "$p 应 200，实得 $rc（没读到内容就**不能**据此宣布它没泄露 secret）"
      continue
    fi
    if grep -q "$SECRET_VAL" "$out"; then
      fail "$p 回显了 secret 明文"
    else
      scanned=$((scanned + 1))
    fi
  done
  if [[ $scanned -eq 6 ]]; then
    pass "六个只读接口都真实读到 200 且都不回显 secret（扫过 $scanned 个）"
  else
    fail "只确认了 $scanned/6 个只读接口不回显 secret"
  fi

  upd="$(curl_status "$WORK/http/upd.json" -X PUT \
    -H "Authorization: Bearer $ADMIN_KEY_VAL" -H 'Content-Type: application/json' \
    -d '{"name":"门禁站点-改名","pow_bits":15,"rate_site_rpm":321}' \
    "http://127.0.0.1:$ADMIN_PORT/api/admin/sites/$SITE_ID")"
  [[ "$upd" == "200" ]] || fail "改名称/策略应 200，实得 $upd：$(cat "$WORK/http/upd.json" 2>/dev/null)"
  reread="$WORK/http/reread.json"
  rrc="$(acode_of "http://127.0.0.1:$ADMIN_PORT/api/admin/sites/$SITE_ID" "$reread")"
  if [[ "$rrc" != "200" ]]; then
    fail "复读站点应 200，实得 $rrc：$(head -c 200 "$reread" 2>/dev/null)"
  else
    reread_ok=1
    for want in '"name":"门禁站点-改名"' '"pow_bits":15' '"rate_site_rpm":321'; do
      if ! grep -q "$want" "$reread"; then
        fail "复读未反映 $want"
        reread_ok=0
      fi
    done
    if [[ $reread_ok -eq 1 ]]; then
      pass "名称与策略改动落库并可从只读接口复读（复读是 200 的真实站点）"
    fi
  fi

  dup="$(curl_status /dev/null -X PUT \
    -H "Authorization: Bearer $ADMIN_KEY_VAL" -H 'Content-Type: application/json' \
    -d '{"name":"门禁站点-改名"}' "http://127.0.0.1:$ADMIN_PORT/api/admin/sites/$SITE_ID")"
  # 同一个站点把名字改成它自己已经有的值：不该被自己的 UNIQUE 判成冲突。
  [[ "$dup" == "200" ]] || fail "同名重提到自己应 200（不是自撞 UNIQUE），实得 $dup"
  bad="$(curl_status /dev/null -X PUT \
    -H "Authorization: Bearer $ADMIN_KEY_VAL" -H 'Content-Type: application/json' \
    -d '{"name":"  带空白  "}' "http://127.0.0.1:$ADMIN_PORT/api/admin/sites/$SITE_ID")"
  over="$(curl_status /dev/null -X PUT \
    -H "Authorization: Bearer $ADMIN_KEY_VAL" -H 'Content-Type: application/json' \
    -d '{"pow_bits":99}' "http://127.0.0.1:$ADMIN_PORT/api/admin/sites/$SITE_ID")"
  [[ "$bad" == "400" ]] || fail "带空白的名称应 400，实得 $bad"
  [[ "$over" == "400" ]] || fail "pow_bits=99 应 400，实得 $over"
  # 被拒的写入不得留下痕迹：只验证 400 是不够的（半生效比全不生效难查得多）。
  acode_of "http://127.0.0.1:$ADMIN_PORT/api/admin/sites/$SITE_ID" "$WORK/http/after-reject.json" >/dev/null
  reject_ok=1
  for keep in '"name":"门禁站点-改名"' '"pow_bits":15' '"rate_site_rpm":321'; do
    if ! grep -q "$keep" "$WORK/http/after-reject.json"; then
      fail "被 400 拒绝的写入改变了库里的 $keep"
      reject_ok=0
    fi
  done
  if [[ "$bad" == "400" && "$over" == "400" && $reject_ok -eq 1 ]]; then
    pass "名称与越界值由服务端拒绝（400），且被拒的写入没有落库"
  fi

  dis="$(curl_status "$WORK/http/dis.json" -X POST \
    -H "Authorization: Bearer $ADMIN_KEY_VAL" -d '{}' \
    "http://127.0.0.1:$ADMIN_PORT/api/admin/sites/$SITE_ID/disable")"
  [[ "$dis" == "200" ]] || fail "停用应 200，实得 $dis"
  grep -q '"enabled":false' "$WORK/http/dis.json" || fail "停用响应未反映 enabled=false"
  en="$(curl_status "$WORK/http/en.json" -X POST \
    -H "Authorization: Bearer $ADMIN_KEY_VAL" -H 'Content-Type: application/json' -d '{}' \
    "http://127.0.0.1:$ADMIN_PORT/api/admin/sites/$SITE_ID/enable")"
  [[ "$en" == "200" ]] || fail "启用应 200，实得 $en：$(cat "$WORK/http/en.json" 2>/dev/null)"
  grep -q '"enabled":true' "$WORK/http/en.json" || fail "启用响应未反映 enabled=true"
  if [[ "$dis" == "200" && "$en" == "200" ]] && grep -q '"enabled":false' "$WORK/http/dis.json" \
    && grep -q '"enabled":true' "$WORK/http/en.json"; then
    pass "停用/启用走真实端点且状态可见"
  fi
fi

log "第 10 步：人工验收证据（缺失时如实登记 NOT_RUN）"
manual_rc=0
manual_result "adminui.http-errors" "403 与 5xx 两支错误文案的人工交互检查" || manual_rc=$?
manual_result "adminui.clipboard-write" "剪贴板真实写入（回落选中不等于写入成功）" || manual_rc=$?
manual_result "adminui.real-mobile" "真机移动端（BiDi 窄视口不替代真实设备）" || manual_rc=$?

log "结论"
if [[ $N_FAIL -gt 0 ]]; then
  printf '[adminui] RESULT=FAIL pass=%d fail=%d blocked=%d not_run=%d\n' \
    "$N_PASS" "$N_FAIL" "$N_BLOCKED" "$N_NOTRUN"
  exit 1
fi
if [[ $N_BLOCKED -gt 0 || $N_NOTRUN -gt 0 ]]; then
  printf '[adminui] RESULT=INCOMPLETE pass=%d fail=%d blocked=%d not_run=%d\n' \
    "$N_PASS" "$N_FAIL" "$N_BLOCKED" "$N_NOTRUN"
  exit 3
fi
printf '[adminui] RESULT=PASS pass=%d fail=%d blocked=%d not_run=%d\n' \
  "$N_PASS" "$N_FAIL" "$N_BLOCKED" "$N_NOTRUN"
exit 0
