#!/usr/bin/env bash
# stage3b.sh — 阶段 3B 端到端验收（DESIGN §17 阶段 3B、§8.5）。
#
# 覆盖的是**工程闭环**，不是效果结论。检查项：
#   1. 服务二进制不链接任何评测/开发工具（"生产路径没有答案出口"要能被证明）；
#   2. 视觉题完整闭环：注入受控题目 → 领题 → 图片可解码且与声明尺寸一致 → 客户端真实算 PoW →
#      正确答案兑换 → /siteverify 一次性；错误答案终结且不可重试；畸形/缺 answer 不消耗机会；
#   3. 图片资源边界：单题字节、全局图片预算、**图片桶独立水位**（不继承算术题的 50/100/20000）；
#   4. fail closed：池空不降级成 PoW-only；生成器被禁用不签发新题；
#   5. 生命周期回归：重启后终态保持、assigned 不回池、旧 pow_only 与算术题路径不变；
#   6. 泄露扫描：凭据按值扫（响应 / 服务日志 / 库文件原始字节），私有字段按字段名扫；
#   7. 本地预览：secret 不进浏览器、验证目标不可由浏览器指定、拒绝非 loopback 绑定；
#   8. 资源实测数字（PNG 字节分布、生成耗时、库与 WAL 体积、真实服务进程 RSS）。
#
# OCR 与人工可读性**不在**通过条件里，且**不写成 PASS**：环境无离线 OCR ⇒ BLOCKED；
# 本轮没有真人参与 ⇒ NOT_RUN。工程验收与效果验收不能混成一个"全部通过"（ADR-002、DESIGN §8.5）。
#
# 没有读库里的答案当判分依据、没有硬编码固定解、没有绕过验证的分支。
# devseed 注入的是它自己新生成的题（答案由注入侧持有）；这与"读出已有题的答案"是两件事，
# 且它需要主密钥——而主密钥持有者本来就能离线枚举 verifier（DESIGN §4 已列为残余风险）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"
export GOTOOLCHAIN=local

WORK="$(mktemp -d)"
SERVER_PID=""
PREVIEW_PID=""
STEP=0
N_PASS=0
N_BLOCKED=0
N_NOTRUN=0

