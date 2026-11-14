// @author: kevinkelly
// @license: Apache License 2.0
// @file: scripts.go
// @time: 2026/10/28

package main

import "strings"

// defaultListen 是 install.sh 传给 admin install 的默认监听地址（安装器自己的默认值是 127.0.0.1:8080，这里不改程序，只在包装脚本里覆盖）。
const defaultListen = "0.0.0.0:8018"

// 包装脚本：只做 root 与架构检查，真正的逻辑在 Go 实现的 tidbops-server admin 子命令里。
// 模板里的 @ARCH@ 在打包时替换；整个文件强制 LF 换行。

const scriptHead = `#!/usr/bin/env bash
# @TITLE@（本包架构：@ARCH@）。
# 只做 root 与架构检查，实际逻辑在 Go 实现的 tidbops-server admin @SUB@ 中。
set -euo pipefail

PKG_ARCH="@ARCH@"

for a in "$@"; do
  case "$a" in -h|--help) SKIP_ROOT=1;; esac
done

if [ "$(id -u)" != "0" ] && [ -z "${SKIP_ROOT:-}" ]; then
  echo "错误: 需要 root 权限，请使用: sudo $0 $*" >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64|amd64) MACHINE_ARCH="amd64";;
  aarch64|arm64) MACHINE_ARCH="arm64";;
  *) MACHINE_ARCH="$(uname -m)";;
esac
if [ "$MACHINE_ARCH" != "$PKG_ARCH" ]; then
  echo "错误: 本发布包适用于 $PKG_ARCH，当前机器架构是 $(uname -m)，请下载对应架构的包" >&2
  exit 1
fi

`

// scriptPlatformConf 是 install.sh / upgrade.sh 里放置备案号配置文件的片段：/etc/tidb-ops/platform.json 不存在时放一份包里的
// （已存在绝不覆盖；预演 / 帮助时不改动系统）。用户直接改这个文件的 icp 即可，保存后 systemctl restart tidbops 生效。
const scriptPlatformConf = `ETC_DIR="/etc/tidb-ops"
PLATFORM_DRY=0
for a in "$@"; do
  case "$a" in -h|--help|-dry-run|--dry-run|-dry-run=true|--dry-run=true) PLATFORM_DRY=1;; esac
done
if [ "$PLATFORM_DRY" = "0" ] && [ -f "$HERE/deploy/ops/platform.json" ] && [ ! -e "$ETC_DIR/platform.json" ]; then
  install -d -m 0755 "$ETC_DIR"
  install -m 0644 "$HERE/deploy/ops/platform.json" "$ETC_DIR/platform.json"
  echo "已放置备案号配置文件 $ETC_DIR/platform.json：编辑其中的 icp，保存后执行 systemctl restart tidbops 生效"
fi
`

// install.sh 默认监听 0.0.0.0:8018（单元模板带 --allow-remote）：放在 "$@" 之前，用户自己传 --listen 会覆盖它（后者生效）。
const scriptInstallUpgrade = scriptHead + `HERE="$(cd "$(dirname "$0")" && pwd)"
` + scriptPlatformConf + `exec "$HERE/bin/tidbops-server" admin @SUB@@DEFAULTS@ "$@"
`

const scriptRollback = scriptHead + `BIN="/opt/tidbops/current/bin/tidbops-server"
if [ ! -x "$BIN" ]; then
  echo "错误: 找不到 $BIN，平台尚未安装或安装目录不是默认的 /opt/tidbops" >&2
  exit 1
fi
exec "$BIN" admin rollback "$@"
`

// wrapperScripts 返回包内的三个包装脚本（文件名 -> 内容）。
func wrapperScripts(arch string) map[string]string {
	mk := func(tpl, title, sub, defaults string) string {
		r := strings.NewReplacer("@TITLE@", title, "@ARCH@", arch, "@SUB@", sub, "@DEFAULTS@", defaults)
		return r.Replace(tpl)
	}
	return map[string]string{
		"install.sh":  mk(scriptInstallUpgrade, "TiDB Ops 首次安装", "install", " --listen "+defaultListen),
		"upgrade.sh":  mk(scriptInstallUpgrade, "TiDB Ops 升级", "upgrade", ""), // 升级沿用已安装单元里的监听地址，不改
		"rollback.sh": mk(scriptRollback, "TiDB Ops 手动回滚到上一版本", "rollback", ""),
	}
}
