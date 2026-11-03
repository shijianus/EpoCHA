#!/usr/bin/env bash
# epocha 试部署前的**只读预检**：在目标 VPS 上，由**将来要跑服务的那个用户**手动执行。
# 它不做任何远程连接，也不做任何变更。
#
# 为什么需要它：试部署有一批事实只能在目标机上得到——架构、内核、内存与 swap、数据目录落在哪个
# 文件系统与还剩多少、当前用户能不能动 Docker、端口有没有被占、有没有已在跑的 Nginx/Caddy、
# 密钥文件的权限位会不会让服务直接拒绝启动。开发机上的数字**不能**冒充 VPS 的结论：
# 本仓库迄今的验收数据来自一台 linux/arm64、uid 1000:1000、无 swap、无 root 的开发机。
#
# 边界（脚本自身的性质，不是口号）：
#   - 只读：不安装、不提权、不拉镜像、不构建、不创建目录/用户、不改防火墙/DNS/证书/已有服务。
#   - 不终止任何进程；端口被占只报告，"要不要停掉它"是人的决定。
#   - 不读取、不打印任何凭据内容，也不列文件名：对密钥目录只取 stat 的属主与权限位。
#   - 不输出容器环境变量、不做完整 inspect：只取 daemon 的两个字段与"运行中容器数量"。
#   - 唯一可能的写操作是 --out 指定的那一个报告文件，且已存在就拒绝覆盖。
#   - 报告本身不是部署授权：Docker 可达、端口空闲、目录可写都不等于运维批准。
#
# 用法：
#   bash preflight.sh
#   bash preflight.sh --data-dir /data/epocha --ports 80,443,8080 --out /tmp/pf.json
#   bash preflight.sh --text
#   环境变量等价：EPOCHA_PREFLIGHT_{DATA_DIR,KEYS_DIR,BACKUP_DIR,PORTS}
# 退出码：0=报告已生成（warn 不改变退出码）；1=生成不了 / --out 已存在；2=用法错误。

set -uo pipefail

DATA_DIR="${EPOCHA_PREFLIGHT_DATA_DIR:-/var/lib/epocha}"
KEYS_DIR="${EPOCHA_PREFLIGHT_KEYS_DIR:-${EPOCHA_PREFLIGHT_KEY_DIR:-/etc/epocha/keys}}"
BACKUP_DIR="${EPOCHA_PREFLIGHT_BACKUP_DIR:-/var/backups/epocha}"
PORTS="${EPOCHA_PREFLIGHT_PORTS:-80 443 8080 8081}"
OUT=""
FORMAT=json

while [[ $# -gt 0 ]]; do
  case "$1" in
    --data-dir)   DATA_DIR="${2:-}"; shift 2 ;;
    --keys-dir)   KEYS_DIR="${2:-}"; shift 2 ;;
    --backup-dir) BACKUP_DIR="${2:-}"; shift 2 ;;
    --ports)      PORTS="${2:-}"; shift 2 ;;
    --out)        OUT="${2:-}"; shift 2 ;;
    --text)       FORMAT=text; shift ;;
    --json)       FORMAT=json; shift ;;
    -h|--help)    sed -n '1,31p' "$0"; exit 0 ;;
    *) printf '用法错误：%s（--help 看用法）\n' "$1" >&2; exit 2 ;;
  esac
done
PORTS="${PORTS//,/ }"

have() { command -v "$1" >/dev/null 2>&1; }
for t in awk sed grep stat id date df uname dirname; do
  have "$t" || { printf '缺少基本工具：%s ⇒ 不出报告而不是猜\n' "$t" >&2; exit 1; }
done

FIELDS=()   # 每项形如 '"key": value'，渲染时统一加逗号
WARNS=()
esc() { local s="${1:-}"; s="${s//\\/\\\\}"; s="${s//\"/\\\"}"; s="${s//$'\n'/ }"; printf '%s' "$s"; }
warn() { WARNS+=("$1"); }
f_str()  { FIELDS+=("\"$1\": \"$(esc "$2")\""); }
f_num()  { local v="${2:-}"; if [[ "$v" =~ ^[0-9]+$ ]]; then FIELDS+=("\"$1\": $v"); else FIELDS+=("\"$1\": null"); fi; }
f_bool() { local v="${2:-false}"; [[ "$v" == true || "$v" == false ]] || v=false; FIELDS+=("\"$1\": $v"); }
f_raw()  { FIELDS+=("\"$1\": ${2:-null}"); }

