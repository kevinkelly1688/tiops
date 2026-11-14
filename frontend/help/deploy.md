# 安装部署

> 💡 在浏览器里填表，生成 TiDB 集群一键部署工具包（tidb-deploy）所需的 `deploy.conf` 和命令序列，并提供离线包与工具包的下载入口；本篇后半部分还说明平台自身的安装、升级、回滚与 `admin` 子命令。页面本身**不连接任何主机、不执行任何命令**，所有模式、所有登录用户看到的内容相同，没有角色限制。

> ⚠️ 部署脚本和平台的 install / upgrade 都会**修改目标机器的系统配置并使用 root 权限**。这些脚本和 `admin` 引擎从未在真实 CentOS / Rocky 上执行过，只做过语法与逻辑层面的验证；首次使用请在测试机上用 `--dry-run` 和 `show-prep` 审阅后再执行，一切以真实环境联调为准。本篇给出的耗时都是数量级估计，不是实测值。

## 这个页面做什么

页面解决「怎么把一套 TiDB 集群装起来」的问题：

1. 左侧 5 个卡片填目标系统、部署模式、操作系统用户、TiDB 版本、安装方式、主机与目录。
2. 右侧实时给出 5 个标签页（中控机模式下还有第 6 个「一键部署」）：`deploy.conf`（可复制 / 下载）、「命令」（按顺序要执行的命令）、「拓扑预览」（脚本将生成的 TiUP 拓扑）、「校验」（错误 / 警告 / 通过项）、「离线包与工具下载」。
3. 你把 `deploy.conf` 和部署工具包传到 **tiup 中控机**（装有 TiUP、能 SSH 到所有目标主机的那台机器），在中控机上以 root 运行 `tidb-deploy.sh`，真正的检查、初始化、部署都由这个脚本完成。
4. **中控机模式下可以不手工执行**：页面上方的「保存配置」把配置存到平台，右侧的「一键部署」标签替你启动脚本并把全部输出实时显示出来，见下文「一键部署（中控机模式）」。

页面里的规则（版本门槛、校验、拓扑）与 `tidb-deploy.sh` 保持一致，但页面只是辅助生成，**最终以脚本在中控机上的检查结果为准**。

区分两件事，不要混淆：

| 名称 | 用途 | 入口 |
|---|---|---|
| tidb-deploy（本页前半部分） | 部署一套 **TiDB 集群**（PD / TiDB / TiKV / 监控等） | 本页 + `tidb-deploy.sh` |
| 平台一键部署 / 升级 / 回滚（本页后半部分） | 安装 **TiDB 运维平台自己** | 发布包里的 `install.sh` / `upgrade.sh` / `rollback.sh` 与 `tidbops-server admin` |

## 怎么用（按步骤）

### 部署 TiDB 集群

#### 准备（动手之前先确认）

| 项目 | 要求 | 不满足的后果 |
|---|---|---|
| 中控机 | 一台 CentOS / Rocky / Alma / RHEL 系 Linux，有 root（脚本用 root 运行）；可以同时是 PD 等部署主机之一；本机有 `ssh` | 脚本无法运行 |
| 目标主机 | 同系列系统，CPU / 内存 / 数据盘满足所选规格（见下文「规格检查阈值」） | 环境检查失败并中止（prod）或警告（dev） |
| **部署用户（你事先配好）** | **所有部署机器和中控机**上都有部署用户（默认 `tidb`）、该用户有 **sudo 免密**、且该用户能用密钥**免密 SSH 登录集群所有机器，包括中控机自己**。脚本**不会**替你创建用户、配 sudoers 或分发公钥，也不用 root 登录 | `check` / `deploy` 在最开始就中止，并列出哪台机器缺哪一项和修复命令 |
| 网络 | 目标主机之间、中控机到目标主机的 TiDB 所需端口互通（2379 / 2380 / 4000 / 10080 / 20160 / 20180 / 9100 等）；这些端口当前未被占用 | 环境检查报端口被占用，或部署后组件间不通 |
| 软件源 | 在线：中控机能访问 `tiup-mirrors.pingcap.com`，目标机有可用 yum / dnf 源；离线：准备好离线包和（可选）依赖 rpm | 依赖安装失败、下载失败 |
| 数据盘 | 目标机数据目录建议放在独立数据盘，文件系统 xfs，或 ext4 且挂载选项含 `nodelalloc` | 脚本只检查并提示，**不会**替你分区、格式化、挂载 |

**部署用户的配置步骤**（部署前由你完成；`tidb` 换成你选的用户，`<口令>` 自己定强口令；页面「命令」标签页的第 0 步给出同样的命令，并按你填的主机列好）：

```bash
# ① 每台机器（含中控机），root 执行：创建用户、开通 sudo 免密
useradd -m -s /bin/bash tidb && echo '<口令>' | passwd tidb --stdin
echo 'tidb ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/tidb && chmod 440 /etc/sudoers.d/tidb && visudo -cf /etc/sudoers.d/tidb

# ② 分发公钥要用口令登录：若 sshd 禁用了口令登录，每台机器先临时打开（已允许可跳过）
sed -i -E 's/^#?PasswordAuthentication .*/PasswordAuthentication yes/' /etc/ssh/sshd_config && systemctl restart sshd

# ③ 中控机，以 tidb 身份生成密钥并分发到集群每台机器，包括中控机自己
sudo -iu tidb
ssh-keygen -t rsa -b 4096 -N '' -f ~/.ssh/id_rsa           # 已有密钥可跳过
ssh-copy-id -i ~/.ssh/id_rsa.pub tidb@172.16.0.202          # 每台机器都做一遍（含中控机自己），每台输入一次 tidb 的口令

# ④ 复测：对每台机器都不输入口令、不报错才算通过
ssh tidb@172.16.0.202 'sudo -n true' && echo OK

# ⑤ 所有机器都分发完、复测通过后，如第 ② 步改过 sshd，改回禁用口令登录（每台机器，root 执行）
sed -i -E 's/^#?PasswordAuthentication .*/PasswordAuthentication no/' /etc/ssh/sshd_config && systemctl restart sshd
```

看到 `Permission denied` 说明公钥没有分发到那台机器的 `~tidb/.ssh/authorized_keys`；`a password is required` 说明 sudo 免密没开通；`Connection timed out` 说明网络或防火墙不通。`sed -i -E` 里的 `-E` 不要写成 `-ir`：`-i` 后紧跟的字母会被当成备份文件后缀，会在 `/etc/ssh/` 下留下 `sshd_configr`。

#### 第 1 步：在页面填表

打开本页，按 5 个卡片自上而下填写；字段含义、可选值、默认值、填错后果见下文「区域与参数说明」。最少要确认这些：

1. 卡片「1 · 目标系统与模式」：「目标操作系统」选与你目标机一致的系统（默认 Rocky Linux 9）；「部署模式」选「单机（全部组件同机，仅测试）」或「多机集群」（默认单机）；「集群名」默认 `tidb-test`。
2. 卡片「2 · 操作系统用户」：默认 `tidb`；多机模式再确认「初始化远程主机的登录用户」（默认 root）、「SSH 端口」、「登录方式」（默认「密钥」）、「SSH 私钥路径」。
3. 卡片「3 · TiDB 版本」：默认「自动」；要装指定版本选「指定系列」或「指定完整版本」。离线安装要用完整版本下载包。
4. 卡片「4 · 安装方式」：默认在线；无外网选「离线安装（使用离线包）」并填「离线包所在的完整目录路径」。
5. 卡片「5 · 主机与目录」：多机模式填 PD / TiDB / TiKV 主机（页面里预填的 10.0.1.x 只是示例，必须换成你的真实 IP）；「规格」生产选 `prod`。

你会看到：右侧「校验」标签页给出红 / 黄 / 绿三类提示。**红色错误**必须全部消除；黄色警告是提醒；绿色是通过项。标签页标题上会写「N 错误」。

#### 第 2 步：校验并下载 deploy.conf

1. 点右侧「校验」标签页，确认没有红色错误，最后一条绿色提示是「配置通过校验，可以下载 deploy.conf」。
2. 切到「deploy.conf」标签页，点「下载 deploy.conf」。有错误时这个按钮是灰的（「复制」仍可用），右侧会写「存在错误，请先修正（见「校验」）」。
3. 你会看到右上角提示「已下载 deploy.conf，请传到中控机并执行 chmod 600 deploy.conf」。浏览器保存的文件名是 `deploy.conf`。

生成的 `deploy.conf` 是 bash 变量赋值文件，形如（多机示例，节选）：

```bash
CLUSTER_NAME=tidb-test
TIDB_VERSION=v7.5
DEPLOY_MODE=cluster
PD_HOSTS="10.0.1.4 10.0.1.5 10.0.1.6"
TIDB_HOSTS="10.0.1.1 10.0.1.2"
TIKV_HOSTS="10.0.1.7 10.0.1.8 10.0.1.9"
DEPLOY_USER=tidb
SSH_KEY=""
SPEC=prod
INSTALL_MODE=offline
OFFLINE_PKG_DIR="/opt/tidb-pkg"
```

每个变量与页面字段的对应关系见「页面生成的配置项对照」。

#### 第 3 步：取得部署工具包（和离线包）

1. 切到「离线包与工具下载」标签页，在「部署工具包」区点「下载 tidb-deploy-0.3.1.tar.gz」，再点「SHA-256」打开校验值。
2. 在你的电脑上用 `sha256sum tidb-deploy-0.3.1.tar.gz` 核对，与 SHA-256 文件里的值一致才继续。
3. 在线安装到此为止，直接把工具包与 `deploy.conf` 拷到中控机（`scp`、U 盘均可）。离线安装还要准备离线包，见下面的「下载离线包」。

**下载离线包（仅离线安装）**：在**能联网的机器**上先把工具包解压，再执行页面「命令行下载（推荐）」给出的命令。页面字段：「TiDB 完整版本」填 `v7.5.4`（离线包必须精确到补丁版本，填系列会提示「请填写完整版本号」），「CPU 架构」选与目标机一致的 amd64 / arm64，「保存到」填 `/opt/tidb-pkg`，勾选「同时下载 toolkit 包」。

```bash
./tidb-deploy.sh download-offline --tidb-version v7.5.4 --dir /opt/tidb-pkg --arch amd64 --with-toolkit --target-os rocky9
```

命令各参数含义：`--tidb-version` 要下载的完整版本；`--dir` 保存目录（绝对路径，在运行脚本的这台机器上）；`--arch` CPU 架构；`--with-toolkit` 同时下载 toolkit 包（离线时合并镜像要用）；`--target-os` 目标系统，用来在下载前检查版本能否运行。

你会看到：脚本依次输出「下载 https://download.pingcap.org/tidb-community-server-v7.5.4-linux-amd64.tar.gz」「SHA-256 校验通过: …」，对 toolkit 包同样处理，最后打印「离线包目录: /opt/tidb-pkg」及后续配置提示。包体通常有数 GB，耗时取决于带宽；中断后重新执行会断点续传，已校验通过的文件会提示「已存在且校验通过，跳过」。校验失败会删除半成品并报「SHA-256 校验失败」，重新执行即可。不想用命令行时，也可点页面的浏览器下载链接，文件放进同一目录且**不要改名**，再用 `sha256sum -c` 校验。

最后把工具包、`deploy.conf` 以及整个 `/opt/tidb-pkg` 目录拷到中控机。

#### 第 4 步：在中控机上准备

```bash
tar xzf tidb-deploy-0.3.1.tar.gz && cd tidb-deploy-0.3.1
cp /path/to/deploy.conf .
chmod 600 deploy.conf
```

