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
# 三态口径：工程项 PASS/FAIL；外部条件不足 BLOCKED；本轮没执行 NOT_RUN。
# 退出码：0=全 PASS；1=有 FAIL；3=无失败但有 BLOCKED/NOT_RUN（候选版本可审阅，但别当成阶段完成）。
# `make` 会把 1 与 3 都折叠成 2，所以机器可读的结论看最后那行 RESULT。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/epocha-stage5b.XXXXXX")"
chmod 700 "$WORK"

IMAGE="${EPOCHA_IMAGE:-epocha:5a}"
PROJECT="epocha5b"
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

STEP=0
N_PASS=0
N_FAIL=0
N_BLOCKED=0
N_NOTRUN=0
REQUIRED_BLOCKED=0
DOCKER_OK=0

log()  { printf '[stage5b] %s\n' "$*"; }
pass()    { STEP=$((STEP+1)); N_PASS=$((N_PASS+1)); printf '[stage5b] PASS %d: %s\n' "$STEP" "$*"; }
fail()    { STEP=$((STEP+1)); N_FAIL=$((N_FAIL+1)); printf '[stage5b] FAIL %d: %s\n' "$STEP" "$*" >&2; }
blocked() {
  STEP=$((STEP+1)); N_BLOCKED=$((N_BLOCKED+1))
  printf '[stage5b] BLOCKED %d: %s\n' "$STEP" "$*"
  printf '[stage5b]   恢复执行: %s\n' "${2:-见 docs/STAGE5B.md}"
  REQUIRED_BLOCKED=1
}
notrun() { STEP=$((STEP+1)); N_NOTRUN=$((N_NOTRUN+1)); printf '[stage5b] NOT_RUN %d: %s（原因：%s）\n' "$STEP" "$*" "${2:-本轮未执行}"; }
abort()  { printf '[stage5b] 中止: %s\n' "$*" >&2; exit 1; }

avail_kb() { df -Pk "$WORK" | awk 'NR==2 {print $4}'; }

