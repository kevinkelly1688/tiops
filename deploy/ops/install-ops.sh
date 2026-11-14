#!/usr/bin/env bash
# 注意：新版本请使用发布包内的 install.sh（Go 实现，支持升级与回滚）；本脚本仅作遗留参考保留。
# 在 tiup 中控机上安装 TiDB Ops 平台（特权分离部署，A3 §3.2）。
#   Web 进程：用户 tidbops（非 root、无登录 shell）
#   受限 exec 入口：sudo -u tidb /opt/tidbops/bin/tidbops-server exec --stdin（sudoers 只放行这一条）
# 用法：sudo ./install-ops.sh --bin ./tidbops-server-linux-amd64 --web ./web --cluster prod-tidb-01 --env prod [--tidb-user tidb] [--dry-run] [--start]
set -euo pipefail

BIN=""; WEB=""; CLUSTER=""; ENVN="prod"; TIDB_USER="tidb"; DRY=0; START=0
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { echo "错误: $*" >&2; exit 1; }
run() { if [ "$DRY" = 1 ]; then echo "[dry-run] $*"; else eval "$@"; fi; }

while [ $# -gt 0 ]; do
  case "$1" in
    --bin) BIN="$2"; shift 2;;
    --web) WEB="$2"; shift 2;;
    --cluster) CLUSTER="$2"; shift 2;;
    --env) ENVN="$2"; shift 2;;
    --tidb-user) TIDB_USER="$2"; shift 2;;
    --dry-run) DRY=1; shift;;
    --start) START=1; shift;;
    -h|--help) sed -n '2,6p' "$0"; exit 0;;
    *) die "未知参数 $1";;
  esac
done

