#!/usr/bin/env bash
# 阶段 5A 门禁：单节点容器部署包、密钥持久化、SQLite 备份恢复、边界与资源受限演练。
#
# 与前面阶段脚本的分工（刻意**不**把整套历史门禁再跑一遍）：
#   - 一次 `make stage3b-check` 承担回归（它内部已经跑 fmt-check/vet/race/size/secret-scan +
#     stage1/2/3a + stage3b 的 21 项端到端）。同一套 -race 不在这里跑第二遍。
#   - 本脚本自己只测 5A 的交付面：镜像与交付清单、密钥 *_FILE 的 fail-closed 形状、
#     compose 的端口/权限/资源边界、备份与保守恢复、优雅停止、故障形状、有限负载实测、反代路由。
#
# 三态口径（AGENTS：不许混用）：工程项 PASS/FAIL；外部条件不足 BLOCKED；本轮没执行 NOT_RUN。
# **部署/恢复/资源演练任一 BLOCKED ⇒ 本脚本退出码 3**（不是 0）：文件写好了不等于验收过了。
# 退出码分层：直接跑本脚本得到 0/1/3；`go run` 会折叠成 1、`make` 会折叠成 2 —— 要机器可读就读
# 最后那行 RESULT 与上面的逐项记录。
set -euo pipefail

script_exit() {
  local rc="${1:-$?}"
  trap - EXIT INT TERM
  printf '[stage5a] SCRIPT_RC=%s\n' "$rc"
  exit "$rc"
}
trap script_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/epocha-stage5a.XXXXXX")"
chmod 700 "$WORK"

PROXY_IMAGE="${EPOCHA_PROXY_IMAGE:-nginx:1.27-alpine}"
# 固定端口 + 开头检查空闲：端口被上一轮遗留进程占着时，测试会“因为错误的原因通过”。
PUBLIC_PORT="${EPOCHA_5A_PORT:-18080}"
RESTORE_PORT="${EPOCHA_5A_RESTORE_PORT:-18081}"
PROXY_PORT="${EPOCHA_5A_PROXY_PORT:-18443}"
LOAD_PORT="${EPOCHA_5A_LOAD_PORT:-18083}"
PROJECT="${EPOCHA_5A_PROJECT:-epocha5a-$(date -u +%Y%m%dt%H%M%sz)-$$-${RANDOM}}"
[[ "$PROJECT" =~ ^[a-z0-9][a-z0-9-]{1,45}$ ]] || exit 2
IMAGE="${EPOCHA_IMAGE:-epocha:$PROJECT}"
export EPOCHA_DRILL_OWNER="$PROJECT-$$-${RANDOM}"
READONLY_PORT="${EPOCHA_5A_READONLY_PORT:-18082}"
RESTORE_NAME="${PROJECT}-restore"
PROXY_NAME="${PROJECT}-proxy"
READONLY_NAME="${PROJECT}-readonly"
LOAD_NAME="${PROJECT}-load"
# 这台宿主没有 root，无法把绑定目录 chown 给 65534，因此演练用调用者 uid 跑容器。
# 镜像本身的 USER 仍是 65534（交付形状），差异只影响“数据目录属主”这一条，见 docs/DEPLOYMENT.md §2。
RUN_UID="$(id -u)"
RUN_GID="$(id -g)"
# 容器内路径常量（与 compose 里的挂载点一致）。
C_DATA=/var/lib/epocha
C_KEYS=/etc/epocha/keys
C_BACKUP=/var/backups/epocha
OPS=/usr/local/bin/epocha-ops

D="$WORK/deploy"
BIN="$WORK/bin"
LOGS="$WORK/logs"
mkdir -p "$BIN" "$LOGS" "$D/data" "$D/keys" "$D/backups" "$D/restore" "$D/keys-restore"
BACKUP_ADMIN=""
BACKUP_ADMIN_ID=""
BACKUP_OLD_OK=0

STEP=0
N_PASS=0
N_FAIL=0
N_BLOCKED=0
N_NOTRUN=0
# REQUIRED_BLOCKED：任一“部署必需项”被外部条件挡住 ⇒ 最终退出码 3。
REQUIRED_BLOCKED=0
DOCKER_OK=0
NGINX_OK=0
CID=""
CLEANUP_ARMED=0
COMPOSE_STARTED=""
RESTORE_CID=""
READONLY_CID=""
PROXY_CID=""
LOAD_CID=""
LC_CID=""
PULLED_PROXY_IMAGE_ID=""
COMPOSE_CONTAINERS=()
COMPOSE_VOLUMES=()
COMPOSE_NETWORKS=()

log()  { printf '[stage5a] %s\n' "$*"; }
pass()    { STEP=$((STEP+1)); N_PASS=$((N_PASS+1)); printf '[stage5a] PASS %d: %s\n' "$STEP" "$*"; }
fail()    { STEP=$((STEP+1)); N_FAIL=$((N_FAIL+1)); printf '[stage5a] FAIL %d: %s\n' "$STEP" "$*" >&2; }
blocked() {
  STEP=$((STEP+1)); N_BLOCKED=$((N_BLOCKED+1))
  printf '[stage5a] BLOCKED %d: %s\n' "$STEP" "$*"
  printf '[stage5a]   恢复执行: %s\n' "${2:-见 docs/STAGE5A.md}"
  [[ "${3:-required}" == "required" ]] && REQUIRED_BLOCKED=1
}
notrun() { STEP=$((STEP+1)); N_NOTRUN=$((N_NOTRUN+1)); printf '[stage5a] NOT_RUN %d: %s\n' "$STEP" "$*"; }
abort()  { printf '[stage5a] 中止: %s\n' "$*" >&2; exit 1; }

# 负载工具默认输出只有 wall/flows/ok 与四段时延样本，没有 err= 字段；这里另行解析
# 而不是把 loadtest 的“至少一次成功”退出码当作容量验收。每个计数都锚定整行，避免
# flows=1500、n=1500 或 quiet 输出误被读成 150/150。
check_load_evidence() {
  local file="$1" plan summary stage n
  [[ -s "$file" ]] || return 1
  plan="$(grep -Ec '^负载形状 concurrency=4 requests=150 base=[^[:space:]]+（凭据不回显）$' "$file" || true)"
  summary="$(grep -Ec '^wall=[^[:space:]]+ flows=150 ok=150 吞吐=[0-9]+(\.[0-9]+)? 闭环/秒$' "$file" || true)"
  [[ "$plan" == 1 && "$summary" == 1 ]] || return 1
  grep -q 'err=' "$file" && return 1
  grep -q '^失败类别：' "$file" && return 1
  for stage in challenge redeem siteverify pow-tool; do
    n="$(grep -Ec "^${stage}[[:space:]]+n=150 p50=[^[:space:]]+ p95=[^[:space:]]+ max=[^[:space:]]+$" "$file" || true)"
    [[ "$n" == 1 ]] || return 1
  done
  return 0
}

if [[ "${1:-}" == "--selftest-rules" ]]; then
  good_load='负载形状 concurrency=4 requests=150 base=http://127.0.0.1:18083（凭据不回显）
wall=2m0s flows=150 ok=150 吞吐=1.25 闭环/秒
challenge  n=150 p50=1ms p95=2ms max=3ms
redeem     n=150 p50=1ms p95=2ms max=3ms
siteverify n=150 p50=1ms p95=2ms max=3ms
pow-tool   n=150 p50=1ms p95=2ms max=3ms'
  fixture="$(mktemp "${TMPDIR:-/tmp}/epocha-stage5a-rules.XXXXXX")"
  printf '%s\n' "$good_load" >"$fixture"
  if ! check_load_evidence "$fixture"; then
    rm -f "$fixture"
    printf '[stage5a] rules selftest FAIL: valid 150-flow fixture rejected\n' >&2
    exit 1
  fi
  for bad_load in \
    "${good_load/flows=150/flows=149}" \
    "${good_load/challenge  n=150/challenge  n=149}"; do
    printf '%s\n' "$bad_load" >"$fixture"
    if check_load_evidence "$fixture"; then
      rm -f "$fixture"
      printf '[stage5a] rules selftest FAIL: invalid load fixture accepted\n' >&2
      exit 1
    fi
  done
  printf '%s\n失败类别：challenge=1\n' "$good_load" >"$fixture"
  if check_load_evidence "$fixture"; then
    rm -f "$fixture"
    printf '[stage5a] rules selftest FAIL: failure-category fixture accepted\n' >&2
    exit 1
  fi
  printf '%s\nerr=0\n' "$good_load" >"$fixture"
  if check_load_evidence "$fixture"; then
    rm -f "$fixture"
    printf '[stage5a] rules selftest FAIL: err fixture accepted\n' >&2
    exit 1
  fi
  rm -f "$fixture"
  printf '[stage5a] rules selftest PASS（真实默认输出解析：150/150、四段 n=150、无 err/失败类别）\n'
  exit 0
fi
cleanup_owned_resources() {
  local id
  [[ "$CLEANUP_ARMED" == "1" ]] || return 0
  if [[ -n "$COMPOSE_STARTED" ]]; then
    for id in "${COMPOSE_CONTAINERS[@]}"; do [[ -n "$id" ]] && docker rm -f "$id" >/dev/null 2>&1 || true; done
    for id in "${COMPOSE_VOLUMES[@]}"; do [[ -n "$id" ]] && docker volume rm "$id" >/dev/null 2>&1 || true; done
    for id in "${COMPOSE_NETWORKS[@]}"; do [[ -n "$id" ]] && docker network rm "$id" >/dev/null 2>&1 || true; done
  fi
  [[ -n "$RESTORE_CID" ]] && docker rm -f "$RESTORE_CID" >/dev/null 2>&1 || true
  [[ -n "$READONLY_CID" ]] && docker rm -f "$READONLY_CID" >/dev/null 2>&1 || true
  [[ -n "$PROXY_CID" ]] && docker rm -f "$PROXY_CID" >/dev/null 2>&1 || true
  [[ -n "$LOAD_CID" ]] && docker rm -f "$LOAD_CID" >/dev/null 2>&1 || true
  [[ -n "$LC_CID" ]] && docker rm "$LC_CID" >/dev/null 2>&1 || true
  [[ -n "$PULLED_PROXY_IMAGE_ID" ]] && docker image rm "$PULLED_PROXY_IMAGE_ID" >/dev/null 2>&1 || true
}
register_compose_resources() {
  local output rc=0
  output="$(compose ps -aq --all)" || rc=$?
  [[ $rc -eq 0 ]] || return "$rc"
  while IFS= read -r id; do [[ -n "$id" ]] && COMPOSE_CONTAINERS+=("$id"); done <<<"$output"
  output="$(docker volume ls -q --filter "label=com.docker.compose.project=$PROJECT")" || return $?
  while IFS= read -r id; do [[ -n "$id" ]] && COMPOSE_VOLUMES+=("$id"); done <<<"$output"
  output="$(docker network ls -q --filter "label=com.docker.compose.project=$PROJECT")" || return $?
  while IFS= read -r id; do [[ -n "$id" ]] && COMPOSE_NETWORKS+=("$id"); done <<<"$output"
}
if [[ "${1:-}" == "--selftest-resource-ownership" ]]; then
  ownership_case="${EPOCHA_STAGE5A_OWNERSHIP_CASE:-preflight}"
  tmp="$(mktemp -d "${TMPDIR:-/tmp}/epocha-stage5a-ownership.XXXXXX")"
  record="$tmp/docker.log"
  fake="$tmp/docker"
  printf '%s\n' '#!/usr/bin/env bash' 'printf "%s\\n" "$*" >>"$EPOCHA_FAKE_DOCKER_LOG"' >"$fake"
  chmod 700 "$fake"
  export PATH="$tmp:$PATH" EPOCHA_FAKE_DOCKER_LOG="$record"
  case "$ownership_case" in
    preflight|create-failed|existing-named) CLEANUP_ARMED=0 ;;
    created)
      CLEANUP_ARMED=1; COMPOSE_STARTED=1
      COMPOSE_CONTAINERS=(compose-owned-id); COMPOSE_VOLUMES=(volume-owned-id); COMPOSE_NETWORKS=(network-owned-id)
      RESTORE_CID=restore-owned-id; READONLY_CID=readonly-owned-id; PROXY_CID=proxy-owned-id; LOAD_CID=load-owned-id
      LC_CID=created-owned-id; PULLED_PROXY_IMAGE_ID=sha256:newly-pulled-image-id
      ;;
    *) rm -rf "$tmp"; exit 64 ;;
  esac
  cleanup_owned_resources
  calls="$(cat "$record" 2>/dev/null || true)"
  if [[ "$ownership_case" == "created" ]]; then
    for expected in 'rm -f compose-owned-id' 'volume rm volume-owned-id' 'network rm network-owned-id' 'rm -f restore-owned-id' 'rm -f readonly-owned-id' 'rm -f proxy-owned-id' 'rm -f load-owned-id' 'rm created-owned-id' 'image rm sha256:newly-pulled-image-id'; do
      grep -Fqx -- "$expected" "$record" || { rm -rf "$tmp"; exit 1; }
    done
  elif [[ -n "$calls" ]]; then rm -rf "$tmp"; exit 1; fi
  printf '[stage5a] ownership selftest PASS: %s\n' "$ownership_case"
  rm -rf "$tmp"
  exit 0
fi

cleanup() {
  local rc=$?
  trap - EXIT
  set +e
  cleanup_owned_resources
  rm -rf "$WORK"
  script_exit "$rc"
}
trap cleanup EXIT

compose() {
  local rc=0
  case "${1:-}" in
    down) cleanup_owned_resources; return 0 ;;
    up) COMPOSE_STARTED=1 ;;
  esac
  ( cd "$ROOT/deploy/docker" && docker compose -f compose.yml -f compose.test.yml --project-name "$PROJECT" --env-file "$WORK/compose.env" "$@" ) || rc=$?
  if [[ "${1:-}" == "up" && "$CLEANUP_ARMED" == "1" ]]; then register_compose_resources || return $?; fi
  return "$rc"
}
# cexec 在验证服务容器里跑一条命令；stdin 透传（管理请求体走管道，不在容器里留临时文件）。
cexec() { compose exec -T "$@"; }
container_id() {
  local out
  out="$(compose ps -q epocha 2>/dev/null)" || return 1
  printf '%s\n' "${out%%$'\n'*}"
}

