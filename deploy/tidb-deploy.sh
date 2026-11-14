#!/usr/bin/env bash
# =============================================================================
# tidb-deploy.sh —— CentOS 7 / 8 / 9（及 Rocky / Alma / RHEL 系）一键部署 TiDB 集群（基于 TiUP）
#
# 做的事：环境检查 → 系统初始化（swap/THP/sysctl/limits/SELinux/防火墙/时间同步/tidb 用户与免密）
#         → 安装 TiUP（在线或离线）→ 生成拓扑 → tiup check --apply → tiup deploy → start → 验证
#
# 先看再用：   ./tidb-deploy.sh help
#            ./tidb-deploy.sh show-prep     # 打印将在每台机器上以 root 执行的初始化脚本，供审阅
#            ./tidb-deploy.sh deploy --dry-run --config deploy.conf   # 只预览，不做任何修改
#
# 兼容性：脚本只使用 bash 4.2（CentOS 7 自带）可用的语法。
# =============================================================================
set -Eeo pipefail

TD_VERSION="0.3.1"
TD_TEST="${TD_TEST:-0}"
OS_RELEASE_FILE="${TD_OS_RELEASE:-/etc/os-release}"
WORKROOT="${TD_WORKROOT:-/var/lib/tidb-deploy}"
LOGDIR="${TD_LOGDIR:-/var/log/tidb-deploy}"
SELF="$(basename "${BASH_SOURCE[0]:-$0}")"

# ---------------------------------------------------------------- 输出与日志 ----
if [[ -t 1 ]]; then
  C_R=$'\033[31m'; C_G=$'\033[32m'; C_Y=$'\033[33m'; C_B=$'\033[36m'; C_0=$'\033[0m'
else
  C_R=""; C_G=""; C_Y=""; C_B=""; C_0=""
fi
LOG_FILE=""
_log() { if [[ -n "$LOG_FILE" ]]; then printf '%s %s\n' "$(date '+%F %T')" "$*" >> "$LOG_FILE" 2>/dev/null || true; fi; }
info() { printf '%s\n' "${C_B}[信息]${C_0} $*"; _log "[INFO] $*"; }
ok()   { printf '%s\n' "${C_G}[ ok ]${C_0} $*"; _log "[ OK ] $*"; }
warn() { printf '%s\n' "${C_Y}[警告]${C_0} $*" >&2; _log "[WARN] $*"; }
err()  { printf '%s\n' "${C_R}[错误]${C_0} $*" >&2; _log "[ERR ] $*"; }
die()  { err "$*"; exit 1; }
step() { printf '\n%s\n' "${C_B}==> $*${C_0}"; _log "==> $*"; }

PASS=0; WARNS=0; FAILS=0
c_ok()   { PASS=$((PASS + 1));   printf '  %s %s\n' "${C_G}✔${C_0}" "$*"; _log "[CHECK ok] $*"; }
c_warn() { WARNS=$((WARNS + 1)); printf '  %s %s\n' "${C_Y}⚠${C_0}" "$*"; _log "[CHECK warn] $*"; }
c_fail() { FAILS=$((FAILS + 1)); printf '  %s %s\n' "${C_R}✖${C_0}" "$*"; _log "[CHECK fail] $*"; }

DRY_RUN=0; ASSUME_YES=0; FORCE=0; CONFIG_FILE=""; CONFIRM_NAME=""; NO_INIT=0; STRICT=0
CLI_USER=""; CLI_VERSION=""; CLI_OFFLINE_DIR=""
DL_VERSION=""; DL_DIR=""; DL_ARCH=""; DL_TOOLKIT=0; DL_TARGET_OS=""; DL_BASE="${TD_DOWNLOAD_BASE:-https://download.pingcap.org}"

# 执行命令：dry-run 只打印；否则执行并把输出同时写入日志
run() {
  if [[ "$DRY_RUN" == 1 ]]; then printf '%s\n' "${C_Y}[DRY]${C_0} $*"; return 0; fi
  _log "[RUN] $*"
  if [[ -n "$LOG_FILE" ]]; then "$@" 2>&1 | tee -a "$LOG_FILE"; else "$@"; fi
}
# 用 bash -c 执行一段命令串（含管道等）
run_sh() { run bash -c "$1"; }

confirm() {
  [[ "$ASSUME_YES" == 1 ]] && return 0
  [[ -t 0 ]] || die "需要交互确认，但当前不是交互终端。请加 --yes 跳过确认。"
  local a; read -r -p "$1 [y/N] " a
  [[ "$a" == "y" || "$a" == "Y" ]]
}

# ---------------------------------------------------------------- 配置 ----
set_defaults() {
  CLUSTER_NAME="tidb-test"
  TIDB_VERSION="auto"            # auto | v8.5 | v7.5.4 ...
  DEPLOY_MODE="single"           # single（单机全部署）| cluster（多机）
  SINGLE_HOST=""                 # single 模式的 IP，留空自动取本机主 IP
  PD_HOSTS=""; TIDB_HOSTS=""; TIKV_HOSTS=""; TIFLASH_HOSTS=""
  MONITOR_HOST=""                # 监控（Prometheus / Grafana / Alertmanager）所在主机，留空取第一台 PD
  WITH_MONITOR="true"
  ZONE_MAP=""                    # 形如 "10.0.2.11=az1,10.0.2.12=az2"；设置后启用 zone,host 拓扑标签
  TIKV_CACHE_GB=""               # TiKV block-cache（GB）；single 模式留空时按内存 25% 自动设置
  DEPLOY_USER="tidb"; SSH_PORT=22   # DEPLOY_USER：TiUP 与集群运行所用的操作系统用户，可自定义（不能是 root）
  DEPLOY_DIR="/tidb-deploy"; DATA_DIR="/tidb-data"
  SSH_KEY=""                     # 部署用户的 SSH 私钥（绝对路径）；留空 = 该用户家目录里的 ~/.ssh/id_rsa
  SPEC="dev"                     # dev（开发测试，低配可继续）| prod（按生产配置要求，不达标则中止）
  FIREWALL="open"                # open（仅放行 TiDB 所需端口）| disable | keep
  REPO_FIX="skip"                # skip | auto（CentOS 7/8 已 EOL，yum 源失效时切换到 vault 镜像）
  REPO_MIRROR="aliyun"           # aliyun | vault
  INSTALL_MODE="auto"            # auto | online | offline
  OFFLINE_PKG_DIR=""            # 离线安装包所在的完整目录（绝对路径）；脚本按版本自动在其中找 tidb-community-server-*.tar.gz
  OFFLINE_SERVER_PKG=""; OFFLINE_TOOLKIT_PKG=""; OFFLINE_RPM_DIR=""   # 也可直接指定单个文件
  ARCH=""                       # amd64 | arm64，留空按本机自动识别
  TIUP_MIRROR=""                 # 自定义 TiUP 镜像地址（可选）
  TIUP_UPDATE="false"
  ALLOW_CHECK_FAIL="false"       # tiup check 仍有 Fail 时是否继续（测试环境可设 true）
  INIT_PASSWORD="true"           # 启动时生成 root 随机密码（tiup start --init）
}

load_config() {
  local f="$1"
  [[ -f "$f" ]] || die "配置文件不存在: $f"
  if [[ "$TD_TEST" != 1 ]]; then
    local mode; mode="$(stat -c '%a' "$f" 2>/dev/null || echo 600)"
    [[ "${mode: -1}" == "0" ]] || warn "配置文件 $f 对其他用户开放了权限（$mode），它会以 root 身份被 source，建议 chmod 600"
  fi
  # 配置文件是 bash 变量赋值，按 root 权限 source，请确认来源可信
  # shellcheck disable=SC1090
  source "$f"
}

norm_hosts() { printf '%s' "$*" | tr ',;' '  ' | tr -s ' ' | sed -e 's/^ //' -e 's/ $//'; }
count_words() { local n=0 w; for w in $1; do n=$((n + 1)); done; echo "$n"; }
valid_ip() {
  local ip="$1" o IFS=.
  [[ "$ip" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] || return 1
  for o in $ip; do (( 10#$o <= 255 )) || return 1; done
}
uniq_words() { local seen="" w out=""; for w in $1; do case " $seen " in *" $w "*) ;; *) seen="$seen $w"; out="$out $w";; esac; done; norm_hosts "$out"; }

# ---------------------------------------------------------------- 版本与系统 ----
ver_ge() { # $1 >= $2 ？
  local a="${1#v}" b="${2#v}"
  [[ "$(printf '%s\n%s\n' "$a" "$b" | sort -V | head -n1)" == "$b" ]]
}

OS_ID=""; OS_VER=""; OS_NAME=""; OS_MAJOR=""; OS_FAMILY=""; OS_LEVEL=""
detect_os() {
  [[ -r "$OS_RELEASE_FILE" ]] || die "无法读取 $OS_RELEASE_FILE，无法识别操作系统"
  OS_ID="$(. "$OS_RELEASE_FILE"; echo "${ID:-}")"
  OS_VER="$(. "$OS_RELEASE_FILE"; echo "${VERSION_ID:-}")"
  OS_NAME="$(. "$OS_RELEASE_FILE"; echo "${NAME:-}")"
  OS_MAJOR="${OS_VER%%.*}"
  case "$OS_ID" in
    centos)
      if [[ "$OS_NAME" == *Stream* ]]; then OS_FAMILY="centos-stream${OS_MAJOR}"; OS_LEVEL="compat"
      else OS_FAMILY="centos${OS_MAJOR}"; OS_LEVEL="legacy"; fi ;;
    rhel|rocky|almalinux) OS_FAMILY="${OS_ID}${OS_MAJOR}"; OS_LEVEL="ok" ;;
    ol)                   OS_FAMILY="ol${OS_MAJOR}";        OS_LEVEL="compat" ;;
    *)                    OS_FAMILY="unsupported";           OS_LEVEL="unsupported" ;;
  esac
  case "$OS_MAJOR" in
    7|8|9) ;;
    *) if [[ "$OS_LEVEL" != unsupported ]]; then OS_LEVEL="unverified"; fi ;;
  esac
  return 0
}

detect_glibc() {
  if [[ -n "${TD_GLIBC:-}" ]]; then echo "$TD_GLIBC"; return; fi
  ldd --version 2>&1 | head -n1 | grep -oE '[0-9]+\.[0-9]+$' || echo "0"
}

# TiDB 官方：运行时依赖 glibc 2.28；自 v8.4 起不再支持 CentOS 7（glibc 2.17）
MIN_GLIBC_NEW="2.28"
MIN_TIDB_NEEDS_NEW_GLIBC="8.4.0"
default_series() { if ver_ge "$1" "$MIN_GLIBC_NEW"; then echo "v8.5"; else echo "v7.5"; fi; }