# 目录还没建时也要能问到文件系统与余量：用"存在的最近祖先"做锚点，并把它如实报出来。
anchor_of() { local p="${1:-/}"; while [[ ! -e "$p" && "$p" != "/" ]]; do p="$(dirname "$p")"; done; printf '%s' "$p"; }
fs_of() { local a; a="$(anchor_of "$1")"
          if have findmnt; then findmnt -T "$a" -no FSTYPE 2>/dev/null; else df -PT "$a" 2>/dev/null | awk 'NR==2{print $2}'; fi; }
mount_of() { local a; a="$(anchor_of "$1")"; have findmnt && findmnt -T "$a" -no TARGET 2>/dev/null; }
avail_kb_of() { df -Pk "$(anchor_of "$1")" 2>/dev/null | awk 'NR==2{print $4}'; }

# 目录块：只 stat。writable 用 test -w，不创建文件（这就是只读边界）；
# 已知偏差：ACL 或只读挂载下 test -w 可能偏乐观，所以 fstype/mount 一起给出让人核对。
dir_block() {
  local field="$1" d="$2" exists=false owner="" mode="" writable=false fs="" mnt="" note="" anchor avail block
  anchor="$(anchor_of "$d")"
  if [[ -d "$d" ]]; then
    exists=true
    owner="$(stat -c '%u:%g' "$d" 2>/dev/null || echo unknown)"
    mode="$(stat -c '%a' "$d" 2>/dev/null || echo unknown)"
    [[ -w "$d" ]] && writable=true
    if [[ "$writable" != true ]]; then
      note="目录对当前用户不可写（属主 $owner）⇒ 服务或离线运维可能拿到 ErrLockAccess（权限问题），而不是 ErrLockHeld（被占用）"
      warn "[$d] $note（见 DEPLOYMENT §5.5）"
    fi
  else
    note="不存在。首次部署要由运维按 DEPLOYMENT §2 创建；本脚本不创建、不 chown"
  fi
  fs="$(fs_of "$d")"; mnt="$(mount_of "$d")"
  avail="$(avail_kb_of "$d")"
  [[ "$avail" =~ ^[0-9]+$ ]] || avail=null
  printf -v block '{"path":"%s","exists":%s,"owner":"%s","mode":"%s","writable_by_runner":%s,"fstype":"%s","mount":"%s","avail_kb":%s,"space_measured_on":"%s","note":"%s"}' \
    "$(esc "$d")" "$exists" "$(esc "${owner:-unknown}")" "$(esc "${mode:-unknown}")" "$writable" \
    "$(esc "${fs:-unknown}")" "$(esc "${mnt:-unknown}")" "$avail" "$(esc "$anchor")" "$(esc "$note")"
  f_raw "$field" "$block"
}

# ---------- 机器与身份 ----------
OSPretty=unknown
[[ -r /etc/os-release ]] && OSPretty="$(. /etc/os-release && printf '%s' "${PRETTY_NAME:-unknown}")"
RUN_UID="$(id -u)"; RUN_GID="$(id -g)"
MEM_T="$(awk '/^MemTotal:/{print $2}' /proc/meminfo 2>/dev/null)"
MEM_A="$(awk '/^MemAvailable:/{print $2}' /proc/meminfo 2>/dev/null)"
SWAP_T="$(awk '/^SwapTotal:/{print $2}' /proc/meminfo 2>/dev/null)"
SWAPPY=unknown; [[ -r /proc/sys/vm/swappiness ]] && SWAPPY="$(cat /proc/sys/vm/swappiness)"