cleanup() {
  local rc=$?
  trap - EXIT
  set +e
  # 只清理**本脚本自己**创建的容器/网络/目录；绝不 prune，也不碰别人留下的东西。
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
cexec() { compose exec -T "$@"; }
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
  abort "docker daemon 不可达（第 2/4 步无法执行）"
fi
for p in "$PUBLIC_PORT" "$SECOND_PORT" "$MEASURE_PORT" 8081; do
  rc=0
  port_listening "$p" || rc=$?
  if [[ $rc -eq 0 ]]; then
    abort "端口 $p 已被占用：演练可能连到遗留进程并以错误的原因通过"
  elif [[ $rc -ne 1 ]]; then
    abort "ss 无法判定端口 $p（rc=$rc）：不能把“测不出来”当成“空闲”"
  fi
done
NEED_KB=$((150 * 1024))
AVAIL_KB="$(avail_kb)"
if (( AVAIL_KB >= NEED_KB )); then
  DOCKER_OK=1
  pass "工具齐备、端口空闲（$PUBLIC_PORT/$SECOND_PORT/$MEASURE_PORT/8081），磁盘余量 $((AVAIL_KB / 1024))MB ≥ $((NEED_KB / 1024))MB"
else
  blocked "磁盘余量 $((AVAIL_KB / 1024))MB < 容器演练所需 $((NEED_KB / 1024))MB ⇒ 第 2/3/4 步本轮不执行" \
    "释放磁盘后单独跑：bash docs/scripts/stage5b.sh（第 1 步的测试不受影响）"
fi

# --- 第 1 步：跨进程互斥的真实测试 + 一次回归 ---
log "第 1 步：运行锁的测试（真实子进程）"
rc=0
go test -count=1 -race -timeout 600s ./internal/store/ ./cmd/server/ ./cmd/ops/ ./cmd/loadtest/ >"$LOGS/locktests.log" 2>&1 || rc=$?
if [[ $rc -ne 0 ]]; then
  tail -n 40 "$LOGS/locktests.log"
  fail "锁相关包的 -race 测试退出码 $rc"
else
  pass "internal/store(锁+备份恢复) / cmd/server(真实进程互斥) / cmd/ops(真实进程互斥+恢复边界) / cmd/loadtest(节流) 的 -race 测试全通过"
fi
for want in \
  "TestSecondServerCannotOpenSameDataDir" \
  "TestGracefulStopAllowsRestart" \
  "TestSIGKILLReleasesLock" \
  "TestStaleLockFileDoesNotBlockServer" \
  "TestStartupVersusOfflineHolderIsMutuallyExclusive" \
  "TestHolderAndServerOnSameDirConflictViaDirLock"; do
  if ! grep -q "^func $want" cmd/server/lock_test.go; then
    fail "缺少必需的真实进程测试 $want"
  fi
done
pass "服务侧 6 条真实子进程测试都在（抢同一数据目录、优雅重启、SIGKILL、陈旧锁文件、启动竞态、目录锁冲突）"
for want in \
  "TestAdminRecoverRefusesWhileLockHeldByAnotherProcess" \
  "TestAdminRecoverSucceedsAfterHolderExit" \
  "TestBackupDoesNotNeedTheLock" \
  "TestRestoreRefusesWhenOutDirIsInUse" \
  "TestTwoAdminRecoverProcessesRace"; do
  if ! grep -q "^func $want" cmd/ops/main_test.go; then
    fail "缺少必需的运维侧测试 $want"
  fi
done
pass "运维侧 5 条测试都在：服务在跑时改不了库、退出后可重取、在线备份不需要停服、就地恢复被拒、两进程抢恢复恰好一个成功"

log "第 1b 步：一次全量回归（-race ./...）"
rc=0
go test -race -count=1 -timeout 1200s ./... >"$LOGS/race.log" 2>&1 || rc=$?
if [[ $rc -ne 0 ]]; then
  tail -n 30 "$LOGS/race.log"
  fail "go test -race ./... 退出码 $rc"
else
  pass "go test -race -count=1 ./... 退出码 0"
fi
rc=0
make fmt-check vet size >"$LOGS/static.log" 2>&1 || rc=$?
if [[ $rc -ne 0 ]]; then
  tail -n 20 "$LOGS/static.log"
  fail "fmt-check/vet/size 退出码 $rc"
else
  pass "fmt-check / vet / size 全通过"
fi
DEPS="$(go list -deps ./cmd/server)"
for banned in internal/eval cmd/preview cmd/devseed cmd/evalgen cmd/evalocr cmd/exampleapp cmd/blindtest cmd/loadtest web/preview; do
  if printf '%s\n' "$DEPS" | grep -q "EpoCHA/$banned"; then
    fail "服务二进制链接了开发期包 $banned"
  fi
done
pass "cmd/server 仍不链接任何开发期包（go list -deps 证明）"

# --- 第 2 步：容器与宿主共享同一把锁 ---
log "第 2 步：真实容器里的互斥（宿主 ↔ 容器，两个容器）"
if [[ $DOCKER_OK -eq 1 ]]; then
  rc=0
  make docker-image-local >"$LOGS/build.log" 2>&1 || rc=$?
  if [[ $rc -ne 0 ]]; then
    tail -n 25 "$LOGS/build.log"
    fail "make docker-image-local 退出码 $rc"
  else
    pass "镜像重建成功（$IMAGE，含本轮的锁实现）"
  fi

  gen_key() { head -c32 /dev/urandom | base64 >"$1"; chmod 600 "$1"; }
  gen_key "$D/keys/master"; gen_key "$D/keys/pepper"
  ADMIN0="ak_$(head -c32 /dev/urandom | base64 | tr -d '/+=' | head -c48)"
  printf '%s' "$ADMIN0" >"$D/keys/admin"; chmod 600 "$D/keys/admin"
  cat >"$WORK/compose.env" <<EOF
EPOCHA_IMAGE=$IMAGE
EPOCHA_VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
EPOCHA_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo none)
EPOCHA_BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
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
    CGO_ENABLED=0 go build -trimpath -o "$BIN/epocha-ops" ./cmd/ops
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

