p = 'deploy/README.md'
s = open(p, encoding='utf-8', newline='').read()

a = s.index('## 在线 / 离线')
b = s.index('## 脚本会做什么')
new = '''## 操作系统用户可选择

TiUP 与集群以一个普通操作系统用户运行，默认 `tidb`，可自定义：

```bash
# 配置文件：DEPLOY_USER=deploy01      命令行（优先于配置文件）：
sudo ./tidb-deploy.sh deploy --config deploy.conf --user deploy01
```

- 不能是 `root`，也不能是系统账号（uid < 1000）；用户名只能含小写字母、数字、下划线、连字符。
- 用户不存在会自动创建；已存在则沿用（家目录自动识别，不假定 `/home/<用户>`）。脚本为该用户配置 sudo 免密与 SSH 互信。
- 初始化**远程主机**时的登录用户由 `SSH_ROOT_USER` 决定（默认 root）；非 root 时需要该用户有免密 sudo，脚本用 `sudo -n` 执行初始化。

## TiDB 版本由用户指定

`TIDB_VERSION`（或 `--tidb-version`）三种写法：

| 写法 | 含义 |
|---|---|
| `auto` | 按系统自动选：glibc ≥ 2.28 → v8.5 LTS 系列；CentOS 7 → v7.5 LTS 系列 |
| `v8.5`、`v7.5`、`v8.1`… | 指定**系列**，取该系列最新补丁版本（在线取镜像里的最新；离线取离线包目录里的最高版本） |
| `v7.5.4` | 指定**完整版本** |

可省略 `v` 前缀。无论哪种写法，都会按系统做版本门槛检查（CentOS 7 不允许 ≥ v8.4）。

## 在线 / 离线安装，离线包可指定完整目录

- **在线**：从 `tiup-mirrors.pingcap.com` 下载 TiUP，组件由 TiUP 在线拉取。
- **离线**：把离线包放进**同一个目录**，用**完整的绝对路径**指定，脚本按所选版本**自动查找**，无需写文件名：

  ```bash
  # 目录里需要有：
  #   tidb-community-server-v7.5.4-linux-amd64.tar.gz        （必需）
  #   tidb-community-toolkit-v7.5.4-linux-amd64.tar.gz       （可选，同版本会自动带上）
  # 配置文件：OFFLINE_PKG_DIR=/opt/tidb-pkg      命令行：
  sudo ./tidb-deploy.sh deploy --config deploy.conf --offline-dir /opt/tidb-pkg --tidb-version v7.5
  ```

  - 路径必须以 `/` 开头；目录不存在会报错（`--dry-run` 下仅警告）。
  - `--tidb-version v7.5` → 取目录里 v7.5.x 最高版本；`auto` → 按系统选系列再取该系列最高版本；`v7.5.4` → 精确匹配。
  - 目录里没有对应版本时，会列出目录里已有的版本，提示你换版本或补包。
  - 按 CPU 架构匹配 `linux-amd64` / `linux-arm64`（`--arch` 可覆盖，默认按本机）。
  - 也可以不用目录，直接指定单个文件：`OFFLINE_SERVER_PKG=/path/tidb-community-server-….tar.gz`（版本从文件名读取）。
  - 无外网装依赖：`OFFLINE_RPM_DIR=/opt/tidb-pkg/rpm`（放 numactl、chrony、libaio 等 rpm）。

### 下载离线包

在**能联网**的机器上，一条命令下载并校验（也可在「安装部署」页面里生成命令 / 拿到浏览器下载链接）：

```bash
./tidb-deploy.sh download-offline --tidb-version v7.5.4 --dir /opt/tidb-pkg --with-toolkit --target-os centos7
```

- 需要**完整版本号**；下载源默认 `https://download.pingcap.org`，可用 `--base-url` 换成内部镜像。
- 自动取同名 `.sha256` 校验；已存在且校验通过则跳过；支持断点续传；校验失败会删除半成品并报错。取不到 `.sha256` 时只警告并打印本地 sha256。
- `--target-os centos7|centos8|stream8|stream9|rocky8|rocky9|alma8|alma9` 会在下载前检查版本能否在目标系统运行（例如目标 CentOS 7 却选 v8.5.2 会被拒绝，省得白下几个 GB）。
- 另一种做法：`./tidb-deploy.sh offline-pack vX.Y.Z` 用 `tiup mirror clone` 自己制作；或从官方下载页获取。

## 安装部署界面

原型里的「运维 → **安装部署**」页面可以不手写配置：选择目标系统、操作系统用户、TiDB 系列或完整版本、在线 / 离线（含离线包完整目录）、主机清单，页面**实时校验**（例如 CentOS 7 选 v8.5 会被拦下、离线目录必须是绝对路径、IP 与重复主机、拓扑标签），并生成 `deploy.conf`（可复制 / 下载）、命令序列、拓扑预览；另有**离线包下载链接**与**部署工具包下载**。页面的规则与脚本一致，真正的部署仍由中控机上的 `tidb-deploy.sh` 执行。

'''
s = s[:a] + new + s[b:]
s = s.replace('- **离线**：先在联网机器上', '- **离线**：先在联网机器上')
open(p, 'w', encoding='utf-8', newline='').write(s)
print('ok')
