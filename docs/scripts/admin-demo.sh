#!/usr/bin/env bash
# 管理控制台的**本地演示入口**（开发用）。
#
# 它做四件事：把服务编译到临时目录、开一个空库、生成**运行时随机**的演示凭据、
# 把公共与管理两个监听器都绑在宿主 loopback 上，然后告诉你浏览器地址与凭据文件位置。
#
# 这不是真实部署的初始化流程：真实部署要的是 DEPLOYMENT §11 的启动配方（密钥由运维自备、
# 走 _FILE 注入、镜像在容器里跑）。这里的一切随 $WORK 一起消失。
#
# 边界（刻意不做的事）：
#   - 不覆盖任何已有数据库、.env、keys/：所有写入都落在 mktemp 出来的临时目录；
#   - 不发布管理端口、不改 Docker 网络边界、不写任何凭据进仓库；
#   - 演示凭据只以 0600 文件交付，**不打印内容**，网页也不会自己去读它。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

PUBLIC_PORT="${EPOCHA_DEMO_PUBLIC_PORT:-8401}"
ADMIN_PORT="${EPOCHA_DEMO_ADMIN_PORT:-8402}"

SERVER_PID=""
WORK=""
TMPFS_SCRATCH=""

say()  { printf '\033[0m[demo] %s\n' "$*"; }
note() { printf '       %s\n' "$*"; }
die()  { printf '[demo] 中止: %s\n' "$*" >&2; exit 1; }

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  if [[ -n "$TMPFS_SCRATCH" ]]; then
    rm -rf $TMPFS_SCRATCH
  fi
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill -TERM "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  if [[ -n "$WORK" && -d "$WORK" ]]; then
    rm -rf "$WORK"
    say "已清理本轮临时目录：$WORK"
  fi
  exit $rc
}
trap cleanup EXIT INT TERM

say "第 1 步：检查端口与磁盘"
for port in "$PUBLIC_PORT" "$ADMIN_PORT"; do
  if ss -ltn 2>/dev/null | grep -q ":$port "; then
    die "端口 $port 已被占用：演示绑固定端口，占用说明遗留进程还在（先停掉它，或用 EPOCHA_DEMO_ADMIN_PORT=8502 换端口）。否则浏览器里看到的是上一轮的库和数据。"
  fi
done
note "loopback:${PUBLIC_PORT}（公共 API）/ loopback:${ADMIN_PORT}（管理面）均空闲"

# 构建缓存默认在根文件系统上。这台机器曾经只剩几十 MB：缓存写满会让构建以"no space left
# on device"失败，而失败点通常在无关的包上。低空间时把本轮缓存与临时目录搬到 tmpfs，
# 不删除任何既有文件、不改全局配置。
CACHE_FS="$(df --output=target "$(go env GOCACHE)" 2>/dev/null | tail -n1 | tr -d ' ')"
FREE_KB="$(df --output=avail "$CACHE_FS" 2>/dev/null | tail -n1 | tr -d ' ')"
if [[ -n "$FREE_KB" && "$FREE_KB" -lt 400000 && -w /dev/shm ]]; then
  say "构建缓存所在文件系统（$CACHE_FS）只剩 $((FREE_KB / 1024)) MB：本轮改用 tmpfs 缓存"
  export GOCACHE="/dev/shm/epocha-gocache.$$"
  export TMPDIR="/dev/shm/epocha-tmp.$$"
  mkdir -p "$GOCACHE" "$TMPDIR"
  TMPFS_SCRATCH="$GOCACHE $TMPDIR"
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/epocha-admin-demo.XXXXXX")"
chmod 700 "$WORK"
mkdir -p "$WORK/bin" "$WORK/data" "$WORK/keys"
chmod 700 "$WORK/keys"

say "第 2 步：构建服务二进制到临时目录（不写入仓库 bin/）"
go build -trimpath -ldflags "-s -w" -o "$WORK/bin/epocha" ./cmd/server
note "$WORK/bin/epocha = $(stat -c %s "$WORK/bin/epocha") 字节"

say "第 3 步：生成运行时随机凭据（0600 文件；仓库与进程环境里都不留明文）"
printf '%s' "$(head -c32 /dev/urandom | base64 | tr -d '\n')" > "$WORK/keys/master"
printf '%s' "$(head -c32 /dev/urandom | base64 | tr -d '\n')" > "$WORK/keys/pepper"
printf '%s' "ak_$(head -c32 /dev/urandom | od -An -tx1 | tr -d ' \n')" > "$WORK/keys/admin"
for f in master pepper admin; do
  chmod 600 "$WORK/keys/$f"
  [[ -s "$WORK/keys/$f" ]] || die "演示凭据文件为空：$WORK/keys/$f"
done
note "master/pepper/admin 三个文件均为 0600，目录 0700"

say "第 4 步：启动（空库、题池 worker 关闭、两个监听器都在 127.0.0.1）"
EPOCHA_DB="$WORK/data/epocha.db" \
EPOCHA_LISTEN="127.0.0.1:$PUBLIC_PORT" \
EPOCHA_ADMIN_LISTEN="127.0.0.1:$ADMIN_PORT" \
EPOCHA_MASTER_KEY_FILE="$WORK/keys/master" \
EPOCHA_PEPPER_FILE="$WORK/keys/pepper" \
EPOCHA_ADMIN_KEY_FILE="$WORK/keys/admin" \
EPOCHA_POOL_ENABLED=false \
  "$WORK/bin/epocha" >"$WORK/server.log" 2>&1 &
SERVER_PID=$!

ready=0
for _ in $(seq 1 100); do
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    die "服务进程提前退出，日志尾部：$(tail -n 5 "$WORK/server.log")"
  fi
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PUBLIC_PORT/readyz" 2>/dev/null || echo 000)"
  ui="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$ADMIN_PORT/admin" 2>/dev/null || echo 000)"
  if [[ "$code" == "200" && "$ui" == "200" ]]; then ready=1; break; fi
  sleep 0.2
done
[[ $ready -eq 1 ]] || die "服务未就绪（公共 /readyz=$code 管理 /admin=$ui）；日志尾部：$(tail -n 10 "$WORK/server.log")"

# 边界自检：这一步的意义是"你现在看到的地址，确实只在管理端口上"。
pub_admin="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PUBLIC_PORT/admin" 2>/dev/null || echo 000)"
pub_api="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PUBLIC_PORT/api/admin/sites" 2>/dev/null || echo 000)"
[[ "$pub_admin" != "200" && "$pub_api" != "200" ]] || die "公共端口上出现了管理面（$pub_admin/$pub_api），本演示不该继续"

cat <<EOF

──────────────────────────────────────────────────────────────
管理控制台已就绪（仅本机 loopback）

  浏览器打开：  http://127.0.0.1:$ADMIN_PORT/admin
  公共 API：    http://127.0.0.1:$PUBLIC_PORT   （不提供 /admin，也不提供 /api/admin/*）

  登录用的管理凭据在这个文件里（0600，本轮随机生成）：
      $WORK/keys/admin
  需要你**自己**看一次再粘进页面，例如：
      cat $WORK/keys/admin

  库里现在是空的：登录后先建站点；secret 只在创建那一次显示，离开详情页就取不回来。
  服务日志：$WORK/server.log

  按 Ctrl-C 停止服务；本轮临时目录（库、凭据、二进制）随之删除，不会碰仓库里任何文件。
──────────────────────────────────────────────────────────────
EOF

wait "$SERVER_PID"