三条命令都没有输出就是成功。`chmod 600` 不能省：脚本会以 root 身份 `source` 这个文件，里面的任何命令都会被执行，务必保证内容可信。

> 💡 下面的命令请**用 root 执行**（`sudo -i` 或 `su -` 切到 root 后再运行）。检查部署用户的 sudo 与 SSH、读取 `tidb` 的私钥都需要 root；脚本以 `tidb@IP` + sudo 登录各主机，不用 root 登录。

#### 第 5 步：只读环境检查（check）

```bash
./tidb-deploy.sh check --config deploy.conf
```

作用：识别本机系统；**先检查前置条件**（中控机上有部署用户、sudo 免密、私钥存在，且 `tidb@每台机器`（含中控机自己）能免密登录并 sudo，任何一项不满足就中止）；再以 `tidb@IP` + sudo 登录每台主机做只读事实收集（CPU、内存、glibc、AVX2、数据目录文件系统与剩余空间、端口占用）；按规格逐台评估；校验 TiDB 版本与最低 glibc；离线时再检查离线包。

你会看到（示意，数字随你的机器而变）：

```
==> 环境检查
[信息] 本机系统: Rocky Linux 9.4 (Blue Onyx) 9.4（rocky9）
  ✔ 系统在官方测试列表内
  ✔ 中控机：tidb 有 sudo 免密
  ✔ 中控机：tidb 的 SSH 私钥 /home/tidb/.ssh/id_rsa 存在
  ✔ tidb@10.0.1.4（中控机自己） 免密登录 + sudo 免密可用
  ✔ tidb@10.0.1.7 免密登录 + sudo 免密可用
  ...
[信息] 主机 10.0.1.7  角色: tikv  CPU 16 核  内存 64 GB  系统 rocky 9.4
  ✔ 10.0.1.7 tikv 资源满足 prod 要求（≥16 核 / 64GB）
  ✔ 10.0.1.7 数据盘可用 800GB
  ✔ 10.0.1.7 数据目录文件系统 xfs
  ✔ 10.0.1.7 所需端口空闲
  ...
  ✔ TiDB v7.5 与系统 glibc 2.34 兼容
[信息] 检查结果：通过 N · 警告 N · 失败 0
```

- 耗时：每台主机一次 SSH，通常数秒；主机多、网络慢时按台数累加。
- **通过**：「失败 0」。警告可以接受，但要读懂每一条。
- **失败**：出现红色 ✖，最后一行「环境检查未通过（N 项失败），请先处理上面标 ✖ 的项目」，命令退出。处理办法见本节末尾「失败怎么办」。
- 这个命令不修改系统配置，但会在中控机上创建工作目录 `/var/lib/tidb-deploy/<集群名>/` 和日志文件 `/var/log/tidb-deploy/<集群名>-<时间>.log`（`<集群名>` 就是 `deploy.conf` 里的 `CLUSTER_NAME`）。

#### 第 6 步：预览全过程（--dry-run）

```bash
./tidb-deploy.sh deploy --config deploy.conf --dry-run
```

`--dry-run` 的正确理解：它只是**不执行修改系统的命令**，把它们以 `[DRY]` 开头打印出来，不创建工作目录和日志文件。但是：

- 阶段 1 的前置条件检查和环境检查**仍然真实执行**，会以 `tidb@IP` 通过 SSH 登录各台主机检查 sudo 并收集只读事实，所以部署用户、SSH、权限、资源、端口问题在这一步就会暴露；检查失败时 dry-run 同样会中止。检查部署用户需要 root（或直接以该用户运行），所以 dry-run 也请用 root 执行。
- 开头仍会询问「确认开始部署？ [y/N]」，在交互终端输入 `y`；非交互终端（脚本、CI）必须加 `--yes`，否则报「需要交互确认，但当前不是交互终端。请加 --yes 跳过确认」。
- 离线包目录在本机不存在时只警告（「dry-run 继续；正式执行前请确认」），正式执行才报错。
- 在线安装下，系列版本只显示「vX.Y.x（实际执行时解析为该系列最新补丁版本）」，真正的补丁号要到正式部署时才确定。
- 如果这台机器上做过真实部署，已记录完成的阶段会显示「已完成（--force 可重做）」而不是 `[DRY]` 行。

你会看到（节选）：

```
[警告] dry-run：只打印将要执行的内容，不做任何修改
部署计划：集群 tidb-test · 模式 cluster · 版本 v7.5 · 规格 prod · 操作系统用户 tidb
  安装方式：离线（包目录 /opt/tidb-pkg，架构 amd64）
  PD: 10.0.1.4 10.0.1.5 10.0.1.6 | TiDB: 10.0.1.1 10.0.1.2 | TiKV: 10.0.1.7 10.0.1.8 10.0.1.9 | 监控: 10.0.1.4
  将修改：swap / THP / sysctl / limits / SELinux / 防火墙(open) / 时间同步（以 tidb@主机 登录并 sudo）
  前置条件（须已由你配置，部署前会检查）：所有机器（含中控机）有 tidb 用户 + sudo 免密，且 tidb 能用密钥免密 SSH 登录所有机器（含中控机自己）
确认开始部署？ [y/N] y
==> 环境检查
...
==> 初始化全部主机（以 tidb@主机 登录并 sudo，幂等）
[DRY] 在 10.0.1.1 上执行 remote_prep（swap/THP/sysctl/limits/SELinux/防火墙/chrony；以 tidb@10.0.1.1 登录并 sudo）；可用 show-prep 查看脚本
...
==> 生成拓扑文件
    | global:
    |   user: "tidb"
    ...
==> tiup cluster check（先自动修复，再复查）
[DRY] runuser -l tidb -c tiup cluster check .../topology.yaml --apply --user tidb -i ...
==> 验证
[DRY] 轮询 http://10.0.1.4:2379/pd/api/v1/health 与 http://10.0.1.1:10080/status（最多 120 秒），再 tiup cluster display tidb-test
```

核对：计划里的主机、版本、规格、离线包目录是否与你的意图一致；「拓扑文件」里每台主机的角色是否正确；没有红色错误。有问题就改 `deploy.conf` 再预览，直到满意。

#### 第 7 步：审阅将经 sudo 执行的初始化脚本（show-prep）

```bash
./tidb-deploy.sh show-prep --config deploy.conf | less
```

`show-prep` 只打印脚本，不连接任何主机，也不修改任何东西。你会看到一份 bash 脚本，前面是几行 `P_USER=…`、`P_FIREWALL=…` 之类的变量赋值，后面是 `remote_prep` 函数：依次是安装依赖、关 swap、关透明大页、写内核参数、写 ulimit、处理 SELinux、处理防火墙、时间同步、确认部署用户已存在（不创建）、创建目录。把它和下文「对目标主机的修改」逐项对照，确认你接受这些改动（尤其是 SELinux 和防火墙）。按 `q` 退出 `less`。

#### 第 8 步：正式部署

```bash
sudo ./tidb-deploy.sh deploy --config deploy.conf
```

（已经是 root 时直接去掉 `sudo`。）输入 `y` 确认后开始。开始前会先检查「准备」一节的部署用户前置条件，不满足就中止、不改任何东西。脚本不再支持用密码登录远程主机。

> ⚠️ 这一步会按下文「对目标主机的修改」改动每一台主机，并使用 `tiup cluster check --apply` 自动修复检查中发现的问题。除 SELinux、sudoers 等项可手工回退外，**没有一键撤销**；彻底撤销只能 destroy 集群并手工还原系统配置。

脚本按阶段输出，每个阶段完成后记录在 `/var/lib/tidb-deploy/<集群名>/state`。你会依次看到：

| 阶段 | 你会看到 | 耗时参考（数量级） |
|---|---|---|
| 0 确认 | 部署计划 + 「确认开始部署？ [y/N]」 | 即时 |
| 1 环境检查 | 同第 5 步；失败即中止 | 秒到一两分钟 |
| 2 解析离线包 | 「离线包目录 /opt/tidb-pkg：选用 TiDB v7.5.4（含 toolkit 包）」；找不到包时列出已有版本并中止 | 秒级 |
| 3 本机用户与密钥 | 「控制机用户 tidb 的公钥已就绪（家目录 /home/tidb）」 | 秒级 |
| 4 初始化全部主机 | 对每台主机「初始化 <IP> ...」，成功后「<IP> 初始化完成」；失败时打印最后 8 行输出并报「主机 <IP> 初始化失败，详见日志 …」 | 每台十几秒到数分钟（取决于依赖是否要安装） |
| 5 安装 TiUP | 在线下载安装或离线解压 `local_install.sh`、合并 toolkit；已装则「TiUP 已安装，跳过」；末尾执行 `tiup cluster` 自检 | 在线数十秒；离线要解压数 GB，约数分钟 |
| 6 解析版本、生成拓扑 | 「将部署 TiDB v7.5.4」「已生成 /var/lib/tidb-deploy/<集群名>/topology.yaml」 | 秒级 |
| 7 tiup cluster check | 先 `check --apply` 自动修复，再复查，「tiup check 复查：Fail 0 · Warn N」；仍有 Fail 会列出并中止 | 一到数分钟 |
| 8 tiup cluster deploy | tiup 的分发与创建目录输出；集群已存在则「集群 X 已存在，跳过 deploy」 | 数分钟（在线要下载组件，离线较快） |
| 9 启动 | 「已生成 TiDB root 随机密码，保存在 /var/lib/tidb-deploy/<集群名>/root-password.txt（权限 600）」 | 一到数分钟 |
| 10 验证 | 「PD 健康检查通过」「TiDB 状态端口可访问」，`tiup cluster display` 表，部署摘要 | 最多约 4 分钟（PD、TiDB 各最多等 120 秒） |

结束时打印摘要：

```
============ 部署完成 ============
集群名      : tidb-test
TiDB 版本   : v7.5.4
SQL 连接    : mysql -h 10.0.1.1 -P 4000 -u root -p
root 密码   : 见 /var/lib/tidb-deploy/tidb-test/root-password.txt（首次登录后请立即修改并删除该文件）
Dashboard   : http://10.0.1.4:2379/dashboard
Grafana     : http://10.0.1.4:3000   （默认账号 admin / admin，首次登录请改密码）
```

中断后（Ctrl-C、断网、某阶段失败），**修复原因后重新执行同一条命令**，会从未完成的阶段继续；`--force` 忽略记录全部重做。已存在的集群不会被覆盖。

#### 第 9 步：验证与收尾

1. 登录 TiDB：`mysql -h <TiDB 主机 IP> -P 4000 -u root -p`，`<TiDB 主机 IP>` 取自摘要里的「SQL 连接」；密码用 `cat /var/lib/tidb-deploy/<集群名>/root-password.txt` 查看。登录成功可执行 `select tidb_version();`。
2. 立刻修改 root 密码：`ALTER USER 'root'@'%' IDENTIFIED BY '你的新密码';`，然后在中控机上删除密码文件 `rm /var/lib/tidb-deploy/<集群名>/root-password.txt`。
3. 登录 Grafana（摘要里的地址，账号 `admin` / `admin`）并立即修改密码。
4. 查看集群状态：`su - tidb -c 'tiup cluster display tidb-test'`，所有实例的 Status 应为 Up（含义见官方 [tiup cluster display](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-display)）。
5. 控制中控机的访问权限：部署用户在每台主机上拥有免密 root 等价权限，其私钥无口令，见下文「对目标主机的修改」。
6. 如需让本平台管理这套集群，到「[权限与审计](go:security)」「部署自检」逐项确认，再按「平台自身的安装」操作。

#### 失败怎么办