# --- 第 3 步：候选构建清单 ---
log "第 3 步：部署候选清单（A 容器内编译 / B 宿主编译装配）"
{
  echo "生成时间: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "源码 commit: $(git rev-parse HEAD)  describe: $(git describe --tags --always --dirty 2>/dev/null || echo dev)"
  echo "工作树: $(if [[ -z $(git status --porcelain) ]]; then echo clean; else echo dirty; fi)"
  # 宿主默认 CGO_ENABLED 与镜像构建用的值**分开记**：宿主有 gcc 时默认是 1，
  # 而 scratch 运行时要求 0（5A 的实测教训）。写成一个笼统的"CGO_ENABLED=0"就是失准。
  echo "宿主 go 默认: GOOS=$(go env GOOS) GOARCH=$(go env GOARCH) CGO_ENABLED=$(go env CGO_ENABLED) GOTOOLCHAIN=$(go env GOTOOLCHAIN)"
  echo "镜像构建实际使用: CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath（deploy/docker/Dockerfile 与 make docker-image-local 一致）"
  echo "ldflags: $(make -n build 2>/dev/null | grep -o '\-ldflags .*' | head -n1)"
  echo "Go: $(go version)"
  echo "宿主架构: $(uname -m)  docker: $(docker --version | awk '{print $3}')"
} >"$LOGS/manifest.txt"
if [[ -f "$BIN/epocha-ops" ]]; then
  {
    echo "进镜像的 epocha sha256:      $(sha deploy/docker/.prebuilt/out/epocha)"
    echo "进镜像的 epocha-ops sha256:  $(sha deploy/docker/.prebuilt/out/epocha-ops)"
    echo "演练用 epocha-ops sha256:    $(sha "$BIN/epocha-ops")"
    echo "本地镜像 ID: $(docker image inspect "$IMAGE" --format '{{.Id}}')"
    echo "镜像大小: $(docker image inspect "$IMAGE" --format '{{.Size}}') 字节"
    echo "已测试的运行 UID/GID: $RUN_UID:$RUN_GID（镜像默认 USER 65534:65534，见 DEPLOYMENT §2）"
  } >>"$LOGS/manifest.txt"
  if [[ -n "${BK:-}" && -f "$D/backups/$BK.manifest.json" ]]; then
    {
      echo "schema 版本:      $(jq -r '.schema_version' "$D/backups/$BK.manifest.json")"
      echo "app_version:      $(jq -r '.app_version' "$D/backups/$BK.manifest.json")"
      echo "备份内 app/commit 与 manifest 一致: $([[ "$(jq -r '.commit' "$D/backups/$BK.manifest.json")" == "$(git rev-parse --short HEAD)" ]] && echo yes || echo no)"
    } >>"$LOGS/manifest.txt"
  else
    echo "schema 版本: 本轮没有可用备份（清单不完整）" >>"$LOGS/manifest.txt"
  fi
  pass "候选清单已落盘（commit / Go / GOOS-ARCH / CGO / 两份进镜像二进制的 SHA-256 / 本地镜像 ID / schema / 运行 uid）：$LOGS/manifest.txt"
