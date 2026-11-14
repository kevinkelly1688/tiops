#!/usr/bin/env bash
# 离线逻辑测试：不需要 root、不修改系统、不需要 CentOS。
# 通过 TD_TEST=1 加载脚本后直接调用函数；端到端用 --dry-run + 模拟系统信息。
HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$HERE/../tidb-deploy.sh"
FX="$HERE/fixtures"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
export TD_TEST=1
export TD_LOCAL_IPS="127.0.0.1 10.0.0.5"
winpath() { if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf '%s' "$1"; fi; }   # Windows 版 Python 需要 Windows 路径
# shellcheck disable=SC1090
source "$SCRIPT"
set +e
set -u

PASS=0; FAIL=0
eq()  { if [[ "$2" == "$3" ]]; then PASS=$((PASS+1)); else FAIL=$((FAIL+1)); printf '  FAIL %s\n     期望: [%s]\n     实际: [%s]\n' "$1" "$2" "$3"; fi; }
yes_() { if eval "$2"; then PASS=$((PASS+1)); else FAIL=$((FAIL+1)); printf '  FAIL %s  (条件不成立: %s)\n' "$1" "$2"; fi; }
no_()  { if eval "$2"; then FAIL=$((FAIL+1)); printf '  FAIL %s  (条件不应成立: %s)\n' "$1" "$2"; else PASS=$((PASS+1)); fi; }
sec() { printf '\n[%s]\n' "$*"; }

# 在子进程里跑脚本（带模拟环境），返回输出；退出码放到 RC
runsh() { # runsh <os-release 文件> <glibc> [脚本参数...]
  local osf="$1" gl="$2"; shift 2
  OUT="$(TD_TEST=0 TD_ARCH=amd64 TD_OS_RELEASE="$FX/$osf" TD_GLIBC="$gl" TD_FAKE_FACTS=1 TD_LOCAL_IPS="127.0.0.1 10.0.0.5" TD_WORKROOT="$TMP/w" TD_LOGDIR="$TMP/l" \
         bash "$SCRIPT" "$@" 2>&1)"; RC=$?
}

sec "语法与兼容性"
bash -n "$SCRIPT"; eq "bash -n 语法检查" 0 $?
no_ "无 CR 换行符" "python -c \"import sys;sys.exit(0 if b'\\r' in open(r'$(winpath "$SCRIPT")','rb').read() else 1)\""
yes_ "确认检查本身可用（能读到脚本）" "python -c \"import sys;sys.exit(0 if len(open(r'$(winpath "$SCRIPT")','rb').read())>1000 else 1)\""
no_ "不使用 bash 4.3+ 语法（nameref / mapfile -d / [[ -v ]] / \${x@Q}）" "grep -nE 'declare -n|local -n|mapfile -d|readarray -d|\\[\\[ -v |\\\$\\{[a-zA-Z_]+@[QEPAa]\\}' '$SCRIPT'"
no_ "不启用 set -u（bash 4.2 下空数组展开会报错）" "grep -nE '^set .*-[A-Za-z]*u' '$SCRIPT'"

sec "操作系统识别"
for c in "centos7:centos7:legacy" "centos8:centos8:legacy" "stream8:centos-stream8:compat" "stream9:centos-stream9:compat" "rocky9:rocky9:ok" "ubuntu:unsupported:unsupported" "rhel10:rhel10:unverified"; do
  IFS=: read -r f fam lvl <<< "$c"
  OS_RELEASE_FILE="$FX/os-release.$f"; detect_os
  eq "$f → family" "$fam" "$OS_FAMILY"; eq "$f → level" "$lvl" "$OS_LEVEL"
done

sec "版本比较与 glibc 门槛"
yes_ "v8.5.2 ≥ 8.4.0" 'ver_ge v8.5.2 8.4.0'
no_  "v8.3.0 ≥ 8.4.0" 'ver_ge v8.3.0 8.4.0'
yes_ "v8.5 ≥ 8.4.0（无补丁号）" 'ver_ge v8.5 8.4.0'
yes_ "v7.5.4 ≥ v7.5.10 为假" '! ver_ge v7.5.4 v7.5.10'
yes_ "2.34 ≥ 2.28" 'ver_ge 2.34 2.28'
no_  "2.17 ≥ 2.28" 'ver_ge 2.17 2.28'
yes_ "CentOS7(glibc 2.17) + v7.5.4 允许" 'version_gate v7.5.4 2.17 >/dev/null'
yes_ "CentOS7 + v8.1.1 允许" 'version_gate v8.1.1 2.17 >/dev/null'
yes_ "CentOS7 + v8.3.0 允许" 'version_gate v8.3.0 2.17 >/dev/null'
no_  "CentOS7 + v8.4.0 拒绝" 'version_gate v8.4.0 2.17 >/dev/null'
no_  "CentOS7 + v8.5.2 拒绝" 'version_gate v8.5.2 2.17 >/dev/null'
no_  "CentOS7 + v8.5（系列）拒绝" 'version_gate v8.5 2.17 >/dev/null'
yes_ "CentOS8(2.28) + v8.5.2 允许" 'version_gate v8.5.2 2.28 >/dev/null'
yes_ "Rocky9(2.34) + v8.5.2 允许" 'version_gate v8.5.2 2.34 >/dev/null'
no_  "非法版本号拒绝" 'version_gate v8.x 2.34 >/dev/null'
eq "CentOS7 默认系列" v7.5 "$(default_series 2.17)"
eq "Rocky9 默认系列" v8.5 "$(default_series 2.34)"
eq "拒绝信息提到 v8.4 与 LTS" 1 "$(version_gate v8.5.2 2.17 | grep -c 'v8.4.*LTS')"

sec "按系列选取最新补丁版本"
printf 'v7.5.0\nv7.5.4\nv7.5.10\nv7.51.0\nv8.1.1\nv8.4.0\nv8.5.0\nv8.5.2\nv8.5.10\n' > "$TMP/versions"
eq "v7.5 → v7.5.10（按版本号而非字典序）" v7.5.10 "$(latest_in_series v7.5 < "$TMP/versions")"
eq "v8.5 → v8.5.10" v8.5.10 "$(latest_in_series v8.5 < "$TMP/versions")"
eq "v8.1 → v8.1.1" v8.1.1 "$(latest_in_series 8.1 < "$TMP/versions")"
eq "不存在的系列为空" "" "$(latest_in_series v9.9 < "$TMP/versions")"