| 现象（脚本输出） | 原因 | 处理 |
|---|---|---|
| 配置文件不存在: … | `--config` 路径写错 | 核对当前目录和文件名 |
| 中控机上没有用户 tidb / tidb@<IP> 免密登录失败 / 可以登录，但 sudo 免密不可用 / 前置条件未满足 | 部署用户、sudo 免密或 SSH 免密登录没配好（含中控机自己）；脚本不会代你配置 | 按输出里列出的步骤和命令配置（同「准备」一节的 ①~⑤）；用 `ssh tidb@<IP> 'sudo -n true'` 逐台复测；核对 `DEPLOY_USER`、`SSH_PORT`、`SSH_KEY` |
| 无法连接 <IP>（以 tidb@<IP> 登录并 sudo）：… | 前置条件检查通过后又连不上：网络波动、端口不对、私钥被换 | 用 `ssh` 单独验证；核对 `SSH_PORT`、`SSH_KEY` |
| 环境检查未通过（N 项失败） | 资源、数据盘、端口、AVX2、版本门槛等 | 按每条 ✖ 的文字处理；资源不足的测试机可改 `SPEC=dev`；端口被占用需先停掉占用进程 |
| 在 /opt/tidb-pkg 中没有找到 … 对应的离线安装包 | 目录、文件名、架构或版本不对 | 脚本会列出该目录已有版本；核对绝对路径、文件名保持 `tidb-community-server-vX.Y.Z-linux-<架构>.tar.gz`、架构与中控机一致 |
| 主机 <IP> 初始化失败，详见日志 … | 依赖安装失败（yum 源不可用）、sudo 不免密等 | 看日志；CentOS 7 / Linux 8 yum 源失效可设 `REPO_FIX=auto`；离线环境设 `OFFLINE_RPM_DIR`；修复后重跑 |
| tiup check 仍有 N 项 Fail | check --apply 修不了的项（如 ext4 缺 `nodelalloc`） | 按 Fail 的提示手工修复；仅测试环境（dev 规格）可勾选「忽略 tiup check 的失败项」（即 `ALLOW_CHECK_FAIL=true`）继续 |
| 集群 X 已存在，跳过 deploy | 同名集群已在 TiUP 里 | 这是预期保护。想重来先 destroy（见下） |
| 集群启动失败，详见 … | 组件启动失败 | 看日志与 `tiup cluster display`，到对应主机查组件日志，参考[故障排查](go:trouble) |
| PD 120 秒内未就绪 / TiDB 120 秒内未就绪 | 仅警告，集群可能仍在启动 | 稍后 `tiup cluster display <集群名>` 复查 |

> ⚠️ 如果 `deploy` 阶段中途失败后再次执行，提示「集群 X 已存在，跳过 deploy」，不要直接当作成功：先用 `su - tidb -c 'tiup cluster display <集群名>'` 看实际状态；不确定时可 destroy 后重来（是否会留下半成品元数据以真实环境为准）。

**销毁集群（⚠️ 不可恢复）**：

```bash
sudo ./tidb-deploy.sh destroy --config deploy.conf --confirm-name tidb-test
```

> ⚠️ `destroy` 会停止服务并删除每个服务的日志目录、部署目录和数据目录，**全部数据不可恢复**（官方 [tiup cluster destroy](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-destroy)）。必须同时给出 `--confirm-name <集群名>`，`--yes` 不能代替，之后还会再确认一次；成功后删除阶段记录。它**不会**还原系统配置（swap、内核参数、sudoers 等）。

### 一键部署（中控机模式）

> 只有**运维模式 + 中控机模式**（平台装在 tiup 中控机上，`install.sh --local-tiup`）、当前账号有 `settings.edit`（平台管理员）时，页面上方才有「保存配置」条，右侧才有「一键部署」标签。静态原型、只读模式、远程接入的平台上不出现。

这个功能把上面「手工执行」的那一套搬进页面：**保存配置 → 预检 → 提交申请 → 另一名管理员批准 → 输入中控机 root 密码开始部署 → 实时看全部输出**。执行的就是包内的 `tidb-deploy.sh`（与手工执行完全相同的脚本和配置），页面只是替你启动它、把输出原样拿回来。

**先决条件**（缺哪项，对应步骤会失败并给出原因）：

| 项 | 要求 | 不满足的表现 |
|---|---|---|
| 平台形态 | 中控机模式；Linux；有 `su` 与 `systemd-run`（CentOS 7 的 systemd 219 即可） | 「一键部署」标签不出现，或点预检时提示 su / 启动失败 |
| 部署用户 | 与手工部署相同：所有机器（含中控机）上有 `tidb`、sudo 免密、能免密 SSH 登录所有机器 | 预检里标 ✖ 并列出缺哪台、缺什么；预检不通过不能提交申请 |
| 两名管理员 | 申请人不能批准自己的申请 | 只有一名管理员时，申请停在「待批准」 |
| 中控机 root 密码 | 知道这台 CentOS 的 root 密码，并且 `su` 允许 `tidbops` 账号使用（没有被 `pam_wheel` 之类限制） | 密码错误提示「没有任何命令被执行」；其他原因会显示 su 的原话 |

**步骤：**

1. 在左侧五个卡片里照常填写（和手工部署完全一样）。页面上方点「**保存配置**」：平台在服务端重新校验，通过后把配置存为一个**带版本号的不可变文件**（v1、v2……，权限 600，记入审计）；内容与最新版本完全相同则不新建版本。上方条里显示「已保存 v1，与表单一致」才能往下走——**预检和部署只使用已保存的版本**，改了表单要重新保存。
2. 右侧点「**一键部署**」标签。表单会收起、输出区占满宽度。
3. 「1 · 预检」：输入中控机 root 密码，点「开始预检」。这是脚本的 `check`，**只读，不改任何机器**。下方实时显示自检日志与「通过 / 警告 / 失败」计数，失败项（✖）会单独列在最上面。预检失败就按提示处理（命令见「命令」标签第 0 步），再点一次。
4. 预检通过后，在「最近一次预检」旁点「**提交部署申请**」。预检结果 24 小时内有效。
5. 「2 · 部署申请」：**另一名管理员**登录后（同一个页面、同一个标签）在这里点「批准」。申请 1 小时内要有人批准。
6. 批准后，在同一块里输入**集群名**以确认，再输入**中控机 root 密码**，点「开始部署」。
7. 「3 · 输出」：左上是阶段（初始化主机 → 安装 TiUP → 生成拓扑 → tiup check → tiup deploy → 启动集群 → 验证），下面是脚本的全部输出，可按「全部 / 警告 / 错误」筛选，页面每 2 秒更新。可「下载完整输出」。
8. 成功后输出区下方出现「访问信息」（TiDB 版本、SQL 连接、Dashboard、Grafana），以及**新集群的 TiDB root 随机密码**——点「显示」只能看**一次**，平台不再保存；同时有按钮「导入到『集群接入』，开始运维」。
9. 失败或被取消：已完成的阶段不会重做。修好原因后，在「部署申请」里点「**从断点继续部署**」（需要再输入 root 密码）。申请批准后 24 小时内有效。

**离线部署（内网没有外网时怎么用一键部署）：**

一键部署**不负责传离线包**——平台没有提供浏览器上传，离线包要你自己先放到中控机上，其余步骤与在线完全一样：

1. **在能联网的机器上下载**：从本页「离线包与工具下载」标签取得部署工具包和下载命令，执行（`--tidb-version` 必须是完整版本号；`--target-os` 让脚本提前检查版本与目标系统是否兼容，CentOS 7 只能装 ≤ v8.3）：`./tidb-deploy.sh download-offline --tidb-version v7.5.4 --dir /opt/tidb-pkg --arch amd64 --with-toolkit --target-os rocky9`。得到 `tidb-community-server-v7.5.4-linux-amd64.tar.gz`（可选 `tidb-community-toolkit-…`）并已校验 SHA-256。
2. **拷到中控机**：整个目录拷过去（如 `/opt/tidb-pkg`），再核对一次 sha256。
3. **在本页第 4 步选「离线安装」**，填「离线包所在的完整目录」（中控机上的绝对路径，只能含字母、数字和 `_ . / -`，不能有空格）。「校验」标签应显示「离线安装：脚本会在 … 中按版本自动查找 …」。选「自动」或「指定系列」时，脚本取目录里该系列的**最高版本**；选完整版本时，目录里必须有这一版。
4. **保存配置**，然后和在线一样：「一键部署」标签 → 预检 → 提交申请 → 批准 → 输入集群名与 root 密码 → 部署。

离线时要注意：

| 项 | 说明 |
|---|---|
| 预检会检查离线包 | 目录不存在、不可读，或没有所选版本的 `tidb-community-server-…` 包时，预检输出会报错，并列出目录里已有的版本；处理好再重新预检 |
| 谁来读这个目录 | 是中控机上的 root（脚本由 root 在平台沙箱之外运行），**不需要**给 `tidbops` 账号授权 |
| TiUP 本身 | 也从离线包里装（`local_install.sh`），中控机不需要访问外网；有 toolkit 包时还会 `tiup mirror merge` |
| 依赖 rpm | 目标机没有可用 yum 源时，需要自己准备 `numactl`、`chrony`、`libaio` 等 rpm，并在第 4 步填「依赖 rpm 所在目录」。按脚本的实现，这个目录是在**每台目标机上**检查并安装的，所以要把 rpm 拷到每台机器的同一路径（未在真实环境验证） |
| 版本与系统 | 页面与脚本都会拒绝目标系统不支持的版本（如 CentOS 7 + v8.4 及以上） |
| 页面不检查目录是否存在 | 路径在中控机上，页面看不到；以预检结果为准 |

**关于 root 密码（务必读）：**

- `tidb-deploy.sh` 要求 root。平台用你输入的密码通过 `su - root` 启动一个**临时 systemd 单元**（`systemd-run`）去运行脚本——这样脚本不受平台服务沙箱的限制（平台服务本身是只读文件系统沙箱，脚本要改 `/etc`、sysctl 等，在沙箱里做不了）。
- 密码只在你点按钮的那一刻从输入框取出，经 HTTPS 发给后端，**只交给 su 一次，然后立即清零**；不写入配置、任务记录、输出日志、审计和下载文件。输入框发出后立即清空；刷新页面不会找回它。
- 连续输错 3 次，这个账号被锁定 10 分钟（防止被用来猜 root 密码）。输错时没有任何命令被执行，也不留任务记录。
- 取消、从断点继续都要**重新输入**，因为没有保存。
- 系统日志 `/var/log/secure` 里会有 `su` 的记录；平台审计里有对应的 `deploy.*` 记录（谁、什么时间、哪个集群、哪个配置版本、申请号），两边可以对上。

**其他规则：**

| 规则 | 说明 |
|---|---|
| 同一时间只有一个任务 | 有预检或部署在运行时，再点会提示忙；运行超过 6 小时仍没有退出码的任务显示「状态未知」，不再占用 |
| 配置不可变 | 申请时锁定配置版本与其 sha256；之后即使又保存了新版本，这次部署仍用申请时的那一版 |
| 平台重启不丢任务 | 任务状态从中控机上的退出码文件推导；部署脚本由 systemd 管理，平台重启不影响它继续跑 |
| 权限 | 全部接口需要 `settings.edit`（平台管理员）；批准的人不能是申请人 |
| 不做的事 | 不销毁 / 重装集群（`destroy` 仍需手工，且要 `--confirm-name`）；不创建用户、不配 sudo、不分发密钥 |

> ⚠️ **验证状态**：这条链路（`su` 经伪终端取得 root → `systemd-run` 起任务 → 回读输出）的各个环节有单元测试和假执行器下的端到端测试，**没有在真实 CentOS / Rocky 上跑过**。尤其是：平台服务单元里有 `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX`，`su` 的 PAM 审计模块若需要 netlink 可能报错；`su` 被 `pam_wheel` / `securetty` 限制时会失败。首次使用请在测试机上完整走一遍，并对照「常见问题」。