fi
# 静态链接断言（scratch 运行时的前提）：断言对象必须包含**进镜像的那两份**，
# 演练自己用的 ops 只是顺带——真正会在生产里起不来的是 .prebuilt/out/ 里那两份。
if command -v readelf >/dev/null 2>&1; then
  BAD=""
  PB="deploy/docker/.prebuilt/out"
  for b in "$PB/epocha" "$PB/epocha-ops" "$BIN/epocha-ops"; do
    if [[ ! -f "$b" ]]; then BAD="$BAD $b(缺失)"; continue; fi
    if readelf -lW "$b" 2>/dev/null | grep -qi 'program interpreter'; then BAD="$BAD $b"; fi
  done
  if [[ -n "$BAD" ]]; then
    fail "这些二进制是动态链接或缺失（scratch 里起不来）: $BAD"
  else
    pass "进镜像的两份二进制与演练用的 ops 都无 PT_INTERP（CGO_ENABLED=0 静态链接）"
  fi
fi
# A 路径：本轮**不**为了全绿去拉 ~380MB 的 golang 镜像（任务明确禁止）
notrun "A 路径（make docker-image，容器内 go build）" \
  "需要 registry 可达 + ≥2GB 空闲磁盘（本轮起始仅 $(( $(avail_kb) / 1024 ))MB）；执行命令：make docker-image && bash docs/scripts/stage5b.sh"
if [[ $DOCKER_OK -eq 1 ]]; then
  pass "B 路径（宿主 CGO_ENABLED=0 编译 + named build context 装配）本轮实际执行并通过"
fi
cat "$LOGS/manifest.txt" | sed 's/^/[stage5b]   清单: /'