f_str  report "epocha 只读预检（不改变机器状态；不构成部署授权）"
f_str  hostname "$(hostname 2>/dev/null || uname -n)"
f_str  ran_at_utc "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
f_str  arch "$(uname -m)"
f_str  kernel "$(uname -r)"
f_str  distro "$OSPretty"
f_str  runner_user "$(id -un)"
f_str  runner_uid_gid "$RUN_UID:$RUN_GID"
f_num  cpu_count "$(nproc 2>/dev/null || grep -c '^processor' /proc/cpuinfo 2>/dev/null)"
f_num  mem_total_kb "$MEM_T"
f_num  mem_available_kb "$MEM_A"
f_num  swap_total_kb "$SWAP_T"
f_str  vm_swappiness "$SWAPPY"
[[ "${SWAP_T:-0}" == "0" ]] && warn "无 swap。DESIGN §12 的宿主形状是 512M swap + swappiness=10：那份 swap 是给 OS 的，不是给服务的（容器侧 memswap_limit==memory 已把服务锁在物理内存里）。"
if [[ -n "$MEM_A" ]] && (( MEM_A < 512 * 1024 )); then
  warn "可用内存 < 512MB：1GB 机器还要留 OS + 反代 + Docker 的余量（服务硬上限 128MiB）。"
fi
if [[ "$RUN_UID" != "0" ]]; then
  f_str note_runner "以非 root 执行：别人的进程名、部分 systemd 状态可能取不到，缺的那项会是 unknown 而不是『没有』"
fi

# ---------- 三个目录 ----------
# 告警与字段必须在主 shell 汇总；命令替换会丢掉子 shell 对 WARNS 的追加。
dir_block data_dir   "$DATA_DIR"
dir_block keys_dir   "$KEYS_DIR"
dir_block backup_dir "$BACKUP_DIR"
[[ "$DATA_DIR" == "$BACKUP_DIR" ]] && warn "数据目录与备份目录同路径：一次误删同时带走库与备份，而且恢复没法在隔离目录做（BACKUP-RESTORE §5 第 2 步）。"
if [[ "$KEYS_DIR" == "$DATA_DIR" ]]; then
  warn "密钥目录与数据目录同路径：pepper 与库同处一地会让摘要保护直接归零（DESIGN §6）。"
fi