### 完整示例：在 8 台 Rocky Linux 9 上离线部署 v7.5 集群

场景：内网无外网，中控机是 10.0.1.4（同时是 PD，所以它在部署主机清单里），共 8 台主机：3 台 PD、2 台 TiDB、3 台 TiKV，操作系统用户 `tidb`，版本用 v7.5 LTS。

0. 先在 8 台机器上完成「准备」一节的部署用户配置：每台都有 `tidb` 用户和 sudo 免密，中控机 10.0.1.4 上的 `tidb` 能免密 SSH 登录 8 台机器（包括它自己）。
1. 联网机器上执行第 3 步的 `download-offline`（`--tidb-version v7.5.4 --arch amd64 --with-toolkit --target-os rocky9`），把 `/opt/tidb-pkg` 与工具包拷到中控机。
2. 页面填写：目标系统「Rocky Linux 9」；「多机集群」，集群名 `tidb-test`；用户 `tidb`；SSH 端口 22，私钥路径留空（默认 `~tidb/.ssh/id_rsa`）；版本「指定系列 v7.5」；「离线安装」，目录 `/opt/tidb-pkg`；PD `10.0.1.4 10.0.1.5 10.0.1.6`，TiDB `10.0.1.1 10.0.1.2`，TiKV `10.0.1.7 10.0.1.8 10.0.1.9`；规格 `prod`。
3. 「校验」应显示「TiDB v7.5 与 Rocky Linux 9 兼容」「离线安装：脚本会在 /opt/tidb-pkg 中按版本自动查找 …」「配置通过校验」，下载 `deploy.conf`。
4. 在中控机依次执行第 4 到第 9 步。
5. 完成后记下摘要里的 `root-password.txt` 位置，登录改密码并删除该文件，同时修改 Grafana 默认账号的密码。

> ⚠️ 示例里选了 `prod` 规格：任一台机器 CPU / 内存 / 数据盘 / 文件系统不达标，脚本会直接中止。测试机资源偏低时请用 `dev`（只警告）。

## 平台自身的安装、升级与回滚

这一部分针对**运维平台程序**（tidbops-server），与上面的 TiDB 集群部署无关。以下命令都在**平台所在的中控机**上执行；程序以发布包 `tidbops-<版本>-linux-<架构>.tar.gz`（旁边有同名 `.sha256`）分发，`<版本>` 是发布包文件名里的版本号，如 `1.2.0`。包内有三个包装脚本 `install.sh`、`upgrade.sh`、`rollback.sh`，它们只检查 root 与 CPU 架构，再调用 `tidbops-server admin` 的对应子命令。

目标机上的目录：

| 路径 | 内容 |
|---|---|
| `/opt/tidbops/releases/<版本>/` | 各版本程序；`/opt/tidbops/current` 是指向当前版本的符号链接 |
| `/var/lib/tidbops` | 数据：用户、审计、变更、设置、`upgrades.jsonl` 升级历史、`VERSION` 数据 schema（目录权限 0700，属主 tidbops） |
| `/var/backups/tidbops/` | 升级前自动备份与手动备份 |
| `/etc/tidb-ops/` | 执行器策略 `exec.json`、令牌密钥 `exec.key`、口令文件 `env` / `exec.env`；升级不改动，**需自行纳入备份** |
| `/etc/systemd/system/tidbops.service`、`/etc/sudoers.d/tidbops` | 服务单元与受限 sudo 入口 |

### 首次安装（install.sh）

**先选部署形态**（`install.sh` 的参数不同）：

| 形态 | 命令 | 适用 |
|---|---|---|
| **中控机模式（推荐）** | `sudo ./install.sh --local-tiup` | 平台装在 tiup 中控机上、不预设集群；登录后在「集群接入」**一键导入**本机 tiup 管理的集群并做全部运维；手工添加的远程集群只读 |
| 独立部署 | `sudo ./install.sh --standalone` | 平台装在任意机器上，只通过网络地址**只读**接入各集群；不需要 tiup / `tidb` 用户 / sudo |
| 同机部署（旧） | `sudo ./install.sh --cluster <集群名>` | 安装时绑定一个 tiup 集群（下面的步骤示例用的就是它） |

三者互斥（`--local-tiup` 隐含 `--standalone`，都不能与 `--cluster` 同用）。中控机模式的前置条件与导入步骤见发布包里的 `docs/运维部署手册.md` 5.1 节：SSH 密钥**不交给平台**，只要部署用户（如 `tidb`）能无交互运行 `tiup cluster display`；平台运行账号 `tidbops` 需能读 `meta.yaml`，但不要放开 `ssh/` 目录。安装结束会打印「访问信息」（访问地址与端口、账号 `admin`、初始口令——只显示一次、各目录绝对路径）。

**前置条件**（同机部署 / 中控机模式；纯独立部署不需要 `tidb` 用户与 `visudo`）：root；`tidb`（或 `--tidb-user` 指定的）部署用户已存在；系统装有 `visudo`（`sudo` 包自带）；发布包已校验。

#### 安装 1：校验并解压发布包

```bash
sha256sum -c tidbops-1.0.0-linux-amd64.tar.gz.sha256
tar xzf tidbops-1.0.0-linux-amd64.tar.gz && cd tidbops-1.0.0-linux-amd64
```

第一条必须输出 `tidbops-1.0.0-linux-amd64.tar.gz: OK`，否则重新获取发布包。

#### 安装 2：预演

```bash
./bin/tidbops-server admin install --cluster prod-tidb-01 --env prod --dry-run
```

`prod-tidb-01` 是你的 tiup 集群名（用 `su - tidb -c 'tiup cluster list'` 查看，只含字母数字与 `_-`，最长 64）。预演不需要 root，缺少前置条件时只给 `[dry-run] 警告`。你会看到：

```
发布包：tidbops 1.0.0（linux/amd64，数据 schema 1），校验通过
将部署 tidbops 1.0.0：集群 prod-tidb-01，环境 prod，tidb 用户 tidb，监听 127.0.0.1:8080
[dry-run] 1/12 创建运行账号 tidbops
[dry-run] 2/12 创建目录并设置权限
...
[dry-run] 12/12 systemctl daemon-reload
[dry-run] 以上步骤均未执行，系统未做任何改动
```

随后打印「下一步（依次执行）」清单，内容与下面「安装 4」一致。

#### 安装 3：正式安装

```bash
sudo ./install.sh --cluster prod-tidb-01 --env prod
```

参数：

| 参数 | 含义 | 可选值 / 默认 |
|---|---|---|
| `--cluster` | tiup 集群名（同机部署必填；与 `--standalone` / `--local-tiup` 互斥） | 无 |
| `--local-tiup` | 中控机模式（隐含 `--standalone`）：不预设集群，可一键导入本机 tiup 管理的集群 | — |
| `--standalone` | 独立部署：不预设集群，只读接入远程集群，不需要 tiup / `tidb` 用户 | — |
| `--env` | 本集群所属环境 | test / staging / prod，默认 prod；决定审批级数等 |
| `--tidb-user` | tiup 部署用户（受限 exec 入口以该用户身份运行；必须已存在） | 默认 tidb |
| `--listen` | Web 服务监听地址 | 经 `install.sh` 安装默认 0.0.0.0:8018（单元带 `--allow-remote`）；直接运行 `admin install` 默认 127.0.0.1:8080 |
| `--public-url` | 平台对外访问地址（通知消息里的链接用） | 默认空 |
| `--dry-run` | 只打印每一步，不改动任何东西 | — |
| `--start` | 安装后启动服务并做健康检查 | 默认不启动 |
| `--force` | 已安装时重装**当前同一版本**（不覆盖已有密钥与配置文件）；升级请用 upgrade | — |

你会看到逐步输出「== 1/12 创建运行账号 tidbops ==」……最后「安装完成：tidbops 1.0.0」。安装做的事（需要 root）：创建运行账号 `tidbops`（系统账号，无登录 shell）→ 创建目录并设置权限 → 安装 release 并切换 `current` → 生成令牌密钥 `/etc/tidb-ops/exec.key`（已存在则保留）→ 由包内样例生成 `/etc/tidb-ops/exec.json`（已存在则保留）→ 创建口令文件 `exec.env` 与 `env` → 安装 sudoers（只放行受限 exec 入口，先 `visudo -cf` 校验）→ 写入 systemd 单元 → 写数据目录 `VERSION` → `systemctl daemon-reload` →（`--start` 时）启用并启动服务、健康检查。

> ⚠️ 安装会新增系统账号 `tidbops`、systemd 单元和 `/etc/sudoers.d/tidbops`。已存在的密钥与配置文件不会被覆盖。

安装器还会为执行器准备运行条件：建临时目录 `/var/lib/tidbops-exec`（属主部署用户、0700，`exec.json` 的 `tempDir` 默认指向它），并写 systemd drop-in `/etc/systemd/system/tidbops.service.d/exec-rw.conf`，放开沙箱里 `~tidb/.tiup` 与该临时目录的可写权限（服务沙箱默认让 `/home` 只读、`/tmp` 私有，不放开的话 `tiup` 经执行器运行会失败）。已有的 `exec.json` 不会被改，`tempDir` 还在 `/tmp` 下时安装输出会提示你改。

**重装 / 卸载后重装要注意**：
- `--force` 重装**不会重启已经在运行的服务**，装完要 `sudo systemctl daemon-reload && sudo systemctl restart tidbops`，再用 `systemctl cat tidbops | grep ExecStart` 确认参数已变。
- `admin uninstall` 默认保留数据目录、`/etc/tidb-ops`、备份和 `tidbops` 账号：数据目录里已有 `users.json` 时**不再生成初始密码**，沿用原来的 `admin` 与口令；旧的 `exec.json` 会被保留。想全新开始，把 `/var/lib/tidbops` 改名挪走、旧 `exec.json` 改名。
- 针对 `tidbops` 的 ACL（读 `meta.yaml`）记的是用户 ID，账号重建后要重做。
- **不要在发布包解压目录里再解压别的东西**（比如 `deploy/tidb-deploy/` 里的部署工具包）：安装器按 `MANIFEST.json` 校验，多出任何文件都会报「发布包里有清单之外的文件」并拒绝安装，删掉多出来的目录即可。
- 旧的「同机部署」（带 `--cluster`）想改成中控机模式：先 `admin backup`，再 `sudo ./install.sh --local-tiup --env prod --force --start`，然后**手工重启服务**；见 `docs/运维部署手册.md` 5.9（未在真实环境验证）。

#### 安装 4：安装后必须完成的配置

**（1）管理员账号**：安装器默认建好**两名管理员 `admin` 和 `admin2`**，两个账号的初始口令都打印在安装结束的「访问信息」里（只显示这一次，请分别记下，登录后各自改密）。写操作开关、授予管理员都要两名不同的管理员确认，所以默认就建两个。**只有**数据目录里已有用户（重装保留了数据）或输出提示创建失败时，才需要按下面的方法手工补建。页面里「授予管理员」本身也要另一名管理员确认，所以只有一名管理员时，第一名额外的管理员**必须用命令行创建**。服务运行时不要改用户库（会被内存里的版本覆盖），服务已启动时先 `sudo systemctl stop tidbops`，建完再 `sudo systemctl start tidbops`（启动后会话失效，重新登录）；有了两名管理员之后，其他用户在侧栏「平台配置 → 用户管理」里建。口令从环境变量读取，要求至少 10 位、含大小写字母 / 数字 / 符号中的 3 类、且不含用户名：