[ -n "$BIN" ] && [ -f "$BIN" ] || die "需要 --bin 指向 Linux 版 tidbops-server"
[ -n "$WEB" ] && [ -f "$WEB/index.html" ] || die "需要 --web 指向前端目录（含 index.html）"
[[ "$CLUSTER" =~ ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$ ]] || die "需要合法的 --cluster 集群名"
case "$ENVN" in test|staging|prod) ;; *) die "--env 只能是 test|staging|prod";; esac
[[ "$TIDB_USER" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || die "tidb 用户名不合法"
if [ "$DRY" = 0 ]; then
  [ "$(id -u)" = 0 ] || die "需要 root（或加 --dry-run 预览）"
  id "$TIDB_USER" >/dev/null 2>&1 || die "用户 $TIDB_USER 不存在（tiup 部署用户）"
  command -v visudo >/dev/null || die "缺少 visudo"
fi

echo "== 1/6 创建运行账号 tidbops =="
run "id tidbops >/dev/null 2>&1 || useradd --system --home-dir /var/lib/tidbops --shell /sbin/nologin tidbops"

echo "== 2/6 目录与文件 =="
run "install -d -m 0755 /opt/tidbops/bin /opt/tidbops/web"
run "install -d -m 0700 -o tidbops -g tidbops /var/lib/tidbops"
run "install -d -m 0755 -o root -g root /etc/tidb-ops"
run "install -m 0755 -o root -g root '$BIN' /opt/tidbops/bin/tidbops-server"
for sub in "" js css help; do
  [ -d "$WEB/$sub" ] || continue
  run "install -d -m 0755 -o root -g root /opt/tidbops/web/$sub"
  for f in "$WEB/$sub"/*; do
    case "$f" in */index.html|*.js|*.css|*.md) [ -f "$f" ] && run "install -m 0644 -o root -g root '$f' /opt/tidbops/web/${sub:+$sub/}$(basename "$f")";; esac
  done
done
if [ -d "$WEB/deploy/dist" ]; then
  run "install -d -m 0755 /opt/tidbops/web/deploy/dist"
  for f in "$WEB"/deploy/dist/tidb-deploy-*.tar.gz*; do [ -f "$f" ] && run "install -m 0644 '$f' /opt/tidbops/web/deploy/dist/"; done
fi

echo "== 3/6 令牌密钥与执行器策略 =="
if [ "$DRY" = 1 ] || [ ! -f /etc/tidb-ops/exec.key ]; then
  run "head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > /etc/tidb-ops/exec.key; echo >> /etc/tidb-ops/exec.key"
  # 所有者 tidbops（Web 进程签发令牌），组 tidb（exec 入口校验令牌）
  run "chown tidbops:$TIDB_USER /etc/tidb-ops/exec.key; chmod 0640 /etc/tidb-ops/exec.key"
fi
if [ "$DRY" = 1 ] || [ ! -f /etc/tidb-ops/exec.json ]; then
  run "sed 's/prod-tidb-01/$CLUSTER/' '$HERE/exec.json.example' > /etc/tidb-ops/exec.json; chown root:root /etc/tidb-ops/exec.json; chmod 0644 /etc/tidb-ops/exec.json"
  echo "  已生成 /etc/tidb-ops/exec.json，请按实际环境修改 tiupBin / mysql 运维账号 / backupRoots / alertURL"
fi
if [ "$DRY" = 1 ] || [ ! -f /etc/tidb-ops/exec.env ]; then
  run "printf '# 运维账号口令（仅 exec 入口读取；sudo 会清空环境）\n# TIDBOPS_OPS_PWD=...\n' > /etc/tidb-ops/exec.env; chown root:$TIDB_USER /etc/tidb-ops/exec.env; chmod 0640 /etc/tidb-ops/exec.env"
fi
if [ "$DRY" = 1 ] || [ ! -f /etc/tidb-ops/env ]; then
  run "printf '# 只读采集账号口令（Web 进程）\n# TIDBOPS_MYSQL_PWD=...\n' > /etc/tidb-ops/env; chown root:tidbops /etc/tidb-ops/env; chmod 0640 /etc/tidb-ops/env"
fi

echo "== 4/6 sudoers（只放行受限 exec 入口）=="
run "sed 's/^tidbops ALL=(tidb)/tidbops ALL=($TIDB_USER)/' '$HERE/sudoers.tidbops' > /tmp/tidbops.sudoers.$$"
run "visudo -cf /tmp/tidbops.sudoers.$$ && install -m 0440 -o root -g root /tmp/tidbops.sudoers.$$ /etc/sudoers.d/tidbops; rm -f /tmp/tidbops.sudoers.$$"

echo "== 5/6 systemd =="
run "sed -e 's/--env prod/--env $ENVN/' -e 's/--exec-sudo-user tidb/--exec-sudo-user $TIDB_USER/' -e 's#--home /home/tidb#--home /home/$TIDB_USER#' '$HERE/tidbops.service' > /etc/systemd/system/tidbops.service"
run "systemctl daemon-reload"

echo "== 6/6 完成 =="
cat <<EOF

下一步（依次执行）：
 1. 创建至少两名管理员（写操作开关需要两人确认）；口令从环境变量读取：
      sudo -u tidbops env TIDBOPS_USER_PASSWORD='强口令1' /opt/tidbops/bin/tidbops-server user add --data-dir /var/lib/tidbops --name alice --role admin
      sudo -u tidbops env TIDBOPS_USER_PASSWORD='强口令2' /opt/tidbops/bin/tidbops-server user add --data-dir /var/lib/tidbops --name bob --role admin
    再按需创建 dba / sre / approver / auditor / developer（--role dba 等；--env 可限定环境）。
 2. 在集群里建两个数据库账号：只读账号（慢 SQL / 配置采集，写进 /etc/tidb-ops/env）与运维账号（仅 SET GLOBAL / SET CONFIG，写进 /etc/tidb-ops/exec.env）。
 3. 编辑 /etc/tidb-ops/exec.json。
 4. 验证 sudo 入口：echo '{}' | sudo -u tidbops sudo -n -u $TIDB_USER /opt/tidbops/bin/tidbops-server exec --stdin   （应返回校验失败的 JSON，而不是 sudo 报错）
 5. 启动：systemctl enable --now tidbops ；反向代理（TLS + 访问控制）指向 127.0.0.1:8080。
 6. 登录后到「权限与审计 → 部署自检」确认各项为绿色；由两名管理员依次确认「开启写操作」。
紧急只读：touch /etc/tidb-ops/READONLY
EOF
if [ "$START" = 1 ]; then run "systemctl enable --now tidbops"; fi