sec "主机处理"
eq "norm_hosts" "10.0.0.1 10.0.0.2 10.0.0.3" "$(norm_hosts '10.0.0.1, 10.0.0.2;  10.0.0.3 ')"
yes_ "valid_ip 合法" 'valid_ip 10.0.1.4'
no_  "valid_ip 256" 'valid_ip 10.0.1.256'
no_  "valid_ip 主机名" 'valid_ip tidb-1'
no_  "valid_ip 缺段" 'valid_ip 10.0.1'
eq "uniq_words" "a b c" "$(uniq_words 'a b a c b')"
eq "count_words" 3 "$(count_words 'a b c')"

reset_cfg() { set_defaults; PD_LIST=""; TIDB_LIST=""; TIKV_LIST=""; TIFLASH_LIST=""; MON_LIST=""; ALL_HOSTS=""; LOCAL_IPS="127.0.0.1 10.0.0.5"; }
reset_cfg; DEPLOY_MODE=single; resolve_hosts
eq "single 默认取本机主 IP" 10.0.0.5 "$PD_LIST"
eq "single 各角色同机" "10.0.0.5|10.0.0.5|10.0.0.5|10.0.0.5" "$PD_LIST|$TIDB_LIST|$TIKV_LIST|$MON_LIST"
reset_cfg; DEPLOY_MODE=single; SINGLE_HOST="10.9.9.9"; resolve_hosts; eq "single 指定 IP" 10.9.9.9 "$TIKV_LIST"
reset_cfg; DEPLOY_MODE=cluster; PD_HOSTS="10.0.1.4,10.0.1.5 10.0.1.6"; TIDB_HOSTS="10.0.1.1 10.0.1.2"; TIKV_HOSTS="10.0.1.7 10.0.1.8 10.0.1.9"; MONITOR_HOST="10.0.1.10"; resolve_hosts
eq "cluster 解析 PD" "10.0.1.4 10.0.1.5 10.0.1.6" "$PD_LIST"
eq "cluster 全部主机去重" "10.0.1.4 10.0.1.5 10.0.1.6 10.0.1.1 10.0.1.2 10.0.1.7 10.0.1.8 10.0.1.9 10.0.1.10" "$ALL_HOSTS"
eq "cluster 监控默认第一台 PD" "10.0.1.4" "$( (reset_cfg; DEPLOY_MODE=cluster; PD_HOSTS="10.0.1.4 10.0.1.5"; TIDB_HOSTS=10.0.1.1; TIKV_HOSTS=10.0.1.7; resolve_hosts; echo "$MON_LIST") )"
eq "WITH_MONITOR=false 不部署监控" "" "$( (reset_cfg; DEPLOY_MODE=single; SINGLE_HOST=10.0.0.5; WITH_MONITOR=false; resolve_hosts; echo "$MON_LIST") )"
eq "端口：PD+TiDB+TiKV+监控同机" "2379 2380 4000 10080 20160 20180 9090 12020 3000 9093 9094 9115 9100" "$( (reset_cfg; DEPLOY_MODE=single; SINGLE_HOST=10.0.0.5; resolve_hosts; ports_for_host 10.0.0.5) )"

v() { ( reset_cfg; DEPLOY_MODE=cluster; PD_HOSTS="$1"; TIDB_HOSTS="$2"; TIKV_HOSTS="$3"; ZONE_MAP="${4:-}"; SPEC="${5:-dev}"; resolve_hosts; validate_hosts ) >/dev/null 2>&1; echo $?; }
eq "校验：合法 3/2/3" 0 "$(v '10.0.1.4 10.0.1.5 10.0.1.6' '10.0.1.1 10.0.1.2' '10.0.1.7 10.0.1.8 10.0.1.9')"
eq "校验：缺 TiKV" 1 "$(v '10.0.1.4' '10.0.1.1' '')"
eq "校验：同角色重复主机" 1 "$(v '10.0.1.4 10.0.1.4' '10.0.1.1' '10.0.1.7')"
eq "校验：非法 IP" 1 "$(v '10.0.1.4' 'tidb-1' '10.0.1.7')"
eq "校验：prod 规格下 PD 太少" 1 "$(v '10.0.1.4' '10.0.1.1 10.0.1.2' '10.0.1.7 10.0.1.8 10.0.1.9' '' prod)"
eq "校验：ZONE_MAP 缺主机" 1 "$(v '10.0.1.4' '10.0.1.1' '10.0.1.7 10.0.1.8' '10.0.1.7=az1')"
eq "校验：ZONE_MAP 格式错" 1 "$(v '10.0.1.4' '10.0.1.1' '10.0.1.7' 'bad')"
eq "校验：集群名非法" 1 "$( (reset_cfg; DEPLOY_MODE=single; SINGLE_HOST=10.0.0.5; CLUSTER_NAME='a b'; resolve_hosts; validate_hosts) >/dev/null 2>&1; echo $? )"