# 返回 0 通过；非 0 并输出原因
version_gate() {
  local ver="$1" glibc="$2"
  [[ "$ver" =~ ^v?[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || { echo "版本号格式不正确: $ver"; return 1; }
  if ! ver_ge "$glibc" "$MIN_GLIBC_NEW" && ver_ge "$ver" "$MIN_TIDB_NEEDS_NEW_GLIBC"; then
    echo "当前系统 glibc 为 $glibc（CentOS 7 级别），而 TiDB ${ver} 需要 glibc ≥ ${MIN_GLIBC_NEW}；官方自 v8.4 起不再支持 CentOS 7。请改用 v8.1.x / v7.5.x LTS，或把系统升级到 Rocky Linux 9 / CentOS Stream 9。"
    return 1
  fi
  return 0
}

# 在可用版本列表（每行一个 vX.Y.Z）里取某个系列（如 v8.5）的最新补丁版本
latest_in_series() {
  local series="${1#v}"
  grep -E "^v${series//./\\.}\.[0-9]+$" | sed 's/^v//' | sort -V | tail -n1 | sed 's/^/v/'
}

# ---------------------------------------------------------------- 主机与角色 ----
PD_LIST=""; TIDB_LIST=""; TIKV_LIST=""; TIFLASH_LIST=""; MON_LIST=""; ALL_HOSTS=""
LOCAL_IPS=""
detect_local_ips() {
  if [[ -n "${TD_LOCAL_IPS:-}" ]]; then LOCAL_IPS="$TD_LOCAL_IPS"; return; fi
  LOCAL_IPS="127.0.0.1 $(hostname -I 2>/dev/null || true)"
}
is_local() { local h="$1" i; for i in $LOCAL_IPS; do [[ "$i" == "$h" ]] && return 0; done; return 1; }
primary_ip() { detect_local_ips; local i; for i in $LOCAL_IPS; do [[ "$i" != 127.0.0.1 ]] && { echo "$i"; return; }; done; echo "127.0.0.1"; }

resolve_hosts() {
  if [[ "$DEPLOY_MODE" == "single" ]]; then
    local h; h="$(norm_hosts "$SINGLE_HOST")"
    [[ -n "$h" ]] || h="$(primary_ip)"
    [[ "$(count_words "$h")" == 1 ]] || die "single 模式只能指定一个主机（SINGLE_HOST），当前: $h"
    PD_LIST="$h"; TIDB_LIST="$h"; TIKV_LIST="$h"; MON_LIST="$h"; TIFLASH_LIST="$(norm_hosts "$TIFLASH_HOSTS")"
  elif [[ "$DEPLOY_MODE" == "cluster" ]]; then
    PD_LIST="$(norm_hosts "$PD_HOSTS")"; TIDB_LIST="$(norm_hosts "$TIDB_HOSTS")"
    TIKV_LIST="$(norm_hosts "$TIKV_HOSTS")"; TIFLASH_LIST="$(norm_hosts "$TIFLASH_HOSTS")"
    MON_LIST="$(norm_hosts "$MONITOR_HOST")"
    [[ -n "$MON_LIST" ]] || MON_LIST="${PD_LIST%% *}"
  else
    die "DEPLOY_MODE 只能是 single 或 cluster，当前: $DEPLOY_MODE"
  fi
  [[ "$WITH_MONITOR" == "true" ]] || MON_LIST=""
  ALL_HOSTS="$(uniq_words "$PD_LIST $TIDB_LIST $TIKV_LIST $TIFLASH_LIST $MON_LIST")"
}

validate_hosts() {
  local role list h
  [[ -n "$PD_LIST" ]]   || die "至少需要 1 台 PD（PD_HOSTS）"
  [[ -n "$TIDB_LIST" ]] || die "至少需要 1 台 TiDB（TIDB_HOSTS）"
  [[ -n "$TIKV_LIST" ]] || die "至少需要 1 台 TiKV（TIKV_HOSTS）"
  for role in PD TIDB TIKV TIFLASH MON; do
    eval "list=\$${role}_LIST"
    [[ -z "$list" ]] && continue
    [[ "$(uniq_words "$list")" == "$list" ]] || die "$role 角色里有重复的主机: $list（同一主机上同一角色只部署一个实例）"
    for h in $list; do valid_ip "$h" || die "$role 的主机 \"$h\" 不是合法的 IPv4 地址"; done
  done
  local np nk nt; np="$(count_words "$PD_LIST")"; nk="$(count_words "$TIKV_LIST")"; nt="$(count_words "$TIDB_LIST")"
  if (( np % 2 == 0 )); then warn "PD 数量为 $np（偶数），不提升容错，建议使用奇数（1/3/5）"; fi
  if [[ "$DEPLOY_MODE" == "cluster" ]]; then
    if (( np < 3 || nk < 3 || nt < 2 )); then
      if [[ "$SPEC" == "prod" ]]; then die "生产配置要求 PD ≥ 3、TiKV ≥ 3、TiDB ≥ 2，当前 PD=$np TiKV=$nk TiDB=$nt"; fi
      warn "当前 PD=$np TiKV=$nk TiDB=$nt，低于生产建议（PD≥3、TiKV≥3、TiDB≥2），仅适合测试"
    fi
  fi
  if [[ -n "$ZONE_MAP" ]]; then
    local pair
    for pair in $(echo "$ZONE_MAP" | tr ',' ' '); do
      [[ "$pair" =~ ^[0-9.]+=[A-Za-z0-9_-]+$ ]] || die "ZONE_MAP 格式不正确: \"$pair\"（应为 IP=zone）"
    done
    for h in $TIKV_LIST; do zone_of "$h" >/dev/null || die "ZONE_MAP 缺少 TiKV 主机 $h 的 zone"; done
  fi
  [[ "$DEPLOY_USER" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || die "操作系统用户名不合法: ${DEPLOY_USER}（只能含小写字母、数字、下划线、连字符，最长 32 位）"
  [[ "$DEPLOY_USER" != root ]] || die "操作系统用户不能是 root：TiUP 应以普通用户运行（脚本会为该用户配置 sudo 免密）。请使用 tidb 或其他普通用户名"
  local uid; uid="$(id -u "$DEPLOY_USER" 2>/dev/null || true)"
  if [[ -n "$uid" ]] && (( uid < 1000 )); then die "用户 ${DEPLOY_USER} 是系统账号（uid ${uid} < 1000），不适合作为部署用户，请换一个普通用户名"; fi
  [[ "$CLUSTER_NAME" =~ ^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$ ]] || die "CLUSTER_NAME 只能包含字母数字、下划线、连字符，且不超过 32 位: $CLUSTER_NAME"
  [[ "$DEPLOY_DIR" == /* && "$DATA_DIR" == /* ]] || die "DEPLOY_DIR / DATA_DIR 必须是绝对路径"
}

zone_of() { # 输出 host 对应的 zone；找不到返回 1
  local pair
  for pair in $(echo "$ZONE_MAP" | tr ',' ' '); do
    [[ "${pair%%=*}" == "$1" ]] && { echo "${pair#*=}"; return 0; }
  done
  return 1
}

# ---------------------------------------------------------------- 拓扑生成 ----
mem_cache_gb() { # $1 = 内存 MB，取 25%，至少 1GB
  local gb=$(( $1 / 1024 / 4 )); (( gb < 1 )) && gb=1; echo "$gb"
}

gen_topology() {
  local nk replicas h z
  nk="$(count_words "$TIKV_LIST")"; replicas=3; (( nk < 3 )) && replicas="$nk"
  echo "# 由 ${SELF} ${TD_VERSION} 生成，集群 ${CLUSTER_NAME}"
  printf 'global:\n  user: "%s"\n  ssh_port: %s\n  deploy_dir: "%s"\n  data_dir: "%s"\n\n' "$DEPLOY_USER" "$SSH_PORT" "$DEPLOY_DIR" "$DATA_DIR"
  echo "server_configs:"
  echo "  pd:"
  echo "    replication.max-replicas: ${replicas}"
  [[ -n "$ZONE_MAP" ]] && echo '    replication.location-labels: ["zone", "host"]'
  if [[ -n "$TIKV_CACHE_GB" ]]; then
    printf '  tikv:\n    storage.block-cache.capacity: "%sGB"\n' "$TIKV_CACHE_GB"
  fi
  printf '\npd_servers:\n';   for h in $PD_LIST;   do printf '  - host: %s\n' "$h"; done
  printf '\ntidb_servers:\n'; for h in $TIDB_LIST; do printf '  - host: %s\n' "$h"; done
  printf '\ntikv_servers:\n'
  for h in $TIKV_LIST; do
    printf '  - host: %s\n' "$h"
    if z="$(zone_of "$h")"; then printf '    config:\n      server.labels: { zone: "%s", host: "%s" }\n' "$z" "${h//./-}"; fi
  done
  if [[ -n "$TIFLASH_LIST" ]]; then printf '\ntiflash_servers:\n'; for h in $TIFLASH_LIST; do printf '  - host: %s\n' "$h"; done; fi
  if [[ -n "$MON_LIST" ]]; then
    printf '\nmonitoring_servers:\n  - host: %s\n\ngrafana_servers:\n  - host: %s\n\nalertmanager_servers:\n  - host: %s\n' "$MON_LIST" "$MON_LIST" "$MON_LIST"
  fi
}

# ---------------------------------------------------------------- 各角色端口 ----
ports_for_host() { # 该主机上需要放行 / 需要空闲的端口
  local h="$1" p=""
  case " $PD_LIST "      in *" $h "*) p="$p 2379 2380";; esac
  case " $TIDB_LIST "    in *" $h "*) p="$p 4000 10080";; esac
  case " $TIKV_LIST "    in *" $h "*) p="$p 20160 20180";; esac
  case " $TIFLASH_LIST " in *" $h "*) p="$p 9000 8123 3930 20170 20292 8234";; esac
  case " $MON_LIST "     in *" $h "*) p="$p 9090 12020 3000 9093 9094 9115";; esac
  norm_hosts "$p 9100"
}
roles_of_host() {
  local h="$1" r=""
  case " $PD_LIST "      in *" $h "*) r="$r pd";; esac
  case " $TIDB_LIST "    in *" $h "*) r="$r tidb";; esac
  case " $TIKV_LIST "    in *" $h "*) r="$r tikv";; esac
  case " $TIFLASH_LIST " in *" $h "*) r="$r tiflash";; esac
  norm_hosts "$r"
}

# ---------------------------------------------------------------- 远程执行 ----
WORK=""
rexec() { # rexec <host>   从标准输入读取脚本，在目标主机以 root 权限执行（本机直接执行）
  local host="$1"
  if is_local "$host"; then bash -s; return; fi
  # 以部署用户（tidb@host）+ 它的密钥登录，再用 sudo -n 提权；前置条件检查已保证：免密登录 + sudo 免密
  local opts=(-p "$SSH_PORT" -i "$SSH_KEY" -o BatchMode=yes -o StrictHostKeyChecking=no -o "UserKnownHostsFile=${WORK:-/dev/null}/known_hosts" -o ConnectTimeout=8 -o LogLevel=ERROR)
  ssh "${opts[@]}" "${DEPLOY_USER}@${host}" "sudo -n bash -s"
}

# 远端只读事实收集（以 root 运行），输出 KEY=VALUE
remote_facts() {
  local d="$P_DATA_DIR" p
  while [[ ! -d "$d" && "$d" != "/" ]]; do d="$(dirname "$d")"; done
  echo "CORES=$(nproc 2>/dev/null || echo 0)"
  echo "MEM_MB=$(awk '/MemTotal/{printf "%d", $2/1024}' /proc/meminfo 2>/dev/null || echo 0)"
  echo "OS_ID=$(. /etc/os-release 2>/dev/null; echo "${ID:-}")"
  echo "OS_VER=$(. /etc/os-release 2>/dev/null; echo "${VERSION_ID:-}")"
  echo "OS_NAME=$(. /etc/os-release 2>/dev/null; echo "${NAME:-}" | tr ' ' '_')"
  echo "GLIBC=$(ldd --version 2>&1 | head -n1 | grep -oE '[0-9]+\.[0-9]+$' || echo 0)"
  echo "AVX2=$(grep -qw avx2 /proc/cpuinfo 2>/dev/null && echo 1 || echo 0)"
  echo "DATA_FS=$(findmnt -n -o FSTYPE -T "$d" 2>/dev/null || echo unknown)"
  echo "DATA_OPTS=$(findmnt -n -o OPTIONS -T "$d" 2>/dev/null || echo unknown)"
  echo "DATA_FREE_GB=$(df -BG --output=avail "$d" 2>/dev/null | tail -n1 | tr -dc '0-9' || echo 0)"
  local busy=""
  for p in $P_PORTS; do ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${p}\$" && busy="$busy $p"; done
  echo "PORTS_BUSY=$(echo $busy)"
}

# 以 root 在目标机执行的初始化（本机与远端同一份代码，可用 show-prep 查看）
remote_prep() {
  set -e
  say() { echo "[prep:$(hostname -s)] $*"; }
  [[ "$(id -u)" == 0 ]] || { echo "需要 root 权限"; return 1; }
  local pm="yum"; command -v dnf >/dev/null 2>&1 && pm="dnf"

  # 1. 依赖包
  local need="" p
  for p in $P_PKGS; do rpm -q "$p" >/dev/null 2>&1 || need="$need $p"; done
  if [[ -n "$need" ]]; then
    say "安装依赖:$need"
    if [[ -n "$P_OFFLINE_RPM_DIR" && -d "$P_OFFLINE_RPM_DIR" ]]; then
      rpm -Uvh --replacepkgs "$P_OFFLINE_RPM_DIR"/*.rpm || true
    fi
    if ! $pm install -y $need; then
      . /etc/os-release
      if [[ "$P_REPO_FIX" == "auto" && "$ID" == "centos" && "$NAME" != *Stream* && ( "${VERSION_ID%%.*}" == 7 || "${VERSION_ID%%.*}" == 8 ) ]]; then
        say "yum 源不可用（CentOS ${VERSION_ID} 已 EOL），切换到 $P_REPO_MIRROR 的 vault 镜像"
        local ts bak rel base key
        ts="$(date +%Y%m%d%H%M%S)"; bak="/etc/yum.repos.d/tidb-deploy-backup-$ts"
        mkdir -p "$bak"; mv /etc/yum.repos.d/*.repo "$bak"/ 2>/dev/null || true
        if [[ "$P_REPO_MIRROR" == "aliyun" ]]; then base="https://mirrors.aliyun.com/centos-vault"; else base="https://vault.centos.org"; fi
        if [[ "${VERSION_ID%%.*}" == 7 ]]; then
          rel="7.9.2009"; key="file:///etc/pki/rpm-gpg/RPM-GPG-KEY-CentOS-7"
          printf '[tidb-deploy-os]\nname=CentOS-7 vault os\nbaseurl=%s/%s/os/$basearch/\ngpgcheck=1\ngpgkey=%s\n[tidb-deploy-updates]\nname=CentOS-7 vault updates\nbaseurl=%s/%s/updates/$basearch/\ngpgcheck=1\ngpgkey=%s\n[tidb-deploy-extras]\nname=CentOS-7 vault extras\nbaseurl=%s/%s/extras/$basearch/\ngpgcheck=1\ngpgkey=%s\n' \
            "$base" "$rel" "$key" "$base" "$rel" "$key" "$base" "$rel" "$key" > /etc/yum.repos.d/tidb-deploy-vault.repo
        else
          rel="8.5.2111"; key="file:///etc/pki/rpm-gpg/RPM-GPG-KEY-centosofficial"
          printf '[tidb-deploy-baseos]\nname=CentOS-8 vault BaseOS\nbaseurl=%s/%s/BaseOS/$basearch/os/\ngpgcheck=1\ngpgkey=%s\n[tidb-deploy-appstream]\nname=CentOS-8 vault AppStream\nbaseurl=%s/%s/AppStream/$basearch/os/\ngpgcheck=1\ngpgkey=%s\n' \
            "$base" "$rel" "$key" "$base" "$rel" "$key" > /etc/yum.repos.d/tidb-deploy-vault.repo
        fi
        say "原 repo 已备份到 $bak"
        $pm clean all >/dev/null 2>&1 || true
        $pm install -y $need
      else
        echo "依赖安装失败:$need"
        echo "若是 CentOS 7/8 的 yum 源失效，可在配置中设置 REPO_FIX=auto；离线环境请设置 OFFLINE_RPM_DIR。"
        return 1
      fi
    fi
  fi

  # 2. 关闭 swap
  swapoff -a 2>/dev/null || true
  if grep -qE '^[^#].*[[:space:]]swap[[:space:]]' /etc/fstab 2>/dev/null; then
    cp -p /etc/fstab /etc/fstab.tidb-deploy.bak
    sed -i -E 's@^([^#].*[[:space:]]swap[[:space:]].*)$@# tidb-deploy: \1@' /etc/fstab
    say "已注释 /etc/fstab 中的 swap（备份 /etc/fstab.tidb-deploy.bak）"
  fi

  # 3. 关闭透明大页（立即生效 + 开机生效）
  if [[ -e /sys/kernel/mm/transparent_hugepage/enabled ]]; then
    echo never > /sys/kernel/mm/transparent_hugepage/enabled || true
    echo never > /sys/kernel/mm/transparent_hugepage/defrag 2>/dev/null || true
    cat > /etc/systemd/system/tidb-disable-thp.service <<'UNIT'
[Unit]
Description=Disable Transparent Huge Pages (TiDB)
After=sysinit.target local-fs.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh -c 'echo never > /sys/kernel/mm/transparent_hugepage/enabled; echo never > /sys/kernel/mm/transparent_hugepage/defrag'

[Install]
WantedBy=multi-user.target
UNIT
    systemctl daemon-reload
    systemctl enable tidb-disable-thp.service >/dev/null 2>&1 || true
  fi

  # 4. 内核参数
  local mem_kb; mem_kb="$(awk '/MemTotal/{print $2}' /proc/meminfo)"
  {
    echo "# 由 tidb-deploy.sh 写入"
    echo "vm.swappiness = 0"
    echo "fs.file-max = 1000000"
    echo "net.core.somaxconn = 32768"
    echo "net.ipv4.tcp_syncookies = 0"
    echo "vm.overcommit_memory = 1"
    if (( mem_kb >= 16 * 1024 * 1024 )); then echo "vm.min_free_kbytes = 1048576"; fi
  } > /etc/sysctl.d/99-tidb.conf
  sysctl -p /etc/sysctl.d/99-tidb.conf >/dev/null

  # 5. ulimit
  cat > /etc/security/limits.d/99-tidb.conf <<LIM
$P_USER soft nofile 1000000
$P_USER hard nofile 1000000
$P_USER soft stack  32768
$P_USER hard stack  32768
$P_USER soft core   unlimited
$P_USER hard core   unlimited
LIM

  # 6. SELinux
  if command -v getenforce >/dev/null 2>&1 && [[ "$(getenforce)" == "Enforcing" ]]; then
    setenforce 0 || true
    if [[ -f /etc/selinux/config ]]; then
      sed -i 's/^SELINUX=.*/SELINUX=disabled/' /etc/selinux/config
      say "已将 SELinux 设为 permissive，并在配置中设为 disabled（彻底关闭需重启）"
    fi
  fi

  # 7. 防火墙
  if systemctl is-active firewalld >/dev/null 2>&1; then
    case "$P_FIREWALL" in
      disable) systemctl disable --now firewalld >/dev/null 2>&1 || true; say "已关闭 firewalld" ;;
      open)
        for p in $P_PORTS; do firewall-cmd --permanent --add-port="$p/tcp" >/dev/null; done
        firewall-cmd --reload >/dev/null; say "已在 firewalld 放行端口: $P_PORTS" ;;
      *) ;;
    esac
  fi

  # 8. 时间同步
  if systemctl is-active ntpd >/dev/null 2>&1; then say "检测到 ntpd 在运行，保持不变"
  else
    systemctl enable --now chronyd >/dev/null 2>&1 || true
    chronyc makestep >/dev/null 2>&1 || true
  fi

  # 9. 部署用户：由你事先创建，并已配好 sudo 免密与 SSH 免密登录（前置条件检查已确认）；这里不创建用户、不改 sudoers、不写 authorized_keys
  id -u "$P_USER" >/dev/null 2>&1 || { echo "部署用户 $P_USER 不存在：请先创建（见前置条件）"; return 1; }

  # 10. 目录、sshd、CPU 调频
  mkdir -p "$P_DEPLOY_DIR" "$P_DATA_DIR"; chown "$P_USER:$P_USER" "$P_DEPLOY_DIR" "$P_DATA_DIR"
  systemctl enable --now sshd >/dev/null 2>&1 || true
  if command -v cpupower >/dev/null 2>&1; then cpupower frequency-set --governor performance >/dev/null 2>&1 || true; fi
  echo "PREP_OK"
}

emit_remote() { # emit_remote <function> ：输出「变量赋值 + 函数定义 + 调用」
  local fn="$1" role
  printf 'P_USER=%q\nP_FIREWALL=%q\nP_REPO_FIX=%q\nP_REPO_MIRROR=%q\nP_DEPLOY_DIR=%q\nP_DATA_DIR=%q\nP_OFFLINE_RPM_DIR=%q\nP_PKGS=%q\nP_PORTS=%q\n' \
    "$DEPLOY_USER" "$FIREWALL" "$REPO_FIX" "$REPO_MIRROR" "$DEPLOY_DIR" "$DATA_DIR" "$OFFLINE_RPM_DIR" \
    "numactl chrony curl tar gzip libaio openssh-server openssh-clients sudo util-linux" "${CUR_PORTS:-}"
  declare -f "$fn"
  echo "$fn"
}

# ---------------------------------------------------------------- 事实与检查 ----
declare -A F_CORES F_MEM F_GLIBC F_AVX2 F_FS F_OPTS F_FREE F_BUSY F_OSID F_OSVER F_OSNAME
collect_facts() { # collect_facts <host>
  local h="$1" line k v out
  if [[ "${TD_FAKE_FACTS:-0}" == 1 ]]; then
    F_CORES[$h]="${TD_CORES:-8}"; F_MEM[$h]="${TD_MEM_MB:-32768}"; F_GLIBC[$h]="$(detect_glibc)"; F_AVX2[$h]=1
    F_FS[$h]="${TD_FS:-xfs}"; F_OPTS[$h]="${TD_FS_OPTS:-rw,noatime}"; F_FREE[$h]="${TD_FREE_GB:-500}"; F_BUSY[$h]="${TD_BUSY:-}"
    F_OSID[$h]="$OS_ID"; F_OSVER[$h]="$OS_VER"; F_OSNAME[$h]="$OS_NAME"
    return 0
  fi
  CUR_PORTS="$(ports_for_host "$h")"
  out="$( { emit_remote remote_facts; } | rexec "$h" 2>&1 )" || { c_fail "无法连接 ${h}（以 ${DEPLOY_USER}@${h} 登录并 sudo）：$(echo "$out" | tail -n1)"; return 1; }
  while IFS='=' read -r k v; do
    case "$k" in
      CORES) F_CORES[$h]="$v";; MEM_MB) F_MEM[$h]="$v";; GLIBC) F_GLIBC[$h]="$v";; AVX2) F_AVX2[$h]="$v";;
      DATA_FS) F_FS[$h]="$v";; DATA_OPTS) F_OPTS[$h]="$v";; DATA_FREE_GB) F_FREE[$h]="$v";; PORTS_BUSY) F_BUSY[$h]="$v";;
      OS_ID) F_OSID[$h]="$v";; OS_VER) F_OSVER[$h]="$v";; OS_NAME) F_OSNAME[$h]="$v";;
    esac
  done <<< "$out"
}