```bash
sudo -u tidbops env TIDBOPS_USER_PASSWORD='强口令1' /opt/tidbops/current/bin/tidbops-server user add --data-dir /var/lib/tidbops --name alice --role admin
sudo -u tidbops env TIDBOPS_USER_PASSWORD='强口令2' /opt/tidbops/current/bin/tidbops-server user add --data-dir /var/lib/tidbops --name bob --role admin
```

成功输出「完成」。命令里的口令会进入 shell 历史，执行后请清理历史。再按需用 `--role dba`、`sre`、`approver`、`auditor`、`developer` 创建其他用户（`--env` 可限定环境）。

**（2）找回自动创建的 admin**：如果安装时用了 `--start`（或先启动了服务）而此时还没有任何用户，服务首次启动会自动创建管理员 `admin`，并把**随机初始口令只在服务日志里打印一次**：`journalctl -u tidbops | grep 首次启动`。登录后立即修改。

**（3）建两个数据库账号**：只读账号（慢 SQL / 配置采集，口令写进 `/etc/tidb-ops/env`）与运维账号（仅 SET GLOBAL / SET CONFIG，口令写进 `/etc/tidb-ops/exec.env`）。

**（4）编辑 `/etc/tidb-ops/exec.json`**：`tiupBin`（tiup 路径）、mysql 运维账号、`backupRoots`（备份存储路径前缀）、`alertURL`（Alertmanager 地址）。

**（5）验证 sudo 入口**，应返回校验失败的 JSON，而不是 sudo 报错：

```bash
echo '{}' | sudo -u tidbops sudo -n -u tidb /opt/tidbops/current/bin/tidbops-server exec --stdin
```

**（6）启动并放到反向代理后面**：`systemctl enable --now tidbops`，并配置带 TLS 与访问控制的反向代理指向监听地址。

**（7）开启写操作**：打开平台，登录后到「[权限与审计](go:security)」「部署自检」确认各项为绿色，再由两名**不同**的管理员依次在「写操作开关」确认「开启写操作」（每个集群各自一个开关；开之前先核对集群的环境，导入时选错了在「集群接入 → 编辑」里改）。管理员角色**不能发起也不能审批集群变更**，要做扩缩容、改配置需另建 dba / sre 与 approver 账号并按环境授予。紧急只读时执行 `touch /etc/tidb-ops/READONLY`。

**（8）导入时 / 导入后常见问题**：
- 采集慢查询报 `ERROR 2059 … mysql_native_password cannot be loaded`：平台机上的 `mysql` 是 MySQL 官方 9.x 客户端，已移除该认证插件；换 `dnf install -y mariadb`，并在「集群接入 → 编辑」把「mysql 可执行文件」填成 `mariadb`。
- 导入时报「读取本机 tiup 集群列表失败：退出码 1」：多半是服务沙箱让 `~tidb/.tiup` 只读，按 `docs/运维部署手册.md` 11.4 的「退出码 1 的定位」排查，旧安装升级一次会自动补 drop-in（5.1 步骤 4），`tempDir` 仍要自己改。
- 总览横幅 `tiup … executable file not found in $PATH`：旧版本在 Web 进程里直接执行 tiup（平台账号没有它）；新版本已改走受限执行器。
- 页面访问被 nginx 返回 403：反向代理里 `allow` 没有包含你的来源 IP（含服务器本机 `127.0.0.1`），见 `docs/运维部署手册.md` 11.2。

### 一键升级（upgrade.sh）

> ⚠️ **升级会先停止服务**，期间平台页面、审批、定时变更调度都不可用，通常约 10 到 30 秒（不影响 TiDB 集群本身）。

**升级前检查清单**

1. 没有「执行中」的变更；没有进行中的主机维护。预检只拦截状态为「执行中」的变更；处于「已排期」的定时变更会在平台重启后到点（或已过点）立即执行，处于「排队中」的变更不会被自动恢复，请升级前先到「[变更排期](go:schedule)」处理。
2. 通知使用者升级窗口。
3. 备份 `/etc/tidb-ops/`（策略与密钥不在自动备份范围内）。如需额外保险，先 `sudo /opt/tidbops/current/bin/tidbops-server admin backup` 做一次手动备份。
4. 发布包已校验并解压。

**步骤**

```bash
sha256sum -c tidbops-1.2.0-linux-amd64.tar.gz.sha256
tar xzf tidbops-1.2.0-linux-amd64.tar.gz && cd tidbops-1.2.0-linux-amd64
sudo ./bin/tidbops-server admin upgrade --dry-run
sudo ./upgrade.sh
```

第一条必须显示 `OK`。第三条预演只做预检并打印计划，不改动任何东西；`./upgrade.sh` 包装脚本本身要求 root，即使加 `--dry-run` 也一样；直接运行二进制的 `--dry-run` 可不用 root，但数据目录属主是 `tidbops`、权限 0700，普通账号读不了，「数据 schema」「执行中的变更」两项可能显示权限错误，所以建议加 `sudo`（根据代码推断，以真实环境为准）。

预演你会看到：

```
预检：
  ✓ root 权限
  ✓ 已安装：当前版本 1.0.0（环境 prod，集群 prod-tidb-01，监听 127.0.0.1:8080）
  ✓ 发布包：tidbops 1.2.0（linux/amd64，数据 schema 2），哈希校验通过
  ✓ 版本比较：1.0.0 → 1.2.0
  ✓ minFrom：不限
  ✓ 数据 schema：数据 schema 1 → 2（将执行数据迁移，已备份可恢复）
  ✓ 磁盘空间：足够
  ✓ 执行中的变更：没有

计划：升级 1.0.0 → 1.2.0
  1. 停止服务 tidbops 并等待退出
  2. 备份数据目录到 /var/backups/tidbops（tar.gz，0600）
  ...
  9. 写升级历史；保留最近 3 个 release 与最近 10 个备份

[dry-run] 预检通过；以上步骤均未执行，系统未做任何改动
```

预检任一项失败即停止，dry-run 会跑完全部。失败项含义：版本比较「不允许降级」或「已经是 X，无需升级」；minFrom「请先升级到中间版本」；数据 schema「发布包 schema 低于数据目录 schema，拒绝升级」；执行中的变更「有正在执行的变更（CHG-xxxx）」；磁盘空间不足。

参数：

| 参数 | 含义 | 可选值 / 默认 |
|---|---|---|
| `--dry-run` | 只打印预检结果与计划 | — |
| `--force` | 忽略「有执行中的变更」（该变更会被标记为中断）；允许重新应用同版本 | — |
| `--allow-downgrade` | 允许降级（通常应使用 rollback） | — |
| `--no-backup` | 跳过数据备份（危险：迁移失败时无法恢复数据） | — |
| `--keep N` | 保留最近 N 个 release（current 与上一个版本永不删除） | 默认 3 |
| `--timeout` | 健康检查总超时 | 默认 60s |

正式升级你会看到逐步输出：「== 停止服务 ==」「== 备份数据目录 ==」（`备份：/var/backups/tidbops/<时间>-1.0.0.tar.gz`）「== 安装新 release ==」「== 更新 systemd 单元 / sudoers ==」「== 数据迁移 ==」「== 切换 current 链接 ==」「== 启动服务并健康检查 ==」，最后「升级完成：1.0.0 → 1.2.0」和备份文件位置。成功后清理旧 release（保留 `--keep` 个）和旧自动备份（保留最近 10 个）。

**验证**

1. `sudo /opt/tidbops/current/bin/tidbops-server admin status`：「当前版本」应是新版本，「服务状态」为 active，「最近升级历史」最后一行形如「upgrade  1.0.0 → 1.2.0  成功」。
2. 浏览器刷新平台，侧栏标题下的版本号变为新版本；「[环境与设置](go:settings)」末尾「关于与升级」卡片里最近升级历史新增一行「✔ 成功」。
3. 到「权限与审计 → 部署自检」再看一遍。

**升级失败时会发生什么**：任一步失败会**自动回滚**：停服务、（数据迁移已开始且有备份时）恢复备份、切回旧链接、启动旧版本并健康检查，结果写进 `upgrades.jsonl`。你会看到：

```
✖ 升级失败：数据迁移失败: …
正在自动回滚到 1.0.0 …
  数据迁移已开始，将恢复升级前备份 /var/backups/tidbops/….tar.gz
...
已自动回滚到 1.0.0，服务健康检查通过
错误: 升级到 1.2.0 失败：…；已自动回滚到 1.0.0
```

命令退出码为 1。此时平台已恢复到旧版本，关于与升级卡片里该行显示「已回滚」。如果自动回滚本身也失败，输出「自动回滚到 1.0.0 也失败 … 现场已保留（升级前备份：…），请人工处理」，不会再做第二次自动操作，请保留现场按下面的手动回滚或 `restore` 处理。

> ⚠️ `--force` 会让执行中的变更被标记为「失败（被中断）」，可能残留驱逐 Leader 调度等中间状态，需人工核对集群。`--no-backup` 在数据 schema 发生变化时无法恢复数据。自动备份**不会**在升级成功后立刻删除，除按 `--keep` / 最近 10 个自动清理外，请定期检查 `/var/backups/tidbops/` 的磁盘占用。

### 手动回滚（rollback.sh）

**什么时候用**：升级成功了，但新版本有问题，想回到旧版本。

```bash
sudo /opt/tidbops/current/bin/tidbops-server admin rollback --dry-run
sudo ./rollback.sh
```

第一条先看计划；`./rollback.sh`（在发布包目录里）是包装脚本，等价于调用 `/opt/tidbops/current/bin/tidbops-server admin rollback`。你会看到：

```
回滚计划：1.2.0 → 1.0.0
  数据：不恢复，只切换二进制（数据 schema 2，目标版本支持到 2）
[dry-run] 未执行任何改动
```

如果上次升级迁移过数据 schema，则变成：

```
  数据：恢复备份 /var/backups/tidbops/….tar.gz（上次升级把数据 schema 从 1 迁移到 2）
  ⚠ 警告：恢复会丢失升级之后产生的审计 / 变更 / 用户等数据变更；恢复前会先把当前数据另存一份（-pre-restore）
```

正式回滚输出「== 停止服务 ==」「（恢复数据）」「== 切换到 1.0.0 ==」「== 启动服务并健康检查 ==」，最后「回滚完成：1.2.0 → 1.0.0」。

| 参数 | 含义 |
|---|---|
| `--to <版本>` | 回到指定版本，默认回到「把当前版本升上来的那次升级」的来源版本；没有升级记录时回到次高的已安装版本 |
| `--restore-data` | 强制恢复升级前的数据备份 |
| `--no-restore-data` | 只切换程序，不恢复数据；目标版本读不了当前数据 schema 时会被拒绝 |
| `--backup <文件>` | 指定要恢复的备份文件（配合 `--restore-data`） |
| `--dry-run` | 只打印回滚计划 |

> ⚠️ **恢复备份意味着升级之后产生的审计、变更、用户等数据全部丢失**（恢复前会先另存一份 `-pre-restore`，可以手工找回）。回滚前先确认是否真的需要恢复数据。回滚失败时现场会保留，不做二次自动操作。

### admin 子命令一览

用法：`sudo ./bin/tidbops-server admin <子命令>`，各子命令 `-h` 有中文说明。公共参数 `--package DIR`（默认取二进制上两级，必须有 MANIFEST.json）与 `--root DIR`（仅演练，生产不要设置）。退出码：成功 0，运行失败 1，用法错误 2。