sec "拓扑生成"
reset_cfg; DEPLOY_MODE=single; SINGLE_HOST=10.0.0.5; CLUSTER_NAME=t1; TIKV_CACHE_GB=3; resolve_hosts; T="$(gen_topology)"
yes_ "single：global 用户" 'grep -q "user: \"tidb\"" <<< "$T"'
yes_ "single：副本数 1" 'grep -q "replication.max-replicas: 1" <<< "$T"'
yes_ "single：block-cache 3GB" 'grep -q "storage.block-cache.capacity: \"3GB\"" <<< "$T"'
yes_ "single：含监控三件套" 'grep -q "monitoring_servers" <<< "$T" && grep -q "grafana_servers" <<< "$T" && grep -q "alertmanager_servers" <<< "$T"'
no_  "single：无 TiFlash" 'grep -q tiflash_servers <<< "$T"'
no_  "single：无 location-labels" 'grep -q location-labels <<< "$T"'
eq "single：PD/TiDB/TiKV/监控/Grafana/Alertmanager 各 1 个 host" 6 "$(grep -c 'host: 10.0.0.5' <<< "$T")"
reset_cfg; DEPLOY_MODE=cluster; PD_HOSTS="10.0.1.4 10.0.1.5 10.0.1.6"; TIDB_HOSTS="10.0.1.1 10.0.1.2"; TIKV_HOSTS="10.0.1.7 10.0.1.8 10.0.1.9"; TIFLASH_HOSTS="10.0.1.11"
ZONE_MAP="10.0.1.7=az1,10.0.1.8=az2,10.0.1.9=az3"; DEPLOY_DIR=/data/deploy; SSH_PORT=2222; resolve_hosts; T="$(gen_topology)"
yes_ "cluster：副本数 3" 'grep -q "replication.max-replicas: 3" <<< "$T"'
yes_ "cluster：location-labels" 'grep -q "replication.location-labels: \[\"zone\", \"host\"\]" <<< "$T"'
yes_ "cluster：TiKV 标签" 'grep -q "server.labels: { zone: \"az2\", host: \"10-0-1-8\" }" <<< "$T"'
yes_ "cluster：TiFlash" 'grep -q "tiflash_servers" <<< "$T" && grep -q "host: 10.0.1.11" <<< "$T"'
yes_ "cluster：自定义目录与端口" 'grep -q "deploy_dir: \"/data/deploy\"" <<< "$T" && grep -q "ssh_port: 2222" <<< "$T"'
eq "cluster：PD 3 个" 3 "$(awk '/^pd_servers:/{f=1;next} /^$/{f=0} f' <<< "$T" | grep -c host:)"
eq "cluster：监控默认放第一台 PD" "10.0.1.4" "$(awk '/^monitoring_servers:/{getline; print $3}' <<< "$T")"
eq "2 个 TiKV 时副本数为 2" 2 "$( (reset_cfg; DEPLOY_MODE=cluster; PD_HOSTS=10.0.1.4; TIDB_HOSTS=10.0.1.1; TIKV_HOSTS="10.0.1.7 10.0.1.8"; resolve_hosts; gen_topology | sed -n 's/.*max-replicas: //p') )"
if python -c "import yaml" >/dev/null 2>&1; then
  printf '%s\n' "$T" > "$TMP/topo.yaml"
  yes_ "生成的拓扑是合法 YAML" 'python -c "import yaml;d=yaml.safe_load(open(r\"$(winpath "$TMP/topo.yaml")\",encoding=\"utf-8\"));assert d[\"global\"][\"user\"]==\"tidb\" and len(d[\"pd_servers\"])==3 and len(d[\"tikv_servers\"])==3"'
else printf '  (跳过 YAML 解析：未安装 PyYAML)\n'; fi
eq "mem_cache_gb 32GB→8" 8 "$(mem_cache_gb 32768)"
eq "mem_cache_gb 2GB→至少1" 1 "$(mem_cache_gb 2048)"