# 角色最低配置（核数 内存GB）：开发测试 / 生产（来自官方软硬件要求）
req_for() { # req_for <spec> <role> -> "cores memgb"
  case "$1:$2" in
    dev:tidb) echo "8 16";; dev:pd) echo "4 8";; dev:tikv) echo "8 32";; dev:tiflash) echo "32 64";;
    prod:tidb) echo "16 48";; prod:pd) echo "8 16";; prod:tikv) echo "16 64";; prod:tiflash) echo "48 128";;
  esac
}

evaluate_host() { # evaluate_host <host>
  local h="$1" roles r need nc nm cores mem busy p conflicts=""
  roles="$(roles_of_host "$h")"; cores="${F_CORES[$h]:-0}"; mem="${F_MEM[$h]:-0}"
  info "主机 $h  角色: ${roles:-监控}  CPU ${cores} 核  内存 $((mem / 1024)) GB  系统 ${F_OSID[$h]:-?} ${F_OSVER[$h]:-?}"
  # 资源
  local n_roles; n_roles="$(count_words "$roles")"
  if (( n_roles >= 3 )); then
    if [[ "$SPEC" == prod ]]; then
      (( cores >= 16 && mem >= 65536 )) && c_ok "$h 资源满足生产混合部署（≥16 核 / 64GB）" || c_fail "$h 资源不足：生产配置混合部署需要 ≥16 核 / 64GB"
    else
      if (( cores < 4 || mem < 8192 )); then c_warn "$h 资源过低（${cores} 核 / $((mem / 1024))GB）：多角色合并部署建议至少 4 核 / 8GB，继续部署可能因内存不足而不稳定"
      elif (( cores < 8 || mem < 16384 )); then c_warn "$h 资源偏低（${cores} 核 / $((mem / 1024))GB），可运行但仅适合功能测试；建议 ≥8 核 / 16GB"
      else c_ok "$h 资源满足开发测试（${cores} 核 / $((mem / 1024))GB）"; fi
    fi
  else
    for r in $roles; do
      need="$(req_for "$SPEC" "$r")"; nc="${need% *}"; nm="${need#* }"
      if (( cores >= nc && mem >= nm * 1024 )); then c_ok "$h $r 资源满足 ${SPEC} 要求（≥${nc} 核 / ${nm}GB）"
      elif [[ "$SPEC" == prod ]] || (( cores < 2 || mem < 4096 )); then c_fail "$h $r 资源不足：${SPEC} 要求 ≥${nc} 核 / ${nm}GB，当前 ${cores} 核 / $((mem / 1024))GB"
      else c_warn "$h $r 资源低于推荐（≥${nc} 核 / ${nm}GB），当前 ${cores} 核 / $((mem / 1024))GB"; fi
    done
  fi
  # 磁盘
  local free="${F_FREE[$h]:-0}" fs="${F_FS[$h]:-unknown}" opts="${F_OPTS[$h]:-}"
  if [[ "$SPEC" == prod ]]; then (( free >= 100 )) && c_ok "$h 数据盘可用 ${free}GB" || c_fail "$h 数据盘可用仅 ${free}GB（生产要求 ≥100GB）"
  else (( free >= 100 )) && c_ok "$h 数据盘可用 ${free}GB" || { (( free >= 20 )) && c_warn "$h 数据盘可用 ${free}GB（建议 ≥100GB）" || c_fail "$h 数据盘可用仅 ${free}GB（至少 20GB）"; }; fi
  case "$fs" in
    xfs) c_ok "$h 数据目录文件系统 xfs";;
    ext4)
      if [[ "$opts" == *nodelalloc* ]]; then c_ok "$h 数据目录 ext4 已含 nodelalloc"
      elif [[ "$SPEC" == prod ]]; then c_fail "$h 数据目录是 ext4 但缺少 nodelalloc 挂载选项（tiup check 会失败）：在 /etc/fstab 加 defaults,nodelalloc,noatime 后重新挂载"
      else c_warn "$h 数据目录是 ext4 但缺少 nodelalloc，tiup check 会报 Fail；测试环境可设 ALLOW_CHECK_FAIL=true 继续"; fi ;;
    *) c_warn "$h 数据目录文件系统为 ${fs}，官方仅推荐 xfs / ext4";;
  esac
  # TiFlash 指令集
  case " $TIFLASH_LIST " in *" $h "*) [[ "${F_AVX2[$h]:-0}" == 1 ]] && c_ok "$h CPU 支持 AVX2（TiFlash 需要）" || c_fail "$h CPU 不支持 AVX2，无法部署 TiFlash";; esac
  # 端口
  busy="${F_BUSY[$h]:-}"
  if [[ -n "$busy" ]]; then for p in $busy; do conflicts="$conflicts $p"; done; c_fail "$h 以下端口已被占用:${conflicts}"; else c_ok "$h 所需端口空闲"; fi
}

