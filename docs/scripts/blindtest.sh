#!/usr/bin/env bash
# 一条命令启动**人工可读性盲测**（阶段 4B，docs/STAGE4B.md §5）。
#
#   docs/scripts/blindtest.sh          # 或 make blindtest
#   然后按终端里打印的 URL 打开页面，用真人做题。
#
# 它拉起的东西全都是临时且自有的：
#   - 一个新 mktemp 目录：SQLite 库、题目图片、答案表、记录文件都在里面
#   - 运行时随机生成的 master key / pepper / admin key / 站点 secret（只活在本进程 env）
#   - 三个演示站点（difficulty 1/3/5），每个配一批受控注入的题
#   - cmd/blindtest 记录服务（只监听 loopback）
# 退出时只杀自己起的进程、只删自己的目录：**不碰**任何已有库、配置或其它实例。
#
# 为什么端口是固定的：站点来源策略只登记精确 Origin（allow_missing_origin 只管"没有 Origin"，
# 不放过未知来源），所以盲测页必须在一个事先登记的 Origin 上。因此开头要检查端口空闲——
# 若被上一轮遗留进程占着，浏览器会打到**那个**进程，测试会因为错误的原因通过。
#
# 答案表（seeded-answers.json）只落在临时目录，从不进服务、从不进页面。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