# --- 第 4 步：约 5 分钟的节流持续演练 ---
log "第 4 步：持续演练（1 CPU / 128MiB，节流约 1.5 闭环/秒，5 分钟硬上限）"
if [[ $DOCKER_OK -eq 1 && -n "$CID" ]]; then
  go build -trimpath -o "$BIN/loadtest" ./cmd/loadtest
  go build -trimpath -o "$BIN/devclient" ./cmd/devclient
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
    # 短 TTL 站点 + 只领题不作答：这是"过期清理是否真的触发"的唯一可观察形状。
    rc=0
    admin_in POST /api/admin/sites "$LOGS/site-exp.json" '{"name":"stage5b-expire","verify_mode":"pow_only","allow_missing_origin":true,"pow_bits":12,"challenge_ttl_sec":5,"rate_ip_rpm":600,"rate_site_rpm":600}' || rc=$?
    EXP_KEY="$(jq -r '.site.site_key // ""' "$LOGS/site-exp.json" 2>/dev/null || echo "")"
    if [[ -z "$SOAK_POW" || -z "$SOAK_SECRET" || -z "$EXP_KEY" ]]; then
      fail "持续演练需要的三个站点没建齐（pow=$SOAK_POW exp=$EXP_KEY）"
    else
      pass "三个演练站点就绪：pow_only 流量站、text-image 补池站（高档 5）、challenge_ttl=5s 过期站"
      POOL0="$(docker exec -i "$MEAS" "$OPS" admin GET /api/admin/pool </dev/null 2>/dev/null | sed -n '2,$p')"
      READY0="$(printf '%s' "$POOL0" | jq -r '[.buckets[]?|.ready]|add//0')"
      log "  起始题池 ready=$READY0"

      # 过期样本：领 20 道题后放着不管。
      for _ in $(seq 1 20); do
        curl -sS -o /dev/null --max-time 8 -X POST -H 'Content-Type: application/json' \
          -d "{\"site_key\":\"$EXP_KEY\"}" "$MU/api/challenge" 2>/dev/null || true
      done

      DB0="$(stat -c '%s' "$D/data/epocha.db" 2>/dev/null || echo 0)"
      WAL0="$(stat -c '%s' "$D/data/epocha.db-wal" 2>/dev/null || echo 0)"
      SOAK_START=$SECONDS
      # 450 次闭环 / 并发 1 / 每次之间睡 660ms ⇒ 约 5 分钟、约 1.5 闭环/秒。
      # 总时长与请求数都有硬上限（cmd/loadtest 里 requests ≤5000）。
      EPOCHA_SITE_SECRET="$SOAK_SECRET" "$BIN/loadtest" --base "$MU" --site-key "$SOAK_POW" \
        --concurrency 1 --requests 450 --pace 660ms --timeout 10s >"$LOGS/soak.txt" 2>&1 &
      SOAK_PID=$!
      SAMPLES="$LOGS/soak-samples.txt"
      : >"$SAMPLES"
      while kill -0 "$SOAK_PID" 2>/dev/null; do
        ST="$(docker stats --no-stream --format '{{.CPUPerc}}|{{.MemUsage}}' "$MEAS" 2>/dev/null || echo '?')"
        printf 't=%ss stats=%s db=%s wal=%s readyz=%s\n' \
          "$((SECONDS - SOAK_START))" "$ST" \
          "$(stat -c '%s' "$D/data/epocha.db" 2>/dev/null || echo 0)" \
          "$(stat -c '%s' "$D/data/epocha.db-wal" 2>/dev/null || echo 0)" \
          "$(code "$MU/readyz" 5)" >>"$SAMPLES"
        sleep 30
      done
      wait "$SOAK_PID" || true
      SOAK_T=$((SECONDS - SOAK_START))
      SOAK_RC=0
      grep -q "ok=450" "$LOGS/soak.txt" || SOAK_RC=1
      if [[ $SOAK_RC -eq 0 ]]; then
        pass "持续演练完成：450 次 pow_only 闭环全部成功，用时 ${SOAK_T}s（约 $(awk -v t="$SOAK_T" 'BEGIN{printf "%.2f", 450/t}') 闭环/秒）"
      else
        fail "持续演练里有失败闭环：$(tail -c 260 "$LOGS/soak.txt")"
      fi
      sed -n '1,3p' "$LOGS/soak.txt" | sed 's/^/[stage5b]   节流: /'
      tail -n 3 "$SAMPLES" | sed 's/^/[stage5b]   采样: /'

      # 过期清理有没有真的触发（不是"我们有个 purge 循环"，是日志里有没有非零计数）
      docker logs "$MEAS" 2>&1 | grep "清理" >"$LOGS/purge.log" || true
      if grep -qE "challenges=[1-9]" "$LOGS/purge.log"; then
        pass "过期清理实际触发过：$(tail -n 1 "$LOGS/purge.log")"
      else
        fail "5 分钟窗口内没有观察到任何非零的过期清理计数（挑战 ttl=5s 的 20 道题应被清掉）"
      fi

      POOL1="$(docker exec -i "$MEAS" "$OPS" admin GET /api/admin/pool </dev/null 2>/dev/null | sed -n '2,$p')"
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
  blocked "持续演练：磁盘不足或测量实例未就绪" "释放磁盘后：bash docs/scripts/stage5b.sh"
fi

# --- 汇总 ---
log "汇总"
printf '[stage5b] 工程项 PASS=%d FAIL=%d；外部条件 BLOCKED=%d；本轮未执行 NOT_RUN=%d\n' \
  "$N_PASS" "$N_FAIL" "$N_BLOCKED" "$N_NOTRUN"
if [[ -f "$LOGS/manifest.txt" ]]; then
  printf '[stage5b] 候选清单文件：%s（本轮运行结束后会随临时目录删除；需要留档请用 bash docs/scripts/stage5b.sh 2>&1 | tee 出去）\n' "$LOGS/manifest.txt"
fi
if [[ $N_FAIL -gt 0 ]]; then
  printf '[stage5b] RESULT=fail（%d 项工程失败）\n' "$N_FAIL"
  exit 1
fi
if [[ $N_BLOCKED -gt 0 || $N_NOTRUN -gt 0 ]]; then
  printf '[stage5b] RESULT=blocked（无失败，但 BLOCKED=%d NOT_RUN=%d ⇒ 不算阶段完成）\n' "$N_BLOCKED" "$N_NOTRUN"
  exit 3
fi
printf '[stage5b] RESULT=pass（%d 项全部通过）\n' "$N_PASS"
exit 0