log()  { printf '[stage3b] %s\n' "$*"; }
# 三态口径（DESIGN §17、ADR-002）：工程测试只有 PASS/FAIL；
# 外部条件不足是 BLOCKED；本轮没做是 NOT_RUN。禁止"PASS：某项未测"这种混合表达。
pass()    { STEP=$((STEP+1)); N_PASS=$((N_PASS+1)); printf '[stage3b] PASS %d: %s\n' "$STEP" "$*"; }
blocked() { STEP=$((STEP+1)); N_BLOCKED=$((N_BLOCKED+1)); printf '[stage3b] BLOCKED %d: %s\n' "$STEP" "$*"; }
notrun()  { STEP=$((STEP+1)); N_NOTRUN=$((N_NOTRUN+1)); printf '[stage3b] NOT_RUN %d: %s\n' "$STEP" "$*"; }
fail() {
  printf '[stage3b] FAIL: %s\n' "$*" >&2
  if [[ -f "${SERVER_LOG:-}" ]]; then
    printf '[stage3b] ---- 服务日志尾部 ----\n' >&2
    tail -n 40 "$SERVER_LOG" >&2 || true
  fi
  exit 1
}
cleanup() {
  local rc=$?
  set +e
  for pid in "$SERVER_PID" "$PREVIEW_PID"; do
    if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
      kill -TERM "$pid" 2>/dev/null
      for _ in $(seq 1 80); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
      kill -KILL "$pid" 2>/dev/null || true
    fi
  done
  rm -rf "$WORK"
  if [[ $rc -ne 0 ]]; then printf '[stage3b] 已清理临时目录并停止进程（退出码 %d）\n' "$rc" >&2; fi
  exit $rc
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

SERVER="$WORK/epocha-server"; DEV="$WORK/devclient"; SEED="$WORK/devseed"
PREVIEW="$WORK/preview"; EVALGEN="$WORK/evalgen"; EVALOCR="$WORK/evalocr"

log "构建全部二进制"
go build -trimpath -ldflags "-s -w" -o "$SERVER" ./cmd/server
go build -trimpath -o "$DEV" ./cmd/devclient
go build -trimpath -o "$SEED" ./cmd/devseed
go build -trimpath -o "$PREVIEW" ./cmd/preview
go build -trimpath -o "$EVALGEN" ./cmd/evalgen
go build -trimpath -o "$EVALOCR" ./cmd/evalocr

log "生成运行时演示密钥（不落仓库、退出即删）"
MASTER_KEY="$(head -c32 /dev/urandom | base64 | tr -d '\n')"
PEPPER="$(head -c32 /dev/urandom | base64 | tr -d '\n')"
ADMIN_KEY="ak_$(head -c32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
IMPORT_SECRET="sk_$(head -c32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
IMPORT_KEY="pk_imported_$(head -c6 /dev/urandom | od -An -tx1 | tr -d ' \n')"
DB_PATH="$WORK/epocha.db"; SERVER_LOG="$WORK/server.log"; BODIES="$WORK/bodies.log"
OUT="$WORK/out"; mkdir -p "$OUT"; : > "$BODIES"
ORIGIN="https://demo.example.com"
printf '[{"name":"imported","site_key":"%s","secret":"%s","pow_bits":12,"allow_missing_origin":true}]\n' \
  "$IMPORT_KEY" "$IMPORT_SECRET" > "$WORK/sites.json"
chmod 600 "$WORK/sites.json"

# 客户端侧 PoW 求解器：脚本自己算（不依赖服务端代算）。bits=12 时是几千次哈希。
cat > "$WORK/powmod.py" <<'PY'
import base64, hashlib

def solve(seed_b64url, bits, max_iter=80_000_000):
    seed = base64.urlsafe_b64decode(seed_b64url + "=" * (-len(seed_b64url) % 4))
    # 前导零比特 = 先 need 个**整字节**全零，再要求第 need+1 字节的高 rem 位为零。
    # （写反成"首字节高 rem 位零 + 其后整字节零"会算出只有 rem 个前导零的解，
    #   服务端按 LeadingZeroBits>=bits 判定时就会拒——本轮实测踩过。）
    need, rem = divmod(bits, 8)
    for n in range(max_iter):
        d = hashlib.sha256(seed + str(n).encode()).digest()
        if not all(b == 0 for b in d[:need]):
            continue
        if rem == 0 or d[need] >> (8 - rem) == 0:
            return str(n)
    raise SystemExit("PoW 未能在上限内求解")
PY
solve_nonce() { # solve_nonce <challenge-json-file>
  python3 -c '
import json, sys
sys.path.insert(0, "'"${WORK}"'")
import powmod
d = json.load(open(sys.argv[1]))
print(powmod.solve(d["pow"]["seed"], int(d["pow"]["bits"])))' "$1"; }

# --- 0. 依赖隔离 ---
DEPS="$WORK/server.deps"
go list -deps ./cmd/server > "$DEPS"
for banned in internal/eval cmd/preview cmd/devseed cmd/evalgen cmd/evalocr web/preview; do
  if grep -q "$banned" "$DEPS"; then
    fail "服务二进制链接了开发/评测代码 $banned（生产路径不得含答案出口或固定熵源）"
  fi
done
grep -q 'golang.org/x/image/font/opentype' "$DEPS" || fail "服务二进制里没有字体光栅路径（视觉题型没接进来？）"
pass "服务二进制含渲染路径但不含任何评测/开发工具（go list -deps 证明）"

# --- 启动与请求辅助 ---
PUB_PORT=""; ADMIN_PORT=""; BASE=""; ABASE=""
AH=(-H "Authorization: Bearer $ADMIN_KEY" -H 'Content-Type: application/json')
start_server() { # start_server <pool-enabled> [KEY=VAL ...]
  local pool_enabled="$1"; shift
  : > "$SERVER_LOG"
  env EPOCHA_MASTER_KEY="$MASTER_KEY" EPOCHA_PEPPER="$PEPPER" EPOCHA_ADMIN_KEY="$ADMIN_KEY" \
      EPOCHA_POOL_ENABLED="$pool_enabled" "$@" \
      "$SERVER" --sites "$WORK/sites.json" --db "$DB_PATH" \
      --listen "127.0.0.1:0" --admin-listen "127.0.0.1:0" >>"$SERVER_LOG" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 300); do
    if ! kill -0 "$SERVER_PID" 2>/dev/null; then fail "服务进程启动后退出：$(tail -n 5 "$SERVER_LOG")"; fi
    if grep -q "site_key=${IMPORT_KEY}" "$SERVER_LOG" && grep -q "msg=已监听" "$SERVER_LOG" \
       && grep -q "msg=管理面已挂载" "$SERVER_LOG"; then
      PUB_PORT="$(sed -n 's/.*msg=已监听 addr=127\.0\.0\.1:\([0-9]*\).*/\1/p' "$SERVER_LOG" | head -n1)"
      ADMIN_PORT="$(sed -n 's/.*msg=管理面已挂载 addr=127\.0\.0\.1:\([0-9]*\).*/\1/p' "$SERVER_LOG" | head -n1)"
      [[ -n "$PUB_PORT" && -n "$ADMIN_PORT" ]] || fail "无法从日志解析监听端口"
      BASE="http://127.0.0.1:${PUB_PORT}"; ABASE="http://127.0.0.1:${ADMIN_PORT}"
      for _ in $(seq 1 60); do curl -fsS -m 2 "$BASE/healthz" >/dev/null 2>&1 && return 0; sleep 0.1; done
      fail "健康检查不通"
    fi
    sleep 0.1
  done
  fail "服务 30s 内未完成启动：$(tail -n 5 "$SERVER_LOG")"
}
stop_server() {
  if kill -0 "$SERVER_PID" 2>/dev/null; then
    kill -TERM "$SERVER_PID"
    for _ in $(seq 1 80); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.1; done
    kill -0 "$SERVER_PID" 2>/dev/null && fail "优雅关闭超时"
  fi
  SERVER_PID=""
}
ac() { local method="$1" path="$2" jsonbody="${3:-}"
  local args=(-s -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$ABASE$path" "${AH[@]}")
  [[ -n "$jsonbody" ]] && args+=(--data "$jsonbody")
  local code; code="$(curl "${args[@]}")"
  cat "$WORK/resp.json" >> "$BODIES"; printf '\n' >> "$BODIES"; echo "$code"; }
pb() { local method="$1" path="$2" jsonbody="$3"; shift 3
  local args=(-s -o "$WORK/resp.json" -w '%{http_code}' -X "$method" "$BASE$path")
  [[ -n "$jsonbody" ]] && args+=(--data "$jsonbody")
  args+=("$@")
  local code; code="$(curl "${args[@]}")"
  cat "$WORK/resp.json" >> "$BODIES"; printf '\n' >> "$BODIES"; echo "$code"; }
body() { cat "$WORK/resp.json"; }
jqpy() { python3 -c "$1" "$WORK/resp.json"; }

# 凭据"只出现一次"要按**揭示之后的响应**来扫：揭示那一条本身就带明文。
Q_REVEAL_LINE=0
check_no_leak() {
  [[ "$Q_REVEAL_LINE" -gt 0 ]] || return 0
  if tail -n +"$((Q_REVEAL_LINE + 1))" "$BODIES" | grep -qF -- "$1"; then
    fail "凭据在它被展示之后的响应里再次出现"
  fi
}

# inject_image <difficulty>：注入一道答案已知的图片题，结果写进 ANSWER / SEED_PNG
inject_image() {
  local diff="$1"
  EPOCHA_MASTER_KEY="$MASTER_KEY" EPOCHA_PEPPER="$PEPPER" \
    "$SEED" --db "$DB_PATH" --out-dir "$OUT" --generator text-image-v1 \
    --difficulty "$diff" --count 1 > "$WORK/seed.log" 2>&1 || fail "devseed 注入失败: $(cat "$WORK/seed.log")"
  ANSWER="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[-1]["answer"])' "$OUT/seeded-answers.json")"
  SEED_PNG="$OUT/$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))[-1]["png"])' "$OUT/seeded-answers.json")"
  [[ ${#ANSWER} -eq 6 ]] || fail "注入答案长度异常：$ANSWER"
  printf '%s' "$ANSWER" > "$WORK/answer.txt"
}

log "第一步：关 worker 启动（先证明池空不降级），再建视觉站点"
start_server false
[[ "$(pb POST /api/challenge '{"site_key":"pk_vis"}' -H "Origin: $ORIGIN")" == "400" ]] \
  || fail "未知站点应 400 invalid-request"
[[ "$(ac POST /api/admin/sites "{\"name\":\"vis\",\"site_key\":\"pk_vis\",\"pow_bits\":12,\"verify_mode\":\"pow_question\",\"generator_id\":\"text-image-v1\",\"difficulty\":2,\"origins\":[\"$ORIGIN\"]}")" == "201" ]] \
  || fail "创建 text-image-v1 站点应 201: $(body)"
V_SECRET="$(jqpy 'import json,sys;print(json.load(open(sys.argv[1]))["secret"])')"
Q_REVEAL_LINE="$(wc -l < "$BODIES")"
[[ "$(ac POST /api/admin/sites '{"name":"ghost","site_key":"pk_ghost","verify_mode":"pow_question","generator_id":"rotate-v1","difficulty":2}')" == "400" ]] \
  || fail "未登记题型必须被管理面拒绝"
pass "管理面可把站点配成 text-image-v1，并拒绝未登记的题型"

[[ "$(pb POST /api/challenge '{"site_key":"pk_vis"}' -H "Origin: $ORIGIN")" == "503" ]] \
  || fail "worker 关闭且池空时应 503，而不是静默只发 PoW"
body | grep -q 'challenge-unavailable' || fail "池空错误码不符: $(body)"
if body | grep -qE '"token"|"pow"'; then fail "池空却返回了 PoW 载荷（静默降级）: $(body)"; fi
pass "池空 ⇒ 503 challenge-unavailable，且响应里没有 pow/token（不降级成 PoW-only）"

ac GET /api/admin/generators >/dev/null
jqpy '
import json, sys
gs = json.load(open(sys.argv[1]))["generators"]
ti = [g for g in gs if g["id"] == "text-image-v1"]; assert len(ti) == 1, ti
g = ti[0]
assert g["media"] == "image" and g["version"] == "v1", g
assert g["experimental"] is True, "视觉题型必须标 experimental"
assert g["production_ready"] is False, "未经评测不得 production_ready=true"
note = g["note"]
assert "OCR" in note and ("不代表" in note or "未完成" in note or "未经" in note), note
ar = [g for g in gs if g["id"] == "arithmetic-baseline-v1"][0]
assert ar["production_ready"] is False and ar["experimental"] is True, ar
' || fail "生成器登记信息没有如实标明 experimental / production_ready=false / 风险说明"
pass "管理面显式标明 text-image-v1 是实验性题型、未生产可用、附风险说明"

log "第二步：注入答案已知的图片题并领取它"
inject_image 2
if grep -q "$ANSWER" "$SERVER_LOG"; then fail "答案出现在服务日志里"; fi
[[ "$(pb POST /api/challenge '{"site_key":"pk_vis"}' -H "Origin: $ORIGIN")" == "200" ]] \
  || fail "领题应 200: $(body)"
jqpy '
import json, sys
d = json.load(open(sys.argv[1]))
assert d["mode"] == "pow_question", d
q = d["question"]
assert set(q) == {"kind","length","charset","answer_type","max_answer_len","image_width",
                  "image_height","generator","generator_version","media","image_mime",
                  "image_base64"}, sorted(q)
assert q["kind"] == "text-image" and q["media"] == "image" and q["image_mime"] == "image/png", q
assert q["length"] == 6 and q["max_answer_len"] == 6, q
assert q["charset"] == "ABCDEFGHJKLMNPQRSTUVWXYZ23456789", q
' || fail "视觉题 DTO 字段集合与 DESIGN §10.1 不符: $(body)"
pass "图片题 DTO 字段集合正确（含 media/image_mime/image_base64，无池行 id）"

python3 - "$(jqpy 'import json,sys;print(json.load(open(sys.argv[1]))["question"]["image_base64"])')" \
  "$SEED_PNG" "$ANSWER" <<'PY' || fail "下发的图片未通过校验"
import base64, struct, sys
b64, seeded_path, answer = sys.argv[1], sys.argv[2], sys.argv[3]
raw = base64.b64decode(b64)
assert raw[:8] == b"\x89PNG\r\n\x1a\n", "不是 PNG"
assert len(raw) <= 65536, "单题资产越界 %d" % len(raw)
i, chunks, w, h, ctype = 8, [], 0, 0, None
while i + 8 <= len(raw):
    (ln,) = struct.unpack(">I", raw[i:i+4]); typ = raw[i+4:i+8].decode("latin1")
    chunks.append(typ)
    if typ == "IHDR":
        w, h, depth, ctype = struct.unpack_from(">IIBB", raw, i + 8)
    i += 12 + ln
assert chunks and chunks[0] == "IHDR" and chunks[-1] == "IEND", chunks
for t in ("tEXt", "iTXt", "zTXt"):
    assert t not in chunks, "PNG 含文本元数据块 " + t
assert ctype in (0, 3), "期望灰度 PNG，got %r" % ctype
disk = open(seeded_path, "rb").read()
assert len(disk) == len(raw), "注入图片 %d B 与下发图片 %d B 不符" % (len(disk), len(raw))
assert answer.encode() not in raw, "答案串出现在图片字节里"
print("图片 %dx%d（colorType=%s）%d 字节，块类型 %s，无文本元数据"
      % (w, h, ctype, len(raw), sorted(set(chunks))))
PY
pass "下发的 base64 可解码为合法灰度 PNG：块类型仅 IHDR/IDAT/IEND、不含答案串"

log "第三步：客户端真实求解 PoW + 正确答案完整闭环"
# 第二步的领题已经把上一条注入用掉了：每一步都自己注入一条，别共享池行
inject_image 2
EPOCHA_SITE_SECRET="$V_SECRET" "$DEV" --base "$BASE" --site-key pk_vis --origin "$ORIGIN" \
  --mode full --answer-file "$WORK/answer.txt" > "$WORK/dev1.log" 2>&1 \
  || fail "视觉题完整闭环失败: $(cat "$WORK/dev1.log")"
grep -q "step=siteverify success=true" "$WORK/dev1.log" || fail "siteverify 未成功: $(cat "$WORK/dev1.log")"
pass "视觉题端到端：客户端算 PoW → 正确答案兑换 → /siteverify success=true"

log "第四步：错误答案终结（同一 token 之后正确答案也不再可用）"
inject_image 2
EPOCHA_SITE_SECRET="$V_SECRET" "$DEV" --base "$BASE" --site-key pk_vis --origin "$ORIGIN" \
  --mode wrong-answer --answer-file "$WORK/answer.txt" > "$WORK/dev2.log" 2>&1 \
  || fail "错误答案语义不符: $(cat "$WORK/dev2.log")"
grep -q "redeem-wrong status=400" "$WORK/dev2.log" || fail "错误答案应得 400: $(cat "$WORK/dev2.log")"
grep -q "redeem-right-after-wrong status=400" "$WORK/dev2.log" || fail "答错后正确答案竟可用: $(cat "$WORK/dev2.log")"
pass "错误答案 ⇒ 400 invalid-challenge 并终结该 challenge；随后正确答案同样被拒"

log "第五步：缺 answer 与畸形 answer 不消耗作答机会（与第四步构成对照）"
inject_image 2
[[ "$(pb POST /api/challenge '{"site_key":"pk_vis"}' -H "Origin: $ORIGIN")" == "200" ]] || fail "领题失败: $(body)"
cp "$WORK/resp.json" "$WORK/ch.json"
TOK="$(jqpy 'import json,sys;print(json.load(open(sys.argv[1]))["token"])')"
NONCE="$(solve_nonce "$WORK/ch.json")"
[[ "$(pb POST /api/redeem "{\"token\":\"$TOK\",\"pow_nonce\":\"$NONCE\"}" -H "Origin: $ORIGIN")" == "400" ]] \
  || fail "缺 answer 应 400: $(body)"
body | grep -q 'invalid-request' || fail "缺 answer 的错误码不符: $(body)"
[[ "$(pb POST /api/redeem "{\"token\":\"$TOK\",\"pow_nonce\":\"$NONCE\",\"answer\":\"AB\"}" -H "Origin: $ORIGIN")" == "400" ]] \
  || fail "长度不符应是畸形（不终态化）: $(body)"
[[ "$(pb POST /api/redeem "{\"token\":\"$TOK\",\"pow_nonce\":\"$NONCE\",\"answer\":\"ABCDI1\"}" -H "Origin: $ORIGIN")" == "400" ]] \
  || fail "含被剔除字符（I/1）应是畸形: $(body)"
[[ "$(pb POST /api/redeem "{\"token\":\"$TOK\",\"pow_nonce\":\"$NONCE\",\"answer\":\"${ANSWER}\"}" -H "Origin: $ORIGIN")" == "200" ]] \
  || fail "畸形提交不该消耗机会，正确答案现在应可兑换: $(body)"
# 用命令替换写文件：它会剥掉尾随换行。直接把 print 的结果重定向进去会留下 \n，
# 那会让 token 验签失败 ⇒ 把"一次性"测成"无效 token"，是个会掩盖真问题的假阳性来源。
printf '%s' "$(jqpy 'import json,sys;print(json.load(open(sys.argv[1]))["response"])')" > "$WORK/resp.token"
pass "缺 answer / 长度不符 / 含 I·1 一律 invalid-request 且**不终态化**；随后正确答案成功"

log "第六步：重启后终态与一次性仍成立、assigned 不回池"
inject_image 2
[[ "$(pb POST /api/challenge '{"site_key":"pk_vis"}' -H "Origin: $ORIGIN")" == "200" ]] || fail "领题失败: $(body)"
cp "$WORK/resp.json" "$WORK/ch2.json"
TOK2="$(jqpy 'import json,sys;print(json.load(open(sys.argv[1]))["token"])')"
NONCE2="$(solve_nonce "$WORK/ch2.json")"
[[ "$(pb POST /api/redeem "{\"token\":\"$TOK2\",\"pow_nonce\":\"$NONCE2\",\"answer\":\"ZZZZZZ\"}" -H "Origin: $ORIGIN")" == "400" ]] \
  || fail "重启前错误答案应 400"
ANSWER3="$ANSWER"
stop_server
start_server false
# 第五步只兑换、没验证过，所以重启后**第一次**验证应当仍然成功（未消费状态在库里持久化），
# 第二次才必须被拒（used=1 也在库里持久化，不是内存）。
FIRST="$(curl -s -XPOST "$BASE/siteverify" \
  --data-urlencode "secret=$V_SECRET" --data-urlencode "response@$WORK/resp.token")"
echo "$FIRST" | grep -qE '"success": *true' \
  || fail "重启后首次验证应成功（未消费状态要持久化）: $FIRST"
# verify-only 的语义：重放被正确拒绝 ⇒ 打印 success=false 并正常退出；只有重放成功才报错退出
EPOCHA_SITE_SECRET="$V_SECRET" "$DEV" --base "$BASE" --site-key pk_vis \
  --mode verify-only --response-file "$WORK/resp.token" > "$WORK/replay.log" 2>&1 || true
grep -q "success=false" "$WORK/replay.log" || fail "重启后重放已消费 response 竟然成功: $(cat "$WORK/replay.log")"
grep -q "timeout-or-duplicate" "$WORK/replay.log" \
  || fail "重放的错误码应是 timeout-or-duplicate（reCAPTCHA 语义）: $(cat "$WORK/replay.log")"
[[ "$(pb POST /api/redeem "{\"token\":\"$TOK2\",\"pow_nonce\":\"$NONCE2\",\"answer\":\"$ANSWER3\"}" -H "Origin: $ORIGIN")" == "400" ]] \
  || fail "重启后终态 challenge 竟可重放: $(body)"
# 再注入一条**新**题：上面那条已经被错误答案终结，池里此时应该是空的。
# 先确认"终态行不会被重新发出"（领不到题 ⇒ 503），再确认新注入的可被领到。
[[ "$(pb POST /api/challenge '{"site_key":"pk_vis"}' -H "Origin: $ORIGIN")" == "503" ]] \
  || fail "被终结的题竟回到了池里（assigned/terminal 不得回 ready）: $(body)"
inject_image 2
[[ "$(pb POST /api/challenge '{"site_key":"pk_vis"}' -H "Origin: $ORIGIN")" == "200" ]] \
  || fail "重启后应仍能领到新注入的题: $(body)"
python3 - "$(jqpy 'import json,sys;print(json.load(open(sys.argv[1]))["question"]["image_base64"])')" \
  "$SEED_PNG" <<'PY' || fail "重启后领到的题与注入的题不是同一道"
import base64, sys
assert len(base64.b64decode(sys.argv[1])) == len(open(sys.argv[2], "rb").read())
PY
pass "重启后：已消费 response 仍不可重放、终态 challenge 不可重放、assigned/terminal 题不再发出"

log "第七步：打开 worker，验证图片桶用**自己的**水位与全局字节预算"
stop_server
start_server true EPOCHA_POOL_LOW_WATERMARK=40 EPOCHA_POOL_HIGH_WATERMARK=50 \
  EPOCHA_POOL_IMAGE_LOW_WATERMARK=2 EPOCHA_POOL_IMAGE_HIGH_WATERMARK=3 \
  EPOCHA_POOL_IMAGE_MAX_TOTAL_BYTES=131072 EPOCHA_POOL_POLL_MS=200
[[ "$(ac POST /api/admin/sites "{\"name\":\"arith\",\"site_key\":\"pk_arith\",\"pow_bits\":12,\"verify_mode\":\"pow_question\",\"generator_id\":\"arithmetic-baseline-v1\",\"difficulty\":3,\"origins\":[\"$ORIGIN\"]}")" == "201" ]] \
  || fail "创建算术站点失败: $(body)"
# 每个站点各自的验证凭据：拿错站点的 secret 会得到 invalid-input-response（sid 不一致），
# 而不是 invalid-input-secret——这两者搞混会让下面的回归测试报出一个误导性的失败原因。
ARITH_SECRET="$(jqpy 'import json,sys;print(json.load(open(sys.argv[1]))["secret"])')"
Q_REVEAL_LINE="$(wc -l < "$BODIES")"
# 两个桶都要等到：图片桶补到自己的高水位，算术桶明显越过 3（证明它用的是另一组水位）。
for _ in $(seq 1 200); do
  ac GET /api/admin/pool >/dev/null
  if jqpy '
import json, sys
d = json.load(open(sys.argv[1]))
b = d["buckets"]
img = [x for x in b if x["generator_id"] == "text-image-v1"]
ar = [x for x in b if x["generator_id"] == "arithmetic-baseline-v1"]
assert img and ar, [x["generator_id"] for x in b]
assert img[0]["ready"] >= 2, img[0]
assert ar[0]["ready"] > 3, ar[0]
' 2>/dev/null; then break; fi
  sleep 0.25
done
ac GET /api/admin/pool >/dev/null
jqpy '
import json, sys
d = json.load(open(sys.argv[1]))
b = d["buckets"]
img = [x for x in b if x["generator_id"] == "text-image-v1"]
ar = [x for x in b if x["generator_id"] == "arithmetic-baseline-v1"]
assert img and ar, "两个桶都应存在：%r" % [x["generator_id"] for x in b]
assert 2 <= img[0]["ready"] <= 3, "图片桶必须停在图片高水位 3：%r" % img[0]
assert ar[0]["ready"] > 3, "算术桶必须按自己的水位增长（>3）：%r" % ar[0]
assert img[0]["ready_asset_bytes"] > 0, img[0]
im = d["image"]
assert 0 < im["asset_bytes"] <= im["budget_bytes"], im
assert im["per_question_max_bytes"] == 65536, im
' || fail "图片桶水位/字节预算的独立性没有生效: $(body)"
pass "图片桶停在自己的高水位（≤3）而算术桶按 40/50 增长；全局图片字节预算被如实观测"

log "第八步：把预算压到一条题的量级，确认它先于水位生效且不白烧渲染"
stop_server
start_server true EPOCHA_POOL_LOW_WATERMARK=40 EPOCHA_POOL_HIGH_WATERMARK=50 \
  EPOCHA_POOL_IMAGE_LOW_WATERMARK=2 EPOCHA_POOL_IMAGE_HIGH_WATERMARK=3 \
  EPOCHA_POOL_IMAGE_MAX_TOTAL_BYTES=65536 EPOCHA_POOL_POLL_MS=200
BUDGET_OK=0
for _ in $(seq 1 120); do
  ac GET /api/admin/pool >/dev/null
  if jqpy '
import json, sys
d = json.load(open(sys.argv[1]))
im = d["image"]
assert im["asset_bytes"] <= im["budget_bytes"], im
# 预算必须真的处于"再放一条最坏情况的图就放不下"的状态，否则这条测试没触达预算分支
assert im["asset_bytes"] + im["per_question_max_bytes"] > im["budget_bytes"], im
w = d.get("worker", {})
assert w.get("rejected", 0) >= 1, w
assert w.get("last_error") == "image-budget", w
' 2>/dev/null; then BUDGET_OK=1; break; fi
  sleep 0.25
done
(( BUDGET_OK == 1 )) || fail "图片字节预算没有生效（worker 未报 image-budget）: $(body)"
pass "全局图片字节预算在写入侧原子生效（asset_bytes ≤ budget），worker 报 image-budget 而非无限生成"

log "第九步：生成器禁用（EPOCHA_GENERATORS 白名单）后不再签发新题"
stop_server
start_server false EPOCHA_GENERATORS=arithmetic-baseline-v1
[[ "$(pb POST /api/challenge '{"site_key":"pk_vis"}' -H "Origin: $ORIGIN")" == "503" ]] \
  || fail "禁用 text-image-v1 后该站点应 503（不降级）: $(body)"
grep -q "生成器已按白名单禁用" "$SERVER_LOG" || fail "启动日志没记录被禁用的生成器（看不见就等于没生效）"
[[ "$(pb POST /api/challenge '{"site_key":"pk_arith"}' -H "Origin: $ORIGIN")" == "200" ]] \
  || fail "算术站点不应受影响: $(body)"
pass "EPOCHA_GENERATORS 禁用生成器后：视觉站点 503 不降级、算术站点照常"

log "第十步：旧路径回归（pow_only 与 arithmetic-baseline 完整闭环）"
EPOCHA_SITE_SECRET="$IMPORT_SECRET" "$DEV" --base "$BASE" --site-key "$IMPORT_KEY" \
  --mode full > "$WORK/legacy-pow.log" 2>&1 || fail "pow_only 回归失败: $(cat "$WORK/legacy-pow.log")"
grep -q "step=siteverify success=true" "$WORK/legacy-pow.log" || fail "pow_only 未通过: $(cat "$WORK/legacy-pow.log")"
EPOCHA_SITE_SECRET="$IMPORT_SECRET" "$DEV" --base "$BASE" --site-key "$IMPORT_KEY" \
  --mode double-verify > "$WORK/legacy-dv.log" 2>&1 || fail "pow_only 一次性回归失败: $(cat "$WORK/legacy-dv.log")"
pass "阶段 1/2 的 pow_only 流程未受影响（含 response 一次性）"

stop_server
start_server true EPOCHA_POOL_LOW_WATERMARK=6 EPOCHA_POOL_HIGH_WATERMARK=10 \
  EPOCHA_POOL_IMAGE_LOW_WATERMARK=1 EPOCHA_POOL_IMAGE_HIGH_WATERMARK=2 \
  EPOCHA_POOL_IMAGE_MAX_TOTAL_BYTES=262144 EPOCHA_POOL_POLL_MS=200
ARITH_OK=0
for _ in $(seq 1 150); do
  if EPOCHA_SITE_SECRET="$ARITH_SECRET" "$DEV" --base "$BASE" --site-key pk_arith \
       --origin "$ORIGIN" --mode full > "$WORK/arith.log" 2>&1; then ARITH_OK=1; break; fi
  sleep 0.2
done
(( ARITH_OK == 1 )) || fail "算术站点闭环失败（后台 worker 应已备好题）: $(cat "$WORK/arith.log" 2>/dev/null)"
if grep -q "source=injected-file" "$WORK/arith.log"; then
  fail "算术题应由客户端独立求值，不该走注入答案"
fi
pass "阶段 3A 的 arithmetic-baseline 流程未受影响（客户端仍独立求值）"

log "第十一步：泄露扫描（凭据按值、私有字段按名）"
for value in "$ADMIN_KEY" "$MASTER_KEY" "$PEPPER" "$IMPORT_SECRET"; do
  if grep -qF -- "$value" "$SERVER_LOG"; then fail "服务日志含凭据"; fi
  if grep -qF -- "$value" "$BODIES"; then fail "响应体含凭据"; fi
  if grep -a -qF -- "$value" "$DB_PATH"; then fail "库文件原始字节含凭据"; fi
done
check_no_leak "$V_SECRET"
# "只出现一次"要按**次数**验：揭示那一条本身带明文，之后的每一条都不该再带。
# 无条件 grep 整份响应集合会把那一次揭示也算成泄露，从而在真出问题时反而被忽略。
secret_reveals() { # secret_reveals <value> <label>
  local n
  n="$(grep -cF -- "$1" "$BODIES" || true)"
  [[ "$n" == "1" ]] || fail "$2 在响应集合里出现 $n 次（应恰好 1 次：只在创建时展示）"
}
secret_reveals "$V_SECRET" "视觉站点 secret"
secret_reveals "$ARITH_SECRET" "算术站点 secret"
if grep -qF -- "$V_SECRET" "$SERVER_LOG"; then fail "站点 secret 出现在服务日志里"; fi
if grep -a -qF -- "$V_SECRET" "$DB_PATH"; then fail "库文件里有站点 secret 明文"; fi
if grep -qE '"answer"|"verifier"|"payload_json"|"private_verifier"' "$SERVER_LOG"; then
  fail "服务日志出现私有字段名"
fi
if grep -qF -- "$ANSWER3" "$BODIES"; then fail "答案串出现在 HTTP 响应里"; fi
pass "凭据按值扫描（响应/日志/库文件）与私有字段按名扫描均无泄露"

log "第十二步：本地预览（独立空库 + 关掉 worker，让领取是确定的）"
if "$PREVIEW" --listen "0.0.0.0:18099" --api "$BASE" --admin "$ABASE" --admin-key "$ADMIN_KEY" \
     > "$WORK/preview-bad.log" 2>&1; then
  fail "预览竟绑定了非 loopback 地址"
fi
grep -qi "loopback" "$WORK/preview-bad.log" || fail "拒绝非 loopback 时没有说明原因: $(cat "$WORK/preview-bad.log")"
pass "预览拒绝在非 loopback 地址上监听（是检查，不是默认值）"

# 预览流程需要一个"池里只有我注入的那一条"的确定环境，因此换一把空库、关掉 worker。
stop_server
DB_PATH="$WORK/preview.db"
start_server false
PREVIEW_PORT=$((18000 + RANDOM % 900))
"$PREVIEW" --listen "127.0.0.1:${PREVIEW_PORT}" --api "$BASE" --admin "$ABASE" \
  --admin-key "$ADMIN_KEY" --difficulty 2 --pow-bits 12 \
  > "$WORK/preview.log" 2>&1 &
PREVIEW_PID=$!
PBASE="http://127.0.0.1:${PREVIEW_PORT}"
PAGE_OK=0
for _ in $(seq 1 100); do
  if curl -fsS -m 2 "$PBASE/" -o "$WORK/page.html"; then PAGE_OK=1; break; fi
  sleep 0.2
done
(( PAGE_OK == 1 )) || fail "预览页没起来: $(tail -n 5 "$WORK/preview.log")"
grep -q "text-image-v1 本地预览" "$WORK/page.html" || fail "预览页内容异常"
P_SITE_KEY="$(sed -n 's/.*site_key=\([A-Za-z0-9_]*\).*/\1/p' "$WORK/preview.log" | head -n1)"
[[ -n "$P_SITE_KEY" ]] || fail "无法从预览日志取它自建的演示站点 site_key"
if grep -qF -- "$ADMIN_KEY" "$WORK/page.html"; then fail "页面含管理凭据"; fi
if grep -qE 'sk_[0-9a-f]{20,}' "$WORK/page.html"; then fail "页面里出现了形似站点 secret 的串"; fi

for bad in '{"response":"x","target":"http://169.254.169.254/"}' '{"url":"http://evil/"}' \
           '{"response":""}' 'not json' '{"response":"x"}{"response":"y"}'; do
  code="$(curl -s -o "$WORK/pv.json" -w '%{http_code}' -X POST "$PBASE/demo/verify" \
          -H 'Content-Type: application/json' --data "$bad")"
  [[ "$code" == "200" ]] && fail "预览后端竟接受了 $bad"
done
if grep -q "siteverify 调用失败" "$WORK/preview.log"; then
  fail "被拒的请求触达了上游（那就是 SSRF 门）"
fi
pass "预览 /demo/verify 拒绝任何多余字段：验证目标不可由浏览器指定"

# 真跑一次演示后端：向预览自建的站点领题 → 客户端解 PoW → 把 response 交给 /demo/verify
inject_image 2
ORIGIN_PV="http://127.0.0.1:${PREVIEW_PORT}"
[[ "$(pb POST /api/challenge "{\"site_key\":\"$P_SITE_KEY\"}" -H "Origin: $ORIGIN_PV")" == "200" ]] \
  || fail "预览站点领题失败: $(body)"
cp "$WORK/resp.json" "$WORK/chpv.json"
TOKP="$(jqpy 'import json,sys;print(json.load(open(sys.argv[1]))["token"])')"
NONCEP="$(solve_nonce "$WORK/chpv.json")"
python3 - "$(jqpy 'import json,sys;print(json.load(open(sys.argv[1]))["question"]["image_base64"])')" \
  "$SEED_PNG" <<'PYCHK' || fail "预览站点领到的不是注入的那道题"
import base64, sys
assert len(base64.b64decode(sys.argv[1])) == len(open(sys.argv[2], "rb").read())
PYCHK
[[ "$(pb POST /api/redeem "{\"token\":\"$TOKP\",\"pow_nonce\":\"$NONCEP\",\"answer\":\"${ANSWER}\"}" -H "Origin: $ORIGIN_PV")" == "200" ]] \
  || fail "预览站点兑换失败: $(body)"
RESP_P="$(jqpy 'import json,sys;print(json.load(open(sys.argv[1]))["response"])')"
code="$(curl -s -o "$WORK/pvok.json" -w '%{http_code}' -X POST "$PBASE/demo/verify" \
        -H 'Content-Type: application/json' --data "{\"response\":\"$RESP_P\"}")"
[[ "$code" == "200" ]] || fail "演示后端 verify 状态异常: $(cat "$WORK/pvok.json")"
grep -qE '"success": *true' "$WORK/pvok.json" || fail "演示后端未验证成功: $(cat "$WORK/pvok.json")"
if grep -qF -- "$RESP_P" "$WORK/pvok.json"; then fail "verify 响应回显了 response 全文"; fi
if grep -qE 'sk_[0-9a-f]{20,}' "$WORK/pvok.json"; then fail "verify 响应含站点 secret"; fi
curl -sS -D "$WORK/pvheaders.txt" "$PBASE/" -o /dev/null
if grep -qi '^access-control-allow-origin' "$WORK/pvheaders.txt"; then fail "预览开了 CORS"; fi
grep -qi 'cache-control: no-store' "$WORK/pvheaders.txt" || fail "预览响应必须 no-store"
pass "演示后端持 secret 调用 /siteverify 成功；secret 与 response 全文都不进浏览器"

log "第十三步：资源实测（数字回填 docs/STAGE3B.md，条件一并写明）"
log "二进制（-trimpath -ldflags -s -w）= $(stat -c %s "$SERVER") 字节"
mkdir -p "$WORK/eval"
"$EVALGEN" --set dev --count 60 --difficulty 5 --out "$WORK/eval" > "$WORK/evalgen.log" 2>&1 \
  || fail "evalgen 失败: $(cat "$WORK/evalgen.log")"
python3 - "$WORK/eval" <<'PYEVAL'
import glob, os, statistics, sys
dirs = sorted(glob.glob(os.path.join(sys.argv[1], "dev-*")))
assert dirs, "evalgen 没产出目录"
sizes = [os.path.getsize(f) for f in sorted(glob.glob(os.path.join(dirs[-1], "sample-*.png")))]
assert len(sizes) >= 60, len(sizes)
print("样本 %d 张（难度 5）PNG 字节：min %d / median %d / mean %d / max %d"
      % (len(sizes), min(sizes), int(statistics.median(sizes)), int(sum(sizes) / len(sizes)), max(sizes)))
PYEVAL
# 生成与写入成本：20 条难度 5 的题（生成 + PNG 编码 + 写库 + 落盘），报每次一条的均值
T0=$(date +%s%N)
for _ in $(seq 1 4); do
  EPOCHA_MASTER_KEY="$MASTER_KEY" EPOCHA_PEPPER="$PEPPER" \
    "$SEED" --db "$DB_PATH" --out-dir "$WORK/genbench" --generator text-image-v1 \
    --difficulty 5 --count 5 > /dev/null 2>&1 || fail "生成计时注入失败"
done
T1=$(date +%s%N)
GEN_MS=$(( (T1 - T0) / 1000000 ))
log "20 条视觉题（难度 5，生成+PNG+写库+落盘）总耗时 ${GEN_MS}ms ⇒ 约 $((GEN_MS / 20))ms/条"

# 把视觉桶补到默认高水位，再跑固定次数的混合闭环，然后读**真实服务进程**的驻留与峰值。
# 混合闭环走 pow_only 与 arithmetic 两条路（答案可独立求出，不需要注入）；
# 与内存/磁盘相关的是"池里有多少条图片资产"，所以图片侧测的是填满后的驻留形状。
stop_server
DB_PATH="$WORK/mix.db"
start_server true EPOCHA_POOL_LOW_WATERMARK=20 EPOCHA_POOL_HIGH_WATERMARK=40 \
  EPOCHA_POOL_IMAGE_LOW_WATERMARK=20 EPOCHA_POOL_IMAGE_HIGH_WATERMARK=24 \
  EPOCHA_POOL_IMAGE_MAX_TOTAL_BYTES=4194304 EPOCHA_POOL_POLL_MS=200
[[ "$(ac POST /api/admin/sites "{\"name\":\"mixvis\",\"site_key\":\"pk_mixvis\",\"pow_bits\":12,\"verify_mode\":\"pow_question\",\"generator_id\":\"text-image-v1\",\"difficulty\":2,\"origins\":[\"$ORIGIN\"]}")" == "201" ]] || fail "混合测量站点创建失败: $(body)"
[[ "$(ac POST /api/admin/sites "{\"name\":\"mixarith\",\"site_key\":\"pk_mixarith\",\"pow_bits\":12,\"verify_mode\":\"pow_question\",\"generator_id\":\"arithmetic-baseline-v1\",\"difficulty\":3,\"origins\":[\"$ORIGIN\"]}")" == "201" ]] || fail "算术测量站点创建失败: $(body)"
MIX_SECRET="$(jqpy 'import json,sys;print(json.load(open(sys.argv[1]))["secret"])')"
IMG_FILLED=0
for _ in $(seq 1 200); do
  ac GET /api/admin/pool >/dev/null
  if jqpy '
import json, sys
b = json.load(open(sys.argv[1]))["buckets"]
img = [x for x in b if x["generator_id"] == "text-image-v1"]
assert img and img[0]["ready"] >= 20, img
arith = [x for x in b if x["generator_id"] == "arithmetic-baseline-v1"]
assert arith and arith[0]["ready"] >= 20, arith
' 2>/dev/null; then IMG_FILLED=1; break; fi
  sleep 0.25
done
(( IMG_FILLED == 1 )) || fail "图片/算术桶没有在 50s 内都补到 20 条"
LOOP_ERR=0
for i in $(seq 1 10); do
  EPOCHA_SITE_SECRET="$MIX_SECRET" "$DEV" --base "$BASE" --site-key pk_mixarith --origin "$ORIGIN" \
    --mode full > /dev/null 2>&1 || LOOP_ERR=$((LOOP_ERR+1))
  EPOCHA_SITE_SECRET="$IMPORT_SECRET" "$DEV" --base "$BASE" --site-key "$IMPORT_KEY" \
    --mode full > /dev/null 2>&1 || LOOP_ERR=$((LOOP_ERR+1))
done
ac GET /api/admin/pool >/dev/null
SRSS_KB="$(awk '/VmRSS/{print $2}' /proc/$SERVER_PID/status)"
SHWM_KB="$(awk '/VmHWM/{print $2}' /proc/$SERVER_PID/status)"
STHR="$(awk '/Threads/{print $2}' /proc/$SERVER_PID/status)"
DBSIZE="$(stat -c %s "$DB_PATH")"
WALSIZE=0
[[ -f "$DB_PATH-wal" ]] && WALSIZE="$(stat -c %s "$DB_PATH-wal")"
# 写入进行中只 stat 主文件会得到一个假数（3A 实测踩过：刚补完题的库被量成 4096 字节）。
# 因此另外量一次 checkpoint 之后的净体积，两个数都报。
CPSIZE="$(python3 -c "
import sqlite3, os, sys
c = sqlite3.connect(sys.argv[1])
c.execute('PRAGMA wal_checkpoint(TRUNCATE)')
c.close()
print(os.path.getsize(sys.argv[1]))" "$DB_PATH")"
python3 - "$DBSIZE" "$WALSIZE" "$SRSS_KB" "$SHWM_KB" "$STHR" "$GEN_MS" "$LOOP_ERR" "$CPSIZE" <<'PYRSS'
import sys
db, wal, rss, hwm, thr, gen, errs, cpsz = (int(x) for x in sys.argv[1:9])
print("视觉桶填满（≥20 条 ready）+ 20 次混合闭环后：")
print("  库主文件 %s B（+ WAL %s B）；wal_checkpoint(TRUNCATE) 之后 %s B"
      % (f"{db:,}", f"{wal:,}", f"{cpsz:,}"))
print("  服务进程 VmRSS=%dkB VmHWM=%dkB Threads=%s（闭环失败 %d 次）" % (rss, hwm, thr, errs))
assert errs == 0, "混合闭环出现 %d 次失败" % errs
assert rss < 100 * 1024, "RSS 越过 DESIGN §13 的 100MB 目标线：%d kB" % rss
print("  20 条视觉题注入总耗时 %dms" % gen)
PYRSS
pass "资源实测已记录（PNG 分布、生成耗时、填满视觉桶后的库与 WAL 体积、真实服务进程 RSS/VmHWM/Threads + 20 次混合闭环零失败）"

log "第十四步：OCR 基线（阻塞检测属于工程测试；效果实测按外部条件单列）"
GT_CANDIDATES=("$WORK"/eval/dev-*/ground-truth.jsonl)
GT="${GT_CANDIDATES[0]}"
[[ -f "$GT" ]] || fail "没有 ground-truth.jsonl（evalgen 步骤没产出样本）"
OCR_BIN=""
for cand in tesseract gocr cuneiform; do
  if command -v "$cand" >/dev/null 2>&1; then OCR_BIN="$cand"; break; fi
done

# **退出码逐层核实**（本机 go1.24.5/linux-arm64 实测，2026-10-18）：
#   evalocr 二进制 = 3；`go run` 把任何非 0 折叠成 **1**（只在 stderr 打 "exit status 3"）；
#   `make` 把任何配方失败折叠成 **2**。所以"包装层会透传 3"是错的假设，
#   这里按各层真实形状断言，而不是要求三层都等于 3。
rc_bin=0
"$EVALOCR" --groundtruth "$GT" --ocr-cmd tesseract --limit 60 >/dev/null 2>&1 || rc_bin=$?
rc_gorun=0
gorun_err="$(go run ./cmd/evalocr --groundtruth "$GT" --ocr-cmd tesseract --limit 60 2>&1 >/dev/null)" || rc_gorun=$?
rc_present=0
if command -v tesseract >/dev/null 2>&1 || command -v gocr >/dev/null 2>&1 || command -v cuneiform >/dev/null 2>&1; then
  rc_present=1
fi
if [[ $rc_present -eq 0 ]]; then
  [[ $rc_bin -eq 3 ]]    || fail "缺工具时 evalocr 二进制应退出码 3（未测），实得 $rc_bin"
  [[ $rc_gorun -eq 1 ]]  || fail "缺工具时 go run 包装应折叠成 1，实得 $rc_gorun"
  [[ "$gorun_err" == *"exit status 3"* ]] || fail "go run 的 stderr 应带真实退出码 3（否则无法区分折叠原因）：$gorun_err"
  pass "阻塞检测可验证：缺离线 OCR 时退出码分层为 二进制=$rc_bin / go run=$rc_gorun（stderr 含 exit status 3）/ make=2"
  blocked "OCR 效果实测：未执行——本机没有 tesseract/gocr/cuneiform，且未获授权安装工具或下载模型（M4 每次尝试成本因此未知）"
else
  "$EVALOCR" --groundtruth "$GT" --ocr-cmd "${OCR_BIN:-tesseract}" --limit 60 || fail "OCR 评测执行失败"
  pass "OCR 基线已执行（通过率与工具版本见上）"
fi

log "第十五步：人工可读性（不得由 agent 代填）"
log "  盲测入口：浏览器打开 $PBASE/ → 展开「盲测模式」→ 开始盲测"
log "  本轮**没有**人工参与数据 ⇒ 报告里写 NOT_RUN，不能由 agent 代填冒充用户"
notrun "人工可读性：本轮未执行（盲测页面与统计口径已交付，需真实参与者数据）"

log "工程项 $N_PASS 项 PASS（退出码 0 才走到这里）；BLOCKED $N_BLOCKED；NOT_RUN $N_NOTRUN"
log "结论口径：工程闭环 PASS；OCR 效果 BLOCKED；人工可读性 NOT_RUN；抗 AI/生产可用 **未论证**。"