# 汇总全部主机的最低 glibc（用来决定版本上限）
min_glibc_all() {
  local h g min=""
  for h in $ALL_HOSTS; do
    g="${F_GLIBC[$h]:-0}"
    if [[ -z "$min" ]] || ! ver_ge "$g" "$min"; then min="$g"; fi
  done
  echo "${min:-0}"
}

# ---------------------------------------------------------------- 前置条件：部署用户 ----
# 由你在部署前完成，脚本只检查、不代劳：所有部署机器和中控机上都有部署用户、该用户有 sudo 免密，
# 且该用户能用密钥免密 SSH 登录集群的所有机器（包括中控机自己）。
as_deploy_user() { # 以部署用户身份在本机执行一条命令串（需要 root，或当前就是该用户）
  if [[ "$(id -un)" == "$DEPLOY_USER" ]]; then bash -c "$1"
  elif [[ "$(id -u)" == 0 ]]; then runuser -l "$DEPLOY_USER" -c "$1"
  else return 126; fi
}
access_hint() {
  local t
  info "前置条件由你在部署前完成（每台部署机器和中控机都要做），然后重新运行："
  echo "  1) 创建部署用户（root 执行）：  useradd -m -s /bin/bash ${DEPLOY_USER} && passwd ${DEPLOY_USER}"
  echo "  2) 开通 sudo 免密（root 执行）：echo '${DEPLOY_USER} ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/${DEPLOY_USER} && chmod 440 /etc/sudoers.d/${DEPLOY_USER} && visudo -cf /etc/sudoers.d/${DEPLOY_USER}"
  echo "  3) 分发公钥要用口令登录：若 sshd 禁用了口令登录，先临时打开（每台机器，root 执行；已允许可跳过）："
  echo "       sed -i -E 's/^#?PasswordAuthentication .*/PasswordAuthentication yes/' /etc/ssh/sshd_config && systemctl restart sshd"
  echo "  4) 在中控机以 ${DEPLOY_USER} 身份生成密钥（sudo -iu ${DEPLOY_USER}；已有可跳过）：ssh-keygen -t rsa -b 4096 -N '' -f ${SSH_KEY}"
  echo "  5) 把公钥分发到下面每台机器，包括中控机自己（每台输入一次 ${DEPLOY_USER} 的口令）："
  for t in $ACCESS_TARGETS; do echo "       ssh-copy-id -i ${SSH_KEY}.pub -p ${SSH_PORT} ${DEPLOY_USER}@${t}"; done
  echo "  6) 复测：su - ${DEPLOY_USER} -c 'ssh -p ${SSH_PORT} ${DEPLOY_USER}@<IP> sudo -n true'  （不输入口令、不报错才算通过）"
  echo "  7) 所有机器都分发完、复测通过后，如第 3 步改过 sshd，改回禁用口令登录（每台机器，root 执行）："
  echo "       sed -i -E 's/^#?PasswordAuthentication .*/PasswordAuthentication no/' /etc/ssh/sshd_config && systemctl restart sshd"
}
ACCESS_TARGETS=""
check_deploy_user_access() {
  local t ctrl="" f0="$FAILS" h out sopts
  if [[ "${TD_FAKE_FACTS:-0}" == 1 ]]; then c_ok "部署用户 ${DEPLOY_USER} 的前置条件（测试模式：跳过实际检查）"; return 0; fi
  # 要检查的机器：全部部署主机 + 中控机（不在部署主机里时用本机主 IP；它在里面时已包含）
  ACCESS_TARGETS="$ALL_HOSTS"
  for h in $ALL_HOSTS; do is_local "$h" && ctrl="in"; done
  [[ -n "$ctrl" ]] || ACCESS_TARGETS="$ALL_HOSTS $(primary_ip)"
  # 中控机本身
  if ! id -u "$DEPLOY_USER" >/dev/null 2>&1; then
    c_fail "中控机上没有用户 ${DEPLOY_USER}：请先创建"
  else
    local rc=0; as_deploy_user 'true' >/dev/null 2>&1 || rc=$?
    if (( rc == 126 )); then
      c_fail "检查 ${DEPLOY_USER} 的 sudo 与 SSH 需要 root：请用 root（sudo ${SELF} ...）运行"
    else
      if as_deploy_user 'sudo -n true' >/dev/null 2>&1; then c_ok "中控机：${DEPLOY_USER} 有 sudo 免密"; else c_fail "中控机：${DEPLOY_USER} 没有 sudo 免密（sudo -n true 失败）"; fi
      if [[ -r "$SSH_KEY" ]]; then c_ok "中控机：${DEPLOY_USER} 的 SSH 私钥 ${SSH_KEY} 存在"; else c_fail "中控机：找不到 ${DEPLOY_USER} 的 SSH 私钥 ${SSH_KEY}（可用 SSH_KEY 指定）"; fi
      printf -v sopts -- '-p %q -i %q -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8 -o LogLevel=ERROR' "$SSH_PORT" "$SSH_KEY"
      for t in $ACCESS_TARGETS; do
        local tag=""; is_local "$t" && tag="（中控机自己）"
        out="$(as_deploy_user "ssh ${sopts} ${DEPLOY_USER}@${t} true </dev/null" 2>&1)" \
          || { c_fail "${DEPLOY_USER}@${t}${tag} 免密登录失败：$(printf '%s' "$out" | tail -n1)"; continue; }
        out="$(as_deploy_user "ssh ${sopts} ${DEPLOY_USER}@${t} 'sudo -n true' </dev/null" 2>&1)" \
          || { c_fail "${DEPLOY_USER}@${t}${tag} 可以登录，但 sudo 免密不可用：$(printf '%s' "$out" | tail -n1)"; continue; }
        c_ok "${DEPLOY_USER}@${t}${tag} 免密登录 + sudo 免密可用"
      done
    fi
  fi
  (( FAILS == f0 )) || access_hint
  return 0
}