| 子命令 | 作用 | 主要参数 | 需要 root | 后果 |
|---|---|---|---|---|
| `status` | 查看当前版本、环境 / 集群 / 监听、已安装 release、服务状态、数据 schema、最近 5 条升级历史、可用备份 | `--json` | 否（读不到的部分会提示需要 root 或 tidbops 账号） | 只读 |
| `backup` | 手动备份数据目录（文件权限 0600） | `--out FILE`（默认 `/var/backups/tidbops/<时间>-<版本>-manual.tar.gz`；不能放在数据目录内） | 是 | 服务运行中也可备份；要严格一致的快照请先停服务。`-manual` 备份不会被自动清理 |
| `restore` | 从备份恢复数据目录 | `--file FILE`（必填） | 是 | 会停止服务（若在运行）、先备份当前数据为 `-pre-restore`、**覆盖**数据目录，备份之后的数据丢失；归档含路径穿越 / 绝对路径 / 符号链接则整体拒绝；恢复的 schema 高于当前程序支持时服务保持停止，需先 rollback |
| `migrate` | 把数据目录迁移到本程序的 schema（幂等；升级内部会调用） | `--data-dir`（必填） | 否 | 修改数据文件；数据 schema 高于程序支持时失败。建议用 tidbops 账号运行 |
| `uninstall` | 卸载服务 | `--dry-run`、`--yes`、`--purge-data` | 是 | 停止并禁用服务，删除 systemd 单元、sudoers 和 current 链接；默认保留数据、备份、releases、`/etc/tidb-ops` 与账号。`--purge-data` 需再加 `--yes`，会**永久删除数据目录与全部备份** |
| `install` / `upgrade` / `rollback` | 见上文 | 见上文 | 是（dry-run 除外） | 见上文 |

升级与回滚的锁文件是 `/var/lock/tidbops-admin.lock`：不检测僵尸锁，进程被强杀后需要手动删除。

### 在页面里查看版本与升级历史

登录运维模式后，「[环境与设置](go:settings)」页末尾的「关于与升级」卡片显示：版本、提交、构建时间、Go 版本、系统 / 架构、数据 schema（与程序支持的 schema 不一致时标「待迁移」或「数据较新」）、已运行时长、当前 release 与已安装版本，以及最近 10 条升级历史（时间、动作、版本、结果、是否已回滚、备份文件、操作人），并附升级 / 回滚操作说明。侧栏标题下的版本号来自 `/api/health`。如果提示「未按发布包布局安装」，说明当前进程不是从 `/opt/tidbops/releases/<版本>/bin/` 启动的（开发或手工部署），此时升级历史与版本号仍可查看，但不能用 `upgrade.sh` 管理。

## 区域与参数说明

### 1 · 目标系统与模式

| 字段 / 选项 | 含义 | 可选值 / 默认 | 影响 |
|---|---|---|---|
| 目标操作系统 | 部署目标机的系统，用来校验 TiDB 版本能否运行、选择自动版本系列、生成下载命令的 `--target-os` | 默认 Rocky Linux 9。下拉：CentOS 7.x、CentOS Linux 8、CentOS Stream 8 / 9 / 10、Rocky Linux 8 / 9 / 10、AlmaLinux 9 / 10、RHEL 9 / 10（10 系 glibc 2.39，本工具未在 10 系上实测；部署脚本在 10 系上会提示「系统主版本 10 未经本工具验证」，仅警告不阻止） | **不会写进 deploy.conf**；脚本在中控机和各目标机上自己检测系统与 glibc。CentOS 7 / CentOS Linux 8 出现「上游已停止维护、yum 源可能失效」警告，CentOS 7 还会提示「只能装 ≤ v8.3」，并在版本选择里禁用 v8.4 及以上 |
| 部署模式 | single：单机，全部组件装在一台机器上；cluster：多机 | 默认 single | single：1 个 PD + 1 个 TiDB + 1 个 TiKV + 监控，TiKV 副本数设为 1，**仅用于开发测试**，无高可用，建议 ≥ 8 核 / 16GB；cluster：按主机清单部署 |
| 集群名 | TiUP 里的集群名 | 默认 tidb-test；字母或数字开头，只能含字母 / 数字 / 下划线 / 连字符，最长 32 位 | 不合法时「校验」报红；同名集群已存在时脚本跳过 deploy 并警告，不会覆盖 |

### 2 · 操作系统用户

| 字段 / 选项 | 含义 | 可选值 / 默认 | 影响 |
|---|---|---|---|
| 运行 TiUP 与集群的用户 | 部署后 TiUP 和集群进程使用的普通系统用户 | 下拉框：tidb（官方默认，默认值）、自定义…；自定义时输入用户名 | 对应 `DEPLOY_USER`。不能是 root，也不能是系统账号（uid < 1000），只能含小写字母 / 数字 / 下划线 / 连字符，以小写字母或下划线开头，最长 32 位。**用户必须由你事先在所有机器（含中控机）上创建**，并配好 sudo 免密（NOPASSWD: ALL）和 SSH 免密登录；脚本**不会**创建用户、改 sudoers 或分发公钥，只在部署前检查（TiUP 部署用户需要免密 sudo，见官方 [tiup cluster deploy](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-deploy)） |
| SSH 端口 | 目标机 SSH 端口 | 默认 22 | 对应 `SSH_PORT`，同时写入 TiUP 拓扑的 ssh_port |
| SSH 私钥路径（可选） | 部署用户的私钥，脚本和 TiUP 都用它 | 留空 = `~<用户>/.ssh/id_rsa`；填写时必须是绝对路径 | 对应 `SSH_KEY`；不是绝对路径时「校验」报红 |

脚本一律以 `<用户>@IP` + sudo 登录各台主机（单机模式也要求 `<用户>` 能免密 SSH 登录本机自己，因为 TiUP 会 ssh 到本机）；不再用 root 登录，也不再支持密码登录。该卡片里的蓝色提示会写出前置条件，「命令」标签页第 0 步给出对应命令。

### 3 · TiDB 版本

| 选项 | 含义 | 可选值 / 默认 | 影响 |
|---|---|---|---|
| 自动 | 按系统选系列 | 默认；glibc ≥ 2.28 → v8.5 LTS，CentOS 7 → v7.5 LTS | 在线安装取该系列最新补丁版，离线取离线包目录里该系列的最高版本 |
| 指定系列 | 只写 vX.Y | 下拉：v6.5、v7.1、v7.5、v8.1、v8.5（默认 v8.5），或「其他系列…」自填 | 取该系列最新补丁版 |
| 指定完整版本 | vX.Y.Z | 如 v7.5.4；可省略 v 前缀 | 精确版本；离线下载只能用完整版本 |

**CentOS 7 上选 v8.4 及以上**：页面报红色错误、脚本在环境检查阶段中止，没有放行开关。原因是 TiDB 依赖 glibc ≥ 2.28，CentOS 7 的 glibc 是 2.17。官方对此的说明比页面更细，见「已知限制与易混淆点」。CentOS 7 请用 v7.5.x 或 v8.1.x；也可以把系统升级到 Rocky Linux 9 / CentOS Stream 9。

> 💡 本平台的写操作（重启、扩缩容、配置变更等）只在 **v7.5 / v8.1 / v8.5** 系列上启用，其他版本降级为只读。页面下拉里的 v6.5、v7.1 可以部署，但部署出来的集群不能用本平台做写操作。

### 4 · 安装方式