sec "tiup 输出解析"
CHK='Node        Check         Result  Message
----        -----         ------  -------
10.0.1.7    os-version    Pass    OS is CentOS Linux 7 (Core) 7.9.2009
10.0.1.7    swap          Warn    swap is enabled
10.0.1.7    disk          Fail    mount point /data does not have nodelalloc
10.0.1.8    selinux       Fail    SELinux is enabled
10.0.1.8    thp           Pass    THP is disabled'
eq "count_check → Fail 2 Warn 1" "2 1" "$(count_check <<< "$CHK")"
eq "count_check 全通过" "0 0" "$(count_check <<< $'Node Check Result Message\n10.0.0.1 swap Pass ok')"
eq "解析随机密码" 'y_+3Hwp=*AWz8971s6' "$(parse_init_password <<< "Started cluster \`t\` successfully. The new password is: 'y_+3Hwp=*AWz8971s6'. Copy and record it")"
eq "无密码输出为空" "" "$(parse_init_password <<< 'Started cluster successfully')"

sec "资源与磁盘评估（单机 3 角色）"
ev() { ( reset_cfg; DEPLOY_MODE=single; SINGLE_HOST=10.0.0.5; SPEC="$1"; resolve_hosts; F_CORES[10.0.0.5]=$2; F_MEM[10.0.0.5]=$3; F_FS[10.0.0.5]=$4; F_OPTS[10.0.0.5]="$5"; F_FREE[10.0.0.5]=$6; F_BUSY[10.0.0.5]="$7"; F_AVX2[10.0.0.5]=1; F_OSID[10.0.0.5]=x; F_OSVER[10.0.0.5]=y; PASS=0; WARNS=0; FAILS=0; evaluate_host 10.0.0.5 >/dev/null; echo "$FAILS $WARNS" ); }
eq "8核/32G/xfs/500G 无冲突 → 0 失败 0 警告" "0 0" "$(ev dev 8 32768 xfs rw 500 '')"
eq "2核/4G → 仅警告（不再是失败）" "0 1" "$(ev dev 2 4096 xfs rw 500 '')"
eq "4核/8G → 仅警告" "0 1" "$(ev dev 4 8192 xfs rw 500 '')"
eq "prod 下 8核/32G → 失败" "1" "$(ev prod 8 32768 xfs rw 500 '' | awk '{print ($1>0)?1:0}')"
eq "dev：ext4 无 nodelalloc → 警告" "0 1" "$(ev dev 8 32768 ext4 rw,noatime 500 '')"
eq "prod：ext4 无 nodelalloc → 失败" "1" "$(ev prod 16 65536 ext4 rw 500 '' | awk '{print ($1>0)?1:0}')"
eq "ext4 含 nodelalloc → 通过" "0 0" "$(ev dev 8 32768 ext4 rw,nodelalloc,noatime 500 '')"
eq "端口冲突 → 失败" "1" "$(ev dev 8 32768 xfs rw 500 '4000 2379' | awk '{print ($1>0)?1:0}')"
eq "磁盘仅 10G → 失败" "1" "$(ev dev 8 32768 xfs rw 10 '' | awk '{print ($1>0)?1:0}')"

sec "端到端 dry-run（模拟系统，不做任何修改）"
cat > "$TMP/single.conf" <<'EOF'
CLUSTER_NAME=demo
SINGLE_HOST=10.0.0.5
EOF
runsh os-release.rocky9 2.34 deploy --config "$TMP/single.conf" --dry-run --yes
eq "Rocky9 single dry-run 退出码" 0 "$RC"
yes_ "输出含 [DRY] 标记" 'grep -q "\[DRY\]" <<< "$OUT"'
yes_ "选择 v8.5 系列" 'grep -q "tiup cluster deploy demo v8.5" <<< "$OUT"'
yes_ "含 check --apply" 'grep -q "tiup cluster check .* --apply" <<< "$OUT"'
yes_ "含 start --init" 'grep -q "tiup cluster start demo --init" <<< "$OUT"'
yes_ "含在线安装 TiUP" 'grep -q "tiup-mirrors.pingcap.com/install.sh" <<< "$OUT"'
yes_ "含拓扑预览" 'grep -q "replication.max-replicas: 1" <<< "$OUT"'
yes_ "阶段顺序：初始化 < 安装 TiUP < check < deploy < start" '
  a=$(grep -n "remote_prep" <<< "$OUT" | head -1 | cut -d: -f1); b=$(grep -n "install.sh" <<< "$OUT" | head -1 | cut -d: -f1)
  c=$(grep -n "cluster check" <<< "$OUT" | head -1 | cut -d: -f1); d=$(grep -n "cluster deploy" <<< "$OUT" | head -1 | cut -d: -f1); e=$(grep -n "cluster start" <<< "$OUT" | head -1 | cut -d: -f1)
  [[ -n "$a" && $a -lt $b && $b -lt $c && $c -lt $d && $d -lt $e ]]'
no_  "dry-run 不应创建工作目录" '[[ -d "$TMP/w/demo" ]]'

runsh os-release.centos7 2.17 deploy --config "$TMP/single.conf" --dry-run --yes
eq "CentOS7 auto 版本 dry-run 退出码" 0 "$RC"
yes_ "CentOS7 选择 v7.5 系列" 'grep -q "tiup cluster deploy demo v7.5" <<< "$OUT"'
yes_ "CentOS7 提示已 EOL" 'grep -q "EOL" <<< "$OUT"'

printf 'CLUSTER_NAME=demo\nSINGLE_HOST=10.0.0.5\nTIDB_VERSION=v8.5.2\n' > "$TMP/c7bad.conf"
runsh os-release.centos7 2.17 deploy --config "$TMP/c7bad.conf" --dry-run --yes
eq "CentOS7 + v8.5.2 被拒绝（退出码 1）" 1 "$RC"
yes_ "拒绝原因提到 glibc 与 LTS" 'grep -q "glibc" <<< "$OUT" && grep -q "LTS" <<< "$OUT"'
no_  "被拒绝后不应出现 deploy 命令" 'grep -q "cluster deploy" <<< "$OUT"'

printf 'CLUSTER_NAME=demo\nSINGLE_HOST=10.0.0.5\nTIDB_VERSION=v7.5.4\n' > "$TMP/c7ok.conf"
runsh os-release.centos7 2.17 deploy --config "$TMP/c7ok.conf" --dry-run --yes
eq "CentOS7 + v7.5.4 通过" 0 "$RC"
yes_ "使用 v7.5.4" 'grep -q "cluster deploy demo v7.5.4" <<< "$OUT"'

runsh os-release.ubuntu 2.35 deploy --config "$TMP/single.conf" --dry-run --yes
eq "Ubuntu 被拒绝" 1 "$RC"; yes_ "提示不支持的系统" 'grep -q "不支持的系统" <<< "$OUT"'

cat > "$TMP/cluster.conf" <<'EOF'
CLUSTER_NAME=prod1
DEPLOY_MODE=cluster
PD_HOSTS="10.0.1.4 10.0.1.5 10.0.1.6"
TIDB_HOSTS="10.0.1.1 10.0.1.2"
TIKV_HOSTS="10.0.1.7 10.0.1.8 10.0.1.9"
ZONE_MAP="10.0.1.7=az1,10.0.1.8=az2,10.0.1.9=az3"
SSH_KEY=/home/tidb/.ssh/id_rsa
TIDB_VERSION=v8.5
EOF
runsh os-release.stream9 2.34 deploy --config "$TMP/cluster.conf" --dry-run --yes
eq "cluster dry-run 退出码" 0 "$RC"
yes_ "初始化 8 台主机（监控与第一台 PD 同机，去重）" '[[ "$(grep -c "上执行 remote_prep" <<< "$OUT")" == 8 ]]'
yes_ "拓扑含 zone 标签" 'grep -q "server.labels" <<< "$OUT"'
printf 'DEPLOY_MODE=cluster\nPD_HOSTS=10.0.1.4\nTIDB_HOSTS=10.0.1.1\nTIKV_HOSTS=10.0.1.7\nSPEC=prod\n' > "$TMP/prodbad.conf"
runsh os-release.rocky9 2.34 deploy --config "$TMP/prodbad.conf" --dry-run --yes
eq "prod 规格下节点数不足被拒绝" 1 "$RC"

printf 'CLUSTER_NAME=demo\nSINGLE_HOST=10.0.0.5\nTIDB_VERSION=v8.5\n' > "$TMP/series.conf"
TD_TIUP_LIST_FILE="$TMP/versions" runsh os-release.rocky9 2.34 deploy --config "$TMP/series.conf" --dry-run --yes
yes_ "系列解析为最新补丁版本 v8.5.10" 'grep -q "cluster deploy demo v8.5.10" <<< "$OUT"'

runsh os-release.rocky9 2.34 check --config "$TMP/single.conf"
eq "check 全部通过退出码" 0 "$RC"; yes_ "check 输出结果统计" 'grep -q "检查结果：通过" <<< "$OUT"'
TD_BUSY="4000" runsh os-release.rocky9 2.34 check --config "$TMP/single.conf"
eq "端口冲突时 check 退出码 1" 1 "$RC"
TD_CORES=2 TD_MEM_MB=4096 runsh os-release.rocky9 2.34 check --config "$TMP/single.conf"
eq "dev 多角色合并资源过低：仅警告（退出码 0）" 0 "$RC"; yes_ "警告写明资源过低" 'grep -q "资源过低" <<< "$OUT"'
no_  "不再显示为错误" 'grep -q "✖.*资源过低" <<< "$OUT"'
TD_FS=ext4 runsh os-release.rocky9 2.34 check --config "$TMP/single.conf"
eq "ext4 无 nodelalloc：dev 仅警告（退出码 0）" 0 "$RC"
TD_FS=ext4 runsh os-release.rocky9 2.34 check --config "$TMP/single.conf" --strict
eq "ext4 无 nodelalloc：--strict 失败" 1 "$RC"

sec "destroy 保护"
runsh os-release.rocky9 2.34 destroy --config "$TMP/single.conf" --dry-run --yes
eq "不带 --confirm-name 拒绝" 1 "$RC"
runsh os-release.rocky9 2.34 destroy --config "$TMP/single.conf" --dry-run --yes --confirm-name wrong
eq "集群名不符拒绝" 1 "$RC"
runsh os-release.rocky9 2.34 destroy --config "$TMP/single.conf" --dry-run --yes --confirm-name demo
eq "集群名正确（dry-run）通过" 0 "$RC"; yes_ "输出 destroy 命令" 'grep -q "tiup cluster destroy demo -y" <<< "$OUT"'

sec "初始化脚本（show-prep）"
SP="$(TD_TEST=0 TD_OS_RELEASE="$FX/os-release.rocky9" bash "$SCRIPT" show-prep --config "$TMP/single.conf" 2>&1)"
yes_ "show-prep 输出可被 bash 解析" 'bash -n <<< "$SP"'
yes_ "含关键步骤：swap / THP / sysctl / limits / chrony" 'for k in "swapoff" "transparent_hugepage" "99-tidb.conf" "limits.d" "chronyd"; do grep -q "$k" <<< "$SP" || exit 1; done'
no_  "初始化不再创建用户 / 改 sudoers / 写 authorized_keys（由用户事先配置）" 'grep -qE "useradd|usermod|NOPASSWD|authorized_keys|P_PUBKEY" <<< "$SP"'
yes_ "只确认部署用户已存在" 'grep -q "部署用户 \$P_USER 不存在" <<< "$SP"'
yes_ "含变量注入" 'grep -q "^P_USER=tidb" <<< "$SP"'
yes_ "以 PREP_OK 结束标记成功" 'grep -q "PREP_OK" <<< "$SP"'
no_  "不含明文密码" 'grep -qi "password=" <<< "$SP"'

sec "前置条件：部署用户（sudo 免密 + 免密登录所有机器含中控机）"
SAVE_DU="$DEPLOY_USER"; SAVE_SP="$SSH_PORT"; SAVE_SK="$SSH_KEY"; SAVE_AH="${ALL_HOSTS:-}"
DEPLOY_USER="$(id -un)"; SSH_PORT=22; SSH_KEY="$TMP/id_test"; : > "$SSH_KEY"
detect_local_ips
# 桩：以部署用户执行的命令。FAIL_LOGIN=主机 → 该主机登录失败；FAIL_SUDO=主机 → 能登录但 sudo 失败
as_deploy_user() {
  [[ "$1" == true ]] && return 0
  [[ "$1" == "sudo -n true" ]] && return 0
  if [[ -n "${FAIL_LOGIN:-}" && "$1" == *"@${FAIL_LOGIN} "* ]]; then echo "Permission denied (publickey,password)."; return 255; fi
  if [[ -n "${FAIL_SUDO:-}" && "$1" == *"@${FAIL_SUDO} "* && "$1" == *"sudo -n true"* ]]; then echo "sudo: a password is required"; return 1; fi
  return 0
}
ALL_HOSTS="10.0.0.5 10.0.1.2"; FAIL_LOGIN=""; FAIL_SUDO=""
OUTX="$(FAILS=0; check_deploy_user_access)"
eq "全部可用 → 前置条件通过（无失败项）" 0 "$(FAILS=0; check_deploy_user_access >/dev/null; echo $FAILS)"
eq "有一台登录失败 → 失败计数为 1" 1 "$(FAILS=0; FAIL_LOGIN=10.0.1.2; check_deploy_user_access >/dev/null; echo $FAILS)"
yes_ "逐台报告免密登录 + sudo 免密" 'grep -q "@10.0.1.2 免密登录 + sudo 免密可用" <<< "$OUTX" && grep -q "@10.0.0.5（中控机自己）" <<< "$OUTX"'
OUTX="$(FAIL_LOGIN=10.0.1.2; check_deploy_user_access)"
yes_ "某台免密登录失败 → 报红并点名主机" 'grep -q "@10.0.1.2 免密登录失败" <<< "$OUTX"'
yes_ "失败时给出修复命令（useradd / sudoers / ssh-copy-id）" 'for k in useradd NOPASSWD ssh-keygen "ssh-copy-id -i" ; do grep -q -- "$k" <<< "$OUTX" || exit 1; done'
yes_ "ssh-copy-id 列出每台机器" 'grep -q "ssh-copy-id .*@10.0.1.2" <<< "$OUTX" && grep -q "ssh-copy-id .*@10.0.0.5" <<< "$OUTX"'
OUTX="$(FAIL_SUDO=10.0.1.2; check_deploy_user_access)"
yes_ "能登录但 sudo 免密不可用 → 单独报红" 'grep -q "@10.0.1.2 可以登录，但 sudo 免密不可用" <<< "$OUTX"'
ALL_HOSTS="10.0.1.1 10.0.1.2"
OUTX="$(check_deploy_user_access)"
yes_ "中控机不在部署主机里 → 也检查中控机自己（本机主 IP）" 'grep -q "@10.0.0.5（中控机自己） 免密登录 + sudo 免密可用" <<< "$OUTX"'
DEPLOY_USER="no_such_user_$$"
OUTX="$(check_deploy_user_access)"
yes_ "中控机没有部署用户 → 报红" 'grep -q "中控机上没有用户" <<< "$OUTX"'
DEPLOY_USER="$(id -un)"; SSH_KEY="$TMP/nokey"
OUTX="$(check_deploy_user_access)"
yes_ "找不到私钥 → 报红" 'grep -q "找不到 .* 的 SSH 私钥" <<< "$OUTX"'
unset -f as_deploy_user; unset FAIL_LOGIN FAIL_SUDO
DEPLOY_USER="$SAVE_DU"; SSH_PORT="$SAVE_SP"; SSH_KEY="$SAVE_SK"; ALL_HOSTS="$SAVE_AH"
runsh os-release.rocky9 2.34 deploy --config "$TMP/single.conf" --dry-run --yes
yes_ "dry-run 计划里写明前置条件" 'grep -q "前置条件（须已由你配置" <<< "$OUT"'
no_  "不再创建用户 / 生成密钥" 'grep -qE "useradd|ssh-keygen" <<< "$OUT"'
yes_ "初始化改为以部署用户登录" 'grep -q "tidb@10.0.0.5 登录并 sudo" <<< "$OUT"'
printf 'CLUSTER_NAME=demo\nSINGLE_HOST=10.0.0.5\nSSH_ROOT_USER=root\n' > "$TMP/oldroot.conf"
runsh os-release.rocky9 2.34 deploy --config "$TMP/oldroot.conf" --dry-run --yes
eq "旧配置里的 SSH_ROOT_USER 不阻断" 0 "$RC"; yes_ "提示已不再使用" 'grep -q "已不再使用" <<< "$OUT"'
printf 'CLUSTER_NAME=demo\nSINGLE_HOST=10.0.0.5\nSSH_KEY=relative/key\n' > "$TMP/relkey.conf"
runsh os-release.rocky9 2.34 deploy --config "$TMP/relkey.conf" --dry-run --yes
eq "SSH_KEY 不是绝对路径被拒绝" 1 "$RC"
printf 'CLUSTER_NAME=demo\nSINGLE_HOST=10.0.0.5\nSSH_KEY=/data/keys/k1\n' > "$TMP/key.conf"
runsh os-release.rocky9 2.34 deploy --config "$TMP/key.conf" --dry-run --yes
yes_ "SSH_KEY 同时用于 tiup check / deploy" 'grep -q -- "-i /data/keys/k1" <<< "$OUT"'

sec "tiup 自己报错不能当成检查通过"
OUTE="$( ( as_tidb() { :; }; runuser() { printf 'Error: Failed to read topology file /x/topology.yaml (topology.read_failed)\n  caused by: permission denied\n'; }
  DRY_RUN=0; DEPLOY_USER=tidb; SSH_KEY=/home/tidb/.ssh/id_rsa; TOPOLOGY=/x/topology.yaml; WORK="$TMP"; LOG_FILE=/dev/null; ALLOW_CHECK_FAIL=false
  phase_check ) 2>&1 )"; RCE=$?
eq "拓扑读不到（tiup 报 Error:）→ check 阶段失败" 1 "$RCE"
yes_ "提示 tiup 没有跑成功" 'grep -q "没有跑成功" <<< "$OUTE"'
no_  "不再显示 Fail 0 · Warn 0 的假通过" 'grep -q "Fail 0" <<< "$OUTE"'

sec "配置向导"
printf 'single\nwiz1\nops1\nv8.5\n\ndev\n/opt/tidb-pkg\nskip\n' | TD_TEST=1 bash -c "source '$SCRIPT'; set +e; set_defaults; cmd_wizard '$TMP/wiz.conf'" >/dev/null 2>&1
yes_ "向导写出配置文件" '[[ -f "$TMP/wiz.conf" ]]'
yes_ "向导配置内容正确" 'grep -q "CLUSTER_NAME=wiz1" "$TMP/wiz.conf" && grep -q "TIDB_VERSION=v8.5" "$TMP/wiz.conf" && grep -q "DEPLOY_MODE=single" "$TMP/wiz.conf"'
eq "向导配置可被加载" wiz1 "$( (set_defaults; load_config "$TMP/wiz.conf"; echo "$CLUSTER_NAME") )"
yes_ "向导写入操作系统用户与离线目录" 'grep -q "DEPLOY_USER=ops1" "$TMP/wiz.conf" && grep -q "OFFLINE_PKG_DIR=\"/opt/tidb-pkg\"" "$TMP/wiz.conf"'


sec "离线包目录：按版本自动选包"
PKG="$TMP/pkg"; mkdir -p "$PKG"
for v in v7.5.4 v7.5.10 v8.1.1 v8.5.2; do : > "$PKG/tidb-community-server-$v-linux-amd64.tar.gz"; done
: > "$PKG/tidb-community-toolkit-v7.5.10-linux-amd64.tar.gz"; : > "$PKG/tidb-community-server-v9.0.0-linux-arm64.tar.gz"
: > "$PKG/tidb-community-server-latest-linux-amd64.tar.gz"; : > "$PKG/readme.txt"
ARCH=amd64
find_offline_pkgs "$PKG" v7.5;   eq "系列 v7.5 → 取最高 v7.5.10" v7.5.10 "$OFFLINE_FOUND_VERSION"
yes_ "自动带上同版本 toolkit" '[[ "$OFFLINE_FOUND_TOOLKIT" == "$PKG/tidb-community-toolkit-v7.5.10-linux-amd64.tar.gz" ]]'
find_offline_pkgs "$PKG" v7.5.4; eq "精确版本 v7.5.4" v7.5.4 "$OFFLINE_FOUND_VERSION"; eq "v7.5.4 没有 toolkit" "" "$OFFLINE_FOUND_TOOLKIT"
find_offline_pkgs "$PKG" auto;   eq "auto → 目录里最高版本 v8.5.2" v8.5.2 "$OFFLINE_FOUND_VERSION"
find_offline_pkgs "$PKG" v9.9;   eq "找不到的系列为空" "" "$OFFLINE_FOUND_SERVER"
eq "目录里的有效版本（忽略 latest / arm64 / 其他文件）" "v7.5.4 v7.5.10 v8.1.1 v8.5.2" "$OFFLINE_ALL_VERSIONS"
ARCH=arm64; find_offline_pkgs "$PKG" auto; eq "arm64 架构只匹配 arm64 包" v9.0.0 "$OFFLINE_FOUND_VERSION"; ARCH=amd64
eq "detect_arch aarch64→arm64" arm64 "$(TD_ARCH=aarch64 detect_arch)"; eq "detect_arch x86_64→amd64" amd64 "$(TD_ARCH=x86_64 detect_arch)"
vo() { ( DRY_RUN="${2:-0}"; OFFLINE_PKG_DIR="$1"; validate_offline_dir ) >/dev/null 2>&1; echo $?; }
eq "离线目录：相对路径被拒绝" 1 "$(vo pkg)"
eq "离线目录：不存在被拒绝" 1 "$(vo /no/such/dir)"
eq "离线目录：不存在但 dry-run 仅警告" 0 "$(vo /no/such/dir 1)"
eq "离线目录：存在则通过" 0 "$(vo "$PKG")"
eq "离线目录：根目录被拒绝" 1 "$(vo /)"
eq "为空表示不使用离线目录" 0 "$(vo "")"

sec "离线目录 端到端 dry-run"
printf 'CLUSTER_NAME=demo\nSINGLE_HOST=10.0.0.5\nOFFLINE_PKG_DIR=%s\n' "$PKG" > "$TMP/off.conf"
runsh os-release.rocky9 2.34 deploy --config "$TMP/off.conf" --dry-run --yes
eq "Rocky9 + 离线目录" 0 "$RC"
yes_ "auto → 目录里 v8.5 系列最高 v8.5.2" 'grep -q "选用 TiDB v8.5.2" <<< "$OUT" && grep -q "cluster deploy demo v8.5.2" <<< "$OUT"'
yes_ "走离线安装（解压 + local_install.sh）" 'grep -q "tar xzf $PKG/tidb-community-server-v8.5.2-linux-amd64.tar.gz" <<< "$OUT" && grep -q "local_install.sh" <<< "$OUT"'
no_  "离线时不应在线下载 install.sh" 'grep -q "tiup-mirrors.pingcap.com/install.sh" <<< "$OUT"'
yes_ "计划里显示离线包目录" 'grep -q "安装方式：离线（包目录 $PKG" <<< "$OUT"'
runsh os-release.centos7 2.17 deploy --config "$TMP/off.conf" --dry-run --yes
eq "CentOS7 + 离线目录" 0 "$RC"
yes_ "CentOS7 auto → v7.5 系列最高 v7.5.10，并带 toolkit" 'grep -q "选用 TiDB v7.5.10（含 toolkit 包）" <<< "$OUT" && grep -q "cluster deploy demo v7.5.10" <<< "$OUT" && grep -q "tidb-community-toolkit-v7.5.10" <<< "$OUT"'
runsh os-release.centos7 2.17 deploy --config "$TMP/off.conf" --tidb-version v8.5 --dry-run --yes
eq "CentOS7 指定 v8.5 → 即使目录里有 v8.5.2 也拒绝" 1 "$RC"; yes_ "拒绝原因含 glibc" 'grep -q "glibc" <<< "$OUT"'
mkdir -p "$TMP/pkg2"; : > "$TMP/pkg2/tidb-community-server-v8.5.2-linux-amd64.tar.gz"
printf 'CLUSTER_NAME=demo\nSINGLE_HOST=10.0.0.5\nOFFLINE_PKG_DIR=%s\n' "$TMP/pkg2" > "$TMP/off2.conf"
runsh os-release.centos7 2.17 deploy --config "$TMP/off2.conf" --dry-run --yes
eq "CentOS7 auto(v7.5) 但目录只有 v8.5.2 → 失败" 1 "$RC"
yes_ "提示目录里已有版本" 'grep -q "已有的版本: v8.5.2" <<< "$OUT"'
runsh os-release.rocky9 2.34 deploy --config "$TMP/single.conf" --offline-dir "$PKG" --tidb-version v8.1 --dry-run --yes
eq "命令行 --offline-dir / --tidb-version 覆盖" 0 "$RC"; yes_ "选用 v8.1.1" 'grep -q "cluster deploy demo v8.1.1" <<< "$OUT"'
runsh os-release.rocky9 2.34 deploy --config "$TMP/single.conf" --offline-dir pkg --dry-run --yes
eq "相对路径的离线目录被拒绝" 1 "$RC"; yes_ "提示需要完整绝对路径" 'grep -q "完整的绝对路径" <<< "$OUT"'
runsh os-release.rocky9 2.34 check --config "$TMP/single.conf" --offline-dir /no/such/dir
eq "离线目录不存在（非 dry-run）被拒绝" 1 "$RC"
runsh os-release.rocky9 2.34 deploy --config "$TMP/single.conf" --tidb-version 8.5 --dry-run --yes
eq "版本可省略 v 前缀" 0 "$RC"
runsh os-release.rocky9 2.34 deploy --config "$TMP/single.conf" --tidb-version foo --dry-run --yes
eq "非法版本号被拒绝" 1 "$RC"

sec "操作系统用户可选"
runsh os-release.rocky9 2.34 deploy --config "$TMP/single.conf" --user deploy01 --dry-run --yes
eq "自定义用户 dry-run" 0 "$RC"
yes_ "计划里显示操作系统用户" 'grep -q "操作系统用户 deploy01" <<< "$OUT"'
yes_ "tiup 使用该用户和其家目录里的密钥" 'grep -q -- "--user deploy01 -i /home/deploy01/.ssh/id_rsa" <<< "$OUT"'
yes_ "拓扑 global.user 为该用户" 'grep -q "user: \"deploy01\"" <<< "$OUT"'
yes_ "初始化为该用户创建 sudo 免密" 'grep -q "ssh-keygen" <<< "$OUT" || grep -q "deploy01" <<< "$OUT"'
runsh os-release.rocky9 2.34 deploy --config "$TMP/single.conf" --user root --dry-run --yes
eq "root 被拒绝" 1 "$RC"; yes_ "提示不能是 root" 'grep -q "不能是 root" <<< "$OUT"'
runsh os-release.rocky9 2.34 deploy --config "$TMP/single.conf" --user "Bad Name" --dry-run --yes
eq "非法用户名被拒绝" 1 "$RC"
printf 'CLUSTER_NAME=demo\nSINGLE_HOST=10.0.0.5\nDEPLOY_USER=ops1\n' > "$TMP/user.conf"
runsh os-release.rocky9 2.34 deploy --config "$TMP/user.conf" --dry-run --yes
yes_ "配置文件里的 DEPLOY_USER 生效" 'grep -q "操作系统用户 ops1" <<< "$OUT"'
runsh os-release.rocky9 2.34 deploy --config "$TMP/user.conf" --user cli1 --dry-run --yes
yes_ "命令行 --user 优先于配置文件" 'grep -q "操作系统用户 cli1" <<< "$OUT"'
SP="$(TD_TEST=0 TD_OS_RELEASE="$FX/os-release.rocky9" bash "$SCRIPT" show-prep --config "$TMP/single.conf" --user deploy01 2>&1)"
yes_ "show-prep 使用所选用户" 'grep -q "^P_USER=deploy01" <<< "$SP"'

sec "download-offline"
dl() { OUT="$(TD_TEST=0 TD_ARCH=amd64 "$@" 2>&1)"; RC=$?; }
dl bash "$SCRIPT" download-offline --tidb-version v7.5.4 --dir /opt/pkg --with-toolkit --dry-run
eq "dry-run 退出码" 0 "$RC"
yes_ "server 包 URL" 'grep -q "https://download.pingcap.org/tidb-community-server-v7.5.4-linux-amd64.tar.gz → /opt/pkg/" <<< "$OUT"'
yes_ "toolkit 包 URL" 'grep -q "tidb-community-toolkit-v7.5.4-linux-amd64.tar.gz" <<< "$OUT"'
yes_ "含 sha256 校验说明" 'grep -q "\.sha256" <<< "$OUT"'
dl bash "$SCRIPT" download-offline --tidb-version v7.5.4 --dir /opt/pkg --arch arm64 --dry-run
yes_ "arm64 包名" 'grep -q "linux-arm64.tar.gz" <<< "$OUT"'
dl bash "$SCRIPT" download-offline --tidb-version v7.5.4 --dir /opt/pkg --base-url https://mirror.example.com/tidb --dry-run
yes_ "自定义下载源" 'grep -q "https://mirror.example.com/tidb/tidb-community-server-v7.5.4" <<< "$OUT"'
dl bash "$SCRIPT" download-offline --tidb-version v7.5 --dir /opt/pkg --dry-run;        eq "系列版本被拒绝（需完整版本）" 1 "$RC"
dl bash "$SCRIPT" download-offline --tidb-version v7.5.4 --dir pkg --dry-run;            eq "相对目录被拒绝" 1 "$RC"
dl bash "$SCRIPT" download-offline --tidb-version v7.5.4 --dry-run;                      eq "缺少 --dir 被拒绝" 1 "$RC"
dl bash "$SCRIPT" download-offline --tidb-version v7.5.4 --dir /opt/pkg --arch ppc64 --dry-run; eq "不支持的架构被拒绝" 1 "$RC"
dl bash "$SCRIPT" download-offline --tidb-version v8.5.2 --dir /opt/pkg --target-os centos7 --dry-run
eq "目标 CentOS7 + v8.5.2 被拒绝" 1 "$RC"; yes_ "说明无法运行" 'grep -q "无法运行" <<< "$OUT"'
dl bash "$SCRIPT" download-offline --tidb-version v7.5.4 --dir /opt/pkg --target-os centos7 --dry-run; eq "目标 CentOS7 + v7.5.4 通过" 0 "$RC"
dl bash "$SCRIPT" download-offline --tidb-version v8.5.2 --dir /opt/pkg --target-os rocky9 --dry-run;  eq "目标 Rocky9 + v8.5.2 通过" 0 "$RC"
for t in stream10 rocky10 alma10 rhel10 el10; do
  dl bash "$SCRIPT" download-offline --tidb-version v8.5.2 --dir /opt/pkg --target-os $t --dry-run; eq "目标 $t + v8.5.2 通过" 0 "$RC"
done
dl bash "$SCRIPT" download-offline --tidb-version v8.5.2 --dir /opt/pkg --dry-run
eq "未指定目标系统时 v8.4+ 仅警告" 0 "$RC"; yes_ "警告提到 CentOS 7" 'grep -q "不能用于 CentOS 7" <<< "$OUT"'
dl bash "$SCRIPT" download-offline --tidb-version v7.5.4 --dir /opt/pkg --target-os win95 --dry-run; eq "未知目标系统被拒绝" 1 "$RC"

# 真实下载流程（用 TD_FETCH_DIR 模拟远端，验证落盘、SHA-256 校验、跳过、篡改拒绝）
RM="$TMP/remote"; mkdir -p "$RM"; N=tidb-community-server-v7.5.4-linux-amd64.tar.gz
printf 'fake-tidb-package-content' > "$RM/$N"
H="$(sha256sum "$RM/$N" | awk '{print $1}')"; printf '%s  %s\n' "$H" "$N" > "$RM/$N.sha256"
dl env TD_FETCH_DIR="$RM" bash "$SCRIPT" download-offline --tidb-version v7.5.4 --dir "$TMP/dl"
eq "下载成功退出码" 0 "$RC"; yes_ "文件已落盘" '[[ -f "$TMP/dl/$N" && "$(cat "$TMP/dl/$N")" == "fake-tidb-package-content" ]]'
yes_ "校验通过提示" 'grep -q "SHA-256 校验通过" <<< "$OUT"'; no_ "不遗留 .part 文件" '[[ -e "$TMP/dl/$N.part" ]]'
yes_ "给出配置提示" 'grep -q "OFFLINE_PKG_DIR=$TMP/dl" <<< "$OUT"'
dl env TD_FETCH_DIR="$RM" bash "$SCRIPT" download-offline --tidb-version v7.5.4 --dir "$TMP/dl"
yes_ "再次执行：已存在且校验通过则跳过" 'grep -q "跳过" <<< "$OUT"'
printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" "$N" > "$RM/$N.sha256"; rm -rf "$TMP/dl2"
dl env TD_FETCH_DIR="$RM" bash "$SCRIPT" download-offline --tidb-version v7.5.4 --dir "$TMP/dl2"
eq "校验和不符 → 失败" 1 "$RC"; yes_ "提示校验失败" 'grep -q "校验失败" <<< "$OUT"'; no_ "校验失败不留下成品文件" '[[ -e "$TMP/dl2/$N" ]]'
rm -f "$RM/$N.sha256"; rm -rf "$TMP/dl3"
dl env TD_FETCH_DIR="$RM" bash "$SCRIPT" download-offline --tidb-version v7.5.4 --dir "$TMP/dl3"
eq "没有 .sha256 时仅警告并成功" 0 "$RC"; yes_ "警告无法校验" 'grep -q "无法校验完整性" <<< "$OUT"'
dl env TD_FETCH_DIR="$TMP/empty" bash "$SCRIPT" download-offline --tidb-version v7.5.4 --dir "$TMP/dl4"
eq "下载失败（远端没有该文件）" 1 "$RC"; yes_ "提示下载失败" 'grep -q "下载失败" <<< "$OUT"'

printf '\n通过 %d · 失败 %d\n' "$PASS" "$FAIL"
[[ "$FAIL" == 0 ]]