phase_precheck() {
  step "环境检查"
  PASS=0; WARNS=0; FAILS=0
  detect_os
  info "本机系统: ${OS_NAME} ${OS_VER}（${OS_FAMILY}）"
  case "$OS_LEVEL" in
    ok) c_ok "系统在官方测试列表内";;
    compat) c_ok "系统为官方「兼容」平台（${OS_NAME} ${OS_VER}）";;
    legacy) c_warn "${OS_NAME} ${OS_VER} 上游已停止维护（CentOS Linux 8 于 2021-12、CentOS 7 于 2024-06 EOL），yum 源可能失效；建议迁移到 Rocky Linux 9 / CentOS Stream 9";;
    unverified) c_warn "系统主版本 ${OS_MAJOR} 未经本工具验证，继续有风险";;
    *) c_fail "不支持的系统: ${OS_NAME:-未知} ${OS_VER}。本工具仅支持 CentOS 7/8/9 及 Rocky / Alma / RHEL 系";;
  esac
  local h f_before="$FAILS"
  check_deploy_user_access
  if (( FAILS > f_before )); then
    echo; info "检查结果：通过 ${PASS} · 警告 ${WARNS} · 失败 ${FAILS}"
    die "前置条件未满足（部署用户 ${DEPLOY_USER}：所有机器含中控机都要有 sudo 免密，且能免密 SSH 登录）：请按上面的说明配置后重试"
  fi
  for h in $ALL_HOSTS; do collect_facts "$h" || true; done
  for h in $ALL_HOSTS; do [[ -n "${F_CORES[$h]:-}" ]] && evaluate_host "$h"; done
  # 版本与 glibc
  local g; g="$(min_glibc_all)"
  local want="$TIDB_VERSION"; [[ "$want" == auto ]] && want="$(default_series "$g")"
  local why
  if why="$(version_gate "$want" "$g")"; then c_ok "TiDB ${want} 与系统 glibc ${g} 兼容"; else c_fail "$why"; fi
  [[ "$TIDB_VERSION" == auto ]] && info "版本选择: auto → ${want} 系列（按系统 glibc ${g} 决定）"
  # 模式提示
  [[ "$DEPLOY_MODE" == single ]] && c_warn "single 模式：所有组件同机、1 个 TiKV 副本，仅用于开发测试，不具备高可用"
  echo
  info "检查结果：通过 ${PASS} · 警告 ${WARNS} · 失败 ${FAILS}"
  (( FAILS == 0 )) || die "环境检查未通过（${FAILS} 项失败），请先处理上面标 ✖ 的项目"
}

# ---------------------------------------------------------------- 阶段实现 ----
STATE_FILE=""
phase_done() { [[ "$DRY_RUN" == 1 ]] || echo "$1" >> "$STATE_FILE"; }
is_done() { [[ "$FORCE" == 1 ]] && return 1; [[ -f "$STATE_FILE" ]] && grep -qx "$1" "$STATE_FILE"; }

init_workdir() {
  WORK="${WORKROOT}/${CLUSTER_NAME}"
  STATE_FILE="${WORK}/state"
  if [[ "$DRY_RUN" != 1 ]]; then
    if ! mkdir -p "$WORK" "$LOGDIR" 2>/dev/null; then
      WORK="$(mktemp -d)"; LOGDIR="$WORK"; STATE_FILE="${WORK}/state"
      warn "无法写入 ${WORKROOT}（非 root？），改用临时目录 $WORK"
    fi
    [[ "$WORK" == "${WORKROOT}/"* ]] && chmod 755 "$WORKROOT" 2>/dev/null || true   # 防 root 的 umask 077 把上级目录建成 700
    # 711：部署用户（tiup）要按完整路径读取其中的 topology.yaml，所以其他用户需要「可进入」权限；
    # 目录里其他文件各自控制权限（root-password.txt 为 600，输出临时文件用 umask 077 创建），别人不能列目录
    chmod 711 "$WORK"
    LOG_FILE="${LOGDIR}/${CLUSTER_NAME}-$(date +%Y%m%d-%H%M%S).log"; : > "$LOG_FILE"; chmod 600 "$LOG_FILE"
    info "日志: $LOG_FILE"
  fi
}

as_tidb() { # 以部署用户的登录环境执行（PATH 含 ~/.tiup/bin）
  if command -v runuser >/dev/null 2>&1; then run runuser -l "$DEPLOY_USER" -c "$*"; else run su - "$DEPLOY_USER" -c "$*"; fi
}
as_tidb_quiet() { runuser -l "$DEPLOY_USER" -c "$*" 2>/dev/null || su - "$DEPLOY_USER" -c "$*" 2>/dev/null; }

phase_prepare() {
  step "初始化全部主机（以 ${DEPLOY_USER}@主机 登录并 sudo，幂等）"
  local h
  for h in $ALL_HOSTS; do
    CUR_PORTS="$(ports_for_host "$h")"
    if [[ "$DRY_RUN" == 1 ]]; then
      printf '%s\n' "${C_Y}[DRY]${C_0} 在 ${h} 上执行 remote_prep（swap/THP/sysctl/limits/SELinux/防火墙/chrony；以 ${DEPLOY_USER}@${h} 登录并 sudo）；可用 show-prep 查看脚本"
      continue
    fi
    info "初始化 $h ..."
    local pout
    pout="$( { emit_remote remote_prep; } | rexec "$h" 2>&1 )" || true
    printf '%s\n' "$pout" >> "$LOG_FILE"
    if ! grep -q '^PREP_OK$' <<< "$pout"; then
      printf '%s\n' "$pout" | tail -n 8 | sed 's/^/    /'
      die "主机 $h 初始化失败，详见日志 $LOG_FILE"
    fi
    ok "$h 初始化完成"
  done
  # 控制机不在部署主机里时，也需要基础依赖（curl/tar 等）和 tidb 用户
  local local_in=0; for h in $ALL_HOSTS; do is_local "$h" && local_in=1; done
  if (( local_in == 0 )) && [[ "$DRY_RUN" != 1 ]]; then
    info "控制机不在部署主机之内，仅安装基础依赖"
    local lout; CUR_PORTS=""
    lout="$( { emit_remote remote_prep; } | bash -s 2>&1 )" || true
    printf '%s\n' "$lout" >> "$LOG_FILE"
    grep -q '^PREP_OK$' <<< "$lout" || die "控制机初始化失败，详见日志 $LOG_FILE"
  fi
}

# ---------------------------------------------------------------- 离线包 / 用户 / 下载 ----
detect_arch() {
  local m="${TD_ARCH:-$(uname -m)}"
  case "$m" in x86_64|amd64) echo amd64;; aarch64|arm64) echo arm64;; *) echo "$m";; esac
}

USER_HOME=""
user_home() { local h; h="$(getent passwd "$DEPLOY_USER" 2>/dev/null | cut -d: -f6)"; echo "${h:-/home/$DEPLOY_USER}"; }