# 密钥文件：只 stat 权限位与属主。服务自己会拒绝 group/other 位非空的密钥文件，
# 这是"部署起不来"的头号原因，所以值得在预检里就数出来（不读内容、不列文件名）。
KEY_TOTAL=0; KEY_BADPERM=0; KEY_BADOWNER=0
if [[ -d "$KEYS_DIR" ]]; then
  while IFS= read -r kf; do
    [[ -f "$kf" ]] || continue
    KEY_TOTAL=$((KEY_TOTAL + 1))
    km="$(stat -c '%a' "$kf" 2>/dev/null || echo 777)"
    ko="$(stat -c '%u' "$kf" 2>/dev/null || echo -1)"
    # bash 的 (( )) 不认 0o 前缀（那是 Python 写法），octal 用 8#。
    if [[ "$km" =~ ^[0-7]{3,4}$ ]] && (( 8#$km & 8#77 )); then KEY_BADPERM=$((KEY_BADPERM + 1)); fi
    if [[ "$ko" != "$RUN_UID" && "$ko" != "65534" ]]; then KEY_BADOWNER=$((KEY_BADOWNER + 1)); fi
  done < <(find "$KEYS_DIR" -maxdepth 1 -type f 2>/dev/null)
fi
f_num  key_files_total "$KEY_TOTAL"
f_num  key_files_with_group_or_other_bits "$KEY_BADPERM"
f_num  key_files_owned_by_neither_runner_nor_65534 "$KEY_BADOWNER"
f_str  key_dir_policy "不读内容、不列文件名：只取每个密钥文件的 stat 属主与权限位"
(( KEY_BADPERM > 0 )) && warn "密钥里有 $KEY_BADPERM 把的 group/other 位不为空 ⇒ 服务会**拒绝启动**（config.readKeyFile 的 fail-closed 规则）。修法是 chmod 600，不是让服务放宽检查。"
(( KEY_BADOWNER > 0 )) && warn "密钥里有 $KEY_BADOWNER 把的属主既不是当前用户也不是 65534 ⇒ 容器 uid 读不到，同样是拒绝启动而不是降级运行。"
[[ -d "$KEYS_DIR" && $KEY_TOTAL -eq 0 ]] && warn "密钥目录存在但没有文件：缺 master/pepper 时服务拒绝启动（没有内置默认密钥，这是设计不是缺陷）。"

# ---------- Docker / Compose ----------
DOCKER_PRESENT=false; DAEMON_OK=false; DOCKER_VER=""; COMPOSE_VER=""; DAEMON_OS=""; CGROUP=""; CONTAINERS=""
if have docker; then
  DOCKER_PRESENT=true
  DOCKER_VER="$(docker --version 2>/dev/null | awk '{print $3}' | tr -d ',' || true)"
  if docker info >/dev/null 2>&1; then
    DAEMON_OK=true
    COMPOSE_VER="$(docker compose version --short 2>/dev/null || true)"
    DAEMON_OS="$(docker info --format '{{.OperatingSystem}}' 2>/dev/null || true)"
    CGROUP="$(docker info --format '{{.CgroupDriver}}' 2>/dev/null || true)"
    CONTAINERS="$(docker ps -q 2>/dev/null | wc -l | tr -d ' ')"
  fi
fi
f_bool docker_cli_present "$DOCKER_PRESENT"
f_bool docker_daemon_reachable_by_runner "$DAEMON_OK"
f_str  docker_version "${DOCKER_VER:-absent}"
f_str  compose_version "${COMPOSE_VER:-absent}"
f_str  daemon_os "$DAEMON_OS"
f_str  cgroup_driver "$CGROUP"
f_num  running_containers_count "$CONTAINERS"
[[ "$DOCKER_PRESENT" == false ]] && warn "没有 docker ⇒ 本脚本不装软件；安装方式与镜像分发方式（无 registry 时是否 docker save/load）要运维先定。"
[[ "$DOCKER_PRESENT" == true && "$DAEMON_OK" == false ]] && warn "docker CLI 在但当前用户连不上 daemon ⇒ 要么把该用户加入 docker 组（提权性质的变更，需要你决定），要么换执行身份。这个选择要和 §5.5 的 uid 规则一起定。"
[[ "$DAEMON_OK" == true && -z "$COMPOSE_VER" ]] && warn "compose 子命令不可用 ⇒ DEPLOYMENT §4/§11 的 \$COMPOSE 那几行跑不起来。"
if [[ "$DAEMON_OK" == true && "$CONTAINERS" =~ ^[0-9]+$ ]] && (( CONTAINERS > 0 )); then
  warn "机器上已有 $CONTAINERS 个运行中的容器（只报数量，不列名字与配置）：端口冲突与磁盘竞争要在部署前对齐。"
fi

# ---------- 端口 ----------
LISTENER_TOOL=none
if have ss; then LISTENER_TOOL=ss
elif have netstat; then LISTENER_TOOL=netstat
elif [[ -r /proc/net/tcp ]]; then LISTENER_TOOL=proc_net_tcp
fi
f_str port_probe_tool "$LISTENER_TOOL"
[[ "$LISTENER_TOOL" == none ]] && warn "没有 ss、没有 netstat、/proc/net/tcp 也读不到 ⇒ 端口占用无法判定。不要把这种状态当成『空闲』就开干。"
ports_json="["; first=1
for p in $PORTS; do
  [[ "$p" =~ ^[0-9]{1,5}$ ]] || { warn "忽略非法端口写法：$p"; continue; }
  st=unknown
  case "$LISTENER_TOOL" in
    ss) if o="$(ss -H -ltn "sport = :$p" 2>/dev/null)"; then
          # -H 去表头。只有成功的空结果才证明本次没有监听；非零出口保持 unknown。
          if [[ -n "$o" ]]; then st=occupied; else st=free; fi
        fi ;;
    netstat) if o="$(netstat -ltn 2>/dev/null)"; then
          if state="$(awk -v p="$p" '$1 ~ /^tcp/ && $4 ~ ("[:.]" p "$") {found=1} END {print found ? "occupied" : "free"}' <<<"$o")"; then
            st="$state"
          fi
        fi ;;
    proc_net_tcp) hp="$(printf '%04X' "$p")"
        if state="$(awk -v h="$hp" '$4=="0A"{n=split($2,a,":"); if (a[n]==h) found=1} END {print found ? "occupied" : "free"}' /proc/net/tcp /proc/net/tcp6 2>/dev/null)"; then
          st="$state"
        fi ;;
  esac
  [[ "$st" == unknown ]] && warn "端口 $p 探测失败或不可用（$LISTENER_TOOL）⇒ unknown；不能当成空闲。"
  [[ $first -eq 1 ]] && first=0 || ports_json+=","
  ports_json+="{\"port\":$p,\"state\":\"$st\"}"
  if [[ "$st" == occupied ]]; then
    if [[ "$p" == "8081" ]]; then
      warn "宿主上 8081 有人在听。管理面按设计**从不**发布（compose 里没有它的端口项），所以这个监听一定来自别的东西或某次手工端口映射 ⇒ 先查清来源再部署。"
    else
      warn "端口 $p 已被占用。本脚本不终止占用者；部署要么改 EPOCHA_PUBLIC_PORT，要么由运维处置。"
    fi
  fi