| 字段 / 选项 | 含义 | 可选值 / 默认 | 影响 |
|---|---|---|---|
| 在线 / 离线 | TiUP 与组件的来源 | 默认在线 | 在线：从 tiup-mirrors.pingcap.com 下载 TiUP，组件由 TiUP 在线拉取；离线：用离线包安装 TiUP 并合并镜像（官方做法见 [使用 TiUP 部署 TiDB 集群](https://docs.pingcap.com/zh/tidb/stable/production-deployment-using-tiup) 与 [搭建私有镜像](https://docs.pingcap.com/zh/tidb/stable/tiup-mirror)）。对应 `INSTALL_MODE` |
| 离线包所在的完整目录路径 | 放离线包的目录 | 默认 /opt/tidb-pkg；必须以 / 开头，不能是 / | 对应 `OFFLINE_PKG_DIR`。目录里放 `tidb-community-server-<版本>-linux-<架构>.tar.gz`（必需）和同版本的 `tidb-community-toolkit-…`（可选，同版本自动带上）；脚本按所选版本自动找包，**无需写文件名**。目录里没有对应版本时，脚本会列出已有版本并中止 |
| 依赖 rpm 所在目录（可选） | 无外网时放 numactl、chrony、libaio 等 rpm | 默认空 | 对应 `OFFLINE_RPM_DIR`；缺少依赖时脚本会对目录里的**全部** rpm 执行安装 |
| 没有离线包？去下载 → | 跳到「离线包与工具下载」标签页 | — | 无 |

### 5 · 主机与目录

| 字段 / 选项 | 含义 | 可选值 / 默认 | 影响 |
|---|---|---|---|
| 单机 IP（single） | 单机部署的机器 | 留空 = 本机主 IP；只能填一个 IPv4 | 对应 `SINGLE_HOST`。「本机主 IP」是 `hostname -I` 里第一个非回环地址，多网卡时可能不是你想要的，建议显式填写 |
| PD / TiDB / TiKV 主机（cluster） | 各角色主机，空格或逗号分隔，仅 IPv4 | 页面里的示例值只是示意；至少各 1 台，建议 PD 3 台（奇数）、TiDB ≥ 2、TiKV ≥ 3 | 同一主机上同一角色只部署一个实例（重复会报错）；PD 为偶数给警告 |
| TiFlash 主机（可选） | 列式副本节点 | 默认空 | 需要 CPU 支持 AVX2，脚本检查不通过则报错 |
| 监控主机 | Prometheus / Grafana / Alertmanager 所在主机 | 空 = 第一台 PD | 勾选「部署监控」时才生效 |
| 拓扑标签（可选） | `IP=zone`，多个用逗号分隔，如 `10.0.1.7=az1,10.0.1.8=az2,10.0.1.9=az3` | 默认空 | 启用后 PD 的 location-labels 设为 zone,host，TiKV 按 zone 打散副本；**每台 TiKV 都必须有 zone**，否则报错 |
| 部署目录 / 数据目录 | TiUP 部署目录与数据目录 | 默认 /tidb-deploy、/tidb-data；必须是绝对路径 | 脚本会创建并把属主改成部署用户。数据目录建议放独立数据盘（xfs，或 ext4 且挂载选项含 nodelalloc） |
| 规格 | dev / prod | 默认 dev | dev：低于推荐配置只警告；prod：按官方生产配置检查，不达标则**中止**，并要求 PD ≥ 3、TiKV ≥ 3、TiDB ≥ 2。命令行 `--strict` 等同 prod |
| 防火墙 | open / disable / keep | 默认 open | open：只放行 TiDB 所需端口；disable：**关闭 firewalld**；keep：不动 |
| yum 源失效时、镜像（仅 CentOS 7 / CentOS Linux 8 显示） | 依赖安装失败时是否切换到 vault 镜像 | skip（默认，不处理）/ auto；镜像 aliyun（默认）/ vault | auto 会把 /etc/yum.repos.d 下全部 .repo 备份并替换成 vault 镜像 |
| 部署监控 | 是否部署 Prometheus / Grafana / Alertmanager | 默认勾选 | 对应 `WITH_MONITOR`；不勾选则监控主机被忽略 |
| 忽略 tiup check 的失败项 | 部署前 `tiup cluster check` 复查仍有 Fail 时是否继续 | 默认不勾选；**仅 dev 规格可选**，选 prod 时置灰并被服务端拒绝 | 对应 `ALLOW_CHECK_FAIL=true`；会忽略**全部**失败项，不只是 ext4 缺 `nodelalloc`。勾选会写进保存的配置、随配置版本一起审批 |

拓扑文件各配置项（global、server_configs、pd_servers 等）的官方说明见 [拓扑文件配置](https://docs.pingcap.com/zh/tidb/stable/tiup-cluster-topology-reference)。

#### 规格检查阈值（脚本内置，来自官方软硬件要求）

| 角色 | dev 最低（核 / GB） | prod 最低（核 / GB） |
|---|---|---|
| TiDB | 8 / 16 | 16 / 48 |
| PD | 4 / 8 | 8 / 16 |
| TiKV | 8 / 32 | 16 / 64 |
| TiFlash | 32 / 64 | 48 / 128 |

一台主机承担 3 个及以上角色（如单机模式）时：prod 要求 ≥ 16 核 / 64GB；dev 低于 4 核 / 8GB 或低于 8 核 / 16GB 都只警告（「资源过低」不再算错误，继续部署可能因内存不足而不稳定）。数据盘可用空间：prod 要求 ≥ 100GB；dev ≥ 100GB 通过，20 到 100GB 警告，低于 20GB 失败。dev 下单角色低于推荐值只警告，但低于 2 核或 4GB 内存仍然失败。数值对应官方 [软件和硬件环境要求](https://docs.pingcap.com/zh/tidb/stable/hardware-and-software-requirements)，请以你所用版本的该页为准。

### 右侧输出标签页

| 标签页 | 内容 | 说明 |
|---|---|---|
| deploy.conf | 根据当前表单生成的配置文件 | 「复制」「下载 deploy.conf」；有校验错误时不能下载 |
| 命令 | 按顺序的命令清单 | 1 解压工具包；2 保存 deploy.conf 并 chmod 600；0 前置条件（创建 tidb 用户与 sudo 免密、临时开口令登录、生成并分发密钥、复测、改回禁用口令登录）；3（离线时）下载离线包；只读环境检查；`--dry-run` 预览；`show-prep` 审阅；正式部署；一行写法（命令行参数优先于配置文件） |
| 拓扑预览 | 脚本将生成的 TiUP 拓扑 YAML | 与脚本一致；single 模式下 TiKV 的 block-cache 由脚本按目标机内存 25%（至少 1GB）自动计算，预览里只是注释 |
| 校验 | 红色错误、黄色警告、绿色通过 | 红色错误会让标签页标题显示「N 错误」 |
| 离线包与工具下载 | 见下表 | 离线包下载链接指向官方 download.pingcap.org |
| 一键部署（新，仅中控机模式） | 预检、部署申请、实时输出与结果 | 见上文「一键部署」；需要 `settings.edit`；打开后左侧表单收起，输出区占满宽度 |

「离线包与工具下载」标签页：

| 字段 / 按钮 | 含义 | 可选值 / 默认 | 影响 |
|---|---|---|---|
| TiDB 完整版本 | 离线包需要精确补丁版本 | 空时取上面选的完整版本；如 v7.5.4 | 填系列（如 v8.5）时提示「请填写完整版本号」 |
| CPU 架构 | amd64 / arm64 | 默认 amd64 | 决定包文件名；**不会写进 deploy.conf**，脚本按中控机架构自动判断（可用 `--arch` 覆盖） |
| 保存到 | 在运行脚本的机器上保存离线包的完整目录 | 默认 /opt/tidb-pkg；必须以 / 开头 | 生成命令里的 `--dir` |
| 同时下载 toolkit 包 | 含 tiup mirror merge 所需的工具 | 默认勾选 | 命令加 `--with-toolkit`，并多一个下载链接 |
| 命令行下载（推荐） | 页面生成的 `./tidb-deploy.sh download-offline …` | 版本在目标系统上不受支持时报红色错误并不给命令 | 自动取同名 .sha256 校验，已存在且校验通过则跳过，支持断点续传，校验失败删除半成品并报错；取不到 .sha256 时只警告并打印本地值 |
| 浏览器下载链接 | server 包、toolkit 包及各自的 .sha256 | — | 包体很大；下载后放进上面的目录，**文件名不要修改** |
| 部署工具包 | `tidb-deploy-0.3.1.tar.gz` 及 SHA-256 | 点「下载」 | 包含 tidb-deploy.sh、deploy.conf.example、README 与离线逻辑测试 |

### 页面校验项

红色错误：版本格式不对；CentOS 7 上选 v8.4 及以上；用户名不合法或为 root；SSH 私钥路径不是绝对路径；部署 / 数据目录不是绝对路径；集群名不合法；主机不是合法 IPv4 或有重复；single 填了多个主机；cluster 缺 PD / TiDB / TiKV；prod 规格下 PD / TiKV / TiDB 数量不足；拓扑标签格式错误或缺 TiKV 的 zone；离线目录为空 / 不是绝对路径 / 是根目录；rpm 目录不是绝对路径。

黄色警告：CentOS 7 / CentOS Linux 8 已 EOL；部署前请先配好部署用户（sudo 免密 + 免密 SSH 登录所有机器，含中控机；脚本只检查不代劳）；single 模式仅供测试；PD 为偶数；低于生产建议的数量；使用了 TiFlash（需要 AVX2）。

### 页面生成的配置项对照（deploy.conf）

页面能生成的变量如下；其余变量只能手工写进 `deploy.conf`（可参考包内 `deploy.conf.example`）。

| 变量 | 页面字段 | 默认 | 说明 |
|---|---|---|---|
| CLUSTER_NAME | 集群名 | tidb-test | |
| TIDB_VERSION | TiDB 版本 | auto | auto / vX.Y / vX.Y.Z |
| DEPLOY_MODE | 部署模式 | single | single / cluster |
| SINGLE_HOST | 单机 IP | 空 | 空 = 本机主 IP |
| PD_HOSTS / TIDB_HOSTS / TIKV_HOSTS / TIFLASH_HOSTS / MONITOR_HOST | 各角色主机 | 空 | 仅 cluster |
| ZONE_MAP | 拓扑标签 | 空 | 仅 cluster |
| WITH_MONITOR | 部署监控 | true | |
| DEPLOY_USER | 操作系统用户 | tidb | 命令行 `--user` 优先 |
| SSH_PORT / SSH_KEY | SSH 端口 / 部署用户的私钥（绝对路径） | 22 / 空（= `~<用户>/.ssh/id_rsa`） | 旧配置里的 `SSH_ROOT_USER` / `SSH_ROOT_KEY` / `TD_ROOT_PASSWORD` 已不再使用，会提示并忽略 |
| DEPLOY_DIR / DATA_DIR | 部署 / 数据目录 | /tidb-deploy、/tidb-data | |
| SPEC / FIREWALL / REPO_FIX / REPO_MIRROR | 规格 / 防火墙 / yum 源 / 镜像 | dev / open / skip / aliyun | |
| INSTALL_MODE | 在线 / 离线 | online 或 offline | 脚本里还支持 auto（有离线包就离线） |
| OFFLINE_PKG_DIR / OFFLINE_RPM_DIR | 离线包目录 / rpm 目录 | 空 | |

只能手工设置的：`TIKV_CACHE_GB`（TiKV block-cache，GB；single 模式留空时按内存 25% 自动设置，至少 1GB）、`OFFLINE_SERVER_PKG` / `OFFLINE_TOOLKIT_PKG`（直接指定单个包文件）、`ARCH`（amd64 / arm64）、`TIUP_MIRROR`、`TIUP_UPDATE`、`INIT_PASSWORD`（`ALLOW_CHECK_FAIL` 页面上有勾选框，见上表；命令行里仍可手工写）。命令行参数优先于配置文件：`--user`、`--tidb-version`、`--offline-dir`、`--strict`、`--no-init`。

### 其他脚本命令

| 命令 | 作用 | 后果 |
|---|---|---|
| `wizard [文件]` | 交互式生成 deploy.conf（默认 ./deploy.conf，已存在需 `--yes` 覆盖） | 只写文件，权限 600 |
| `status` / `start` / `stop` | 调用 `tiup cluster display` / `start` / `stop` | `stop` 会停止整个集群（官方 [tiup cluster stop](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-stop)） |
| `destroy --confirm-name <集群名>` | 销毁集群 | **停止服务并删除全部数据，不可恢复** |
| `download-offline` | 下载并校验离线包 | 只写目标目录，需要完整版本号 |
| `offline-pack vX.Y.Z` | 在联网且已装 tiup 的机器上用 `tiup mirror clone` 制作离线包 | 只生成 amd64 的 server 包 |
| `--force` | 忽略已完成阶段记录重新执行 | 集群已存在时加 `--force` 会直接报错，需要先 destroy；不加 `--force` 时已存在的集群只是跳过 deploy 并警告 |

## 执行后果与风险

### 对目标主机的修改（⚠️ 阶段 4，每台部署主机，以 tidb@IP 登录后 sudo 执行）

同一份脚本，可用 `show-prep` 先看。这些都是对**生产系统配置**的修改，请逐项确认能接受（官方在 [环境与系统配置检查](https://docs.pingcap.com/zh/tidb/stable/check-before-deployment) 也建议 swap、透明大页、SELinux、防火墙、时间同步、内核参数、ulimit 的相应处理）：

| 项 | 做法 | 回退 |
|---|---|---|
| ⚠️ 依赖包 | 安装 numactl、chrony、curl、tar、gzip、libaio、openssh-server / clients、sudo、util-linux；有离线 rpm 目录时先安装其中全部 rpm | — |
| ⚠️ yum 源（可选） | `REPO_FIX=auto` 且安装失败时，把 /etc/yum.repos.d 下全部 .repo 移到 `tidb-deploy-backup-<时间>` 并写入 vault 镜像 | 从备份目录还原 |
| ⚠️ swap | `swapoff -a`，把 /etc/fstab 里的 swap 行注释掉 | 备份在 /etc/fstab.tidb-deploy.bak |
| ⚠️ 透明大页 | 立即设为 never，并安装开机生效的 `tidb-disable-thp.service` | 删除该 service |
| ⚠️ 内核参数 | 写入（**覆盖**）`/etc/sysctl.d/99-tidb.conf`：swappiness=0、file-max、somaxconn、tcp_syncookies=0、overcommit_memory=1；内存 ≥ 16GB 时加 min_free_kbytes | 删除该文件并重载 |
| ⚠️ ulimit | 写入 `/etc/security/limits.d/99-tidb.conf`（nofile 1000000、stack、core） | 删除该文件 |
| ⚠️ SELinux | 若是 Enforcing，立即设为 permissive，并把 /etc/selinux/config 改成 disabled | 改回配置文件；**重启后 SELinux 为 disabled** |
| ⚠️ 防火墙 | firewalld 在运行时：open 放行本机角色所需端口并 reload；disable 关闭并禁用 firewalld；keep 不动 | `firewall-cmd --remove-port` / 重新启用 firewalld |
| ⚠️ 时间同步 | 没有 ntpd 时启用并启动 chronyd，执行 makestep | — |
| 部署用户 | **不创建用户、不改 sudoers、不写 authorized_keys**：由你事先配好（见「准备」），脚本只确认用户存在 | — |
| ⚠️ 目录 | 创建部署目录和数据目录并改属主 | — |
| ⚠️ 其他 | 启用并启动 sshd；若有 cpupower，把 CPU 调频策略设为 performance | — |
| ⚠️ tiup cluster check --apply | 阶段 7 中 tiup 对检查失败项自动修复（官方说明 `--apply` 会尝试自动修复） | 视被修复项而定，需手工还原 |

> ⚠️ **特权与安全**：你为部署用户开通的 sudo 免密（NOPASSWD: ALL）等于每台主机上的免密 root 等价权限，而中控机上该用户的私钥无口令，谁拿到中控机上该用户的权限就等于拿到全部集群主机的 root。部署完成后请控制中控机的访问，并妥善保管私钥。

> ⚠️ **配置文件以 root 身份执行**：`deploy.conf` 是 bash 变量赋值文件，脚本以 root `source` 它，其中的任何命令都会被执行。务必确认来源可信，并 `chmod 600`。

> ⚠️ **首次 SSH 不校验主机指纹**：脚本用 `StrictHostKeyChecking=no` 连接远程主机（指纹记录在工作目录里的临时 known_hosts），在不可信网络里首次部署存在中间人风险。

> ⚠️ **不要关掉 root 随机密码**：`--no-init` 或 `INIT_PASSWORD=false` 会让 TiDB 的 root 保持**空密码**。随机密码保存在 `root-password.txt`，登录后请立刻修改密码并删除该文件；Grafana 默认账号是 admin / admin，首次登录请修改。官方说明 `tiup cluster start --init` 会生成 root 随机密码并只显示一次（官方 [tiup cluster start](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-start)）。

> ⚠️ **多机部署时中控机若不在部署主机清单里**：脚本的提示是「仅安装基础依赖」，但代码实际对中控机执行的是与其他主机相同的**完整初始化脚本**（关 swap、改内核参数、关 SELinux、改防火墙等）。请先用 `show-prep` 确认，必要时把中控机也规划成部署主机之一，或在测试机上验证。

### 对中控机的修改

脚本**不再**创建部署用户和密钥（由你事先配好）。阶段 5：部署用户家目录下新增 `.tiup`（TiUP）与 `tidb-offline`（离线解压目录）；工作目录 `/var/lib/tidb-deploy/<集群名>/`（`state`、`topology.yaml`、`root-password.txt` 等，目录权限 700）与日志 `/var/log/tidb-deploy/<集群名>-<时间>.log`（权限 600，root 随机密码不会写入）。

## 常见问题与注意

- **下载按钮是灰的？** 「校验」里还有红色错误，修正后可下载；「复制」不受影响。
- **离线部署提示找不到包？** 检查目录是否绝对路径、文件名是否保持 `tidb-community-server-vX.Y.Z-linux-<架构>.tar.gz`、架构是否与中控机一致；脚本会列出目录里已有的版本。
- **部署中断了怎么办？** 修复原因后重新执行同一条 deploy 命令，会从未完成的阶段继续；已 deploy 的集群不会被覆盖。想重来先 destroy。
- **脚本日志在哪？** `/var/log/tidb-deploy/<集群名>-<时间>.log`（权限 600，root 随机密码不会写入）。
- **脚本限制：** 仅 IPv4；仅 x86_64 的 `offline-pack`；同一主机上同一角色只部署一个实例；TiKV 数据盘的分区、格式化与 nodelalloc 挂载不由脚本处理（只检查并提示）。
- **工具包和平台发布包是什么关系？** 发布包里已附带同一份 tidb-deploy 工具包（`deploy/tidb-deploy/` 与 `web/deploy/dist/`），因此用发布包部署的平台，本页就能下载。工具包的静态下载地址不需要登录。
- **页面选「CentOS Linux 8」，脚本里叫什么？** 脚本只靠 /etc/os-release 识别系统，页面的选择只影响页面自己的校验和命令。

## 已知限制与易混淆点

核对于代码、脚本与官方文档，格式为「界面 / 文档写的是 A，实际是 B」。

| 界面 / 文档写的是 | 实际是 | 建议 |
|---|---|---|
| 目标系统选「RHEL 9」，页面把 `--target-os rhel9` 写进下载命令 | 脚本的 `--target-os` 只认 centos7 / centos8 / stream8 / stream9 / stream10 / rocky8 / rocky9 / rocky10 / alma8 / alma9 / alma10 / rhel10，以及 el7 / el8 / el9 / el10（脚本帮助没有列出 el 系列）；`rhel9` 会被拒绝，提示「--target-os 只能是 …」 | 把命令里的 `rhel9` 手工改成 `el9`，或删掉该参数 |
| 页面「命令」标签页第 3 条的下载命令 | 没有带 `--arch`（默认按运行脚本这台机器的架构）；「离线包与工具下载」标签页里的命令才带 `--arch` | 要给 arm64 主机下载包时，用下载标签页的命令，或手工加 `--arch arm64` |
| 页面把 Rocky Linux 8、AlmaLinux 9、CentOS Stream 9 标为 OK / 兼容，脚本对 RHEL / Rocky / Alma 一律打印「系统在官方测试列表内」 | 官方 stable 的系统表中，测试验证平台是 RHEL 8.6+/9.4+、Rocky Linux 9.1+ 等；CentOS Stream 8 列为「兼容」且已于 2024-05-31 End of Builds；Rocky 8、AlmaLinux、CentOS Stream 9 在我们核对时没有出现在该表 | 以你所用版本的官方 [软件和硬件环境要求](https://docs.pingcap.com/zh/tidb/stable/hardware-and-software-requirements) 为准 |
| 「TiDB v8.4 及以上不支持 CentOS 7」，页面与脚本一律拒绝 | 官方 stable 写明：v8.4.0 DMR 与 v8.5.0 移除了对 glibc 2.17 / CentOS Linux 7 的适配，**v8.5.1 起重新适配 glibc 2.17**，但仍建议迁移到更新的系统；页面与脚本比官方更保守，且没有放行开关 | 仍坚持用 CentOS 7 就选 v7.5 / v8.1；想用 v8.5.1+ 请先迁移系统 |
| 平台「关于与升级」卡片里的升级流程「校验清单 → 备份数据目录 → 安装 → 数据迁移 → 切换 → 重启 → 健康检查」 | 实际**第一步是停止服务**（卡片只在旁边的橙色提示里写「约 10–30 秒不可用」）；升级期间页面、审批和定时变更调度都不可用 | 升级前通知使用者，按上文「升级前检查清单」处理变更 |
| 升级「预演」可用普通账号运行 | 包装脚本 `./upgrade.sh` 本身要求 root；直接运行二进制的 `--dry-run` 不要求 root，但数据目录 0700 属主 tidbops，普通账号读不到时部分预检项会报权限错误（代码推断） | 预演也用 `sudo` |
| `tidb-deploy.sh check` 是「只读」检查 | 不改系统配置，但会创建 `/var/lib/tidb-deploy/<集群名>/` 与日志文件（无权限时改用临时目录并警告），并通过 SSH 读取各主机事实；非 root 时读不到 `/root/.ssh` 下的默认私钥 | 用 root 运行（需要 root 才能以 `tidb` 身份检查 sudo 和 SSH）；把它当成「不改系统配置」而非「不写任何文件」 |
| `--dry-run` 是「完全不连接任何机器」 | 阶段 1 的前置条件与环境检查仍真实 SSH（`tidb@IP`）到各主机；仍需要确认（非交互需 `--yes`）；检查失败同样中止 | 先保证 SSH 可用再预览 |
| 多机模式中控机不在清单里时，提示「仅安装基础依赖」 | 实际对中控机执行完整初始化脚本（关 swap、改内核参数、改 SELinux / 防火墙等） | 先 `show-prep` 确认，或把中控机规划进部署主机 |
| 部署用户的 sudo / SSH 免密检查「通过」就等于一定能部署 | 检查用的是 `ssh … sudo -n true`；真实部署时 TiUP 还要靠同一用户做更多操作（写部署目录、systemd 等），这部分本机未在真实环境验证 | 以 `tiup cluster check` 的结果为准，失败按提示处理 |
| 页面生成 `INSTALL_MODE=online` 或 `offline` | 脚本自身默认是 `auto`（有离线包就离线） | 用页面生成的文件时无差别；手写配置时注意 |
| 部署摘要末尾提示「下一步：用 tidbops-fixtures 工具…」 | 这只是项目内的开发提示，与部署成败无关 | 忽略即可 |
| 「一键部署」点了「开始部署」，页面还会让我输 root 密码 | 脚本要 root；平台没有保存任何 root 密码，所以预检、部署、取消、从断点继续每次都要输入 | 这是有意的；不想让平台经手 root 密码，就用「命令」标签里的命令在中控机上手工执行 |
| 「一键部署」支持离线安装，我在页面里上传离线包 | **不支持上传**：离线包要你自己拷到中控机上的目录，页面只保存目录路径；预检才会检查包在不在 | 见「一键部署」里的「离线部署」；包很大，用 scp / 内网文件服务器 |
| 「保存配置」保存的是我看到的 deploy.conf 文本 | 保存的是**服务端按表单重新校验、重新渲染**的结果：每个值都按白名单字符检查（配置会被脚本以 root 身份 source），所以带引号、`$`、反引号、换行的值会被拒绝；内容与页面「deploy.conf」标签一致 | 被拒绝时看提示改掉相应字段 |
| 预检显示「通过」就等于部署一定成功 | 预检是 `check`：检查前置条件和机器环境；部署时 `tiup cluster check`、下载组件、网络抖动等仍可能失败 | 失败后修好原因，从断点继续 |
| 「导入到集群接入」按钮 | 只是跳到「集群接入」页，需要你在那里勾选并确认导入 | 部署成功后手工导入一次 |

## 官方参考

以下链接均已打开核实；平台支持 v7.5 / v8.1 / v8.5，页面以 stable 为准，**以你集群的版本为准**，必要时把地址里的 stable 换成对应版本（如 v8.1）。

- [使用 TiUP 部署 TiDB 集群](https://docs.pingcap.com/zh/tidb/stable/production-deployment-using-tiup)：在线 / 离线部署整体步骤（`check --apply`、`deploy`、`start --init`、`display`）。
- [tiup cluster deploy](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-deploy)：`--user`（需免密 sudo）、`-i` 密钥、`--skip-create-user` 等选项。
- [tiup cluster check](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-check)：`--apply`、`--cluster`，以及时间同步、swap、THP、内核参数、nodelalloc、SELinux、防火墙等检查项。
- [tiup cluster start](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-start)、[display](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-display)、[stop](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-stop)、[destroy](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-destroy)、[list](https://docs.pingcap.com/zh/tidb/stable/tiup-component-cluster-list)：`--init` 随机密码、状态列、停止与销毁的后果、集群列表（按操作系统用户隔离）。
- [拓扑文件配置](https://docs.pingcap.com/zh/tidb/stable/tiup-cluster-topology-reference)：global、server_configs、各组件 servers、监控组件。
- [软件和硬件环境要求](https://docs.pingcap.com/zh/tidb/stable/hardware-and-software-requirements)：各版本支持的操作系统、CentOS 7 的版本边界、生产最低配置、磁盘与端口。
- [环境与系统配置检查](https://docs.pingcap.com/zh/tidb/stable/check-before-deployment)：swap、THP、NTP / chrony、防火墙、SELinux、sysctl、ulimit。
- [搭建私有镜像](https://docs.pingcap.com/zh/tidb/stable/tiup-mirror)：`tiup mirror clone` / `merge` / `set`（离线镜像）。
- [使用 TiUP no-sudo 模式部署](https://docs.pingcap.com/zh/tidb/stable/tiup-cluster-no-sudo-mode)：默认模式要求部署用户具备 sudo 权限，no-sudo 是替代方案（本脚本不使用）。
- [使用 TiUP 升级 TiDB](https://docs.pingcap.com/zh/tidb/stable/upgrade-tidb-using-tiup)：集群升级的官方流程（与本页平台自身升级是两件事）。

## 相关页面

- [日常运维](go:ops)：集群部署完成后的巡检、备份、参数建议等。
- [环境与设置](go:settings)：「关于与升级」卡片、变更窗口规则、通知渠道。
- [权限与审计](go:security)：部署自检、写操作开关、用户与审批策略。
- [变更排期](go:schedule)：升级平台前确认没有执行中 / 排队中的变更。
- 帮助：[快速入门](help:quickstart)、[常见问题](help:faq)