# 版本参数：auto | vX.Y | vX.Y.Z（允许省略 v）
validate_version_spec() {
  [[ "$TIDB_VERSION" =~ ^[0-9] ]] && TIDB_VERSION="v${TIDB_VERSION}"
  [[ "$TIDB_VERSION" == auto || "$TIDB_VERSION" =~ ^v[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] \
    || die "TIDB_VERSION 只能是 auto、系列（如 v8.5）或完整版本（如 v7.5.4），当前: ${TIDB_VERSION}"
  return 0
}

# 在目录里找离线包 tidb-community-server-vX.Y.Z-linux-<arch>.tar.gz
# $1 目录  $2 版本：vX.Y.Z（精确）| vX.Y（系列）| auto（任意，取最高）
OFFLINE_FOUND_SERVER=""; OFFLINE_FOUND_VERSION=""; OFFLINE_FOUND_TOOLKIT=""; OFFLINE_ALL_VERSIONS=""
find_offline_pkgs() {
  local dir="$1" want="$2" f v
  OFFLINE_FOUND_SERVER=""; OFFLINE_FOUND_VERSION=""; OFFLINE_FOUND_TOOLKIT=""; OFFLINE_ALL_VERSIONS=""
  for f in "$dir"/tidb-community-server-v*-linux-"$ARCH".tar.gz; do
    [[ -f "$f" ]] || continue
    v="$(basename "$f")"; v="${v#tidb-community-server-}"; v="${v%-linux-${ARCH}.tar.gz}"
    [[ "$v" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || continue
    OFFLINE_ALL_VERSIONS="$OFFLINE_ALL_VERSIONS $v"
    if [[ "$want" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
      [[ "$v" == "$want" ]] || continue
    elif [[ "$want" =~ ^v[0-9]+\.[0-9]+$ ]]; then
      [[ "$v" == "$want".* ]] || continue
    fi
    if [[ -z "$OFFLINE_FOUND_VERSION" ]] || ver_ge "$v" "$OFFLINE_FOUND_VERSION"; then OFFLINE_FOUND_SERVER="$f"; OFFLINE_FOUND_VERSION="$v"; fi
  done
  OFFLINE_ALL_VERSIONS="$(norm_hosts "$(printf '%s
' $OFFLINE_ALL_VERSIONS | sort -V | tr '
' ' ')")"
  if [[ -n "$OFFLINE_FOUND_VERSION" && -f "$dir/tidb-community-toolkit-${OFFLINE_FOUND_VERSION}-linux-${ARCH}.tar.gz" ]]; then
    OFFLINE_FOUND_TOOLKIT="$dir/tidb-community-toolkit-${OFFLINE_FOUND_VERSION}-linux-${ARCH}.tar.gz"
  fi
  return 0
}

validate_offline_dir() {
  [[ -n "$OFFLINE_PKG_DIR" ]] || return 0
  [[ "$OFFLINE_PKG_DIR" == /* ]] || die "离线包目录必须是完整的绝对路径（以 / 开头），当前: ${OFFLINE_PKG_DIR}"
  OFFLINE_PKG_DIR="${OFFLINE_PKG_DIR%/}"
  [[ -n "$OFFLINE_PKG_DIR" ]] || die "离线包目录不能是根目录 /"
  if [[ ! -d "$OFFLINE_PKG_DIR" ]]; then
    if [[ "$DRY_RUN" == 1 ]]; then warn "离线包目录 ${OFFLINE_PKG_DIR} 在本机不存在（dry-run 继续；正式执行前请确认）"; return 0; fi
    die "离线包目录不存在或不是目录: ${OFFLINE_PKG_DIR}"
  fi
  [[ -r "$OFFLINE_PKG_DIR" ]] || die "没有读取离线包目录的权限: ${OFFLINE_PKG_DIR}"
  return 0
}

# 从离线包目录里按所选版本找到安装包，并据此确定要部署的具体版本
resolve_offline_pkg() {
  [[ -n "$OFFLINE_PKG_DIR" ]] || return 0
  if [[ ! -d "$OFFLINE_PKG_DIR" ]]; then RESOLVED_VERSION="（取决于离线包目录内容）"; return 0; fi
  local g want="$TIDB_VERSION" why
  g="$(min_glibc_all)"
  if [[ "$want" == auto ]]; then want="$(default_series "$g")"; fi
  find_offline_pkgs "$OFFLINE_PKG_DIR" "$want"
  if [[ -z "$OFFLINE_FOUND_SERVER" ]]; then
    err "在 ${OFFLINE_PKG_DIR} 中没有找到 ${want} 对应的离线安装包：tidb-community-server-${want}*-linux-${ARCH}.tar.gz"
    if [[ -n "$OFFLINE_ALL_VERSIONS" ]]; then err "该目录里已有的版本: ${OFFLINE_ALL_VERSIONS}"; else err "该目录里没有任何 tidb-community-server-*-linux-${ARCH}.tar.gz 文件"; fi
    die "请把对应版本的离线包放进该目录（可用 ${SELF} download-offline 下载），或调整 TIDB_VERSION"
  fi
  OFFLINE_SERVER_PKG="$OFFLINE_FOUND_SERVER"
  if [[ -n "$OFFLINE_FOUND_TOOLKIT" && -z "$OFFLINE_TOOLKIT_PKG" ]]; then OFFLINE_TOOLKIT_PKG="$OFFLINE_FOUND_TOOLKIT"; fi
  RESOLVED_VERSION="$OFFLINE_FOUND_VERSION"
  why="$(version_gate "$RESOLVED_VERSION" "$g")" || die "$why"
  info "离线包目录 ${OFFLINE_PKG_DIR}：选用 TiDB ${RESOLVED_VERSION}${OFFLINE_TOOLKIT_PKG:+（含 toolkit 包）}"
  return 0
}

fetch_url() { # fetch_url <url> <输出文件>（支持断点续传）
  if [[ -n "${TD_FETCH_DIR:-}" ]]; then cp "${TD_FETCH_DIR}/$(basename "$1")" "$2" 2>/dev/null; return; fi
  curl -fL --retry 3 --retry-delay 3 --connect-timeout 15 -C - -o "$2" "$1"
}
fetch_text() { # fetch_text <url>
  if [[ -n "${TD_FETCH_DIR:-}" ]]; then cat "${TD_FETCH_DIR}/$(basename "$1")" 2>/dev/null; return; fi
  curl -fsS --connect-timeout 15 "$1"
}
target_glibc() {
  case "$1" in
    centos7|el7) echo 2.17;;
    centos8|stream8|el8|rocky8|alma8) echo 2.28;;
    stream9|rocky9|alma9|el9|centos9) echo 2.34;;
    stream10|rocky10|alma10|rhel10|el10|centos10) echo 2.39;;
    *) return 1;;
  esac
}

phase_tiup() {
  step "安装 TiUP（控制机，用户 ${DEPLOY_USER}）"
  local mode="$INSTALL_MODE"
  if [[ "$mode" == auto ]]; then
    if [[ -n "$OFFLINE_SERVER_PKG" || -n "$OFFLINE_PKG_DIR" ]]; then mode=offline; else mode=online; fi
  fi
  if [[ "$DRY_RUN" != 1 ]] && as_tidb_quiet "command -v tiup" >/dev/null; then ok "TiUP 已安装，跳过"
  elif [[ "$mode" == offline ]]; then
    if [[ "$DRY_RUN" != 1 || -n "$OFFLINE_SERVER_PKG" ]]; then
      [[ -f "$OFFLINE_SERVER_PKG" ]] || die "离线模式需要离线安装包：请设置 OFFLINE_PKG_DIR（包所在的完整目录）或 OFFLINE_SERVER_PKG（文件）。当前: ${OFFLINE_SERVER_PKG:-未找到}"
    else
      OFFLINE_SERVER_PKG="${OFFLINE_PKG_DIR}/tidb-community-server-<版本>-linux-${ARCH}.tar.gz"
    fi
    local dir="${USER_HOME}/tidb-offline"
    run mkdir -p "$dir"; run tar xzf "$OFFLINE_SERVER_PKG" -C "$dir"; run chown -R "${DEPLOY_USER}:${DEPLOY_USER}" "$dir"
    as_tidb "cd ${dir}/tidb-community-server-* && sh local_install.sh && source ~/.bash_profile"
    if [[ -n "$OFFLINE_TOOLKIT_PKG" ]]; then
      [[ "$DRY_RUN" == 1 || -f "$OFFLINE_TOOLKIT_PKG" ]] || die "OFFLINE_TOOLKIT_PKG 不存在: $OFFLINE_TOOLKIT_PKG"
      run tar xzf "$OFFLINE_TOOLKIT_PKG" -C "$dir"; run chown -R "${DEPLOY_USER}:${DEPLOY_USER}" "$dir"
      as_tidb "source ~/.bash_profile && cp -rp ${dir}/tidb-community-server-*/keys ~/.tiup/ && tiup mirror merge ${dir}/tidb-community-toolkit-*"
    fi
  else
    local url="${TIUP_INSTALL_URL:-https://tiup-mirrors.pingcap.com/install.sh}"
    info "在线安装 TiUP: $url"
    as_tidb "curl --proto '=https' --tlsv1.2 -sSf '${url}' -o /tmp/tiup-install.sh && sh /tmp/tiup-install.sh && rm -f /tmp/tiup-install.sh"
  fi
  [[ -n "$TIUP_MIRROR" ]] && as_tidb "tiup mirror set '${TIUP_MIRROR}'"
  as_tidb "tiup cluster"
  if [[ "$TIUP_UPDATE" == true && "$mode" == online ]]; then as_tidb "tiup update --self && tiup update cluster"; fi
}

list_versions() { # 输出可用版本（每行 vX.Y.Z）
  if [[ -n "${TD_TIUP_LIST_FILE:-}" ]]; then cat "$TD_TIUP_LIST_FILE"; return; fi
  as_tidb_quiet "tiup list tidb --all" | awk '$1 ~ /^v[0-9]+\.[0-9]+\.[0-9]+$/ {print $1}'
}

RESOLVED_VERSION=""
resolve_version() {
  local v="$TIDB_VERSION" g series
  g="$(min_glibc_all)"
  if [[ -n "$OFFLINE_PKG_DIR" && -n "$RESOLVED_VERSION" ]]; then info "将部署 TiDB ${RESOLVED_VERSION}（来自离线包）"; return 0; fi
  if [[ -n "$OFFLINE_SERVER_PKG" && "$(basename "$OFFLINE_SERVER_PKG")" =~ ^tidb-community-server-(v[0-9]+\.[0-9]+\.[0-9]+)-linux- ]]; then
    RESOLVED_VERSION="${BASH_REMATCH[1]}"
    local why0; why0="$(version_gate "$RESOLVED_VERSION" "$g")" || die "$why0"
    info "将部署 TiDB ${RESOLVED_VERSION}（来自离线包文件名）"; return 0
  fi
  [[ "$v" == auto ]] && v="$(default_series "$g")"
  if [[ "$v" =~ ^v?[0-9]+\.[0-9]+$ ]]; then
    series="v${v#v}"
    if [[ "$DRY_RUN" == 1 && -z "${TD_TIUP_LIST_FILE:-}" ]]; then RESOLVED_VERSION="${series}.x（实际执行时解析为该系列最新补丁版本）"; return 0; fi
    RESOLVED_VERSION="$(list_versions | latest_in_series "$series")"
    [[ -n "$RESOLVED_VERSION" ]] || die "在 TiUP 镜像里找不到 ${series} 系列的可用版本（离线模式请确认离线包版本；在线请检查网络）"
  else
    RESOLVED_VERSION="v${v#v}"
  fi
  local why
  why="$(version_gate "${RESOLVED_VERSION%%（*}" "$g")" || die "$why"
  info "将部署 TiDB ${RESOLVED_VERSION}"
}

phase_topology() {
  step "生成拓扑文件"
  # 单机模式：按内存给 TiKV 设置 block-cache，避免与同机 TiDB/PD 抢内存
  if [[ "$DEPLOY_MODE" == single && -z "$TIKV_CACHE_GB" ]]; then
    local mem="${F_MEM[$ALL_HOSTS]:-0}"
    (( mem > 0 )) && TIKV_CACHE_GB="$(mem_cache_gb "$mem")"
  fi
  TOPOLOGY="${WORK}/topology.yaml"
  if [[ "$DRY_RUN" == 1 ]]; then gen_topology | sed 's/^/    | /'; return 0; fi
  gen_topology > "$TOPOLOGY"; chmod 644 "$TOPOLOGY"
  chown "${DEPLOY_USER}:${DEPLOY_USER}" "$TOPOLOGY" 2>/dev/null || true
  ok "已生成 $TOPOLOGY"
}

# 解析 tiup cluster check 的表格输出：统计 Fail / Warn 行（第 3 列为结果）
count_check() { # 从标准输入读取，输出 "fail warn"
  awk 'BEGIN{f=0;w=0} $3=="Fail"{f++} $3=="Warn"{w++} END{print f, w}'
}

phase_check() {
  step "tiup cluster check（先自动修复，再复查）"
  local key="$SSH_KEY" auth="--user ${DEPLOY_USER} -i ${SSH_KEY}"
  as_tidb "tiup cluster check ${TOPOLOGY} --apply ${auth}" || warn "check --apply 返回非 0，继续复查"
  if [[ "$DRY_RUN" == 1 ]]; then as_tidb "tiup cluster check ${TOPOLOGY} ${auth}"; return 0; fi
  local out="${WORK}/check.out" counts f w
  runuser -l "$DEPLOY_USER" -c "tiup cluster check ${TOPOLOGY} ${auth}" > "$out" 2>&1 || true
  cat "$out" >> "$LOG_FILE"
  if grep -q '^Error:' "$out"; then   # tiup 自己报错（如读不到拓扑文件），不是检查结果；不能当成「Fail 0」放过
    grep -A2 '^Error:' "$out" | head -n 6 | sed 's/^/    /'
    die "tiup cluster check 没有跑成功（上面是 tiup 的报错，不是检查结果），详见日志 $LOG_FILE"
  fi
  counts="$(count_check < "$out")"; f="${counts% *}"; w="${counts#* }"
  info "tiup check 复查：Fail ${f} · Warn ${w}"
  if (( f > 0 )); then
    awk '$3=="Fail"' "$out" | sed 's/^/    /'
    [[ "$ALLOW_CHECK_FAIL" == true ]] || die "tiup check 仍有 ${f} 项 Fail。修复后重试；仅测试环境可在配置里设 ALLOW_CHECK_FAIL=true 继续"
    warn "ALLOW_CHECK_FAIL=true，忽略 ${f} 项 Fail 继续部署"
  fi
}

phase_deploy() {
  step "tiup cluster deploy"
  local key="$SSH_KEY" exists=0
  if [[ "$DRY_RUN" != 1 ]] && as_tidb_quiet "tiup cluster list" | awk '{print $1}' | grep -qx "$CLUSTER_NAME"; then exists=1; fi
  if (( exists )); then
    [[ "$FORCE" == 1 ]] && die "集群 ${CLUSTER_NAME} 已存在。--force 不会覆盖已有集群，如确需重建请先执行: ${SELF} destroy --config ... --confirm-name ${CLUSTER_NAME}"
    warn "集群 ${CLUSTER_NAME} 已存在，跳过 deploy"; return 0
  fi
  as_tidb "tiup cluster deploy ${CLUSTER_NAME} ${RESOLVED_VERSION%%（*} ${TOPOLOGY} --user ${DEPLOY_USER} -i ${key} -y"
}

INIT_PWD_FILE=""
parse_init_password() { sed -n "s/.*[Tt]he new password is: '\\([^']*\\)'.*/\\1/p" | head -n1; }

phase_start() {
  step "启动集群"
  local cmd="tiup cluster start ${CLUSTER_NAME}"
  [[ "$INIT_PASSWORD" == true && "$NO_INIT" != 1 ]] && cmd="$cmd --init"
  if [[ "$DRY_RUN" == 1 ]]; then as_tidb "$cmd"; return 0; fi
  local out="${WORK}/start.out"
  ( umask 077; : > "$out" )   # 输出里含随机 root 密码：先以 600 创建
  runuser -l "$DEPLOY_USER" -c "$cmd" > "$out" 2>&1 || { cat "$out" >> "$LOG_FILE"; tail -n 20 "$out"; die "集群启动失败，详见 $LOG_FILE"; }
  # 输出里含随机 root 密码：写入 0600 文件，不写进日志
  local pwd; pwd="$(parse_init_password < "$out")"
  sed "s/The new password is: '[^']*'/The new password is: '******'/" "$out" >> "$LOG_FILE"
  if [[ -n "$pwd" ]]; then
    INIT_PWD_FILE="${WORK}/root-password.txt"; ( umask 077; printf '%s\n' "$pwd" > "$INIT_PWD_FILE" )
    ok "已生成 TiDB root 随机密码，保存在 ${INIT_PWD_FILE}（权限 600）"
  fi
  rm -f "$out"
}

wait_http() { # wait_http <url> <秒>
  local url="$1" t="$2" i
  for ((i = 0; i < t; i += 3)); do curl -fsS -m 3 "$url" >/dev/null 2>&1 && return 0; sleep 3; done
  return 1
}

phase_verify() {
  step "验证"
  local pd="${PD_LIST%% *}" td="${TIDB_LIST%% *}" mon="${MON_LIST%% *}"
  if [[ "$DRY_RUN" == 1 ]]; then
    printf '%s\n' "${C_Y}[DRY]${C_0} 轮询 http://${pd}:2379/pd/api/v1/health 与 http://${td}:10080/status（最多 120 秒），再 tiup cluster display ${CLUSTER_NAME}"; return 0
  fi
  wait_http "http://${pd}:2379/pd/api/v1/health" 120 && ok "PD 健康检查通过" || warn "PD 120 秒内未就绪，请查看 tiup cluster display ${CLUSTER_NAME}"
  wait_http "http://${td}:10080/status" 120 && ok "TiDB 状态端口可访问" || warn "TiDB 120 秒内未就绪"
  as_tidb "tiup cluster display ${CLUSTER_NAME}"
  if command -v mysql >/dev/null 2>&1 && [[ -n "$INIT_PWD_FILE" && -f "$INIT_PWD_FILE" ]]; then
    MYSQL_PWD="$(cat "$INIT_PWD_FILE")" mysql -h "$td" -P 4000 -u root -e "select tidb_version()\\G" 2>&1 | head -n 4 || true
  fi
  print_summary "$pd" "$td" "$mon"
}

print_summary() {
  local pd="$1" td="$2" mon="$3"
  echo
  echo "${C_G}============ 部署完成 ============${C_0}"
  echo "集群名      : ${CLUSTER_NAME}"
  echo "TiDB 版本   : ${RESOLVED_VERSION%%（*}"
  echo "SQL 连接    : mysql -h ${td} -P 4000 -u root -p"
  [[ -n "$INIT_PWD_FILE" ]] && echo "root 密码   : 见 ${INIT_PWD_FILE}（首次登录后请立即修改并删除该文件）"
  echo "Dashboard   : http://${pd}:2379/dashboard"
  [[ -n "$mon" ]] && echo "Grafana     : http://${mon}:3000   （默认账号 admin / admin，首次登录请改密码）"
  echo "管理命令    : su - ${DEPLOY_USER} -c 'tiup cluster display ${CLUSTER_NAME}'"
  echo "拓扑 / 日志 : ${WORK}/topology.yaml , ${LOG_FILE}"
  echo
  echo "下一步：用本项目的 tidbops-fixtures 工具可在中控机上只读导出样例数据（用法见 tidbops-fixtures -h）。"
}

# ---------------------------------------------------------------- 子命令 ----
prepare_all() { # 共用：加载配置、解析主机、校验
  [[ -n "$CONFIG_FILE" ]] && load_config "$CONFIG_FILE"
  # 命令行参数优先于配置文件
  [[ -n "$CLI_USER" ]] && DEPLOY_USER="$CLI_USER"
  [[ -n "$CLI_VERSION" ]] && TIDB_VERSION="$CLI_VERSION"
  [[ -n "$CLI_OFFLINE_DIR" ]] && OFFLINE_PKG_DIR="$CLI_OFFLINE_DIR"
  [[ "$STRICT" == 1 ]] && SPEC="prod"
  [[ -n "$ARCH" ]] || ARCH="$(detect_arch)"
  [[ "$ARCH" == amd64 || "$ARCH" == arm64 ]] || die "不支持的 CPU 架构: ${ARCH}（仅支持 amd64 / arm64）"
  validate_version_spec
  detect_local_ips
  resolve_hosts; validate_hosts
  validate_offline_dir
  USER_HOME="$(user_home)"
  [[ -n "${SSH_ROOT_USER:-}${SSH_ROOT_KEY:-}${TD_ROOT_PASSWORD:-}" ]] && warn "SSH_ROOT_USER / SSH_ROOT_KEY / TD_ROOT_PASSWORD 已不再使用（已忽略）：现在统一以 ${DEPLOY_USER}@主机 登录并 sudo，部署用户须事先配置好"
  [[ -n "$SSH_KEY" ]] || SSH_KEY="${USER_HOME}/.ssh/id_rsa"
  [[ "$SSH_KEY" == /* ]] || die "SSH_KEY 必须是绝对路径: ${SSH_KEY}"
  case "$SPEC" in dev|prod) ;; *) die "SPEC 只能是 dev 或 prod";; esac
  case "$FIREWALL" in open|disable|keep) ;; *) die "FIREWALL 只能是 open / disable / keep";; esac
  case "$REPO_FIX" in skip|auto) ;; *) die "REPO_FIX 只能是 skip / auto";; esac
  case "$INSTALL_MODE" in auto|online|offline) ;; *) die "INSTALL_MODE 只能是 auto / online / offline";; esac
  init_workdir
}

need_root() { [[ "$TD_TEST" == 1 || "$(id -u)" == 0 || "$DRY_RUN" == 1 ]] || die "请使用 root 执行（或 sudo ${SELF} ...）"; }

cmd_check() { prepare_all; phase_precheck; resolve_offline_pkg; }

cmd_show_prep() { # 供审阅
  [[ -n "$CONFIG_FILE" ]] && load_config "$CONFIG_FILE"
  [[ -n "$CLI_USER" ]] && DEPLOY_USER="$CLI_USER"
  detect_local_ips; resolve_hosts 2>/dev/null || true
  CUR_PORTS="$(ports_for_host "${ALL_HOSTS%% *}")"
  emit_remote remote_prep
}

cmd_deploy() {
  need_root; prepare_all
  [[ "$DRY_RUN" == 1 ]] && warn "dry-run：只打印将要执行的内容，不做任何修改"
  echo "部署计划：集群 ${CLUSTER_NAME} · 模式 ${DEPLOY_MODE} · 版本 ${TIDB_VERSION} · 规格 ${SPEC} · 操作系统用户 ${DEPLOY_USER}"
  if [[ -n "$OFFLINE_PKG_DIR" ]]; then echo "  安装方式：离线（包目录 ${OFFLINE_PKG_DIR}，架构 ${ARCH}）"; elif [[ -n "$OFFLINE_SERVER_PKG" ]]; then echo "  安装方式：离线（${OFFLINE_SERVER_PKG}）"; else echo "  安装方式：在线（${INSTALL_MODE}）"; fi
  echo "  PD: ${PD_LIST} | TiDB: ${TIDB_LIST} | TiKV: ${TIKV_LIST}${TIFLASH_LIST:+ | TiFlash: ${TIFLASH_LIST}}${MON_LIST:+ | 监控: ${MON_LIST}}"
  echo "  将修改：swap / THP / sysctl / limits / SELinux / 防火墙(${FIREWALL}) / 时间同步（以 ${DEPLOY_USER}@主机 登录并 sudo）"
  echo "  前置条件（须已由你配置，部署前会检查）：所有机器（含中控机）有 ${DEPLOY_USER} 用户 + sudo 免密，且 ${DEPLOY_USER} 能用密钥免密 SSH 登录所有机器（含中控机自己）"
  confirm "确认开始部署？" || die "已取消"
  phase_precheck
  resolve_offline_pkg
  if ! is_done prepare;  then phase_prepare;  phase_done prepare;  else info "初始化已完成（--force 可重做）"; fi
  if ! is_done tiup;     then phase_tiup;     phase_done tiup;     else info "TiUP 已安装（--force 可重做）"; fi
  resolve_version
  phase_topology
  if ! is_done check;    then phase_check;    phase_done check;    else info "tiup check 已通过（--force 可重做）"; fi
  if ! is_done deploy;   then phase_deploy;   phase_done deploy;   else info "已 deploy（--force 可重做）"; fi
  if ! is_done start;    then phase_start;    phase_done start;    else info "已启动（--force 可重做）"; fi
  phase_verify
}

cmd_simple_tiup() { # status / start / stop
  need_root; prepare_all
  as_tidb "tiup cluster $1 ${CLUSTER_NAME}"
}

cmd_destroy() {
  need_root; prepare_all
  warn "即将销毁集群 ${CLUSTER_NAME}：停止服务并删除全部数据，不可恢复！"
  [[ "$CONFIRM_NAME" == "$CLUSTER_NAME" ]] || die "请用 --confirm-name ${CLUSTER_NAME} 明确确认要销毁的集群名（--yes 不能代替）"
  confirm "再次确认销毁 ${CLUSTER_NAME}？" || die "已取消"
  as_tidb "tiup cluster destroy ${CLUSTER_NAME} -y"
  [[ "$DRY_RUN" == 1 ]] || rm -f "$STATE_FILE"
}

cmd_offline_pack() { # 在可联网的机器上制作离线包
  local v="${1:-}"; [[ "$v" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "用法: ${SELF} offline-pack vX.Y.Z   （在可联网且已安装 tiup 的机器上执行）"
  command -v tiup >/dev/null 2>&1 || die "未找到 tiup，请先安装"
  local d="tidb-community-server-${v}-linux-amd64"
  run tiup mirror clone "$d" "$v" --os=linux --arch=amd64
  run tar czf "${d}.tar.gz" "$d"
  ok "离线包: $(pwd)/${d}.tar.gz   （拷贝到中控机，配置 OFFLINE_PKG_DIR 指向它所在的目录）"
  info "如需 toolkit（含 tiup cluster 以外的工具）请从官方下载页获取 tidb-community-toolkit 包，配置 OFFLINE_TOOLKIT_PKG"
}

cmd_download_offline() {
  local v="${DL_VERSION:-$CLI_VERSION}" dir="${DL_DIR:-$CLI_OFFLINE_DIR}" arch="${DL_ARCH:-$(detect_arch)}" g why n url dest expected actual
  [[ "$v" =~ ^[0-9] ]] && v="v${v}"
  [[ "$v" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "请用 --tidb-version 指定完整版本号，例如 v7.5.4（系列版本请先查官方 Release Notes 确认具体补丁号）"
  [[ "$arch" == amd64 || "$arch" == arm64 ]] || die "--arch 只能是 amd64 或 arm64"
  [[ "$dir" == /* ]] || die "--dir 必须是完整的绝对路径（以 / 开头），当前: ${dir:-未指定}"
  dir="${dir%/}"; [[ -n "$dir" ]] || die "下载目录不能是根目录 /"
  if [[ -n "$DL_TARGET_OS" ]]; then
    g="$(target_glibc "$DL_TARGET_OS")" || die "--target-os 只能是 centos7 | centos8 | stream8 | stream9 | stream10 | rocky8 | rocky9 | rocky10 | alma8 | alma9 | alma10 | rhel10 | el7 | el8 | el9 | el10"
    why="$(version_gate "$v" "$g")" || die "目标系统 ${DL_TARGET_OS} 无法运行 ${v}：${why}"
    info "版本 ${v} 与目标系统 ${DL_TARGET_OS}（glibc ${g}）兼容"
  elif ver_ge "$v" "$MIN_TIDB_NEEDS_NEW_GLIBC"; then
    warn "${v} 需要 glibc ≥ ${MIN_GLIBC_NEW}，不能用于 CentOS 7；目标是 CentOS 7 时请加 --target-os centos7 让脚本检查"
  fi
  local names=("tidb-community-server-${v}-linux-${arch}.tar.gz")
  if (( DL_TOOLKIT == 1 )); then names+=("tidb-community-toolkit-${v}-linux-${arch}.tar.gz"); fi
  step "下载离线安装包到 ${dir}"
  for n in "${names[@]}"; do
    url="${DL_BASE}/${n}"; dest="${dir}/${n}"; expected=""; actual=""
    if [[ "$DRY_RUN" == 1 ]]; then printf '%s\n' "${C_Y}[DRY]${C_0} 下载 ${url} → ${dest}（并用 ${url}.sha256 校验）"; continue; fi
    mkdir -p "$dir" || die "无法创建目录 ${dir}"
    expected="$(fetch_text "${url}.sha256" 2>/dev/null | awk '{print $1}' | head -n1)" || true
    if [[ -f "$dest" && -n "$expected" ]]; then
      actual="$(sha256sum "$dest" | awk '{print $1}')"
      if [[ "$actual" == "$expected" ]]; then ok "已存在且校验通过，跳过: ${dest}"; continue; fi
    fi
    info "下载 ${url}"
    fetch_url "$url" "${dest}.part" || die "下载失败: ${url}（请确认版本号存在，且本机能访问 ${DL_BASE}）"
    actual="$(sha256sum "${dest}.part" | awk '{print $1}')"
    if [[ -n "$expected" ]]; then
      if [[ "$actual" != "$expected" ]]; then rm -f "${dest}.part"; die "SHA-256 校验失败: ${n}（期望 ${expected:0:12}…，实际 ${actual:0:12}…）"; fi
      ok "SHA-256 校验通过: ${n}"
    else
      warn "未取得 ${n}.sha256，无法校验完整性；本地 sha256: ${actual}"
    fi
    mv -f "${dest}.part" "$dest"
  done
  echo
  echo "离线包目录: ${dir}"
  echo "在 deploy.conf 中设置:  OFFLINE_PKG_DIR=${dir}   TIDB_VERSION=${v}"
  echo "或命令行:              ${SELF} deploy --config deploy.conf --offline-dir ${dir} --tidb-version ${v}"
}

cmd_wizard() {
  [[ -t 0 || "$TD_TEST" == 1 ]] || die "wizard 需要交互终端"
  local out="${1:-deploy.conf}" a
  [[ -e "$out" && "$ASSUME_YES" != 1 ]] && die "$out 已存在，请换文件名或加 --yes 覆盖"
  echo "—— 一键部署配置向导（直接回车使用默认值）——"
  read -r -p "部署模式 single(单机) / cluster(多机) [single]: " a; local mode="${a:-single}"
  local conf=""; conf+="# 由 ${SELF} wizard 生成于 $(date '+%F %T')"$'\n'
  read -r -p "集群名 [tidb-test]: " a; conf+="CLUSTER_NAME=${a:-tidb-test}"$'\n'
  read -r -p "操作系统用户（运行 TiUP 与集群，可自定义，不能是 root）[tidb]: " a; conf+="DEPLOY_USER=${a:-tidb}"$'\n'
  read -r -p "TiDB 版本 auto / 系列 v7.5、v8.1、v8.5 / 完整版本 v7.5.4 [auto]: " a; conf+="TIDB_VERSION=${a:-auto}"$'\n'
  conf+="DEPLOY_MODE=${mode}"$'\n'
  if [[ "$mode" == single ]]; then
    read -r -p "本机 IP（留空自动）: " a; conf+="SINGLE_HOST=\"${a}\""$'\n'
  else
    read -r -p "PD 主机（空格或逗号分隔，建议 3 台）: " a; conf+="PD_HOSTS=\"${a}\""$'\n'
    read -r -p "TiDB 主机（建议 ≥2 台）: " a; conf+="TIDB_HOSTS=\"${a}\""$'\n'
    read -r -p "TiKV 主机（建议 ≥3 台）: " a; conf+="TIKV_HOSTS=\"${a}\""$'\n'
    read -r -p "TiFlash 主机（可空）: " a; conf+="TIFLASH_HOSTS=\"${a}\""$'\n'
    read -r -p "监控主机（空=第一台 PD）: " a; conf+="MONITOR_HOST=\"${a}\""$'\n'
    read -r -p "部署用户的 SSH 私钥绝对路径（空=该用户家目录的 ~/.ssh/id_rsa）: " a; conf+="SSH_KEY=\"${a}\""$'\n'
  fi
  read -r -p "规格 dev(开发测试) / prod(生产，不达标则中止) [dev]: " a; conf+="SPEC=${a:-dev}"$'\n'
  read -r -p "离线安装包所在的完整目录，如 /opt/tidb-pkg（空=在线安装）: " a; conf+="OFFLINE_PKG_DIR=\"${a}\""$'\n'
  read -r -p "yum 源失效时自动切换 vault 镜像？(CentOS 7/8) auto/skip [skip]: " a; conf+="REPO_FIX=${a:-skip}"$'\n'
  printf '%s' "$conf" > "$out"; chmod 600 "$out"
  ok "已写入 $out，下一步: ${SELF} deploy --config $out --dry-run"
}

usage() {
  cat <<EOF
${SELF} ${TD_VERSION} —— CentOS 7/8/9 一键部署 TiDB 集群（基于 TiUP）

用法: ${SELF} <命令> [选项]

命令:
  wizard [文件]        交互式生成配置文件（默认 deploy.conf）
  check                只做环境检查（只读）
  deploy               一键部署：检查 → 初始化 → 安装 TiUP → 拓扑 → check → deploy → start → 验证
  status|start|stop    调用 tiup cluster display / start / stop
  destroy              销毁集群（必须同时给 --confirm-name <集群名>）
  show-prep            打印将在每台主机上（经 sudo）执行的初始化脚本，供审阅
  download-offline     下载离线安装包（server，可选 toolkit）到指定目录并校验 SHA-256
  offline-pack vX.Y.Z  在联网机器上用 tiup mirror clone 制作离线安装包
  help | version

选项:
  --config FILE        配置文件（见 deploy.conf.example）
  --dry-run            只打印将要执行的内容，不做任何修改
  --yes, -y            不再询问确认
  --force              忽略已完成阶段的记录，重新执行
  --strict             按生产配置要求检查（等同 SPEC=prod）
  --no-init            启动时不生成随机 root 密码
  --confirm-name NAME  destroy 时确认集群名
  --user NAME          操作系统用户（运行 TiUP 与集群），覆盖配置文件里的 DEPLOY_USER；不能是 root
  --tidb-version V     TiDB 版本：auto | 系列 v8.5 | 完整版本 v7.5.4，覆盖配置文件
  --offline-dir DIR    离线安装包所在的完整目录（绝对路径），自动按版本查找包；设置后走离线安装

download-offline 选项:
  --tidb-version vX.Y.Z   必填，完整版本号
  --dir DIR               必填，保存目录（绝对路径）
  --with-toolkit          同时下载 toolkit 包
  --arch amd64|arm64      默认按本机
  --target-os OS          目标系统（centos7|centos8|stream8|stream9|stream10|rocky8|rocky9|rocky10|alma8|alma9|alma10|rhel10），用于检查版本兼容
  --base-url URL          下载源，默认 https://download.pingcap.org

系统与版本:
  CentOS 8 / Stream 8 / Stream 9 / Rocky / Alma / RHEL（glibc ≥ 2.28）: 默认 v8.5 LTS 系列
  CentOS 7（glibc 2.17）: 官方自 v8.4 起不再支持，仅允许 ≤ v8.3，默认 v7.5 LTS 系列；上游已 EOL，yum 源可能失效
离线安装: 把 tidb-community-server-<版本>-linux-<架构>.tar.gz（可选 tidb-community-toolkit-…）放进同一目录，
          用 --offline-dir /完整/路径 指定；--tidb-version 写系列（如 v8.5）则自动取目录里该系列的最高版本
前置条件（部署前由你完成，脚本只检查、不代劳；不满足时 check / deploy 会中止并给出修复命令）:
  所有部署机器和中控机上都要有部署用户（默认 tidb）并开通 sudo 免密；
  部署用户能用密钥免密 SSH 登录集群所有机器，包括中控机自己（私钥默认 ~/.ssh/id_rsa，可用 SSH_KEY 指定）。
  脚本用 root 运行，但登录各目标机都是 tidb@IP + sudo，不再用 root 登录，也不再支持密码登录。
EOF
}

main() {
  set_defaults
  local cmd="${1:-help}"; shift || true
  local rest=()
  while (( $# )); do
    case "$1" in
      --config) CONFIG_FILE="${2:?--config 需要文件名}"; shift 2;;
      --dry-run) DRY_RUN=1; shift;;
      --yes|-y) ASSUME_YES=1; shift;;
      --force) FORCE=1; shift;;
      --strict) STRICT=1; shift;;
      --no-init) NO_INIT=1; shift;;
      --confirm-name) CONFIRM_NAME="${2:?--confirm-name 需要集群名}"; shift 2;;
      --user) CLI_USER="${2:?--user 需要用户名}"; shift 2;;
      --tidb-version) CLI_VERSION="${2:?--tidb-version 需要版本}"; DL_VERSION="$2"; shift 2;;
      --offline-dir) CLI_OFFLINE_DIR="${2:?--offline-dir 需要目录}"; shift 2;;
      --dir) DL_DIR="${2:?--dir 需要目录}"; shift 2;;
      --arch) DL_ARCH="${2:?--arch 需要 amd64 或 arm64}"; ARCH="$2"; shift 2;;
      --with-toolkit) DL_TOOLKIT=1; shift;;
      --target-os) DL_TARGET_OS="${2:?--target-os 需要系统名}"; shift 2;;
      --base-url) DL_BASE="${2:?--base-url 需要地址}"; shift 2;;
      -h|--help) usage; exit 0;;
      *) rest+=("$1"); shift;;
    esac
  done
  case "$cmd" in
    help|-h|--help) usage;;
    version|--version) echo "${SELF} ${TD_VERSION}";;
    wizard) cmd_wizard "${rest[0]:-}";;
    check) cmd_check;;
    deploy) cmd_deploy;;
    status|start|stop) cmd_simple_tiup "$cmd";;
    destroy) cmd_destroy;;
    show-prep) cmd_show_prep;;
    offline-pack) cmd_offline_pack "${rest[0]:-}";;
    download-offline) cmd_download_offline;;
    *) usage; exit 2;;
  esac
}

if [[ "$TD_TEST" != 1 ]]; then main "$@"; fi