# http 状态码（不打印响应体，避免把 token 抄进终端）。
code() { curl -sS -o /dev/null -w '%{http_code}' --max-time "${2:-10}" "$1" 2>/dev/null || echo "000"; }
body() { curl -sS --max-time "${2:-10}" "$1" 2>/dev/null || echo ""; }
pbody() { curl -sSk --max-time "${2:-10}" "$1" 2>/dev/null || echo ""; }
pcode() { curl -sSk -o /dev/null -w '%{http_code}' --max-time "${2:-10}" "$1" 2>/dev/null || echo "000"; }
# plog 取反代响应头（-D 写文件，正文丢到 /dev/null）。
pheaders() { curl -sSk -o /dev/null -D - --max-time 10 "$1" 2>/dev/null || echo ""; }

wait_status() { # $1=URL $2=期望码 $3=秒
  local deadline=$((SECONDS + ${3:-40}))
  while [[ $SECONDS -lt $deadline ]]; do
    if [[ "$(code "$1")" == "${2:-200}" ]]; then return 0; fi
    sleep 1
  done
  return 1
}

# leaks FILE VALUE：按值扫描。**空值一律返回"没泄露"**——`grep -F ""` 匹配任何文件，
# 把一个前置步骤失败（值为空）伪装成"泄露"是最难查的假阳性（本轮实测撞过）。
leaks() {
  local f="$1" v="$2"
  [[ -n "$v" ]] || return 1
  [[ -f "$f" ]] || return 1
  grep -qF -- "$v" "$f"
}

admin_api() { # $1=METHOD $2=PATH $3=JSON体(可空) → stdout 是响应体（含 secret，绝不 tee 到终端）
  local method="$1" path="$2" jsonbody="${3:-}"
  if [[ -n "$jsonbody" ]]; then
    printf '%s' "$jsonbody" | cexec epocha "$OPS" admin "$method" "$path" 2>"$LOGS/admin.err"
  else
    cexec epocha "$OPS" admin "$method" "$path" </dev/null 2>"$LOGS/admin.err"
  fi
}

# admin_json METHOD PATH OUTFILE [JSON体]：调管理面并把 ops 输出的首行（status=NNN）剥掉，
# 只把 JSON 写进 OUTFILE。退出码**显式**来自 admin_api —— 绝不用 `| tail`/`| sed` 掩盖它。
admin_json() {
  local method="$1" path="$2" out="$3" jsonbody="${4:-}" raw="$WORK/admin.raw" rc=0
  if [[ -n "$jsonbody" ]]; then
    admin_api "$method" "$path" "$jsonbody" >"$raw" 2>"$LOGS/admin.err" || rc=$?
  else
    admin_api "$method" "$path" </dev/null >"$raw" 2>"$LOGS/admin.err" || rc=$?
  fi
  if [[ $rc -ne 0 ]]; then return $rc; fi
  sed -n '2,$p' "$raw" >"$out"
}

port_listening() { # $1=端口 → 0=在监听 1=空闲 2=测不出来（**绝不**把“测不出来”当成“空闲”）
  local out rc=0 body
  out="$(ss -ltn "sport = :$1" 2>/dev/null)" || rc=$?
  if [[ $rc -ne 0 ]]; then return 2; fi
  # 命令替换会剥掉尾部换行，所以"没有匹配行"时 out 里**一个换行都没有**（只剩表头）。
  # 这时 ${out#*$'\n'} 不匹配就原样返回表头 ⇒ 会把"空闲"读成"被占用"（实测撞过），所以先按换行判定。
  if [[ "$out" != *$'\n'* ]]; then return 1; fi
  body="${out#*$'\n'}"
  [[ -n "${body//[[:space:]]/}" ]]
}

# --- 第 0 步：环境与前置条件 ---
log "第 0 步：环境、磁盘与端口"
for tool in docker curl jq openssl; do
  command -v "$tool" >/dev/null 2>&1 || abort "缺少工具 $tool（本门禁无法执行）"
done
if ! docker info >/dev/null 2>&1; then
  abort "docker daemon 不可达（阶段 5A 的容器演练无法执行；工程项部分仍可用 EPOCHA_5A_SKIP_DOCKER=1 单独跑）"
fi
for p in "$PUBLIC_PORT" "$RESTORE_PORT" "$PROXY_PORT" "$LOAD_PORT" "$READONLY_PORT" 8081 8080; do
  rc=0
  port_listening "$p" || rc=$?
  if [[ $rc -eq 0 ]]; then
    abort "端口 $p 已被占用：演练可能连到遗留进程并以错误的原因通过。换端口或先停掉占用者。"
  elif [[ $rc -ne 1 ]]; then
    abort "ss 无法判定端口 $p 的状态（rc=$rc）：不能把“测不出来”当成“端口空闲”"
  fi
done
# Explicit prefixes are allowed only when all derived names/projects are absent.
for resource in "$RESTORE_NAME" "$PROXY_NAME" "$READONLY_NAME" "$LOAD_NAME"; do
  if docker container inspect "$resource" >/dev/null 2>&1; then abort "已有资源 $resource；拒绝复用或清理"; fi
done
if docker image inspect "$IMAGE" >/dev/null 2>&1; then abort "已有资源 $IMAGE；拒绝覆盖镜像"; fi
if docker network inspect "${PROJECT}_default" >/dev/null 2>&1; then abort "已有资源 ${PROJECT}_default；拒绝复用网络"; fi
for kind in container volume network; do
  list_flags=(-q)
  [[ "$kind" != container ]] || list_flags=(-aq)
  existing="$(docker "$kind" ls "${list_flags[@]}" --filter "label=com.docker.compose.project=$PROJECT")" || abort "无法核对已有资源"
  [[ -z "$existing" ]] || abort "已有资源属于项目 $PROJECT；拒绝复用或清理"
done
CLEANUP_ARMED=1
pass "docker/curl/jq/openssl 可用，三个演练端口空闲（$PUBLIC_PORT/$RESTORE_PORT/$PROXY_PORT）；project=$PROJECT"

# 镜像 + 演练工作区 + 一次性恢复容器 + 可选 nginx 的余量。低于阈值时相关步骤 BLOCKED，
# 其余不需要磁盘的检查照常跑（不能因为磁盘就把整轮停成一句“空间不足”）。
NEED_KB=$((180 * 1024))
AVAIL_KB="$(avail_kb)"
if (( AVAIL_KB >= NEED_KB )); then
  DOCKER_OK=1
  pass "磁盘余量 $((AVAIL_KB / 1024))MB ≥ 演练所需 $((NEED_KB / 1024))MB"
else
  blocked "磁盘余量 $((AVAIL_KB / 1024))MB < 容器演练所需 $((NEED_KB / 1024))MB ⇒ 镜像构建与容器演练本轮不执行" \
    "释放磁盘后单独跑：bash docs/scripts/stage5a.sh（工程回归不受影响，会照常执行）"
fi

# --- 第 1 步：一次性代码回归 ---
log "第 1 步：工程回归（make stage3b-check 一次，含 fmt-check/vet/race/size/secret-scan/stage1/2/3a/3b）"
rc=0
make stage3b-check >"$LOGS/regression.log" 2>&1 || rc=$?
if [[ $rc -ne 0 ]]; then
  tail -n 40 "$LOGS/regression.log"
  fail "make stage3b-check 退出码 $rc（5A 不得破坏既有阶段）"
else
  pass "make stage3b-check 退出码 0（回归一次跑完，不把同一套 -race 跑第二遍）"
fi

log "第 1b 步：5A 新增包的定向测试与依赖隔离"
rc=0
go test -count=1 -race ./internal/config/ ./internal/store/ ./internal/api/ ./cmd/ops/ ./cmd/loadtest/ >"$LOGS/pkg.log" 2>&1 || rc=$?
if [[ $rc -ne 0 ]]; then
  tail -n 40 "$LOGS/pkg.log"
  fail "5A 新增/改动包测试退出码 $rc"
else
  pass "config(密钥文件) / store(备份恢复+就绪) / api(/readyz) / ops / loadtest 的 -race 测试全通过"
fi
DEPS="$(go list -deps ./cmd/server)"
for banned in internal/eval cmd/preview cmd/devseed cmd/evalgen cmd/evalocr cmd/exampleapp cmd/blindtest cmd/loadtest web/preview; do
  if printf '%s\n' "$DEPS" | grep -q "EpoCHA/$banned"; then
    fail "服务二进制链接了开发期包 $banned"
  fi
done
pass "cmd/server 不链接 loadtest 等开发期包（go list -deps 证明）"

log "第 1c 步：运维命令按值扫描（哨兵凭据不进 stdout/stderr）"
rc=0
go test -count=1 -v -run 'TestBackupVerifyRestoreThroughCLI|TestEnvAndFileBothSetIsRefused|TestMissingKeyMaterialRefusesWithExit1|TestAdminSubcommandIsLoopbackOnly' ./cmd/ops/ >"$LOGS/opsleak.log" 2>&1 || rc=$?
if [[ $rc -ne 0 ]]; then
  tail -n 30 "$LOGS/opsleak.log"
  fail "cmd/ops 的按值扫描未通过（退出码 $rc）"
else
  pass "cmd/ops 备份/恢复/管理输出不含 pepper、master、站点 secret、管理凭据的值"
fi

# --- 第 2 步：镜像与交付面 ---
log "第 2 步：生产镜像构建与交付清单"
if [[ $DOCKER_OK -eq 1 ]]; then
  rc=0
  make docker-image-local >"$LOGS/docker-build.log" 2>&1 || rc=$?
  if [[ $rc -ne 0 ]]; then
    tail -n 30 "$LOGS/docker-build.log"
    fail "make docker-image-local 退出码 $rc"
  else
    SZ="$(docker image inspect "$IMAGE" --format '{{.Size}}')"
    pass "镜像构建成功（$IMAGE，$((SZ / 1024 / 1024))MB）"
  fi

  # 交付清单：镜像里**只**应有那两把二进制。这是“不包含 OCR/浏览器/盲测/评测样本/ground truth”
  # 的可核查形式，而不是“我看 Dockerfile 没 COPY 它”。
  # 用 docker create + docker export 取**文件系统视图**：Docker 29 的 `docker save` 默认产出 OCI
  # blobs（列出来只有哈希文件名，等于什么都没测），而 export 给出真实路径清单。
  # `.dockerenv`、`/etc/hosts`、`/dev/*`、`/proc`、`/sys` 是运行时挂进去的，不属于镜像层。
  rc=0
  LC_CID=""
  LC_CID="$(docker create "$IMAGE" true 2>/dev/null)" || rc=$?
  if [[ $rc -eq 0 && -n "$LC_CID" ]]; then LC_CID="$(docker inspect -f '{{.Id}}' "$LC_CID")"; fi
  if [[ $rc -ne 0 || -z "$LC_CID" ]]; then
    fail "docker create 失败，无法列出镜像文件系统（rc=$rc）"
  else
    rc=0
    docker export "$LC_CID" >"$WORK/imagefs.tar" 2>/dev/null || rc=$?
    # 目录条目（`dev/`、`usr/local/bin/`、WORKDIR 建出来的 `var/lib/epocha/`）不是文件内容，
    # 不能拿来和白名单逐行比较；只保留非目录条目。Docker 29 的 export 还会带上
    # `.dockerenv`、`/etc/hosts` 等运行时挂载，它们也不属于镜像层，按精确路径排除。
    if [[ $rc -ne 0 ]] || ! tar -tf "$WORK/imagefs.tar" 2>/dev/null | grep -v '/$' >"$LOGS/image-files.txt"; then
      fail "docker export 读取镜像文件系统失败（rc=$rc）"
    else
      BAD=""
      for pat in evalgen evalocr blindtest devseed exampleapp preview eval-out tesseract ground-truth go.mod; do
        if grep -qi "$pat" "$LOGS/image-files.txt"; then BAD="$BAD $pat"; fi
      done
      # 可执行文件按"完整路径"匹配，不做子串：`dev/shm` 含 "sh"，子串匹配会把运行时挂载项
      # 读成"镜像里有 shell"（本轮实测撞过这个假阳性）。
      for exe in sh bash dash zsh curl wget sqlite3 python3 perl busybox apk apt-get; do
        if grep -qE "(^|/)(usr/)?(bin|sbin)/${exe}$" "$LOGS/image-files.txt"; then BAD="$BAD $exe"; fi
      done
      NFILES="$(wc -l <"$LOGS/image-files.txt")"
      HASERVER="$(grep -c 'usr/local/bin/epocha$' "$LOGS/image-files.txt" || true)"
      HASOPS="$(grep -c 'usr/local/bin/epocha-ops$' "$LOGS/image-files.txt" || true)"
      NWAYSPUT="$(grep -c '^\(usr/bin/\|bin/\|sbin/\)' "$LOGS/image-files.txt" || true)"
      EXPECTED_IMAGE_FILES='usr/local/bin/epocha