done
ports_json+="]"
f_raw ports "$ports_json"

# ---------- 反代候选 ----------
web_json="["; wfirst=1
for svc in nginx caddy; do
  present=false; active=unknown; ver=""
  if have "$svc"; then
    present=true
    ver="$("$svc" -v 2>&1 | head -n1 | tr -d '\r' || true)"
    if have systemctl; then active="$(systemctl is-active "$svc" 2>/dev/null || echo inactive)"; fi
  fi
  [[ $wfirst -eq 1 ]] && wfirst=0 || web_json+=","
  web_json+="{\"name\":\"$svc\",\"binary_present\":$present,\"systemd_active\":\"$(esc "$active")\",\"version\":\"$(esc "$ver")\"}"
done
web_json+="]"
f_raw reverse_proxy_candidates "$web_json"

f_str not_done_by_this_script "安装、提权、创建目录/用户、拉镜像、构建、启动容器、改防火墙/DNS/证书、终止进程、读取或打印任何凭据内容"
f_str authorization "Docker 可达、端口空闲、目录可写都不等于部署已获授权"

# ---------- 渲染 ----------
render_json() {
  printf '{\n'
  local i firsti=1
  for i in "${FIELDS[@]}"; do
    [[ $firsti -eq 1 ]] && firsti=0 || printf ',\n'
    printf '  %s' "$i"
  done
  printf ',\n  "warnings": ['
  firsti=1
  for i in ${WARNS[@]+"${WARNS[@]}"}; do
    [[ $firsti -eq 1 ]] && firsti=0 || printf ','
    printf '\n    "%s"' "$(esc "$i")"
  done
  [[ ${#WARNS[@]} -gt 0 ]] && printf '\n  '
  printf ']\n}\n'
}
render_text() {
  printf 'epocha 只读预检（不改变机器状态，不构成部署授权）\n'
  printf '── 机器：%s / %s / %s，执行人 %s (%s)\n' \
    "$(uname -m)" "$(uname -r)" "$OSPretty" "$(id -un)" "$RUN_UID:$RUN_GID"
  local i
  for i in "${FIELDS[@]}"; do printf '%s\n' "$i"; done
  if [[ ${#WARNS[@]} -gt 0 ]]; then
    printf '\n需要人判断的 %d 项：\n' "${#WARNS[@]}"
    for i in "${WARNS[@]}"; do printf '  - %s\n' "$i"; done
  else
    printf '\n没有需要你判断的项。\n'
  fi
  printf '\n本脚本未做：安装/提权/建目录/拉镜像/构建/启动/改防火墙/终止进程/读凭据\n'
}

BODY_FILE=""
if [[ -n "$OUT" ]]; then
  if [[ -e "$OUT" ]]; then
    printf '拒绝覆盖已存在的 --out 文件：%s\n' "$OUT" >&2
    exit 1
  fi
  BODY_FILE="$(mktemp)"
  trap 'rm -f "${BODY_FILE:-}"' EXIT
fi
if [[ "$FORMAT" == json ]]; then
  if [[ -n "$BODY_FILE" ]]; then render_json >"$BODY_FILE"; else render_json; fi
else
  if [[ -n "$BODY_FILE" ]]; then render_text >"$BODY_FILE"; else render_text; fi
fi
if [[ -n "$BODY_FILE" ]]; then
  mv "$BODY_FILE" "$OUT"
  BODY_FILE=""
  printf '报告已写入 %s\n' "$OUT" >&2
fi
printf '预检完成：warn=%d（warn 给人判断，不是失败）；未改变机器状态；这不是部署授权\n' "${#WARNS[@]}" >&2
exit 0