BLIND_PORT="${BLINDTEST_PORT:-8491}"
BATCH="${BLINDTEST_BATCH:-20}"
DIFFS=(1 3 5)
PER_TIER=$((BATCH + 4))            # 每题一道 + 换题余量；注入上限是 50
(( PER_TIER <= 50 )) || { echo "batch 太大：每档需要 $PER_TIER 道题，devseed 上限 50" >&2; exit 1; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/epocha-blindtest.XXXXXX")"
SERVER_PID=""
BLIND_PID=""
STEP=0

say() { printf '[blindtest] %s\n' "$*"; }
die() { printf '[blindtest] 失败: %s\n' "$*" >&2; exit 1; }

cleanup() {
  local rc=$?
  trap - EXIT
  set +e
  for pid in "$BLIND_PID" "$SERVER_PID"; do
    [[ -n "$pid" ]] && kill -TERM "$pid" 2>/dev/null
  done
  for _ in 1 2 3 4 5 6 7 8; do
    local any=0
    for pid in "$BLIND_PID" "$SERVER_PID"; do
      [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null && any=1
    done
    [[ $any -eq 0 ]] && break
    sleep 0.2
  done
  for pid in "$BLIND_PID" "$SERVER_PID"; do
    [[ -n "$pid" ]] && kill -KILL "$pid" 2>/dev/null
  done
  # 只删本脚本创建的那个目录（mktemp 出来的，名字带 epocha-blindtest.前缀）
  [[ -n "$WORK" && -d "$WORK" ]] && rm -rf "$WORK"
  exit $rc
}
trap cleanup EXIT INT TERM

say "第 0 步：检查前置条件"
[[ -z "${EPOCHA_MASTER_KEY:-}" && -z "${EPOCHA_ADMIN_KEY:-}" ]] || die "环境里已有 EPOCHA_* 密钥：本脚本自己生成，不与你现有的配置混用（请在新 shell 里跑，或 unset 后重试）"
if ss -ltn 2>/dev/null | grep -q ":$BLIND_PORT "; then
  die "端口 $BLIND_PORT 已被占用：可能有上一轮盲测进程还活着。先确认并结束它（或 BLINDTEST_PORT=85xx 换端口），否则你会测到遗留实例。"
fi

export EPOCHA_MASTER_KEY="$(head -c32 /dev/urandom | base64 | tr -d '\n')"
export EPOCHA_PEPPER="$(head -c32 /dev/urandom | base64 | tr -d '\n')"
export EPOCHA_ADMIN_KEY="ak_$(head -c32 /dev/urandom | xxd -p | tr -d '\n')"

say "第 1 步：构建（生产服务 + 开发期工具，输出到临时目录）"
mkdir -p "$WORK/bin" "$WORK/seed"
go build -trimpath -ldflags "-s -w" -o "$WORK/bin/epocha" ./cmd/server
go build -o "$WORK/bin/devseed" ./cmd/devseed
go build -o "$WORK/bin/blindtest" ./cmd/blindtest
say "  bin/epocha = $(stat -c %s "$WORK/bin/epocha") 字节"

say "第 2 步：启动验证服务（临时库、随机端口、后台补题关闭）"
DB_PATH="$WORK/epocha.db"
# 限流放开：真人做题会反复领题，撞限流会让人以为"工具坏了"。
# 后台补题 worker 关掉：题池只有注入的那几批，"这次做到的是哪道题"才可复现。
EPOCHA_RATE_IP_RPM=6000 EPOCHA_RATE_SITE_RPM=6000 EPOCHA_MAX_ACTIVE_CHALLENGES=2000 \
EPOCHA_MAX_ACTIVE_CHALLENGES_PER_SITE=2000 EPOCHA_POOL_ENABLED=0 \
"$WORK/bin/epocha" --db "$DB_PATH" --listen 127.0.0.1:0 --admin-listen 127.0.0.1:0 >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
BASE=""; ABASE=""
for _ in $(seq 1 120); do
  BASE="$(sed -n 's/.*msg=已监听 addr=127\.0\.0\.1:\([0-9]*\).*/\1/p' "$WORK/server.log" | head -n1)"
  ABASE="$(sed -n 's/.*msg=管理面已挂载 addr=127\.0\.0\.1:\([0-9]*\).*/\1/p' "$WORK/server.log" | head -n1)"
  [[ -n "$BASE" && -n "$ABASE" ]] && break
  sleep 0.1
done
[[ -n "$BASE" && -n "$ABASE" ]] || die "服务没报出监听地址：$(tail -n 20 "$WORK/server.log")"
BASE="http://127.0.0.1:$BASE"; ABASE="http://127.0.0.1:$ABASE"
say "  公开端口 $BASE　管理端口 $ABASE"

say "第 3 步：建三个档位站点（bits=12：盲测量的是可读性，不该被计算成本干扰）"
TIERS=()
KEYS=()
for d in "${DIFFS[@]}"; do
  key="pk_blind_d${d}_$(head -c4 /dev/urandom | xxd -p)"
  created="$(curl -sS -H "Authorization: Bearer $EPOCHA_ADMIN_KEY" -H 'Content-Type: application/json' \
    -X POST "$ABASE/api/admin/sites" -d "{
      \"name\":\"盲测 d$d\",\"site_key\":\"$key\",\"pow_bits\":12,
      \"verify_mode\":\"pow_question\",\"generator_id\":\"text-image-v1\",\"difficulty\":$d,
      \"origins\":[\"http://127.0.0.1:$BLIND_PORT\"],\"allow_missing_origin\":false}")"
  secret="$(printf '%s' "$created" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("secret",""))')"
  [[ -n "$secret" ]] || die "建 d$d 站点失败：$created"
  TIERS+=("难度档 $d（difficulty=$d）=$key")
  KEYS+=("$key")
  say "  d$d 已建（secret 只出现这一次，只留在本进程的 curl 输出里，不落盘：$WORK 会被删）"
done

say "第 4 步：为每个档位注入 $PER_TIER 道受控题（答案只落临时目录）"
for d in "${DIFFS[@]}"; do
  "$WORK/bin/devseed" --db "$DB_PATH" --out-dir "$WORK/seed/d$d" \
    --generator text-image-v1 --difficulty "$d" --count "$PER_TIER" --ttl 3h \
    >"$WORK/devseed-d$d.log" 2>&1 || die "devseed(d$d) 失败：$(cat "$WORK/devseed-d$d.log")"
done
# 三个档位的题目都必须与站点配置同 (generator, version, difficulty)，否则领不到题。
# devseed 的文件名是 question-*.png；这里只做信息核对，真正的"能不能领到题"由下一步显式验证。
SEEDED="$(find "$WORK/seed" -name 'question-*.png' | wc -l)"
[[ "$SEEDED" -eq $(( ${#DIFFS[@]} * PER_TIER )) ]] || die "注入题数不对：$SEEDED，期望 $(( ${#DIFFS[@]} * PER_TIER ))"
say "  注入完成（$SEEDED 张图）"

say "第 5 步：逐档确认**真的领到题**（带盲测页那个 Origin）"
# 这一步不是保险起见：站点的 (generator, difficulty) 与注入的题不一致、或 Origin 登记错了，
# 表现都是"真人打开页面 → 组件报错"，而人不会知道是编排坏了还是工具坏了。
# 每次验证消耗一道题，所以注入量是 batch+4（三个档共 3 道，仍够做完一批）。
i=0
for d in "${DIFFS[@]}"; do
  key="${KEYS[$i]}"
  code="$(curl -sS -o "$WORK/challenge-d$d.json" -w '%{http_code}' \
    -X POST "$BASE/api/challenge?site_key=$key" \
    -H "Origin: http://127.0.0.1:$BLIND_PORT" -H 'Content-Type: application/json' \
    --data "{\"site_key\":\"$key\"}")"
  [[ "$code" == "200" ]] || die "档位 d$d 领不到题（HTTP $code）：$(head -c 300 "$WORK/challenge-d$d.json")"
  grep -q '"image_base64"' "$WORK/challenge-d$d.json" || die "档位 d$d 的响应里没有题图字段：$(head -c 300 "$WORK/challenge-d$d.json")"
  say "  d$d 领题正常（响应含题图）"
  i=$(( i + 1 ))
done

say "第 6 步：启动盲测记录服务"
"$WORK/bin/blindtest" --listen "127.0.0.1:$BLIND_PORT" --api "$BASE" \
  --tiers "$(IFS=,; printf '%s' "${TIERS[*]}")" --batch "$BATCH" --out "$WORK/records.jsonl" \
  >"$WORK/blindtest.log" 2>&1 &
BLIND_PID=$!
for _ in $(seq 1 100); do
  curl -fsS "http://127.0.0.1:$BLIND_PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS "http://127.0.0.1:$BLIND_PORT/healthz" >/dev/null 2>&1 || die "记录服务没起来：$(cat "$WORK/blindtest.log")"

# 页面里的 widget 脚本必须**真的取得到**：漏掉 <script src> 或地址写错时，页面照常显示、
# 按钮照常能点，只有组件永远不出现——真人会以为"工具坏了"。（本轮真实浏览器里踩过。）
say "  核对页面引入的 widget 脚本可取回"
curl -fsS "http://127.0.0.1:$BLIND_PORT/" -o "$WORK/page.html" || die "读不到盲测页面"
wsrc="$(python3 -c '
import re,sys
h=open(sys.argv[1],encoding="utf-8").read()
m=re.search(r"<script src=\"([^\"]*widget/v1\.js)\" defer>",h)
print(m.group(1) if m else "")' "$WORK/page.html")"
[[ -n "$wsrc" ]] || die "页面里没有 deferred 的 widget 脚本标签（EpoCHA 必然未定义）"
hdrs="$(curl -sS -D - -o "$WORK/widget.js" "$wsrc")"
printf '%s' "$hdrs" | grep -qi '^HTTP/1.1 200' || die "widget 脚本取不回：$wsrc（$(printf '%s' "$hdrs" | head -n1)）"
printf '%s' "$hdrs" | grep -qi '^content-type: text/javascript' || die "widget 脚本内容类型不对：$wsrc"
grep -q 'epocha widget' "$WORK/widget.js" || die "取回的内容不像 widget 脚本：$wsrc"
say "  widget 脚本可取回：$wsrc"

say ""
say "盲测入口：http://127.0.0.1:$BLIND_PORT/"
say "每档 $BATCH 题，共 ${#DIFFS[@]} 档。做完点页面上的「导出 CSV / 导出 JSON」。"
say "记录文件：$WORK/records.jsonl（不含凭据、不含输入原文）"
say "Ctrl-C 结束；临时目录与进程会被本脚本清掉。"
say ""
# 前台等两个子进程之一退出：让 Ctrl-C 落在脚本上，由 trap 统一收尸。
wait -n "$SERVER_PID" "$BLIND_PID" 2>/dev/null || true
if [[ -f "$WORK/records.jsonl" ]]; then
  REC="$(wc -l < "$WORK/records.jsonl")"
  # 落到 eval-out/（已在 .gitignore 里）而不是仓库根目录：人工测试数据不该被顺手 commit。
  KEEP="$ROOT/eval-out/blindtest-records-$(date -u +%Y%m%dT%H%M%SZ).jsonl"
  mkdir -p "$ROOT/eval-out"
  cp "$WORK/records.jsonl" "$KEEP"
  say "本次共 $REC 条记录，已复制到 $KEEP（eval-out/ 不入库）"
else
  say "没有记录（没做完任何一题就退出了）"
fi
