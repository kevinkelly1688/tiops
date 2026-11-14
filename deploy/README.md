# tidb-deploy —— CentOS 7 / 8 / 9 一键部署 TiDB 集群

基于 TiUP 的一键部署脚本：**环境检查 → 系统初始化 → 安装 TiUP（在线 / 离线）→ 生成拓扑 → check → deploy → start → 验证**。
单个 Bash 脚本，不依赖 Python / Go，生产机上可直接审阅、修改。

## 先看这三条

1. **CentOS 7 能装的 TiDB 版本有限**：TiDB 运行时依赖 glibc ≥ 2.28，官方**自 v8.4 起不再支持 CentOS 7**（glibc 2.17）。脚本会按系统自动限制：
   | 系统 | glibc | 默认版本 | 允许范围 |
   |---|---|---|---|
   | CentOS 8 / Stream 8 / Stream 9 / Rocky / Alma / RHEL 8、9 | ≥ 2.28 | v8.5 LTS 系列 | 全部 |
   | **CentOS 7** | 2.17 | **v7.5 LTS 系列** | **≤ v8.3**（推荐 v7.5.x / v8.1.x） |
2. CentOS 7（2024-06）、CentOS Linux 8（2021-12）上游**已停止维护**，官方 yum 源可能失效。脚本提供 `REPO_FIX=auto`（备份 repo 并切换到 vault 镜像），默认不动你的 repo。生产环境建议迁移到 Rocky Linux 9 / CentOS Stream 9。
3. 脚本会**修改系统**（见下表）。务必先 `--dry-run`，并用 `show-prep` 审阅将在每台机器上（经 sudo）执行的脚本。
4. **部署用户、sudo 和 SSH 免密要你事先配好**（见下一节）：脚本只检查，不再代你创建用户、配 sudoers、分发公钥，也不再用 root 登录。

## 前置条件（部署前由你完成）

在**所有部署机器和中控机**上：

1. 创建部署用户（默认 `tidb`，可用 `DEPLOY_USER` / `--user` 改），能登录；
2. 该用户开通 **sudo 免密**；
3. 该用户能用**密钥免密 SSH 登录集群所有机器，包括中控机自己**（TiUP 会 ssh 到每台机器，含本机）。

```bash
# 每台机器（含中控机），root 执行
useradd -m -s /bin/bash tidb && passwd tidb
echo 'tidb ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/tidb && chmod 440 /etc/sudoers.d/tidb && visudo -cf /etc/sudoers.d/tidb

# 分发公钥要用口令登录。若 sshd 禁用了口令登录，先临时打开（每台机器，root 执行；已允许可跳过）
sed -i -E 's/^#?PasswordAuthentication .*/PasswordAuthentication yes/' /etc/ssh/sshd_config && systemctl restart sshd

# 中控机，以 tidb 身份
sudo -iu tidb
ssh-keygen -t rsa -b 4096 -N '' -f ~/.ssh/id_rsa          # 已有密钥可跳过
ssh-copy-id -i ~/.ssh/id_rsa.pub tidb@<每台机器IP>         # 每台都要做，包括中控机自己（每台输入一次 tidb 的口令）

# 复测：不输入口令、不报错才算通过（对每台机器都试）
ssh tidb@<IP> 'sudo -n true' && echo OK

# 所有机器都分发完、复测通过后，如上面改过 sshd，改回禁用口令登录（每台机器，root 执行）
sed -i -E 's/^#?PasswordAuthentication .*/PasswordAuthentication no/' /etc/ssh/sshd_config && systemctl restart sshd
```

> `sed -i -E` 里的 `-E` 不要写成 `-ir`：`-i` 后面紧跟的字母会被当成备份文件后缀，会在 `/etc/ssh/` 下留下 `sshd_configr` 备份文件。口令请自己定强口令，不要沿用文档或示例里的口令。

`check` 和 `deploy` 开始时会逐台验证：部署用户存在、sudo 免密可用、中控机上有私钥、`tidb@每台机器`（含中控机自己）能免密登录。**任何一项不满足就中止**，并列出缺的是哪台机器、哪一项，以及修复命令。私钥不在 `~/.ssh/id_rsa` 时用 `SSH_KEY` 指定。脚本本身仍用 root 运行（`sudo ./tidb-deploy.sh ...`），但登录各目标机一律是 `tidb@IP` + `sudo`，**不需要 root 的 SSH 登录，也不再支持密码登录**。