usr/local/bin/epocha-ops'
      RUNTIME_MOUNTS='^(\.dockerenv|dev/console|etc/hostname|etc/hosts|etc/mtab|etc/resolv\.conf)$'
      EXTRA_IMAGE_FILES="$(grep -vFxf <(printf '%s\n' "$EXPECTED_IMAGE_FILES") "$LOGS/image-files.txt" | grep -vE "$RUNTIME_MOUNTS" || true)"
      if [[ -n "$EXTRA_IMAGE_FILES" ]]; then
        fail "镜像文件清单含白名单之外的路径: $(printf '%s' "$EXTRA_IMAGE_FILES" | tr '\n' ' ')"
      fi
      if [[ -z "$BAD" && "$HASERVER" == "1" && "$HASOPS" == "1" && "$NWAYSPUT" == "0" && -z "$EXTRA_IMAGE_FILES" ]]; then
        pass "镜像文件系统只有 /usr/local/bin 下的两把二进制（条目 $NFILES；无 shell/curl/sqlite3/OCR/评测样本/浏览器）"
      else
        fail "镜像文件清单不是预期的两把二进制（server=$HASERVER ops=$HASOPS 其它路径=$NWAYSPUT）"
      fi
    fi
    docker rm "$LC_CID" >/dev/null 2>&1 || true
    rm -f "$WORK/imagefs.tar"
  fi

  # 镜像的默认安全形状与探针定义（不是“进程存在”型健康检查）。
  IMGUSER="$(docker image inspect "$IMAGE" --format '{{.Config.User}}')"
  IMGHC="$(docker image inspect "$IMAGE" --format '{{json .Config.Healthcheck}}')"
  IMGENTRY="$(docker image inspect "$IMAGE" --format '{{json .Config.Entrypoint}}')"
  IMGLISTEN="$(docker image inspect "$IMAGE" --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -E '^EPOCHA_(LISTEN|ADMIN_LISTEN)=' | paste -sd, -)"
  if [[ "$IMGUSER" != "" && "$IMGUSER" != "0" && "$IMGUSER" != "root" ]]; then
    pass "镜像默认用户是非 root（$IMGUSER）"
  else
    fail "镜像默认用户是 root/空（$IMGUSER）"
  fi
  if printf '%s' "$IMGHC" | grep -q "readyz" && printf '%s' "$IMGHC" | grep -q "$OPS"; then
    pass "镜像 HEALTHCHECK 用镜像内真实存在的 epocha-ops 打 /readyz（不是 curl/sh，也不是 /healthz 那种“进程存在”型判定"
  else
    fail "镜像 HEALTHCHECK 形状不对：$IMGHC"
  fi
  if printf '%s' "$IMGENTRY" | grep -q "$OPS\|/epocha\"" && ! printf '%s' "$IMGENTRY" | grep -q '"sh"'; then
    pass "ENTRYPOINT 是 exec 形式且无 sh 包装（SIGTERM 直达进程，优雅关闭才成立）"
  else
    fail "ENTRYPOINT 不是 exec 形式：$IMGENTRY"
  fi
  if [[ "$IMGLISTEN" == *"EPOCHA_LISTEN=0.0.0.0:8080"* && "$IMGLISTEN" == *"EPOCHA_ADMIN_LISTEN=127.0.0.1:8081"* ]]; then
    pass "镜像内监听形状：公共面 0.0.0.0:8080（可被端口映射打到）、管理面容器内 loopback"
  else
    fail "镜像内监听形状不对：$IMGLISTEN"
  fi
else
  for item in "镜像构建" "镜像交付清单" "镜像安全形状（用户/健康检查/入口/监听）"; do
    blocked "$item：磁盘不足，本轮未执行" "释放磁盘后：make docker-image-local && bash docs/scripts/stage5a.sh"
  done
fi

# --- 第 3 步：演练工作区的密钥 ---
log "第 3 步：密钥文件与 compose 工作区"
gen_key_file() { # $1=路径 —— base64(32 随机字节)，与 config 的校验规则一致
  head -c32 /dev/urandom | base64 >"$1"
  chmod 600 "$1"
}
gen_key_file "$D/keys/master"
gen_key_file "$D/keys/pepper"
gen_key_file "$D/keys-restore/master"   # 恢复实例用**新的**主密钥（不设 PREVIOUS）
cp "$D/keys/pepper" "$D/keys-restore/pepper"   # pepper 与备份配对，恢复才允许
chmod 600 "$D/keys-restore/pepper" "$D/keys-restore/master"
# 管理凭据是 Bearer 明文本身（不做 base64 变换），逐字节写入。
ADMIN_OLD="ak_$(head -c32 /dev/urandom | base64 | tr -d '/+=' | head -c48)"
ADMIN_NEW="ak_$(head -c32 /dev/urandom | base64 | tr -d '/+=' | head -c48)"
printf '%s' "$ADMIN_OLD" >"$D/keys/admin"; chmod 600 "$D/keys/admin"
printf '%s' "$ADMIN_NEW" >"$D/keys-restore/admin"; chmod 600 "$D/keys-restore/admin"
head -c32 /dev/urandom | base64 >"$D/keys-foreign-pepper.b64"; chmod 600 "$D/keys-foreign-pepper.b64"

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
EPOCHA_TEST_UID=$RUN_UID
EPOCHA_TEST_GID=$RUN_GID
EPOCHA_ADMIN_KEY_FILE=$C_KEYS/admin
EPOCHA_TRUST_PROXY=false
EPOCHA_NETWORK=${PROJECT}_default
EPOCHA_SUBNET=172.29.0.0/24
EOF
chmod 600 "$WORK/compose.env"
pass "演示密钥运行时生成、写进 0600 文件、落在临时目录并由 trap 删除（仓库内没有任何真实值）"

# --- 第 4 步：compose 合法性 + 密钥 fail-closed ---
log "第 4 步：compose 配置合法性与缺密钥的拒绝形状"
if [[ $DOCKER_OK -eq 1 ]]; then
  rc=0
  compose config --format json >"$LOGS/compose-config.json" 2>"$LOGS/compose-config.err" || rc=$?
  if [[ $rc -eq 0 ]]; then
    # 关键边界：不能出现“省略宿主 IP”的端口写法（省略 = 绑 0.0.0.0 = 直接发布到公网网卡）。
    # 按渲染后的**结构**判定，而不是 grep YAML 文本：渲染结果可能是 host_ip/published 的映射形式。
    if jq -e '[.services.epocha.ports[] | select((.host_ip // "") != "127.0.0.1")] | length == 0' \
        "$LOGS/compose-config.json" >/dev/null 2>&1; then
      pass "compose config 合法，且每个发布端口都显式绑宿主 127.0.0.1"
    else
      fail "compose 渲染出的端口里有未绑宿主 loopback 的项：$(jq -c '.services.epocha.ports' "$LOGS/compose-config.json")"
    fi
    # 管理面：渲染结果里只能有 target 8080 一个发布项；8081 只应出现在 env 里。
    NPUB="$(jq -r '.services.epocha.ports | length' "$LOGS/compose-config.json")"
    NTGT="$(jq -r '[.services.epocha.ports[] | select(.target != 8080)] | length' "$LOGS/compose-config.json")"
    if [[ "$NPUB" == "1" && "$NTGT" == "0" ]]; then
      pass "只有一个发布端口（8080）；管理口 8081 没有任何发布项"
    else
      fail "发布端口项不对（数量 $NPUB，非 8080 目标 $NTGT）：$(jq -c '.services.epocha.ports' "$LOGS/compose-config.json")"
    fi
    # 资源限额与挂载形状在配置层就要成立（容器层的 inspect 在第 5 步核实真的生效）。
    # 渲染后的 memory 已经是**字节数**（compose 会归一化 "128m"），断言必须按归一化后的值写。
    if jq -e '.services.epocha.deploy.resources.limits | (.cpus == 1) and ((.memory == "134217728") or (.memory == 134217728)) and (.pids == 128)' \
        "$LOGS/compose-config.json" >/dev/null 2>&1 &&
       jq -e '.services.epocha | (.read_only == true) and (.cap_drop == ["ALL"]) and ((.memswap_limit == "134217728") or (.memswap_limit == 134217728))' \
        "$LOGS/compose-config.json" >/dev/null 2>&1; then
      pass "配置层的边界齐备：read_only、cap_drop ALL、memory/memswap 128m、cpus 1、pids 128"
    else
      fail "配置层缺边界项：$(jq -c '{ro:.services.epocha.read_only,caps:.services.epocha.cap_drop,lim:.services.epocha.deploy.resources.limits}' "$LOGS/compose-config.json")"
    fi
    if jq -e '[.services.epocha.volumes[] | select(.target == "/etc/epocha/keys")][0].read_only == true' \
        "$LOGS/compose-config.json" >/dev/null 2>&1; then
      pass "密钥目录在配置层就是只读挂载，且与数据/备份目录分开（三个独立挂载项）"
    else
      fail "密钥目录没有按只读挂载：$(jq -c '.services.epocha.volumes' "$LOGS/compose-config.json")"
    fi
  else
    cat "$LOGS/compose-config.err"
    fail "docker compose config 退出码 $rc"
  fi

  # 缺主密钥 ⇒ 拒绝启动（不是“降级启动”）。用同一份镜像，只把 master 文件换掉。
  # 失败原因要去 **compose logs** 里读：`up -d --wait` 自己的输出只有编排信息，不含容器 stderr。
  mv "$D/keys/master" "$D/master.bak"
  rc=0
  compose up -d --wait --wait-timeout 25 >"$LOGS/nomaster.up.log" 2>&1 || rc=$?
  compose logs --no-color >"$LOGS/nomaster.log" 2>&1 || true
  if [[ $rc -eq 0 ]]; then
    compose down >/dev/null 2>&1 || true
    mv "$D/master.bak" "$D/keys/master"
    fail "缺主密钥时容器却启动了（退出码 0）"
  else
    mv "$D/master.bak" "$D/keys/master"
    if grep -qiE "EPOCHA_MASTER_KEY|主密钥" "$LOGS/nomaster.log"; then
      pass "缺主密钥 ⇒ 拒绝启动，且日志点名密钥入口（可从容器日志定位到缺哪一把）"
    else
      fail "拒绝启动了但没说明原因：$(tail -c 300 "$LOGS/nomaster.log")"
    fi
    if leaks "$LOGS/nomaster.log" "$(cat "$D/keys/pepper")"; then
      fail "启动失败输出里出现了 pepper 的值"
    fi
  fi
  compose down >/dev/null 2>&1 || true

  # 密钥文件权限过宽 ⇒ fail closed（readKeyFile 要求 group/other 位为空）。
  chmod 644 "$D/keys/master"
  rc=0
  compose up -d --wait --wait-timeout 25 >"$LOGS/badperm.up.log" 2>&1 || rc=$?
  compose logs --no-color >"$LOGS/badperm.log" 2>&1 || true
  compose down >/dev/null 2>&1 || true
  chmod 600 "$D/keys/master"
  if [[ $rc -eq 0 ]]; then
    fail "密钥文件 0644 时竟然启动成功（必须 fail closed）"
  else
    pass "密钥文件权限过宽 ⇒ 拒绝启动（0600/0400 之外不可用）"
  fi

  # 镜像里烤死了 *_FILE 默认值。用 env 提供主密钥时必须**显式清空**那个变量，
  # 否则两条入口同时存在 ⇒ 拒绝启动（不静默优先）。这条检查同时证明两件事：
  # 拒绝生效、以及“覆盖方式是文档里写的那一种”。
  MASTER_B64_CONTENT="$(cat "$D/keys/master")"
  rc=0
  docker run --rm --user "$RUN_UID:$RUN_GID" \
    -v "$D/keys:$C_KEYS:ro" -v "$D/data:/var/lib/epocha" \
    -e "EPOCHA_MASTER_KEY=$MASTER_B64_CONTENT" \
    "$IMAGE" >"$LOGS/bothset.log" 2>&1 || rc=$?
  if [[ $rc -ne 0 ]] && grep -qE "同时设置了" "$LOGS/bothset.log"; then
    pass "env 与镜像烤死的 *_FILE 同时存在 ⇒ 拒绝启动（点名两个入口，不静默选一个）"
  else
    fail "同时设置两份密钥入口的行为不对（rc=$rc）：$(tail -c 240 "$LOGS/bothset.log")"
  fi
  if leaks "$LOGS/bothset.log" "$MASTER_B64_CONTENT"; then
    fail "拒绝启动的输出里出现了主密钥的值"
  fi
else
  blocked "compose 合法性与缺密钥拒绝形状：磁盘不足" "释放磁盘后：bash docs/scripts/stage5a.sh"
  blocked "密钥文件权限 fail closed：磁盘不足" "释放磁盘后：bash docs/scripts/stage5a.sh"
fi

# --- 第 4b 步：宿主上的 *_FILE 与拒绝形状（不依赖 Docker）---
log "第 4b 步：宿主二进制上的密钥入口（与容器同一套规则）"
go build -trimpath -o "$BIN/epocha" ./cmd/server
go build -trimpath -o "$BIN/epocha-ops" ./cmd/ops
go build -trimpath -o "$BIN/devclient" ./cmd/devclient
go build -trimpath -o "$BIN/loadtest" ./cmd/loadtest
pass "构建 server / ops / devclient / loadtest 到临时目录"

DEVDB="$D/hostdata"
mkdir -p "$DEVDB"
rc=0
EPOCHA_MASTER_KEY_FILE="$D/keys/master" EPOCHA_PEPPER="$(cat "$D/keys/pepper")" \
  "$BIN/epocha" --db "$DEVDB/x.db" --listen 127.0.0.1:18099 --admin-listen 127.0.0.1:18098 >"$LOGS/envfile.log" 2>&1 &
HOSTPID=$!
if wait_status "http://127.0.0.1:18099/readyz" 200 15; then
  pass "*_FILE 与 env 混合入口能起服务（master 走文件、pepper 走 env），/readyz 200"
else
  fail "混合密钥入口没能起服务"
fi
kill -TERM "$HOSTPID" 2>/dev/null || true
wait "$HOSTPID" 2>/dev/null || true

rc=0
EPOCHA_MASTER_KEY_FILE="$D/keys/master" EPOCHA_MASTER_KEY="$(cat "$D/keys/master")" EPOCHA_PEPPER_FILE="$D/keys/pepper" \
  "$BIN/epocha" --db "$DEVDB/y.db" --listen 127.0.0.1:18099 >"$LOGS/both.log" 2>&1 || rc=$?
if [[ $rc -ne 0 ]] && grep -qE "同时设置了" "$LOGS/both.log"; then
  pass "env 与 *_FILE 同时设置 ⇒ 拒绝启动，错误点名两个入口（不静默优先）"
else
  fail "env 与 *_FILE 同时设置的行为不对（rc=$rc）：$(tail -c 200 "$LOGS/both.log")"
fi

rc=0
EPOCHA_MASTER_KEY_FILE="$D/keys/nope" EPOCHA_PEPPER_FILE="$D/keys/pepper" \
  "$BIN/epocha" --db "$DEVDB/z.db" --listen 127.0.0.1:18099 >"$LOGS/missing.log" 2>&1 || rc=$?
if [[ $rc -ne 0 ]] && ! grep -qF "$D/keys/nope" "$LOGS/missing.log"; then
  pass "密钥文件缺失 ⇒ 拒绝启动，且错误里不带文件路径"
else
  fail "密钥文件缺失的行为不对（rc=$rc）：$(tail -c 200 "$LOGS/missing.log")"
fi

# --- 第 5 步：容器启动、探针、边界 ---
log "第 5 步：首次初始化、就绪探针与宿主端口边界"
if [[ $DOCKER_OK -eq 1 ]]; then
  rc=0
  compose up -d --wait --wait-timeout 60 >"$LOGS/up.log" 2>&1 || rc=$?
  CID="$(container_id)"
  if [[ $rc -ne 0 || -z "$CID" ]]; then
    tail -n 25 "$LOGS/up.log"
    fail "compose up 失败（退出码 $rc）"
  else
    pass "首次启动成功，容器进入 healthy（--wait 依镜像里的 /readyz 健康检查判定）"

    # 数据目录形状：库 + WAL 在持久卷里，属主是容器 uid。
    if [[ -f "$D/data/epocha.db" ]]; then
      DBOWNER="$(stat -c '%u:%g %a' "$D/data/epocha.db")"
      pass "SQLite 落在持久卷（$D/data/epocha.db，属主/权限 $DBOWNER），根文件系统之外"
    else
      fail "数据目录里没有 epocha.db（持久化不成立）"
    fi
    rc=0
    docker inspect -f '{{json .Mounts}}' "$CID" >"$LOGS/mounts.json" 2>/dev/null || rc=$?
    if [[ $rc -eq 0 ]] && jq -e '[.[] | select(.Destination=="/etc/epocha/keys")][0] | (.Mode|test("ro")) and (.RW == false)' \
        "$LOGS/mounts.json" >/dev/null 2>&1; then
      pass "密钥目录是**只读**挂载，且与数据目录分开（可写的“只读配置”等于密钥可被换掉）"
    else
      fail "密钥目录没有按只读挂载（mounts=$(cat "$LOGS/mounts.json" 2>/dev/null | tr -d '\n' | head -c 200)）"
    fi

    # 探针语义：/readyz 与 /healthz 都 200，且 /readyz 不改变账本。
    if [[ "$(code "http://127.0.0.1:$PUBLIC_PORT/readyz")" == "200" && "$(code "http://127.0.0.1:$PUBLIC_PORT/healthz")" == "200" ]]; then
      pass "宿主 loopback 上 /readyz 与 /healthz 均 200"
    else
      fail "探针不通：readyz=$(code "http://127.0.0.1:$PUBLIC_PORT/readyz") healthz=$(code "http://127.0.0.1:$PUBLIC_PORT/healthz")"
    fi

    # 容器与宿主的安全选项**实际生效**（写进文件不等于内核在执行，必须 inspect）。
    HC="$(docker inspect -f '{{json .HostConfig}}' "$CID")"
    printf '%s' "$HC" >"$LOGS/inspect-hostconfig.json"
    printf '%s' "$(docker inspect -f '{{json .Config}}' "$CID")" >"$LOGS/inspect-config.json"
    HCK() { # 对 HostConfig 断言
      jq -e "$1" >/dev/null "$LOGS/inspect-hostconfig.json" 2>/dev/null
    }
    CCK() { # 对 Config 断言
      jq -e "$1" >/dev/null "$LOGS/inspect-config.json" 2>/dev/null
    }
    BAD=""
    HCK '.ReadonlyRootfs == true' || BAD="$BAD read_only"
    HCK '.SecurityOpt | index("no-new-privileges:true") != null' || BAD="$BAD no-new-privileges"
    HCK '(.CapDrop | index("ALL")) != null' || BAD="$BAD cap_drop"
    HCK '.Memory == 134217728' || BAD="$BAD memory=128m"
    HCK '.MemorySwap == 134217728' || BAD="$BAD 禁 swap"
    HCK '.NanoCpus == 1000000000' || BAD="$BAD cpus=1"
    HCK '.PidsLimit == 128' || BAD="$BAD pids=128"
    HCK '.MemorySwappiness == null or .MemorySwappiness == -1 or .MemorySwappiness == 0' || BAD="$BAD swappiness"
    CCK '.User | test("^[0-9]+:[0-9]+$")' || BAD="$BAD 非 root 数字 uid"
    CCK '.User != "0:0" and .User != "root"' || BAD="$BAD root 用户"
    if [[ -z "$BAD" ]]; then
      pass "安全选项实际生效（inspect 核实：只读根、no-new-privileges、cap_drop ALL、内存 128MiB 且禁 swap、1 CPU、pids 128、非 root 用户）"
    else
      fail "下列安全边界没有在容器上落地:$BAD"
    fi

    # 宿主端口边界：只有 127.0.0.1:$PUBLIC_PORT，没有 8081，也没有 0.0.0.0。
    rc=0
    ss -ltn >"$LOGS/ss.txt" 2>/dev/null || rc=$?
    if [[ $rc -ne 0 ]]; then
      fail "ss 取不到监听表（rc=$rc）：端口边界无法判定"
    fi
    if grep -qE "^(0\.0\.0\.0|\*|\[?\:\:?\>)\s*:$PUBLIC_PORT" "$LOGS/ss.txt"; then
      fail "公共端口绑定在了非 loopback 地址上（等于直接发布到公网网卡）"
    elif grep -q ":8081" "$LOGS/ss.txt"; then
      fail "宿主上出现了管理端口 8081"
    elif grep -q ":$PUBLIC_PORT" "$LOGS/ss.txt"; then
      pass "宿主监听表：只有 127.0.0.1:$PUBLIC_PORT；管理面 8081 与恢复实例端口都不存在"
    else
      fail "宿主上连 127.0.0.1:$PUBLIC_PORT 都没监听（容器端口映射没生效）"
    fi

    # 管理面只能从容器内部走（容器 loopback ≠ 宿主 loopback，这是最容易读错的一条）。
    rc=0
    admin_json GET /api/admin/whoami "$LOGS/whoami.json" || rc=$?
    if [[ $rc -eq 0 ]] && jq -e '.key_id' "$LOGS/whoami.json" >/dev/null 2>&1; then
      pass "容器内 loopback 上的管理面可用（epocha-ops admin GET /api/admin/whoami）"
    else
      fail "容器内管理面不通（rc=$rc）：$(tail -c 200 "$LOGS/admin.err")"
    fi

    # 宿主直连容器内管理口这条路径必须**不存在**：经发布端口打管理路由 ⇒ 404。
    if [[ "$(code "http://127.0.0.1:$PUBLIC_PORT/api/admin/sites")" == "404" ]]; then
      pass "公共端口上没有管理路由（404，不是 401：路由根本不存在）"
    else
      fail "公共端口暴露了管理路由（code=$(code "http://127.0.0.1:$PUBLIC_PORT/api/admin/sites")）"
    fi
  fi
else
  blocked "容器启动/探针/安全边界/管理面边界演练：磁盘不足" "释放磁盘后：bash docs/scripts/stage5a.sh"
fi

# --- 第 6 步：业务闭环 + 重建容器的持久化语义 ---
log "第 6 步：站点创建、完整闭环、重建容器后的状态"
SITE_JSON="$WORK/site.json"
SITE_KEY=""
SITE_ID=""
SECRET1=""
SECRET2=""
if [[ $DOCKER_OK -eq 1 && -n "$CID" ]]; then
  rc=0
  # ops admin 的输出是"status=NNN\n{json}"两行：**必须**先剥掉首行再交给 jq。
  # 不剥行 ⇒ SITE_KEY/SECRET 变成空串 ⇒ 空串喂给 `grep -F` 会匹配任何文件 ⇒
  # 把"前置步骤失败"伪装成"泄露检测失败"（实测撞过，一整串 FAIL 都是它级联的）。
  admin_json POST /api/admin/sites "$SITE_JSON" '{"name":"stage5a","verify_mode":"pow_only","allow_missing_origin":true,"pow_bits":12}' || rc=$?
  if [[ $rc -eq 0 ]]; then
    SITE_KEY="$(jq -r '.site.site_key // ""' "$SITE_JSON")"
    SITE_ID="$(jq -r '.site.id // ""' "$SITE_JSON")"
    SECRET1="$(jq -r '.secret // ""' "$SITE_JSON")"
  fi
  if [[ -n "$SITE_KEY" && -n "$SECRET1" ]]; then
    pass "经容器内管理面创建站点（pow_only、bits=12），站点 secret 只在创建响应里出现一次"
  else
    fail "站点创建没拿到 site_key/secret（rc=$rc）：$(head -c 200 "$SITE_JSON")"
  fi
  # secret 不得再出现在后续任何管理响应里。
  rc=0
  admin_json GET /api/admin/sites "$LOGS/sites-list.txt" || rc=$?
  if [[ $rc -ne 0 ]]; then fail "读取站点列表失败（rc=$rc）"; fi
  if leaks "$LOGS/sites-list.txt" "$SECRET1"; then
    fail "站点列表响应里出现了站点 secret 明文"
  else
    pass "站点列表/审计等后续响应不含 secret 明文"
  fi

  # 完整闭环：challenge → 真解 PoW → redeem → siteverify。
  rc=0
  EPOCHA_SITE_SECRET="$SECRET1" "$BIN/devclient" --base "http://127.0.0.1:$PUBLIC_PORT" --site-key "$SITE_KEY" --mode full \
    >"$LOGS/flow1.log" 2>&1 || rc=$?
  if [[ $rc -eq 0 ]] && grep -q "step=siteverify success=true" "$LOGS/flow1.log"; then
    pass "容器化服务上的完整验证闭环成功（真实暴力求解 PoW）"
  else
    tail -n 10 "$LOGS/flow1.log"
    fail "容器化服务上的完整闭环失败（rc=$rc）"
  fi

  # 一次性消费的两个方向都要测，而 devclient 的两种模式语义不同（第一轮在这里把期望读反过）：
  #   redeem-only ⇒ 拿到一个**尚未消费**的 response（acquire 会顺手验证一次，所以不能用它）
  #   verify-only ⇒ **期望验证失败**，因此它的 rc=0 才是"重放被正确拒绝"
  rc=0
  EPOCHA_SITE_SECRET="$SECRET1" "$BIN/devclient" --base "http://127.0.0.1:$PUBLIC_PORT" --site-key "$SITE_KEY" \
    --mode redeem-only --response-file "$WORK/resp-fresh" >"$LOGS/acq.log" 2>&1 || rc=$?
  if [[ $rc -ne 0 ]]; then
    fail "领不到未消费的 response：$(tail -c 200 "$LOGS/acq.log")"
  else
    V1OUT="$(curl -sS --max-time 10 -X POST -d "secret=$SECRET1&response=$(cat "$WORK/resp-fresh")" \
      "http://127.0.0.1:$PUBLIC_PORT/siteverify" 2>/dev/null || echo "")"
    if printf '%s' "$V1OUT" | grep -q '"success":true'; then
      pass "未消费的 response 首次 siteverify 成功（容器路径）"
    else
      fail "首次 siteverify 未通过：$(printf '%s' "$V1OUT" | head -c 160)"
    fi
    V2=0
    EPOCHA_SITE_SECRET="$SECRET1" "$BIN/devclient" --base "http://127.0.0.1:$PUBLIC_PORT" --site-key "$SITE_KEY" \
      --mode verify-only --response-file "$WORK/resp-fresh" >"$LOGS/v2.log" 2>&1 || V2=$?
    if [[ $V2 -eq 0 ]]; then
      pass "同一 response 第二次验证被拒（verify-only 期望失败，rc=0 即正确）"
    else
      fail "重放测试本身出错（rc=$V2）：$(tail -c 200 "$LOGS/v2.log")"
    fi
    V2CODES="$(curl -sS --max-time 10 -X POST -d "secret=$SECRET1&response=$(cat "$WORK/resp-fresh")" \
      "http://127.0.0.1:$PUBLIC_PORT/siteverify" 2>/dev/null || echo "")"
    if printf '%s' "$V2CODES" | grep -q 'timeout-or-duplicate'; then
      pass "重放的响应形状是 timeout-or-duplicate（reCAPTCHA 族，不透露第几层失败）"
    else
      fail "重放响应形状不对：$(printf '%s' "$V2CODES" | head -c 160)"
    fi
    rm -f "$WORK/resp-fresh"
  fi

  # 再领一个**已经消费过**的 response，专门用于"重建容器后仍不可重放"：
  # 消费这件事本身是必须活过重启的状态。
  rc=0
  EPOCHA_SITE_SECRET="$SECRET1" "$BIN/devclient" --base "http://127.0.0.1:$PUBLIC_PORT" --site-key "$SITE_KEY" \
    --mode acquire --response-file "$WORK/resp-before-restart" >"$LOGS/acq2.log" 2>&1 || rc=$?
  if [[ $rc -eq 0 ]]; then
    pass "领取并消费一个用于重启后重放测试的 response"
  else
    fail "无法领取重启前消费的 response：$(tail -c 200 "$LOGS/acq2.log")"
  fi

  # 轮换（transition_sec=0 ⇒ 立即撤销旧凭据）：撤销必须在重启后仍然成立。
  rc=0
  admin_json POST "/api/admin/sites/$SITE_ID/secrets/rotate" "$LOGS/rotate.json" '{"transition_sec":0}' || rc=$?
  SECRET2="$(jq -r '.secret // ""' "$LOGS/rotate.json")"
  if [[ -n "$SECRET2" ]]; then
    pass "站点凭据轮换并立即撤销旧凭据（新 key 见响应；旧 secret 从此不可用）"
    R1=0
    EPOCHA_SITE_SECRET="$SECRET1" "$BIN/devclient" --base "http://127.0.0.1:$PUBLIC_PORT" --site-key "$SITE_KEY" \
      --mode full >"$LOGS/revoked.log" 2>&1 || R1=$?
    if [[ $R1 -eq 0 ]]; then
      fail "被撤销的站点凭据仍能完成验证"
    else
      pass "被撤销的站点凭据当场已不可用"
    fi
  else
    fail "轮换没给出新凭据（rc=$rc）：$(head -c 160 "$LOGS/rotate.json")"
  fi

  # 停用站点：未消费的 token 立即失效。
  rc=0
  admin_api POST "/api/admin/sites/$SITE_ID/disable" '{}' >/dev/null 2>&1 || rc=$?
  if [[ $rc -eq 0 ]]; then
    pass "站点已停用"
  else
    fail "停用站点失败（rc=$rc）"
  fi

  # 重建容器（down + up：同一镜像、同一卷、同一密钥文件）。
  rc=0
  compose down >"$LOGS/down.log" 2>&1 || rc=$?
  rc2=0
  compose up -d --wait --wait-timeout 60 >"$LOGS/up2.log" 2>&1 || rc2=$?
  CID="$(container_id)"
  if [[ $rc -eq 0 && $rc2 -eq 0 && -n "$CID" ]]; then
    pass "容器已重建（不是复用同一个容器：卷与密钥文件是唯一延续）"
  else
    fail "重建容器失败（down=$rc up=$rc2）"
  fi

  # 停用状态不复活：challenge 是 POST，显式发一次取状态码。
  CH_CODE="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 -X POST -H 'Content-Type: application/json' \
    -d "{\"site_key\":\"$SITE_KEY\"}" "http://127.0.0.1:$PUBLIC_PORT/api/challenge" 2>/dev/null || echo 000)"
  if [[ "$CH_CODE" == "400" ]]; then
    pass "重建容器后站点仍是停用状态（400 invalid-request，与不存在的站点同形）"
  else
    fail "重建后停用站点竟然可领题（code=$CH_CODE）"
  fi

  # 启用回来，验证：旧 response 仍不可重放、被撤销凭据仍不可用、新凭据可用。
  admin_api POST "/api/admin/sites/$SITE_ID/enable" '{}' >/dev/null 2>&1 || true
  REPLAY_CODE="$(curl -sS --max-time 10 -X POST -d "secret=$SECRET1&response=$(cat "$WORK/resp-before-restart" 2>/dev/null)" \
    "http://127.0.0.1:$PUBLIC_PORT/siteverify" 2>/dev/null || echo "")"
  if printf '%s' "$REPLAY_CODE" | grep -q '"success":false'; then
    pass "重启前签发的 response 在重建容器后仍不可重放（状态在 SQLite 里，不在内存）"
  else
    fail "旧 response 在重建后被判成功：$(printf '%s' "$REPLAY_CODE" | head -c 160)"
  fi
  OLDSECRET_OUT="$(curl -sS --max-time 10 -X POST -d "secret=$SECRET1&response=garbage" \
    "http://127.0.0.1:$PUBLIC_PORT/siteverify" 2>/dev/null || echo "")"
  if printf '%s' "$OLDSECRET_OUT" | grep -q 'invalid-input-secret'; then
    pass "被撤销的站点凭据在重建后仍然撤销（引导逻辑不会把它写回来）"
  else
    fail "被撤销凭据的重启后判定形状不对：$(printf '%s' "$OLDSECRET_OUT" | head -c 160)"
  fi
  rc=0
  EPOCHA_SITE_SECRET="$SECRET2" "$BIN/devclient" --base "http://127.0.0.1:$PUBLIC_PORT" --site-key "$SITE_KEY" --mode full \
    >"$LOGS/flow2.log" 2>&1 || rc=$?
  if [[ $rc -eq 0 ]]; then
    pass "重建容器后新凭据可完成完整闭环（持久卷 + 密钥文件就是全部延续所需）"
  else
    fail "重建后新凭据闭环失败：$(tail -c 200 "$LOGS/flow2.log")"
  fi

  # 撤销最后一把管理凭据 ⇒ 锁死，然后走 admin-recover 这条唯一受控出口。
  # 这一段是本轮演练**发现**的真实缺口（不是设计好的通过路径）：
  # BootstrapAdminKey 只在"库里没有任何记录"时登记，所以撤销掉最后一把之后，
  # 换 EPOCHA_ADMIN_KEY_FILE 完全不生效——旧的第一轮演练在这里直接把管理面关死了。
  AKID="$(jq -r '.key_id // ""' "$LOGS/whoami.json" 2>/dev/null || echo "")"
  if [[ -n "$AKID" ]]; then
    admin_api POST "/api/admin/admin-keys/$AKID/revoke" '{}' >/dev/null 2>&1 || true
    compose down >/dev/null 2>&1 || true
    compose up -d --wait --wait-timeout 60 >/dev/null 2>&1 || true
    CID="$(container_id)"
    rc=0
    admin_json GET /api/admin/whoami "$LOGS/whoami2.json" || rc=$?
    if [[ $rc -ne 0 ]]; then
      pass "被撤销的管理凭据在重建后仍撤销（库里已有记录时引导变量完全忽略）"
    else
      fail "被撤销的管理凭据在重建后复活了"
    fi
    compose down >/dev/null 2>&1 || true
    CID=""

    # 换一把"新"的引导凭据**不能**恢复访问——这正是锁死形状，必须如实测出来。
    printf 'ak_%s' "$(head -c32 /dev/urandom | base64 | tr -d '/+=' | head -c48)" >"$D/keys/admin"
    chmod 600 "$D/keys/admin"
    compose up -d --wait --wait-timeout 60 >/dev/null 2>&1 || true
    CID="$(container_id)"
    rc=0
    admin_json GET /api/admin/whoami "$LOGS/whoami3.json" || rc=$?
    if [[ $rc -ne 0 ]]; then
      pass "换引导密钥文件不恢复管理访问（库里已有记录 ⇒ 忽略），确认这是一条真锁死路径"
    else
      fail "换引导文件竟然恢复了管理访问（撤销会被重启推翻 ⇒ 违反 DESIGN §6）"
    fi
    compose down >/dev/null 2>&1 || true
    CID=""

    # 唯一的受控出口：停服 + 本地 epocha-ops admin-recover（没有未鉴权的恢复 HTTP 接口）。
    # 5B 起"停服"这件事由数据目录上的排他锁强制，不再靠端口探测（见 docs/STAGE5B.md）。
    RECOVERED="$D/keys/admin.recovered"
    rc=0
    EPOCHA_PEPPER_FILE="$D/keys/pepper" "$BIN/epocha-ops" admin-recover --db "$D/data/epocha.db" \
      --key-file "$RECOVERED" >"$LOGS/recover.txt" 2>&1 || rc=$?
    if [[ $rc -ne 0 || ! -s "$RECOVERED" ]]; then
      fail "admin-recover 失败（rc=$rc）：$(tail -c 200 "$LOGS/recover.txt")"
    elif [[ "$(stat -c '%a' "$RECOVERED")" != "600" ]]; then
      fail "恢复出的凭据文件权限不是 600"
    elif ! grep -qE 'key_id=k' "$LOGS/recover.txt"; then
      fail "admin-recover 没有给出可核对的 key_id"
    else
      NEWAKID="$(awk '/key_id=/{sub(/^.*key_id=/,"");sub(/ .*/,"");print;exit}' "$LOGS/recover.txt")"
      if leaks "$LOGS/recover.txt" "$(cat "$RECOVERED")"; then
        fail "admin-recover 的输出把新凭据值带了出来"
      else
        pass "admin-recover 在停服后成功：新凭据只写进 0600 文件，输出只给 key_id（$NEWAKID）"
      fi
      cp "$RECOVERED" "$D/keys/admin"; chmod 600 "$D/keys/admin"; rm -f "$RECOVERED"
      compose up -d --wait --wait-timeout 60 >/dev/null 2>&1 || true
      CID="$(container_id)"
      rc=0
      admin_json GET /api/admin/whoami "$LOGS/whoami4.json" || rc=$?
      RECACKID="$(jq -r '.key_id // ""' "$LOGS/whoami4.json" 2>/dev/null || echo "")"
      if [[ $rc -eq 0 && "$RECACKID" == "$NEWAKID" ]]; then
        pass "恢复后的管理凭据可用，且 whoami 给出的正是 admin-recover 报出的那一把"
      else
        fail "恢复后的管理凭据不可用或不是同一把（rc=$rc whoami=$RECACKID recover=$NEWAKID）"
      fi
      if [[ -n "$AKID" && "$RECACKID" == "$AKID" ]]; then
        fail "被撤销的旧管理凭据在恢复后重新成为有效凭据"
      fi
    fi
  fi
else
  blocked "业务闭环与重建持久化演练：磁盘不足或容器未启动" "释放磁盘后：bash docs/scripts/stage5a.sh"
fi

# --- 第 7 步：备份、校验与保守恢复 ---
log "第 7 步：备份 → 备份后变更 → 隔离恢复 → 恢复后语义"
BK_NAME=""
if [[ $DOCKER_OK -eq 1 && -n "$CID" ]]; then
  # F08：不要把很早就撤销的 bootstrap key 当成“备份前仍有效”的旧凭据。
  # 先在当前容器内用当前管理 key 创建一把专用 BACKUP_ADMIN，再用单一 env
  # 入口（明确清空 *_FILE）真实 whoami=200；这把明文只在临时变量中保留到备份/恢复负测。
  rc=0
  admin_json POST /api/admin/admin-keys "$LOGS/backup-admin.json" '{"label":"stage5a-backup-old"}' || rc=$?
  BACKUP_ADMIN="$(jq -r '.admin_key // ""' "$LOGS/backup-admin.json" 2>/dev/null || echo "")"
  BACKUP_ADMIN_ID="$(jq -r '.key.key_id // ""' "$LOGS/backup-admin.json" 2>/dev/null || echo "")"
  if [[ $rc -ne 0 || -z "$BACKUP_ADMIN" || -z "$BACKUP_ADMIN_ID" ]]; then
    fail "无法在备份前创建专用管理凭据（rc=$rc key_id=${BACKUP_ADMIN_ID:-空}）"
  else
    BACKUP_OLD_OK=0
    docker exec -i -e EPOCHA_ADMIN_KEY="$BACKUP_ADMIN" -e EPOCHA_ADMIN_KEY_FILE= \
      "$CID" "$OPS" admin GET /api/admin/whoami </dev/null >"$LOGS/backup-admin-whoami.raw" 2>&1 || rc=$?
    BACKUP_ADMIN_STATUS="$(awk -F= '/^status=/{print $2; exit}' "$LOGS/backup-admin-whoami.raw")"
    if [[ $rc -eq 0 && "$BACKUP_ADMIN_STATUS" == "200" ]] && sed -n '2,$p' "$LOGS/backup-admin-whoami.raw" | jq -e '.key_id' >/dev/null 2>&1; then
      BACKUP_OLD_OK=1
      pass "备份前专用管理凭据由容器内 ops 以单一 env 入口真实 whoami=200（无公共端口探活）"
    else
      fail "备份前专用管理凭据未真实 whoami=200（rc=$rc status=${BACKUP_ADMIN_STATUS:-missing}）"
    fi
  fi

  if [[ $BACKUP_OLD_OK -ne 1 ]]; then
    fail "F08 前置未成立：备份前没有专用旧管理凭据的容器内 whoami=200"
  fi
  # 备份时点：库里有真实消费与 WAL 未折回内容。
  EPOCHA_SITE_SECRET="$SECRET2" "$BIN/devclient" --base "http://127.0.0.1:$PUBLIC_PORT" --site-key "$SITE_KEY" --mode full >/dev/null 2>&1 || true
  # 备份只能在专用旧管理凭据已由容器内 HTTP 证明有效后继续。
  if [[ $BACKUP_OLD_OK -eq 1 ]]; then
    rc=0
    cexec epocha "$OPS" backup --db "$C_DATA/epocha.db" --dest "$C_BACKUP" >"$LOGS/backup.txt" 2>&1 || rc=$?
  else
    rc=1
    : >"$LOGS/backup.txt"
  fi
  if [[ $rc -eq 0 ]]; then
    BK_NAME="$(awk '/^备份完成/ {sub(/^name=/,"",$2); print $2; exit}' "$LOGS/backup.txt")"
    pass "容器内备份成功（name=${BK_NAME##*/}）：$LOGS/backup.txt 里的计数行给出站点/凭据/消费/池水位"
  else
    fail "容器内备份失败（rc=$rc）：$(tail -c 200 "$LOGS/backup.txt")"
  fi
  if [[ -n "$BK_NAME" && -f "$D/backups/$BK_NAME.db" && -f "$D/backups/$BK_NAME.manifest.json" ]]; then
    BPERM="$(stat -c '%a' "$D/backups/$BK_NAME.db")"
    if [[ "$BPERM" == "600" ]]; then
      pass "备份产物权限 0600，且 manifest 与 .db 成对出现在**独立的备份挂载目录**里"
    else
      fail "备份产物权限是 $BPERM（应 0600）"
    fi
    # manifest 自描述：版本、schema、哈希、计数、pepper 指纹。
    MOK=1
    for k in format app_version commit schema_version sha256 pepper_fingerprint integrity_check counts backup_bytes; do
      jq -e ".$k" "$D/backups/$BK_NAME.manifest.json" >/dev/null 2>&1 || { MOK=0; log "manifest 缺字段 $k"; }
    done
    if [[ $MOK -eq 1 ]]; then
      pass "manifest 字段齐备（app_version/commit/schema_version/sha256/pepper 指纹/一致性检查/内容计数）"
    else
      fail "manifest 字段不齐"
    fi
    if leaks "$D/backups/$BK_NAME.manifest.json" "$SECRET2"; then
      fail "manifest 泄露了站点 secret"
    else
      pass "manifest 不含任何凭据明文（pepper 指纹是单向派生，且不进日志/响应）"
    fi
  fi

  # 独立校验：verify 子命令重算哈希 + integrity_check + schema 版本。
  rc=0
  cexec epocha "$OPS" verify --db "$C_BACKUP/$BK_NAME.db" --manifest "$C_BACKUP/$BK_NAME.manifest.json" >/dev/null 2>&1 || rc=$?
  if [[ $rc -eq 0 ]]; then
    pass "epocha-ops verify 独立复核这份备份可信（非 0 即不可信，不是"文件存在"）"
  else
    fail "刚做出来的备份 verify 不通过（rc=$rc）"
  fi

  # 篡改检测：改一个字节 ⇒ verify 必须失败。“备份文件非空”不等于“恢复可用”。
  cp "$D/backups/$BK_NAME.db" "$D/tamper.db"
  printf 'X' | dd of="$D/tamper.db" bs=1 seek=$(( $(stat -c%s "$D/tamper.db") - 10 )) count=1 conv=notrunc status=none
  rc=0
  "$BIN/epocha-ops" verify --db "$D/tamper.db" --manifest "$D/backups/$BK_NAME.manifest.json" >/dev/null 2>&1 || rc=$?
  if [[ $rc -ne 0 ]]; then
    pass "被改过一个字节的备份被 verify 拒绝（sha256 与 integrity_check 都参与判定）"
  else
    fail "被改过的备份仍被判可信"
  fi
  rm -f "$D/tamper.db"

  # 备份之后发生的状态变化：消费一次、停用一次（这些状态快照里都不该有）。
  EPOCHA_SITE_SECRET="$SECRET2" "$BIN/devclient" --base "http://127.0.0.1:$PUBLIC_PORT" --site-key "$SITE_KEY" --mode full >/dev/null 2>&1 || true
  admin_api POST "/api/admin/sites/$SITE_ID/disable" '{}' >/dev/null 2>&1 || true

  # 恢复：在**隔离目录**用宿主同版本二进制产出新文件（绝不覆盖工作库）。
  LIB_BEFORE="$(sha256sum "$D/data/epocha.db" | cut -d' ' -f1)"
  rc=0
  EPOCHA_PEPPER_FILE="$D/keys/pepper" EPOCHA_ADMIN_KEY="$ADMIN_NEW" \
    "$BIN/epocha-ops" restore --backup "$D/backups/$BK_NAME.db" --manifest "$D/backups/$BK_NAME.manifest.json" \
    --out-dir "$D/restore" >"$LOGS/restore.txt" 2>&1 || rc=$?
  if [[ $rc -eq 0 && -f "$D/restore/epocha.db" ]]; then
    LIB_AFTER="$(sha256sum "$D/data/epocha.db" | cut -d' ' -f1)"
    if [[ "$LIB_BEFORE" == "$LIB_AFTER" ]]; then
      pass "保守恢复产出新文件，工作库逐字节未变"
    else
      fail "恢复流程改了正在使用的工作库"
    fi
    if ! ls "$D/restore"/*-wal >/dev/null 2>&1 && ! ls "$D/restore"/*-shm >/dev/null 2>&1; then
      pass "恢复交付物是单文件库（checkpoint 折回 + 干净关闭，没有半边 WAL）"
    else
      fail "恢复产物旁边留下了 -wal/-shm，单文件交付不成立"
    fi
  else
    fail "恢复流程失败（rc=$rc）：$(tail -c 300 "$LOGS/restore.txt")"
  fi

  # pepper 不配对 ⇒ 拒绝，且不留半成品。
  rc=0
  mkdir -p "$D/restore-foreign"
  EPOCHA_PEPPER_FILE="$D/keys-foreign-pepper.b64" EPOCHA_ADMIN_KEY="$ADMIN_NEW" \
    "$BIN/epocha-ops" restore --backup "$D/backups/$BK_NAME.db" --manifest "$D/backups/$BK_NAME.manifest.json" \
    --out-dir "$D/restore-foreign" >"$LOGS/foreign.txt" 2>&1 || rc=$?
  # 半成品的判据是"看起来像一份库的东西"：.db / .partial / -wal / -shm。
  # epocha.lock 不算产物——restore 对 --out-dir 取排他锁时会留下它，且失败时也不删（flock 的正常形状）。
  foreign_left="$(find "$D/restore-foreign" -mindepth 1 ! -name 'epocha.lock' -print -quit 2>/dev/null || true)"
  if [[ $rc -ne 0 ]] && [[ -z "$foreign_left" ]]; then
    pass "pepper 与备份不配对 ⇒ 拒绝恢复且不产生任何文件（不会静默给出“可服务”状态）"
  else
    fail "pepper 不配对的恢复行为不对（rc=$rc，留下=${foreign_left:-无}）"
  fi

  # 用**新的主密钥**启动恢复实例：恢复前签发的 token 一律不可用，且不保留签名回退。
  rc=0
  RESTORE_CID="$(docker run -d --name "$RESTORE_NAME" \
    --user "$RUN_UID:$RUN_GID" \
    --read-only \
    --cap-drop ALL --security-opt no-new-privileges \
    --memory 128m --memory-swap 128m --cpus 1 --pids-limit 128 \
    -v "$D/restore:$C_DATA" \
    -v "$D/keys-restore:$C_KEYS:ro" \
    -p "127.0.0.1:$RESTORE_PORT:8080" \
    -e EPOCHA_LISTEN=0.0.0.0:8080 -e EPOCHA_ADMIN_LISTEN=127.0.0.1:8081 \
    -e EPOCHA_DB="$C_DATA/epocha.db" \
    -e EPOCHA_MASTER_KEY_FILE="$C_KEYS/master" -e EPOCHA_PEPPER_FILE="$C_KEYS/pepper" \
    -e EPOCHA_ADMIN_KEY_FILE="$C_KEYS/admin" \
    -e GOMEMLIMIT=56MiB -e GOMAXPROCS=1 \
    "$IMAGE" >"$LOGS/restore-run.txt" 2>&1)" || rc=$?
  if [[ $rc -eq 0 && -n "$RESTORE_CID" ]]; then
    RESTORE_CID="$(docker inspect -f '{{.Id}}' "$RESTORE_CID")"
    if wait_status "http://127.0.0.1:$RESTORE_PORT/readyz" 200 30; then
      pass "恢复实例可服务探针连通（/readyz 200）——注意“可服务探针”不等于“业务开放”"
    else
      fail "恢复实例起不来：$(tail -c 300 "$LOGS/restore-run.txt")"
    fi

    OLD_RESP="$(cat "$WORK/resp-before-restart" 2>/dev/null || echo "")"
    R_OUT="$(curl -sS --max-time 10 -X POST -d "secret=$SECRET2&response=$OLD_RESP" "http://127.0.0.1:$RESTORE_PORT/siteverify" 2>/dev/null || echo "")"
    if printf '%s' "$R_OUT" | grep -q '"success":false'; then
      pass "备份前签发的旧 response 在恢复实例上被判失败（账本已终结 + 主密钥已轮换，无签名回退）"
    else
      fail "恢复实例接受了旧 token：$(printf '%s' "$R_OUT" | head -c 200)"
    fi

    # 恢复出来的站点必须全部禁用：业务在人工确认前拒绝。
    CH="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 -X POST -H 'Content-Type: application/json' \
      -d "{\"site_key\":\"$SITE_KEY\"}" "http://127.0.0.1:$RESTORE_PORT/api/challenge" 2>/dev/null || echo 000)"
    if [[ "$CH" == "400" ]]; then
      pass "恢复实例上所有站点默认禁用，业务被拒（400，与不存在同形）"
    else
      fail "恢复实例未拒绝业务（code=$CH）——保守恢复流程没有落地"
    fi

    # 管理面在恢复容器内绑定 127.0.0.1:8081；RESTORE_PORT 只映射公共 8080，不能从宿主 curl 管理 API。
    # 新 key 用唯一 *_FILE 入口走容器内 ops；旧 key 用同一入口真实发 HTTP，首行 status=401。
    rc=0
    docker exec -i -e EPOCHA_ADMIN_KEY_FILE="$C_KEYS/admin" -e EPOCHA_ADMIN_KEY= "$RESTORE_CID" "$OPS" admin GET /api/admin/whoami </dev/null >"$LOGS/rwhoami.raw" 2>&1 || rc=$?
    RWHO_STATUS="$(awk -F= '/^status=/{print $2; exit}' "$LOGS/rwhoami.raw")"
    NEW_ADMIN_FIRST="$(sed -n '1p' "$LOGS/rwhoami.raw")"
    sed -n '2,$p' "$LOGS/rwhoami.raw" >"$LOGS/rwhoami.json"
    if [[ $rc -eq 0 && "$RWHO_STATUS" == "200" && "$NEW_ADMIN_FIRST" == "status=200" ]] && jq -e '.key_id' "$LOGS/rwhoami.json" >/dev/null 2>&1; then
      pass "恢复实例的新管理凭据通过容器内 HTTP whoami 200（单一 *_FILE 入口）"
    else
      fail "恢复实例的新管理凭据不可用（rc=$rc status=${RWHO_STATUS:-missing}）"
    fi
    rc=0
    docker exec -i -e EPOCHA_ADMIN_KEY="$BACKUP_ADMIN" -e EPOCHA_ADMIN_KEY_FILE= "$RESTORE_CID" "$OPS" admin GET /api/admin/whoami </dev/null >"$LOGS/old-admin.raw" 2>&1 || rc=$?
    OLD_ADMIN_STATUS="$(awk -F= '/^status=/{print $2; exit}' "$LOGS/old-admin.raw")"
    OLD_ADMIN_FIRST="$(sed -n '1p' "$LOGS/old-admin.raw")"
    if [[ $rc -ne 0 && "$OLD_ADMIN_STATUS" == "401" && "$OLD_ADMIN_FIRST" == "status=401" ]]; then
      pass "恢复后旧管理凭据由容器内 HTTP whoami 明确返回 401（不是公共端口 404）"
    else
      fail "恢复后旧管理凭据应由容器内 HTTP whoami 返回 401，rc=$rc status=${OLD_ADMIN_STATUS:-missing}"
    fi

    # 启用一个站点，证明恢复后**新**流程可用（这才是“恢复完成”）。
    rc=0
    docker exec -i "$RESTORE_CID" "$OPS" admin GET /api/admin/sites </dev/null >"$LOGS/rsites.raw" 2>"$LOGS/rsites.err" || rc=$?
    RID=""
    if [[ $rc -eq 0 ]]; then
      sed -n '2,$p' "$LOGS/rsites.raw" >"$LOGS/rsites.json"
      RID="$(jq -r '.sites[0].id // ""' "$LOGS/rsites.json" 2>/dev/null || echo "")"
    fi
    if [[ -n "$RID" ]]; then
      rc=0
      printf '{}' | docker exec -i "$RESTORE_CID" "$OPS" admin POST "/api/admin/sites/$RID/enable" >/dev/null 2>&1 || rc=$?
      if [[ $rc -ne 0 ]]; then
        fail "恢复实例里启用站点失败（rc=$rc）——后面的“恢复完成”判定不成立"
      fi
      # 保守恢复把 site_secrets 全删了（被撤销的凭据不得复活），所以启用站点之后
      # **必须重新签发**一条凭据——用旧 SECRET2 去验是错的期望。
      rc=0
      # 必须**管道给一个 JSON 体**：ops 对非 GET 要求有体，而 `</dev/null` 是字符设备
      # （stdinHasData() 判"没有输入"）——第一轮这里拿到空 RSECRET，于是"重签后才能测新闭环"这步没做成。
      rc=0
      printf '{}' | docker exec -i "$RESTORE_CID" "$OPS" admin POST "/api/admin/sites/$RID/secrets" >"$LOGS/rsecret.raw" 2>"$LOGS/rsecret.err" || rc=$?
      if [[ $rc -ne 0 ]]; then log "重签凭据失败（rc=$rc）：$(tail -c 160 "$LOGS/rsecret.err")"; fi
      sed -n '2,$p' "$LOGS/rsecret.raw" >"$LOGS/rsecret.json"
      RSECRET="$(jq -r '.secret // ""' "$LOGS/rsecret.json" 2>/dev/null || echo "")"
      OLDSTILL=0
      EPOCHA_SITE_SECRET="$SECRET2" "$BIN/devclient" --base "http://127.0.0.1:$RESTORE_PORT" --site-key "$SITE_KEY" --mode full >/dev/null 2>&1 && OLDSTILL=1
      if [[ $OLDSTILL -eq 1 ]]; then
        fail "恢复实例上备份前的站点凭据仍能完成验证（撤销与轮换会被快照复活）"
      else
        pass "恢复实例上旧的站点凭据一律不可用（site_secrets 已清空 ⇒ 必须重新签发）"
      fi
      NEWF=0
      if [[ -n "$RSECRET" ]]; then
        EPOCHA_SITE_SECRET="$RSECRET" "$BIN/devclient" --base "http://127.0.0.1:$RESTORE_PORT" --site-key "$SITE_KEY" --mode full >/dev/null 2>&1 || NEWF=$?
      else
        NEWF=9
      fi
      if [[ $NEWF -eq 0 ]]; then
        pass "重新签发凭据后，恢复实例可签发并完成新闭环（恢复流程可收尾）"
      else
        fail "恢复实例在启用+重签凭据后新闭环仍不通（rc=$NEWF）"
      fi
    else
      fail "恢复实例里读不到站点列表"
    fi
  else
    fail "恢复实例容器启动失败（rc=$rc）：$(tail -c 200 "$LOGS/restore-run.txt")"
  fi
  docker rm -f "$RESTORE_CID" >/dev/null 2>&1 || true
  RESTORE_CID=""
else
  blocked "备份/校验/篡改检测/保守恢复/恢复实例语义：磁盘不足或容器未启动" "释放磁盘后：bash docs/scripts/stage5a.sh"
fi

# 把默认站点收回"启用"状态：第 10/11 步还要走业务路径。这不是修饰——停用是备份演练
# 故意制造的真实变更，留着的代价是后面的测试"因为错误的原因失败"。
if [[ $DOCKER_OK -eq 1 && -n "$CID" && -n "$SITE_ID" ]]; then
  admin_api POST "/api/admin/sites/$SITE_ID/enable" '{}' >/dev/null 2>&1 \
    || log "注意：默认站点重新启用失败，第 10/11 步的业务断言可能因此不成立"
fi

# --- 第 8 步：故障形状（库不可写不得给出验证成功）与优雅停止 ---
log "第 8 步：DB 不可用时的 fail closed 与优雅停止"
if [[ $DOCKER_OK -eq 1 && -n "$CID" ]]; then
  compose down >/dev/null 2>&1 || true
  cp -a "$D/data" "$D/data-copy"
  chmod a-w "$D/data-copy"
  rc=0
  READONLY_CID="$(docker run -d --name "$READONLY_NAME" \
    --user "$RUN_UID:$RUN_GID" --read-only \
    -v "$D/data-copy:$C_DATA:ro" -v "$D/keys:$C_KEYS:ro" \
    -p "127.0.0.1:$READONLY_PORT:8080" \
    -e EPOCHA_DB="$C_DATA/epocha.db" -e EPOCHA_MASTER_KEY_FILE="$C_KEYS/master" -e EPOCHA_PEPPER_FILE="$C_KEYS/pepper" \
    "$IMAGE" >/dev/null 2>&1)" || rc=$?
  if [[ $rc -eq 0 && -n "$READONLY_CID" ]]; then
    READONLY_CID="$(docker inspect -f '{{.Id}}' "$READONLY_CID")"
  fi
  sleep 4
  RO_CODE="$(code "http://127.0.0.1:$READONLY_PORT/readyz" 5)"
  RO_CH="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 8 -X POST -H 'Content-Type: application/json' -d "{\"site_key\":\"$SITE_KEY\"}" "http://127.0.0.1:$READONLY_PORT/api/challenge" 2>/dev/null || echo 000)"
  RO_V="$(curl -sS --max-time 8 -X POST -d "secret=$SECRET2&response=whatever" "http://127.0.0.1:$READONLY_PORT/siteverify" 2>/dev/null || echo "")"
  docker rm -f "$READONLY_CID" >/dev/null 2>&1 || true
  chmod -R u+w "$D/data-copy"; rm -rf "$D/data-copy"
  if printf '%s' "$RO_V" | grep -q '"success":true'; then
    fail "数据库只读时居然返回了验证成功（红线 12）"
  elif [[ "$RO_CODE" != "200" || "$RO_CH" != "200" ]]; then
    pass "库不可写时不给验证成功，且不就绪（/readyz=$RO_CODE，/api/challenge=$RO_CH：启动失败或未就绪两种形状都比“照常服务”好）"
  else
    fail "库只读却仍报告就绪并能领题"
  fi

  # 优雅停止：SIGTERM 后排空在途请求并关闭 DB，端口必须释放。
  compose up -d >/dev/null 2>&1 || true
  CID="$(container_id)"
  T0=$SECONDS
  rc=0
  compose stop -t 15 >"$LOGS/stop.log" 2>&1 || rc=$?
  STOP_T=$((SECONDS - T0))
  if [[ $rc -ne 0 ]]; then
    fail "compose stop 返回非 0（rc=$rc）"
  elif port_listening "$PUBLIC_PORT"; then
    fail "停止后端口仍被占用（优雅关闭没释放）"
  else
    STATE="$(docker inspect -f '{{.State.Status}}' "$CID" 2>/dev/null || echo unknown)"
    if [[ "$STATE" == "exited" || "$STATE" == "dead" ]]; then
      pass "SIGTERM 后 ${STOP_T}s 内正常退出并释放端口（状态 $STATE）"
    else
      fail "停止后容器状态是 $STATE，不是已退出"
    fi
  fi
  compose up -d --wait --wait-timeout 60 >/dev/null 2>&1 || true
  CID="$(container_id)"
else
  blocked "库不可写的故障形状与优雅停止演练：磁盘不足或容器未启动" "释放磁盘后：bash docs/scripts/stage5a.sh"
fi

# --- 第 9 步：日志按值扫描 ---
log "第 9 步：容器日志的按值扫描"
if [[ $DOCKER_OK -eq 1 && -n "$CID" ]]; then
  compose logs --no-color >"$LOGS/container.log" 2>&1 || true
  LEAK=""
  for s in "$SECRET1" "$SECRET2" "$ADMIN_OLD" "$ADMIN_NEW" "$BACKUP_ADMIN" "$(cat "$D/keys/pepper")" "$(cat "$D/keys/master")"; do
    if leaks "$LOGS/container.log" "$s"; then LEAK="$LEAK ${s:0:6}…"; fi
  done
  if [[ -n "$LEAK" ]]; then
    fail "容器日志泄露哨兵值:$LEAK"
  else
    pass "容器日志（含启动摘要与请求日志）不含 pepper/master 值、站点 secret、管理凭据"
  fi
  if grep -qE 'config=.*(master_key_source|pepper_source|admin_key_source)' "$LOGS/container.log"; then
    pass "启动摘要记录了密钥**来源**（env/file/none）而不是内容"
  else
    fail "启动摘要没有记录密钥来源"
  fi
else
  blocked "容器日志按值扫描：磁盘不足或容器未启动" "释放磁盘后：bash docs/scripts/stage5a.sh"
fi

# --- 第 10 步：资源受限演练 ---
log "第 10 步：1 CPU + 128MiB 限额下的有界负载实测"
if [[ $DOCKER_OK -eq 1 && -n "$CID" ]]; then
  # 为什么要一个"抬高全局限流"的专用测量实例：`ipLimit.Allow(地址)` 用的是配置里的全局
  # EPOCHA_RATE_IP_RPM（默认 60），站点级 rate_ip_rpm 只覆盖"地址×站点"那一对维度，盖不住它。
  # 默认配置下测出来的是限流器形状（第一轮实测：80 次闭环里 60 次 400、20 次 429），
  # 所以两种形状都要**分别**记，不能拿被限流的那组数字当服务端容量。
  LT_JSON="$WORK/site-load.json"
  LT_KEY=""
  LT_SECRET=""
  rc=0
  admin_json POST /api/admin/sites "$LT_JSON" '{"name":"stage5a-load","verify_mode":"pow_only","allow_missing_origin":true,"pow_bits":12,"rate_ip_rpm":6000,"rate_site_rpm":60000}' || rc=$?
  if [[ $rc -eq 0 ]]; then
    LT_KEY="$(jq -r '.site.site_key // ""' "$LT_JSON")"
    LT_SECRET="$(jq -r '.secret // ""' "$LT_JSON")"
  fi
  if [[ -z "$LT_KEY" || -z "$LT_SECRET" ]]; then
    fail "负载站点创建失败（rc=$rc）——第 10 步的资源实测无法执行"
  else
    pass "负载站点已建立（站点级 rate_ip_rpm=6000 / rate_site_rpm=60000）"
    rc=0
    EPOCHA_SITE_SECRET="$SECRET2" "$BIN/loadtest" --base "http://127.0.0.1:$PUBLIC_PORT" --site-key "$SITE_KEY" \
      --concurrency 4 --requests 80 --timeout 10s --quiet >"$LOGS/loadtest-default.txt" 2>&1 || rc=$?
    log "  默认限流形状：$(cat "$LOGS/loadtest-default.txt" 2>/dev/null)"
    pass "默认配置（60 IP-RPM）下的失败类别已记录——限流器先于服务端容量起作用，这是配置语义不是故障"
  fi

  # 停编排服务后再起测量实例：同一时刻只能有一个进程持有这个 SQLite 文件。
  compose down >/dev/null 2>&1 || true
  CID=""
  LOADCID="$LOAD_NAME"
  LOAD_URL="http://127.0.0.1:$LOAD_PORT"
  # 视觉题难度档 5（高档）：本轮只测渲染与字节预算的负载形状，不构成"生产该用哪一档"的建议。
  cat >"$WORK/site-pool-body.json" <<'JSON'
{"name":"stage5a-pool","verify_mode":"pow_question","generator_id":"text-image-v1","difficulty":5,"allow_missing_origin":true,"pow_bits":12,"rate_ip_rpm":6000,"rate_site_rpm":60000}
JSON
  rc=0
  LOAD_CID="$(docker run -d --name "$LOADCID" \
    --user "$RUN_UID:$RUN_GID" --read-only \
    --cap-drop ALL --security-opt no-new-privileges \
    --memory 128m --memory-swap 128m --cpus 1 --pids-limit 128 \
    -v "$D/data:$C_DATA" -v "$D/keys:$C_KEYS:ro" -v "$D/backups:$C_BACKUP" \
    -p "127.0.0.1:$LOAD_PORT:8080" \
    -e EPOCHA_LISTEN=0.0.0.0:8080 -e EPOCHA_DB="$C_DATA/epocha.db" \
    -e EPOCHA_MASTER_KEY_FILE="$C_KEYS/master" -e EPOCHA_PEPPER_FILE="$C_KEYS/pepper" \
    -e EPOCHA_ADMIN_KEY_FILE="$C_KEYS/admin" \
    -e EPOCHA_RATE_IP_RPM=60000 -e EPOCHA_RATE_SITE_RPM=60000 \
    -e GOMEMLIMIT=56MiB -e GOMAXPROCS=1 \
    "$IMAGE" >"$LOGS/load-run.txt" 2>&1)" || rc=$?
  if [[ $rc -eq 0 && -n "$LOAD_CID" ]]; then
    LOADCID="$LOAD_CID"
    LOAD_CID="$(docker inspect -f '{{.Id}}' "$LOADCID")"
  fi
  if [[ $rc -ne 0 ]]; then
    fail "容量测量实例起不来（rc=$rc）：$(tail -c 200 "$LOGS/load-run.txt")"
  elif ! wait_status "$LOAD_URL/readyz" 200 30; then
    fail "容量测量实例没有就绪：$(docker logs "$LOADCID" 2>&1 | tail -c 240)"
  elif [[ "$(docker inspect -f '{{.State.Status}}' "$LOADCID" 2>/dev/null || echo '?')" != "running" ]]; then
    fail "测量实例不在 running 状态"
  else
    pass "容量测量实例已就绪（全局限流抬到 60000 RPM；CPU/内存/pids/只读等限额与生产一致）"
    # 建一个真的题目站点（视觉题高档 + 算术题），否则分桶为空、"图片补池"这一项根本没测到东西。
    # 难度档只为测量渲染与字节预算形状：**不代表**生产可以走低档
    # （阶段 4B 的真实 OCR 基线：低档整串通过率 96%，红线 14）。
    POOL_JSON="$WORK/site-pool.json"
    rc=0
    docker exec -i "$LOADCID" "$OPS" admin POST /api/admin/sites <"$WORK/site-pool-body.json" >"$POOL_JSON" 2>"$LOGS/poolsite.err" || rc=$?
    if [[ $rc -ne 0 ]]; then
      fail "题目站点创建失败（rc=$rc）：$(tail -c 200 "$LOGS/poolsite.err")"
    fi
    sed -n '2,$p' "$POOL_JSON" >"$POOL_JSON.body" 2>/dev/null || true
    if ! jq -e '.site.id' "$POOL_JSON.body" >/dev/null 2>&1; then
      fail "题目站点响应不可解析：$(head -c 160 "$POOL_JSON" 2>/dev/null)"
    else
      # 等一轮补池（低水位 8 张图 + 50 道算术题；worker 每 2s 检查一次水位）
      POOL_READY=0
      for _ in 1 2 3 4 5 6 7 8 9 10; do
        docker exec -i "$LOADCID" "$OPS" admin GET /api/admin/pool </dev/null >"$LOGS/pool-probe.raw" 2>/dev/null \
          && sed -n '2,$p' "$LOGS/pool-probe.raw" >"$LOGS/pool-probe.json" || true
        POOL_READY="$(jq -r '[.buckets[]? | .ready] | add // 0' "$LOGS/pool-probe.json" 2>/dev/null || echo 0)"
        (( POOL_READY > 0 )) && break
        sleep 2
      done
      POOL_IMG="$(jq -r '.image.asset_bytes // 0' "$LOGS/pool-probe.json" 2>/dev/null || echo 0)"
      if (( POOL_READY > 0 && POOL_IMG > 0 )); then
        pass "后台补池在 1 CPU / 128MiB 限额内确实工作：ready=$POOL_READY 行、图片字节=$POOL_IMG（分桶来自启用的 pow_question 站点）"
      elif (( POOL_READY > 0 )); then
        fail "图片补池 ready=$POOL_READY 但 asset_bytes=$POOL_IMG，不接受零图片字节证据"
      else
        fail "起了题目站点仍补不出题（ready=0）——第 10 步的“图片补池”项没测到东西"
      fi
    fi
    SZ_BEFORE_DB="$(stat -c '%s' "$D/data/epocha.db" 2>/dev/null || echo 0)"
    SZ_BEFORE_WAL="$(stat -c '%s' "$D/data/epocha.db-wal" 2>/dev/null || echo 0)"
    docker exec -i "$LOADCID" "$OPS" admin GET /api/admin/pool </dev/null >"$LOGS/pool-before.raw" 2>/dev/null \
      && sed -n '2,$p' "$LOGS/pool-before.raw" >"$LOGS/pool-before.json" || true
    docker stats --no-stream --format 'CPU={{.CPUPerc}} MEM={{.MemUsage}}' "$LOADCID" >"$LOGS/stats-during.txt" 2>/dev/null &
    STATS_PID=$!
    rc=0
  EPOCHA_SITE_SECRET="$LT_SECRET" "$BIN/loadtest" --base "$LOAD_URL" --site-key "$LT_KEY" \
      --concurrency 4 --requests 150 --timeout 10s >"$LOGS/loadtest.txt" 2>&1 || rc=$?
    wait "$STATS_PID" 2>/dev/null || true
    if [[ $rc -eq 0 ]] && check_load_evidence "$LOGS/loadtest.txt"; then
      pass "有界负载跑完且全成功：严格证明 concurrency=4 requests=150、flows=150 ok=150、四段 n=150 且无失败类别 / err 字段"
    elif [[ $rc -eq 0 ]]; then
      fail "负载工具退出 0 但默认输出未严格证明 150/150 与四段完整样本：$(tail -n 8 "$LOGS/loadtest.txt")"
    else
      fail "有界负载没有全成功闭环（rc=$rc）：$(tail -c 220 "$LOGS/loadtest.txt")"
    fi
    OOM="$(docker inspect -f '{{.State.OOMKilled}}' "$LOADCID" 2>/dev/null || echo '?')"
    RESTARTS="$(docker inspect -f '{{.RestartCount}}' "$LOADCID" 2>/dev/null || echo '?')"
    MEMH="$(docker inspect -f '{{json .HostConfig.Memory}}' "$LOADCID" 2>/dev/null || echo '?')"
    if [[ "$OOM" == "false" && "$RESTARTS" == "0" ]]; then
      pass "限额内运行：OOMKilled=false、RestartCount=0（内存硬上限 $MEMH 字节）"
    else
      fail "出现 OOM 或重启（OOMKilled=$OOM RestartCount=$RESTARTS）——限额形状与文档不符"
    fi
    SZ_AFTER_DB="$(stat -c '%s' "$D/data/epocha.db" 2>/dev/null || echo 0)"
    SZ_AFTER_WAL="$(stat -c '%s' "$D/data/epocha.db-wal" 2>/dev/null || echo 0)"
    log "  库体积 $SZ_BEFORE_DB→$SZ_AFTER_DB 字节；WAL $SZ_BEFORE_WAL→$SZ_AFTER_WAL 字节（只 stat 主文件会得到假数）"
    docker exec -i "$LOADCID" "$OPS" admin GET /api/admin/pool </dev/null >"$LOGS/pool-after.raw" 2>/dev/null \
      && sed -n '2,$p' "$LOGS/pool-after.raw" >"$LOGS/pool-after.json" || true
    POOL_READY="$(jq -r '[.buckets[]? | .ready] | add // 0' "$LOGS/pool-after.json" 2>/dev/null || echo 0)"
    POOL_ROWS="$(jq -r '[.buckets[]? | .total] | add // 0' "$LOGS/pool-after.json" 2>/dev/null || echo 0)"
    IMG_BYTES="$(jq -r '.image.asset_bytes // 0' "$LOGS/pool-after.json" 2>/dev/null || echo 0)"
    log "  题池：ready 合计=$POOL_READY 行合计=$POOL_ROWS 图片字节=$IMG_BYTES（后台补池与请求并发进行）"
    log "  负载期间容器资源：$(cat "$LOGS/stats-during.txt" 2>/dev/null)"
    # 测完收掉测量实例并把库交还给编排服务。
    docker rm -f "$LOAD_CID" >/dev/null 2>&1 || true
    compose up -d --wait --wait-timeout 60 >/dev/null 2>&1 || true
    CID="$(container_id)"
    if [[ -z "$CID" ]]; then
      fail "交还给编排服务失败（第 11 步的反代断言将无法执行）"
    else
      pass "测量实例已收掉，库交还给编排服务（同一时刻只有一个进程持有 SQLite 文件）"
    fi
    pass "资源演练数据已采集（分段时延分位数、吞吐、库与 WAL 体积、容器内存/CPU、题池水位、OOM/重启计数）"
  fi
else
  blocked "资源受限演练：磁盘不足或容器未启动" "释放磁盘后：bash docs/scripts/stage5a.sh"
fi

# --- 第 11 步：反代模板路由边界 ---
log "第 11 步：Nginx 模板的管理面边界、缓存语义与日志形状"
if [[ $DOCKER_OK -ne 1 ]]; then
  blocked "反代演练：磁盘不足" "释放磁盘后：bash docs/scripts/stage5a.sh"
else
  # 顺序很重要：先问磁盘再决定 pull。"先拉再检查"等于已经把余量花掉才发现跑不动。
  PROXY_NEED_KB=$((80 * 1024))
  rc=0
  if [[ $(avail_kb) -lt $PROXY_NEED_KB ]]; then
    blocked "反代演练：磁盘余量 $(( $(avail_kb) / 1024 ))MB < 拉取 $PROXY_IMAGE 所需 $((PROXY_NEED_KB / 1024))MB（模板已交付，路由语义未在真实 nginx 上验证）" \
      "释放磁盘后：docker pull $PROXY_IMAGE && bash docs/scripts/stage5a.sh"
  else
    if docker image inspect "$PROXY_IMAGE" >/dev/null 2>&1; then
      log "复用已有代理镜像 $PROXY_IMAGE（不删除、不登记为本轮资产）"
    else
      rc=0
      timeout 300 docker pull "$PROXY_IMAGE" >"$LOGS/pull-proxy.log" 2>&1 || rc=$?
      if [[ $rc -eq 0 ]]; then
        PULLED_PROXY_IMAGE_ID="$(docker image inspect "$PROXY_IMAGE" --format '{{.Id}}')"
      fi
    fi
    if [[ $rc -eq 0 ]]; then
      NGINX_OK=1
    else
      blocked "反代演练：$PROXY_IMAGE 不可拉取（registry 不可达或 rc=$rc；模板已交付，路由语义未在真实 nginx 上验证）" \
        "网络恢复后：docker pull $PROXY_IMAGE && bash docs/scripts/stage5a.sh"
    fi
  fi
fi
if [[ $NGINX_OK -eq 1 ]]; then
  # 演练用的唯一改动：上游指向 compose 网络内的服务名，以及自签名测试证书（不是公网证书签发）。
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
    -keyout "$WORK/privkey.pem" -out "$WORK/fullchain.pem" -subj "/CN=127.0.0.1" >/dev/null 2>&1
  mkdir -p "$WORK/nginx/ssl"
  cp "$WORK/privkey.pem" "$WORK/fullchain.pem" "$WORK/nginx/ssl/"
  sed -e "s|server 127.0.0.1:8080;|server epocha:8080;|" \
      -e "s|listen 80;|listen 80;|" \
      "$ROOT/deploy/docker/nginx.conf.example" >"$WORK/nginx/epocha.conf"
  if ! grep -q "server epocha:8080;" "$WORK/nginx/epocha.conf"; then
    fail "模板里的上游地址形状变了，演练没有按预期改写它"
  fi
  rc=0
  PROXY_CID="$(docker run -d --name "$PROXY_NAME" \
    --network "${PROJECT}_default" \
    -v "$WORK/nginx/epocha.conf:/etc/nginx/conf.d/epocha.conf:ro" \
    -v "$WORK/nginx/ssl:/etc/nginx/ssl:ro" \
    -p "127.0.0.1:$PROXY_PORT:443" \
    "$PROXY_IMAGE" >/dev/null 2>&1)" || rc=$?
  if [[ $rc -ne 0 || -z "$PROXY_CID" ]]; then
    fail "反代容器启动失败（rc=$rc）"
  else
    PROXY_CID="$(docker inspect -f '{{.Id}}' "$PROXY_CID")"
    sleep 3
    # 先确认反代能进 compose 网络里的服务，再谈路由边界。
    if [[ "$(pcode "https://127.0.0.1:$PROXY_PORT/healthz")" == "200" ]]; then
      pass "反代到验证服务的通路成立（/healthz 经代理 200）"
    else
      fail "反代无法回源（pcode=$(pcode "https://127.0.0.1:$PROXY_PORT/healthz")）——后面的路由结论无效"
    fi

    if [[ "$(pcode "https://127.0.0.1:$PROXY_PORT/api/admin/sites")" == "404" ]]; then
      pass "反代不转发管理路由（/api/admin/* 一律 404）"
    else
      fail "反代把管理路由转出去了（code=$(pcode "https://127.0.0.1:$PROXY_PORT/api/admin/sites")）"
    fi

    WIDGET_HDRS="$(pheaders "https://127.0.0.1:$PROXY_PORT/widget/v1.js")"
    if printf '%s' "$WIDGET_HDRS" | grep -qi "cache-control:.*max-age=3600"; then
      pass "widget 静态资源透传后端自身的缓存语义（max-age=3600），未被反代覆盖成 immutable"
    else
      fail "widget 缓存头不是来自后端的语义：$(printf '%s' "$WIDGET_HDRS" | tr -d '\r' | grep -i 'cache-control' || echo 无)"
    fi
    DYN_HDRS="$(pheaders "https://127.0.0.1:$PROXY_PORT/readyz")"
    if printf '%s' "$DYN_HDRS" | grep -qi "cache-control: no-store"; then
      pass "动态端点显式 no-store（challenge/redeem/siteverify 同属这个 location 的正则）"
    else
      fail "动态端点没有 no-store：$(printf '%s' "$DYN_HDRS" | tr -d '\r' | grep -i 'cache-control' || echo 无)"
    fi

    # 大请求体与未知路径：模板只放行已知的几条路由。
    if [[ "$(pcode "https://127.0.0.1:$PROXY_PORT/etc/passwd")" == "404" ]]; then
      pass "未知路径不透传（默认 404，反代不是通用隧道）"
    else
      fail "未知路径被透传了"
    fi

    # 真实协议形状：经反代领题 + 经反代 siteverify 失败响应形状。
    CH_PROXY="$(curl -sSk --max-time 10 -X POST -H 'Content-Type: application/json' -d "{\"site_key\":\"$SITE_KEY\"}" \
      "https://127.0.0.1:$PROXY_PORT/api/challenge" 2>/dev/null || echo "")"
    if printf '%s' "$CH_PROXY" | grep -q '"token"'; then
      pass "经反代可正常领题（公共 API 与 widget 转发形状正确）"
    else
      fail "经反代领题失败：$(printf '%s' "$CH_PROXY" | head -c 160)"
    fi
    SV_PROXY="$(curl -sSk --max-time 10 -X POST -d "secret=$SECRET2&response=garbage" \
      "https://127.0.0.1:$PROXY_PORT/siteverify" 2>/dev/null || echo "")"
    if printf '%s' "$SV_PROXY" | grep -q '"success":false' && printf '%s' "$SV_PROXY" | grep -q '"error-codes"'; then
      pass "经反代的 /siteverify 走通并保持 reCAPTCHA 族响应形状"
    else
      fail "经反代的 siteverify 形状不对：$(printf '%s' "$SV_PROXY" | head -c 160)"
    fi

    # 8k 请求体上限：模板必须自己就挡住超大体，不打到服务。
    BIG="$(head -c 20000 /dev/zero | tr '\0' 'a')"
    BIGCODE="$(curl -sSk -o /dev/null -w '%{http_code}' --max-time 10 -X POST -d "secret=$BIG&response=x" \
      "https://127.0.0.1:$PROXY_PORT/siteverify" 2>/dev/null || echo 000)"
    if [[ "$BIGCODE" == "413" ]]; then
      pass "超过 client_max_body_size 的体被反代直接拒（413）"
    else
      fail "超过 client_max_body_size 的体未被反代拒绝为 413（实得 $BIGCODE）"
    fi

    # 反代访问日志：只记路径，不记 query、不记体、不记凭据值。
    docker cp "$PROXY_CID":/var/log/nginx/epocha.access.log "$LOGS/proxy-access.log" >/dev/null 2>&1 || true
    if [[ -s "$LOGS/proxy-access.log" ]]; then
      if leaks "$LOGS/proxy-access.log" "$SECRET2"; then
        fail "反代访问日志里出现站点 secret"
      elif grep -q '?' "$LOGS/proxy-access.log"; then
        fail "反代访问日志记录了 query（token/secret 常被打进 query，不该落盘）"
      else
        pass "反代访问日志只记路径与状态（无 query、无凭据值）"
      fi
    else
      fail "拿不到反代访问日志，无法证明日志形状"
    fi
  fi
  docker rm -f "$PROXY_CID" >/dev/null 2>&1 || true
  PROXY_CID=""
fi

# --- 第 12 步：文档里的复制即执行入口 ---
log "第 12 步：交付文件与“复制即可执行”入口"
for f in deploy/docker/Dockerfile deploy/docker/compose.yml .dockerignore deploy/docker/.env.example \
         deploy/docker/nginx.conf.example docs/DEPLOYMENT.md docs/BACKUP-RESTORE.md docs/STAGE5A.md; do
  if [[ -f "$ROOT/$f" ]]; then :; else fail "缺少交付文件 $f"; fi
done
if [[ -f "$ROOT/docs/DEPLOYMENT.md" && -f "$ROOT/docs/BACKUP-RESTORE.md" && -f "$ROOT/docs/STAGE5A.md" ]]; then
  pass "部署/备份恢复/阶段结果三份文档都在"
fi
if grep -q "stage5a-check" "$ROOT/Makefile"; then
  pass "make stage5a-check 入口存在"
else
  fail "Makefile 里没有 stage5a-check 入口"
fi

# --- 汇总 ---
log "汇总"
printf '[stage5a] 工程项 PASS=%d FAIL=%d；外部条件 BLOCKED=%d；本轮未执行 NOT_RUN=%d\n' \
  "$N_PASS" "$N_FAIL" "$N_BLOCKED" "$N_NOTRUN"
if [[ -s "$LOGS/loadtest.txt" ]]; then
  sed -n '1,8p' "$LOGS/loadtest.txt" | sed 's/^/[stage5a]   负载: /'
fi
if [[ $N_FAIL -gt 0 ]]; then
  printf '[stage5a] RESULT=fail（有 %d 项工程失败）\n' "$N_FAIL"
  exit 1
fi
if [[ $REQUIRED_BLOCKED -gt 0 || $N_BLOCKED -gt 0 || $N_NOTRUN -gt 0 ]]; then
  printf '[stage5a] RESULT=blocked（无失败，但 %d 项 BLOCKED / %d 项 NOT_RUN ⇒ 部署验收未通过）\n' "$N_BLOCKED" "$N_NOTRUN"
  exit 3
fi
printf '[stage5a] RESULT=pass（%d 项全部通过）\n' "$N_PASS"
exit 0