## 快速开始

```bash
tar xzf tidb-deploy-*.tar.gz && cd tidb-deploy-*
# 0) 先完成上面的「前置条件」
# 1) 生成配置（或 cp deploy.conf.example deploy.conf 手改）
./tidb-deploy.sh wizard
# 2) 只做环境检查（只读，不改任何东西；先检查前置条件）
sudo ./tidb-deploy.sh check --config deploy.conf
# 3) 预览完整流程（不做任何修改）
sudo ./tidb-deploy.sh deploy --config deploy.conf --dry-run
# 4) 审阅初始化脚本
./tidb-deploy.sh show-prep --config deploy.conf | less
# 5) 正式部署（root 执行）
sudo ./tidb-deploy.sh deploy --config deploy.conf
```

最小的单机配置只需两行：

```bash
CLUSTER_NAME=tidb-test
DEPLOY_MODE=single
```

## 两种部署模式

- **single**：本机装全部组件（1×PD + 1×TiDB + 1×TiKV + 监控），TiKV 副本数自动设为 1，block-cache 按内存 25% 设置。**仅用于开发测试，不具备高可用。** 建议 ≥ 8 核 / 16GB。
- **cluster**：按 `PD_HOSTS / TIDB_HOSTS / TIKV_HOSTS / TIFLASH_HOSTS / MONITOR_HOST` 多机部署。生产建议 PD ≥ 3（奇数）、TiDB ≥ 2、TiKV ≥ 3。支持 `ZONE_MAP` 拓扑标签。也可以自己写 `topology.yaml`，直接用 `tiup cluster deploy`（本脚本的初始化部分仍可单独复用：`show-prep`）。

## 操作系统用户可选择

TiUP 与集群以一个普通操作系统用户运行，默认 `tidb`，可自定义：

```bash
# 配置文件：DEPLOY_USER=deploy01      命令行（优先于配置文件）：
sudo ./tidb-deploy.sh deploy --config deploy.conf --user deploy01
```

- 不能是 `root`，也不能是系统账号（uid < 1000）；用户名只能含小写字母、数字、下划线、连字符。
- 用户必须**事先在所有机器上创建好**（脚本不会创建），家目录自动识别，不假定 `/home/<用户>`；sudo 免密和 SSH 免密登录也要事先配好，见上面的「前置条件」。
- 初始化各机器时，脚本以 `<用户>@<IP>` + 该用户的私钥（`SSH_KEY`，默认 `~/.ssh/id_rsa`）登录，再用 `sudo -n` 执行。旧配置里的 `SSH_ROOT_USER` / `SSH_ROOT_KEY` / `TD_ROOT_PASSWORD` 已不再使用（会提示并忽略）。

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
- `--target-os centos7|centos8|stream8|stream9|stream10|rocky8|rocky9|rocky10|alma8|alma9|alma10|rhel10` 会在下载前检查版本能否在目标系统运行（例如目标 CentOS 7 却选 v8.5.2 会被拒绝，省得白下几个 GB）。
- 另一种做法：`./tidb-deploy.sh offline-pack vX.Y.Z` 用 `tiup mirror clone` 自己制作；或从官方下载页获取。

## 安装部署界面

原型里的「运维 → **安装部署**」页面可以不手写配置：选择目标系统、操作系统用户、TiDB 系列或完整版本、在线 / 离线（含离线包完整目录）、主机清单，页面**实时校验**（例如 CentOS 7 选 v8.5 会被拦下、离线目录必须是绝对路径、IP 与重复主机、拓扑标签），并生成 `deploy.conf`（可复制 / 下载）、命令序列、拓扑预览；另有**离线包下载链接**与**部署工具包下载**。页面的规则与脚本一致，真正的部署仍由中控机上的 `tidb-deploy.sh` 执行。

## 脚本会做什么（修改系统的部分，每台部署主机）

| 项 | 做法 | 回退 |
|---|---|---|
| 依赖包 | 安装 numactl、chrony、curl、tar、gzip、libaio、openssh、sudo | — |
| swap | `swapoff -a`，注释 `/etc/fstab` 里的 swap 行 | 备份 `/etc/fstab.tidb-deploy.bak` |
| 透明大页 | 立即关闭 + `tidb-disable-thp.service` 开机关闭 | 删除该 service |
| 内核参数 | `/etc/sysctl.d/99-tidb.conf`（swappiness、file-max、somaxconn、tcp_syncookies、overcommit_memory；内存 ≥ 16GB 时加 min_free_kbytes） | 删除该文件 |
| ulimit | `/etc/security/limits.d/99-tidb.conf` | 删除该文件 |
| SELinux | Enforcing 时设为 permissive，并在配置里设为 disabled（彻底关闭需重启） | 改回 `/etc/selinux/config` |
| 防火墙 | `FIREWALL=open`（默认）只放行 TiDB 所需端口；`disable` 关闭；`keep` 不动 | `firewall-cmd --remove-port` |
| 时间同步 | 启用 chronyd 并 `makestep`（已有 ntpd 则不动） | — |
| 部署用户 | **不创建、不改 sudoers、不写公钥**：由你事先配好（见「前置条件」），脚本只确认用户存在 | — |
| 目录 | 创建 `DEPLOY_DIR` / `DATA_DIR` 并归属 tidb | — |
| yum 源（可选） | `REPO_FIX=auto` 且安装失败时，备份 `/etc/yum.repos.d/*.repo` 并改用 vault 镜像 | 从 `/etc/yum.repos.d/tidb-deploy-backup-*` 还原 |

## 环境检查项

系统与 glibc → TiDB 版本门槛；每台主机的 CPU / 内存（dev：低于推荐配置只警告，多角色合并低于 4 核 / 8GB 也只警告；prod：按官方生产配置，不达标中止）；数据盘可用空间；数据目录文件系统（xfs ✔；ext4 需 `nodelalloc`）；TiFlash 的 AVX2；端口占用。之后 `tiup cluster check --apply` 自动修复，再复查，仍有 Fail 则中止（测试环境可设 `ALLOW_CHECK_FAIL=true`）。

## 其他命令

```bash
./tidb-deploy.sh status   --config deploy.conf     # tiup cluster display
./tidb-deploy.sh stop|start --config deploy.conf
./tidb-deploy.sh destroy  --config deploy.conf --confirm-name tidb-test   # 销毁，必须给集群名
./tidb-deploy.sh deploy   --config deploy.conf --force   # 忽略已完成阶段记录重新执行
```

阶段记录在 `/var/lib/tidb-deploy/<集群名>/state`，中断后重新执行会从未完成的阶段继续。日志在 `/var/log/tidb-deploy/`（root 随机密码不会写入日志，只保存在 `root-password.txt`，权限 600）。

## 初始化各机器时如何登录

本机直接执行（脚本以 root 运行）。远程主机一律用 `tidb@IP`（用户名取 `DEPLOY_USER`）+ 该用户的私钥（`SSH_KEY`，默认 `~/.ssh/id_rsa`）登录，再 `sudo -n bash -s` 执行初始化；随后 TiUP 也用同一个用户和同一把私钥工作。因此不需要 root 的 SSH 登录、root 密码或 `sshpass`。

## 测试与已知限制

```bash
bash tests/run-tests.sh        # 离线逻辑测试，不需要 root、不需要 CentOS
```

已覆盖：系统识别、glibc / 版本门槛矩阵、按系列选版本、主机解析与校验、拓扑生成、tiup 输出解析、资源与磁盘评估、端到端 `--dry-run`（含 CentOS 7 拒绝 v8.5）、destroy 保护、初始化脚本语法与关键步骤、配置向导、bash 4.2 语法兼容检查。

**尚未在真实 CentOS 上执行过**：开发机是 Windows，没有 Docker / 虚拟机。因此系统修改部分（yum、systemd、sysctl、firewalld、sudoers、useradd）与真实的 `tiup cluster check / deploy` 输出格式，只做了语法与逻辑层面的验证。**首次请务必在测试机上用 `--dry-run` 和 `show-prep` 审阅后再执行。**

其他限制：仅 IPv4；仅 x86_64（`offline-pack` 与 TiUP 默认架构）；同一主机上同一角色只部署一个实例；TiKV 数据盘的分区、格式化与 `nodelalloc` 挂载不由本脚本处理（只检查并给出修复提示）；`tiup cluster check` 里的磁盘类 Fail 在非生产环境可用 `ALLOW_CHECK_FAIL=true` 放行。
