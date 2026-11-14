# TiDB Ops 运维部署手册

> 适用版本：TiDB Ops 1.6.x　·　目标系统：CentOS 7.9 / Rocky / Alma / Ubuntu 等带 systemd 的 64 位 Linux（x86_64 与 aarch64）
>
> **读者**：负责**部署和长期运维这个平台**的运维 / SRE。使用平台功能（看总览、做变更、审批）请看《用户手册》（`docs/用户手册.md`）和页面「使用帮助」。
>
> **本文怎么用**：第一次部署，按 1 → 2 → 3 → 4 → 5 的顺序做；日常运维看 7；升级回滚看 8；备份与灾备看 9；出问题看 11。每一步都有命令、预期结果和出错怎么办。
>
> **监听默认值（1.6.12 起）**：`install.sh` 默认以 `--listen 0.0.0.0:8018` 安装，systemd 单元带 `--allow-remote`，即**默认监听全部网卡的 8018 端口，明文 HTTP**，装好后放行防火墙端口即可访问。生产环境建议只监听本机并放在 HTTPS 反向代理之后（5.4）。`upgrade.sh` 沿用已装单元里的监听地址，不会因为新的默认值被改动。
>
> **验证状态（务必先读）**：安装器、升级回滚、只读闸门等流程主要在开发机的测试环境（含「假 tiup」和回放数据）里验证过；已有真实环境的使用反馈：在 CentOS Stream 10 + TiDB v8.5.x 上完成安装、浏览器访问、从本机 tiup 导入集群。**各类写命令（重启、扩缩容、配置、维护、备份、升级）对真实集群的完整联调仍建议先在测试集群做**（12.7）。本文用 ⚠️「未验证」标出推断出来的点，**第一次上线前，请在一台与生产同版本的测试机上把第 5、8、9 章完整走一遍**，并以实际输出为准。

---

## 目录

1. 先弄清平台是什么：组件、账号、目录、端口
2. 规划：选形态、容量、网络、时间、环境与账号约定
3. 部署前检查（逐项命令）
4. 安装包管理
5. 部署 Runbook（安装、登录、访问方式、接入集群、开启写操作、部署新集群、中控机命令）
6. 配置参考（systemd 单元、启动参数、`exec.json`、`/etc/tidb-ops`、日志、备案号）
7. 日常运维
8. 升级与回滚 Runbook
9. 备份、恢复与灾备
10. 安全运维
11. 故障处置手册
12. 附录（命令速查、路径表、版本与兼容、未验证项、术语、联调清单、诊断指标核对、打包与发布、文档说明）

---

## 1. 先弄清平台是什么

### 1.1 一句话

TiDB Ops 是**一个 Go 写的单进程 Web 服务**（`tidbops-server`）+ 静态前端，数据全部存在本机一个目录里的 JSON 文件中，**没有数据库依赖**。它主动访问 TiDB 集群的 PD / Prometheus / Alertmanager / TiDB（只读采集），集群不需要连回它；平台宕机**不影响 TiDB 集群本身**。

### 1.2 三种部署形态

| | A. 独立部署 | B. 同机部署（旧） | C. 中控机模式（推荐） |
|---|---|---|---|
| 安装命令 | `install.sh --standalone` | `install.sh --cluster <名>` | `install.sh --local-tiup` |
| 平台装在哪 | 任意机器 | tiup 中控机 | tiup 中控机 |
| 对集群能做什么 | 只读（总览、拓扑、慢 SQL、热点、告警、诊断） | 读 + 写（重启、扩缩容、配置、维护、备份、升级） | 导入的集群读 + 写；手工添加的集群只读；另有页面「一键部署」新集群（经 root 密码 + su 执行）与「中控机命令」（以 tidb 身份经 sudoers 固定入口执行，不需要 root 密码，权限为 `shell.run` / `shell.approve`） |
| 需要 tiup / `tidb` 用户 / sudo | 否 | 是 | 是 |
| 集群怎么进平台 | 页面手工添加 | 安装时绑定一个，之后还能再接 | 页面「从本机 tiup 导入」，或手工添加（只读） |
| 创建 sudoers / `exec.json` / `exec.key` | 否 | 是 | 是 |
| `NoNewPrivileges` | yes | no | no |

**怎么选**：只想看监控诊断、平台不放在中控机上 → A；平台装在中控机、要在平台里做变更 → **C**；B 是历史形态，新部署不要选。「在平台里部署一套**新**集群」：形态 C 可以在页面「安装部署」里一键完成（5.6.2），其他形态用包里的 `tidb-deploy` 手工部署（5.6.1）。

### 1.3 结构图（形态 C）

```
 浏览器 ──HTTPS──► nginx（同机，TLS + 访问控制）
                      │ http://127.0.0.1:8018
                      ▼
        tidbops-server（systemd 服务，用户 tidbops，非 root）
          │ 采集：HTTP / mysql 客户端（只读）          │ 写 / 读命令：sudo -n -u tidb … exec --stdin
          ▼                                              ▼
   PD 2379 / Prometheus 9090 /                 受限执行入口（用户 tidb）
   Alertmanager 9093 / TiDB 10080、4000          校验白名单 + 参数 + 拓扑 + 审批令牌 → tiup / pd-ctl / br / mysql
                                                          │ SSH（tidb 用户自己的密钥，平台碰不到）
                                                          ▼
                                                     集群各节点
```

两个关键的信任边界：

1. **Web 进程（tidbops）不能直接执行任何集群命令**：只能经 sudo 调用唯一一条固定入口 `exec --stdin`，入口里独立再校验一遍（命令白名单、参数、拓扑、审批令牌），`destroy / clean / patch / ssh / bash -c` 永远没有。
2. **SSH 私钥不进平台**：tiup 在 `tidb` 用户下用它自己的密钥连各节点；平台只接触命令输出。

### 1.4 账号、目录、端口、进程

| 项 | 值 | 说明 |
|---|---|---|
| 运行账号 | `tidbops`（系统账号，无登录 shell，主目录 `/var/lib/tidbops`） | 安装器创建 |
| 部署账号 | `tidb`（`--tidb-user` 可改） | 形态 B / C：tiup 集群的部署用户；exec 入口以它运行 |
| 服务名 | `tidbops.service` | `/etc/systemd/system/tidbops.service` |
| 程序 | `/opt/tidbops/current/bin/tidbops-server` | `current` 是指向 `releases/<版本>` 的符号链接 |
| 前端 | `/opt/tidbops/current/web` | |
| 数据目录 | `/var/lib/tidbops`（0700，tidbops） | 用户 / 角色 / 审计 / 集群注册表 / 变更 / 设置 / `master.key` 等 |
| 配置目录 | `/etc/tidb-ops` | 形态 B / C：`exec.json`、`exec.key`、`exec.env`、`env`、`READONLY` |
| 备份目录 | `/var/backups/tidbops`（0700） | 升级前自动备份 / 手动备份 |
| sudoers | `/etc/sudoers.d/tidbops` | 形态 B：只放行 `tidbops → tidb` 的 `exec --stdin`；形态 C：再多一条 `shell-exec --stdin`（「中控机命令」入口）。两条都是固定命令，参数也固定 |
| 监听 | `0.0.0.0:8018`（带 `--allow-remote`） | 默认监听全部网卡、明文 HTTP；生产建议只监听 `127.0.0.1` 并放在 HTTPS 反向代理之后（5.4） |
| 进程 | 1 个 `tidbops-server`（外加按需的 `exec --stdin` 子进程） | 无常驻子进程 |
| 临时单元（形态 C） | `tidbops-deploy-<任务号>`、`tidbops-shell-<任务号>` | 「一键部署」经 `su - root` + `systemd-run`、「中控机命令」的变更命令经 tidb 的 `sudo -n systemd-run` 起的 transient 单元，在平台服务沙箱之外运行；任务结束即消失，`journalctl -u <单元名>` 可查 |

### 1.5 数据目录里有什么（备份与恢复的对象）

| 文件 | 内容 | 丢了会怎样 |
|---|---|---|
| `master.key` | 加密集群数据库密码、通知渠道 URL / 密钥的主密钥 | **所有加密保存的口令作废**，必须在页面重新填写 |
| `users.json` / `roles.json` | 用户（含口令哈希、MFA）与自定义角色 | 没人能登录；只能 `user add` 重建管理员 |
| `audit.log` | 审计哈希链（只追加） | 审计记录没了（哈希链断） |
| `clusters.json` | 集群注册表（连接信息；数据库密码为密文） | 要重新接入 / 导入集群 |
| `settings.json` / `changes.json` / `hostpool.json` / `notify.json` / `notify-log.json` / `proposals.json` / `watermark.json` / `inspect-last.json` | 全局设置、变更单、主机池、通知渠道与投递记录、双人复核提案、水印、最近一次巡检报告 | 对应功能的配置 / 历史丢失 |
| `clusters/<id>/…` | 每个集群自己的变更 / 设置 / 通知 / 主机池 | 该集群的运行数据丢失 |
| `deploy/configs/<集群名>/v<N>.conf`、`deploy/requests.json`、`deploy/jobs/<任务号>/` | 一键部署：已保存的部署配置（带版本）、部署申请、每次预检 / 部署的输出与退出码（`out.log`、`exit`、`run.sh`、工具包解压目录） | 配置要重新保存；历史输出丢失；**不含任何口令**（新集群 root 密码只在成功后短暂存放，显示一次即删） |
| `shell/settings.json`、`shell/requests.json`、`shell/jobs/<任务号>/` | 中控机命令：策略设置、变更申请、每次执行的输出（已遮蔽）与退出码 | 设置退回默认（标准档位 / 保留 30 天）；历史输出丢失。输出虽已遮蔽，**仍按敏感数据对待**（备份时一并带走） |
| `exec.key` | 审批令牌密钥（独立部署由服务自己生成；形态 B / C 用 `/etc/tidb-ops/exec.key`） | 令牌是短期的，重新生成即可 |
| `VERSION` / `upgrades.jsonl` | 数据 schema 版本 / 升级历史 | 升级引擎按「旧数据（schema 0）」处理，会自动迁移 |

---

## 2. 规划

### 2.1 容量

| 项 | 建议 | 依据 |
|---|---|---|
| CPU / 内存 | 2 核 / 2 GiB 起 | systemd 单元里 `MemoryMax=1G`，进程超过会被杀；集群很多（上限 20）、实例很多时再加，并用 drop-in 调高 `MemoryMax` |
| 磁盘 | 数据目录 5 GiB 起；备份目录再留 2–3 倍数据目录的空间 | 数据目录主要增长项是 `audit.log`（只追加，**没有自带轮转**，不要手工截断——会破坏哈希链校验）和各集群的 `changes.json` |
| 文件句柄 | 单元已设 `LimitNOFILE=65536` | 一般不用动 |
| 集群数 | 每个平台最多 20 个（平台实例不计） | 超过要拆平台 |

> ⚠️ 未验证：真实环境下的内存占用与采集周期压力；接入集群数量多、集群很大（Region / 实例很多）时请先观察 `systemctl status tidbops` 里的内存，再决定是否调大 `MemoryMax`。

### 2.2 网络矩阵

| 方向 | 源 → 目的 | 端口 | 必须吗 | 备注 |
|---|---|---|---|---|
| 用户访问 | 浏览器 → nginx | 443（80 跳转） | 是 | 只对办公网段 / VPN 开放 |
| 反向代理 | nginx → 平台 | 127.0.0.1:8018 | 走反向代理时 | 同机，不出网卡 |
| 用户访问（默认方式） | 浏览器 → 平台 | 8018 | 直接监听时 | 默认 `0.0.0.0:8018`，明文 HTTP，只对可信网段开放；走反向代理时不需要 |
| 采集 | 平台 → PD | 2379 | **必须** | 拓扑、热点、Region |
| 采集 | 平台 → Prometheus | 9090 | 强烈建议 | 趋势、主机指标、DB Time |
| 采集 | 平台 → Alertmanager | 9093 | 建议 | 告警列表 |
| 采集 | 平台 → TiDB status | 10080 | 建议 | 实例信息 |
| 采集 | 平台（`mysql` 客户端）→ TiDB | 4000 | 建议 | 慢 SQL、`cluster_info`、版本 |
| 通知 | 平台 → Webhook | 443 | 按需 | 官方域名：`qyapi.weixin.qq.com`、`oapi.dingtalk.com`、`open.feishu.cn`、`open.larksuite.com`、`hooks.slack.com`；内网网关 / 「其他」渠道的主机需 `--notify-allow-hosts`；**目标为内网 / 回环 IP 一律拒绝（防 SSRF）** |
| 运维（形态 B / C） | 中控机 tidb 用户 → 各集群节点 | 22 | 是 | tiup 本来就要的，与平台无关 |

平台是**主动方**：集群侧不需要放行到平台的任何入站。

### 2.3 时间与 DNS

- **时钟必须准**（chrony / ntpd）：会话过期、MFA 动态码、变更窗口、审计时间、审批令牌有效期都依赖它。
- 平台机要能解析集群里用到的主机名（若接入地址写的是域名）；写 IP 可避开。

### 2.4 环境与账号约定（先定规矩再建账号）

- 每个集群在接入时选一个**环境**：测试 / 预发 / 生产。用户的角色按环境授予，所以**环境划分决定了谁能看、谁能动哪个集群**。建议：生产集群只给最少的人；DBA / SRE 在测试 / 预发角色可以宽一些。
- 至少 **2 名平台管理员**（开启写操作、授予管理员角色都要两个不同的人；`admin` 初始口令必须改掉）。
- 命名建议：管理员 / 值班账号用个人名（`alice`、`bob`），不要共享账号；`admin` 只留作应急。
- 数据库账号：**每个集群两个**——只读采集账号（`tidbops_ro`）和运维账号（`tidbops_ops`，只给 `SET GLOBAL` / `SET CONFIG` 权限），限定来源主机为平台所在机器 IP。
- 备份与恢复目标（先定再做）：RPO（最多丢多久的数据）、RTO（多久恢复）。平台自身数据量小、变化慢，常见取值：RPO 24 小时、RTO 1 小时。

---

## 3. 部署前检查（逐项命令）

在**目标机**上以 root（或 sudo）逐项确认。形态标注：A = 独立部署，C = 中控机模式（B 同 C）。

| # | 检查 | 命令 | 期望 |
|---|---|---|---|
| 1 | 架构 | `uname -m` | `x86_64` → 用 amd64 包；`aarch64` → arm64 包 |
| 2 | systemd | `systemctl --version \| head -1` | 有输出 |
| 3 | 时钟 | `timedatectl \| grep -i "synchronized\|NTP"` | 已同步 |
| 4 | 磁盘 | `df -h /var /opt` | `/var` 剩余 ≥ 10 GiB |
| 5 | 端口未占用 | `ss -lntp \| grep ':8018 '` | 无输出 |
| 6 | mysql 客户端（A、C） | `mysql --version` | 有输出，且**必须能认 `mysql_native_password`**（TiDB 账号默认用它）。MySQL 官方 9.x 客户端已移除该插件，连 TiDB 会报 `ERROR 2059 … mysql_native_password cannot be loaded`；这种情况换 `dnf install -y mariadb`，并在「集群接入」把「mysql 可执行文件」填成 `mariadb`（见 11.3） |
| 7 | 到集群的连通性（A） | 见第 5.2 节第 3 步的 curl 自测 | 全部通 |
| 8 | `tidb` 用户存在（B、C） | `id tidb` | 有输出（部署用户不叫 tidb 时安装加 `--tidb-user`） |
| 9 | tiup 可无交互运行（B、C） | `sudo -u tidb tiup cluster list` | 列出集群，不提示口令、不卡住 |
| 10 | SSH 密钥状态（B、C） | `ls -l ~tidb/.tiup/storage/cluster/clusters/<集群>/ssh/` | `id_rsa` 属主 `tidb`、权限 `0600`；**不带口令短语**，不依赖 ssh-agent 转发 |
| 11 | `visudo`（B、C） | `command -v visudo` | 有输出 |
| 12 | 平台读 `meta.yaml`（B、C） | `sudo -u tidbops cat ~tidb/.tiup/storage/cluster/clusters/<集群>/meta.yaml \| head -3` | 有内容。CentOS 7 默认家目录 0700，需放开**到 `meta.yaml` 所在目录**的进入权限；**不要放开 `ssh/` 目录** |
| 13 | SELinux 状态 | `getenforce` | 记下结果；Enforcing 时，nginx 需要 `setsebool -P httpd_can_network_connect 1`；其余见 11.7 |
| 14 | 防火墙 | `firewall-cmd --state` | 记下结果；直接监听时对可信网段放行 8018，走反向代理时只开 80 / 443、**不开 8018** |

第 12 项的做法（**只做到 `meta.yaml` 所在目录，保持 `ssh/` 不可读**）。推荐用 ACL，只授权给 `tidbops` 一个账号，不向所有用户开放：

```bash
sudo dnf install -y acl          # 没有 setfacl 命令时才需要
C=~tidb/.tiup/storage/cluster/clusters/<集群>

# 沿路径每一级目录只给「进入」权限（x），不给读
sudo setfacl -m u:tidbops:x ~tidb ~tidb/.tiup ~tidb/.tiup/storage ~tidb/.tiup/storage/cluster ~tidb/.tiup/storage/cluster/clusters
# 集群目录：可进入、可读；meta.yaml：可读
sudo setfacl -m u:tidbops:rx $C
sudo setfacl -m u:tidbops:r $C/meta.yaml

sudo -u tidbops cat $C/meta.yaml | head -3      # 应显示 user: tidb
sudo -u tidbops ls $C/ssh                       # 必须是 Permission denied
```

注意：

- **`tidbops` 账号被删了重建（卸载重装）后要重做**：ACL 记的是用户 ID，新账号 ID 可能不同，旧授权就失效了。
- **tiup 重写 `meta.yaml` 后要重做最后一条 `setfacl`**：扩容、`edit-config` 等操作可能用「写临时文件再改名」的方式更新它，新文件不带 ACL，「配置变更」会因读不到而拒绝生成方案。
- 文件系统不支持 ACL（`setfacl` 报 `Operation not supported`）时，改用 `chmod o+x`（上面那些目录）、`chmod o+rx $C`、`chmod o+r $C/meta.yaml`。这是对所有用户开放，比 ACL 宽，只在 ACL 用不了时才用。
- **不要对 `ssh/` 目录或其中任何文件做任何放开。**

> ⚠️ 未验证：目录布局按 tiup 的通行布局推断；以你的 tiup 版本为准。meta.yaml 里可能含密码类字段（平台读取后会脱敏展示，但文件本身对 `tidbops` 可读），这是读取它的代价。

---

## 4. 安装包管理

### 4.1 包的命名与内容

`tidbops-<版本>-linux-<架构>.tar.gz` + 同名 `.sha256`。解压后：`install.sh` / `upgrade.sh` / `rollback.sh`（包装脚本）、`bin/tidbops-server`（静态编译）、`web/`、`deploy/ops/`（单元 / sudoers / `exec.json` 与 `clusters.json` 样例模板，以及备案号配置文件 `platform.json`）、`deploy/tidb-deploy/`、`docs/`、`MANIFEST.json`（逐文件哈希）、`CHANGELOG.md`、`VERSION`。包被改动任意一个字节，清单校验就会失败。

### 4.2 获取、校验、传输

```bash
# 在发布机上（有 Go 的开发机）
./build-release.sh -a amd64                         # 版本取 VERSION 文件；输出 backend/dist/release/
./build-release.sh --verify /绝对路径/tidbops-<版本>-linux-amd64.tar.gz    # 校验包自身

# 传到目标机，放在同一目录
scp tidbops-<版本>-linux-amd64.tar.gz{,.sha256} ops@10.0.9.10:/tmp/

# 在目标机上
cd /tmp && sha256sum -c tidbops-<版本>-linux-amd64.tar.gz.sha256      # 必须显示：…: OK
tar xzf tidbops-<版本>-linux-amd64.tar.gz && cd tidbops-<版本>-linux-amd64
ls
```

> ⚠️ **不要在发布包解压目录里再解压别的东西**。包里的 `deploy/tidb-deploy/tidb-deploy-<版本>.tar.gz` 是部署工具包，要用就复制到别处（如 `/opt/tidb-deploy-tool`）再解压。安装器与升级器会校验目录内容和 `MANIFEST.json` 逐文件一致，多出任何文件都会报「发布包里有清单之外的文件：…」并拒绝安装；处理办法是删掉那个多出来的目录。

### 4.3 保管规则

- 每个上线过的版本的 `.tar.gz` + `.sha256` **留档**（制品库或文件服务器），保留至少最近 3 个版本——回滚需要旧版本的程序，而本机 `/opt/tidbops/releases/` 默认也留最近几个（`--keep`，默认 3）。
- 不要在目标机上手改包内文件、也不要把 `bin/tidbops-server` 单独拷出来运行。
- 版本升级只通过 `upgrade.sh` / `admin upgrade`，不要手工覆盖 `/opt/tidbops/current`。

---

## 5. 部署 Runbook

> 通用约定：命令前缀 `sudo` 表示需要 root；`<尖括号>` 是要你替换的值；每一步末尾的「预期」没出现就**停下排查**，别往下做。

### 5.1 形态 C（中控机模式，推荐）

#### 步骤 1 预演

```bash
cd tidbops-<版本>-linux-amd64
sudo ./install.sh --local-tiup --env prod --dry-run
```

预期：打印「发布包：tidbops <版本> … 校验通过」「将部署 tidbops <版本>（中控机模式…）」和一串 `[dry-run] n/m 步骤`，最后一行「以上步骤均未执行，系统未做任何改动」，随后打印「访问信息」（不含口令）。缺少前置条件时只给 `[dry-run] 警告`，据此补齐第 3 章的项目。

#### 步骤 2 正式安装

```bash
sudo ./install.sh --local-tiup --env prod --public-url https://ops.example.com --start
# 部署用户不叫 tidb：加 --tidb-user <用户>
# 默认监听 0.0.0.0:8018（带 --allow-remote）；只想监听本机就加 --listen 127.0.0.1:8018（见 5.4）
```

`--local-tiup` 隐含 `--standalone`，**不能**再带 `--cluster`。它做的事：创建 `tidbops` 账号与目录 → 安装 release 并切换 `current` → 生成 `exec.key`（0640，tidbops:tidb）、`exec.json`（集群名通配 `["*"]`）、`exec.env` / `env`（口令文件，初始只有注释）、sudoers（`visudo -cf` 校验后装）→ 写 systemd 单元 → 写数据目录 `VERSION` → **在启动服务前创建管理员 `admin` 并生成随机口令** → `daemon-reload` →（`--start`）启动并轮询 `/api/health` 直到版本一致。

预期：最后出现「安装完成：tidbops <版本>」和**「访问信息」块**。

> ⚠️ **安装器默认建两名管理员 `admin` 与 `admin2`，两个账号的初始口令都在「访问信息」里，只显示这一次**，立刻抄到密码管理器，登录后各自改密。没拿到的情况：① 数据目录里已有用户（重装 / 保留数据）——沿用原口令；② 自动创建失败——输出里写了原因，服务首次启动会自己创建 `admin`，口令只在日志里：`journalctl -u tidbops | grep 初始口令`。
>
> 两处都找不到时，按 7.5 重置（先 `systemctl stop tidbops`，再 `user passwd --data-dir /var/lib/tidbops --name admin`，最后启动）。

#### 步骤 3 确认服务正常

```bash
systemctl status tidbops --no-pager                    # active (running)
curl -s http://127.0.0.1:8018/api/health               # {"ok":true,…,"standalone":true,"localTiup":true,…}
journalctl -u tidbops -n 30 --no-pager                 # 应有「独立部署模式（中控机）…」「监听 http://0.0.0.0:8018」
ss -lntp | grep 8018                                   # 默认监听 0.0.0.0:8018（改成只监听本机的则是 127.0.0.1）
```

#### 步骤 4 执行器所需的两处配置（新版安装器自动完成，旧安装的要手工补）

执行器要能正常工作，需要两件事：**临时目录不在 `/tmp` 下**，以及**沙箱里放开 `~tidb/.tiup` 和临时目录的可写权限**。原因：服务单元带 `PrivateTmp=yes`、`ProtectSystem=strict`、`ProtectHome=read-only`，经 `sudo` 起来的执行器子进程继承这个沙箱，系统 `/tmp` 里建的目录它看不到，`/home` 下也默认只读。不做的话，**扩容 / 改配置 / 预检等需要临时文件或写 `~tidb/.tiup` 的命令会失败**，导入集群时 `tiup cluster list` 也可能以退出码 1 失败。

**新装（本版及以后）**：安装器会自动做——建好 `/var/lib/tidbops-exec`（属主 `tidb`、0700），新生成的 `exec.json` 里 `tempDir` 指向它，并写入 drop-in `/etc/systemd/system/tidbops.service.d/exec-rw.conf`（`ReadWritePaths=-/home/tidb/.tiup -/var/lib/tidbops-exec`；路径前的 `-` 表示不存在时忽略）。已存在的 drop-in 不会被覆盖，升级也不会动它。**请检查**：

```bash
grep tempDir /etc/tidb-ops/exec.json                       # "tempDir": "/var/lib/tidbops-exec"
systemctl cat tidbops | grep ReadWritePaths                # 应能看到 drop-in 里的那一行
```

**旧安装（旧版安装器装的，`exec.json` 里 `tempDir` 是 `/tmp/tidbops-exec`）**：1.6.13 起，`upgrade.sh` 升级时会自动补建临时目录与 drop-in（已存在的保留），所以通常**升级一次即可**，不必再 `--force` 重装；`exec.json` 不会被改，`tempDir` 不合适时升级输出会提示，仍要自己改。想立刻处理、不等升级，也可以 `--force` 重装（同样补建目录和 drop-in），再改 `tempDir`，然后重启：

```bash
sudo ./install.sh --local-tiup --env prod --force --start      # 补建临时目录与 drop-in（不改 exec.json）
sudo sed -i 's#"tempDir": *"[^"]*"#"tempDir": "/var/lib/tidbops-exec"#' /etc/tidb-ops/exec.json
grep tempDir /etc/tidb-ops/exec.json
sudo systemctl daemon-reload && sudo systemctl restart tidbops
```

> ⚠️ 未验证（行为是按 systemd 与代码推断）：重启后请用第 5.7 节的「执行器全链路验证」确认。若某个 `tiup` 命令仍报 `read-only file system`，把报错里的路径追加进 drop-in 的 `ReadWritePaths=`；若 `.tiup` 不在 `/home/tidb`（`--home` 指定过别的目录），把 drop-in 里的 `/home/tidb/.tiup` 换成实际目录。`-` 前缀的路径在服务启动时不存在会被忽略，之后装好 tiup 要**重启一次服务**才会放开。

#### 步骤 5 数据库账号与口令文件

在**每个**要导入的集群里建账号（来源主机写平台机 IP；口令用强口令并记在密码管理器）：

```sql
-- 只读采集账号：慢 SQL、cluster_info、配置读取
CREATE USER 'tidbops_ro'@'10.0.9.10' IDENTIFIED BY '<强口令1>';
GRANT SELECT ON *.* TO 'tidbops_ro'@'10.0.9.10';

-- 运维账号：只允许 SET GLOBAL / SET CONFIG（执行器的 sql.set-global / sql.set-config 用）
CREATE USER 'tidbops_ops'@'10.0.9.10' IDENTIFIED BY '<强口令2>';
GRANT SYSTEM_VARIABLES_ADMIN, CONFIG ON *.* TO 'tidbops_ops'@'10.0.9.10';
```

> ⚠️ 未验证：权限名取自 12.7 的联调清单（`SYSTEM_VARIABLES_ADMIN`、`CONFIG`），个别系统表可能还需要额外权限（如 `PROCESS`）；以你的 TiDB 版本为准，接入后若「数据缺失」里出现权限类报错就按报错补。

运维账号口令写进 `exec.env`（只被执行器读取，因为 sudo 会清空环境变量）：

```bash
sudoedit /etc/tidb-ops/exec.env
# 取消注释并填写：
# TIDBOPS_OPS_PWD=<强口令2>
ls -l /etc/tidb-ops/exec.env                 # root:tidb 0640
```

只读账号的口令**在导入集群时在页面里填**（加密保存），不需要写文件。（`/etc/tidb-ops/env` 里的 `TIDBOPS_MYSQL_PWD` 仅用于形态 B 里「主集群」的只读账号。）

编辑 `/etc/tidb-ops/exec.json` 的其余字段，见第 6.3 节。保存后无需重启（执行器每条命令都重新读），但平台进程自己只在启动时读一次，建议 `sudo systemctl restart tidbops` 让两边一致。

#### 步骤 6 首次登录与基础安全设置

1. 浏览器打开 `http://<平台机器 IP>:8018`（默认直接监听；防火墙要放行 8018。若装的时候改成了只监听本机，用 5.4 的 SSH 隧道），用 `admin` 与安装时的口令登录。
2. 右上角「我的账号」**改掉 admin 的初始口令**；再用 `admin2`（口令同样在安装输出里）登录，同样改掉。
3. **第二名管理员**：安装器已默认建好 `admin2`，**正常情况下这一条不用做**。只有数据目录里已有用户（重装）、或安装输出提示创建失败时，才需要手工补建——此时**只有一名管理员，页面做不到**：在页面里创建带「管理员」角色的用户，平台不会直接生效，而是生成一条「授予管理员角色」提案，要**另一名管理员**确认，一个人确认不了。所以要停服务后用命令行建（服务运行时改用户库会被内存里的版本覆盖）：

   ```bash
   sudo systemctl stop tidbops
   sudo -u tidbops env TIDBOPS_USER_PASSWORD='<强口令>' \
     /opt/tidbops/current/bin/tidbops-server user add --data-dir /var/lib/tidbops --name bob --display "Bob" --role admin
   sudo -u tidbops /opt/tidbops/current/bin/tidbops-server user list --data-dir /var/lib/tidbops   # 账号都启用、带 admin
   sudo systemctl start tidbops
   ```

   重启后所有会话失效，两个账号都重新登录。之后再新增用户在「平台配置 → 用户管理」里建（侧栏「平台配置」分组，不是「权限与审计」），并按最小权限建 dba / sre / approver / auditor / developer / viewer，**按环境**授予角色。再要授予别人管理员，则一人在页面发起、另一人到「权限与审计 → 待确认提案」确认（1 小时内有效）。
4. 「权限与审计 → 部署自检」确认没有红色项（尤其「至少两名管理员」「不以 root 运行」「数据文件权限 0600」）。
5. （建议）「平台配置 → 水印设置」开启页面水印。

> ⚠️ **服务运行时不要用命令行改用户**（`user add / passwd / disable …`）：服务把用户表放在内存里，下次保存会整表写回，命令行的改动会被覆盖。必须用命令行时先停服务，见 7.5。

#### 步骤 7 导入集群并联调

1. 「平台配置 → 集群接入」→「从本机 tiup 导入」，勾选集群，填显示名、选**环境**（**生产集群选「生产」**，它决定审批强度和谁能操作；选错了在「编辑」里改，改完再开写开关），可选填一个只读 SQL 账号（步骤 5 的 `tidbops_ro`），点「导入所选」。没有这个按钮说明平台不是中控机模式，见 5.9。
2. 回到列表，对每个集群点「测试」，逐项（PD / Prometheus / Alertmanager / SQL）通过。
3. **先在测试集群**按 12.7 的顺序逐项联调（只读 → 告警静默 → 单实例重启 → 配置 → 扩缩容 → 维护），期间由两名管理员依次在「权限与审计 → 写操作开关」开启写操作。生产集群的写开关**联调通过后再开**。

> 导入只运行只读的 `tiup cluster list / display`；不改集群，也不读 SSH 私钥。手工添加的集群恒为只读（写闸门会写明原因）。导入时报「读取本机 tiup 集群列表失败：退出码 1」见 11.4；总览横幅报 `tiup … executable file not found` 见 11.3。

### 5.2 形态 A（独立部署）

与 5.1 的差别：没有 sudo / tiup / 执行器相关步骤（跳过 5.1 的步骤 4、5 中的运维账号与 `exec.env`）。

1. 前置：`mysql` 客户端；平台机到各集群 PD / Prometheus / Alertmanager / TiDB 的端口通。
2. 安装：`sudo ./install.sh --standalone --env prod --public-url https://ops.example.com --start`。
3. **先在平台机上自测连通性**（地址换成你的）：

   ```bash
   curl -s  http://10.0.1.21:2379/pd/api/v1/health                          # PD：返回成员健康列表
   curl -s -o /dev/null -w "%{http_code}\n" http://10.0.1.11:10080/status    # TiDB status：200
   curl -s "http://10.0.1.31:9090/api/v1/query?query=up" | head -c 200       # Prometheus：status=success
   curl -s -o /dev/null -w "%{http_code}\n" http://10.0.1.31:9093/api/v2/status   # Alertmanager：200
   mysql -h 10.0.1.11 -P 4000 -u tidbops_ro -p -e "select tidb_version()\G"   # TiDB SQL
   ```

4. 步骤 6（首次登录）同 5.1。
5. 「集群接入 → ＋ 接入集群」：**PD 地址必填**（本机没有 tiup 可自动探测），建议同时填 TiDB status、Prometheus、Alertmanager 与只读 SQL 账号；提交后点「测试」。
6. 页面顶部「N 项数据缺失」展开看原因；独立部署下「拓扑由 PD / cluster_info 推导」「中控机 / tiup 项显示未探测到」是**正常现象**。

### 5.3 形态 B（同机部署，旧）

`sudo ./install.sh --cluster <tiup 集群名> --env prod --start`，之后同 5.1 的步骤 4–6，并编辑 `exec.json`。形态 B 把安装时的集群当「主集群」，运维账号 / 只读账号口令分别写 `exec.env` / `env`。新部署请改用 5.1。

### 5.4 访问方式：直接监听、SSH 隧道与反向代理

平台有三种对外方式，按安全性从低到高排列。**默认是第一种**（直接监听 `0.0.0.0:8018`），要更安全就改成第二种或第三种。

| 方式 | 平台监听 | 特点 | 适合 |
|---|---|---|---|
| ① 直接监听（默认） | `0.0.0.0:8018` + `--allow-remote` | 浏览器直接打开 `http://<平台 IP>:8018`；**明文 HTTP**，口令与会话不加密 | 受信任内网、测试 / POC |
| ② SSH 隧道 | `127.0.0.1:8018` | 不开放任何端口；用的人要有机器的 SSH 权限 | 少数管理员、先试用 |
| ③ nginx 反向代理 + TLS | `127.0.0.1:8018` | HTTPS、来源网段白名单、对外只开 80 / 443 | **生产推荐** |

#### ① 直接监听（默认）

`install.sh` 默认以 `--listen 0.0.0.0:8018` 安装，单元里同时带 `--allow-remote`（服务端对非回环监听要求显式加它）。装好后放行防火墙端口就能从浏览器访问：

```bash
sudo firewall-cmd --permanent --add-port=8018/tcp && sudo firewall-cmd --reload     # 只对可信网段开放更好（rich rule 或安全组）
ss -lntp | grep 8018                                                              # 默认应看到监听 0.0.0.0:8018
```

**换端口 / 改成只监听本机**：安装时自己指定，后面的 `--listen` 会覆盖默认值：

```bash
sudo ./install.sh --local-tiup --listen 127.0.0.1:8018 --start     # 只监听本机（配 ② 或 ③）
sudo ./install.sh --local-tiup --listen 0.0.0.0:9000 --start       # 换端口
```

**已经装好的机器**：升级（`upgrade.sh`）沿用已装单元里的监听地址，不会因为新的默认值被改动。要改它，用 systemd drop-in 覆盖启动命令（drop-in 不会被升级覆盖）：先 `systemctl cat tidbops` 看现在的 `ExecStart`，原样复制到 drop-in 里，只改 `--listen`（要对外就保留 `--allow-remote`）：

```bash
sudo mkdir -p /etc/systemd/system/tidbops.service.d
sudo vi /etc/systemd/system/tidbops.service.d/listen.conf
#   [Service]
#   ExecStart=
#   ExecStart=<把 systemctl cat tidbops 里的 ExecStart 整段复制过来，改 --listen>
sudo systemctl daemon-reload && sudo systemctl restart tidbops
```

注意：① drop-in 里的启动命令是**固定的一份**，以后升级新版本若在单元模板里增加了参数，需要你手工同步（对照新版本包里 `deploy/ops/tidbops.service`）；② 升级的健康检查读主单元里的监听地址（`0.0.0.0` 会换成 `127.0.0.1` 去请求），所以 drop-in 里改的端口要和主单元一致，否则健康检查会失败。

#### ② SSH 隧道

平台改为只监听 `127.0.0.1:8018` 后，在自己电脑上：

```bash
ssh -L 8018:127.0.0.1:8018 你的用户@平台机器      # 保持这个窗口不关
# 浏览器打开 http://127.0.0.1:8018
```

#### ③ nginx 反向代理 + TLS（生产推荐）

nginx 与平台装在**同一台机器**，平台改为只监听 `127.0.0.1:8018`（按 ① 的办法改），nginx 的 `proxy_pass` 指向它：

```bash
sudo yum install -y nginx
sudo setsebool -P httpd_can_network_connect 1        # SELinux Enforcing 时，否则 nginx 访问后端会 502
sudo install -d -m 0750 /etc/nginx/ssl               # 放证书与私钥
```

`/etc/nginx/conf.d/tidbops.conf`：

```nginx
server {
    listen 80;
    server_name ops.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    server_name ops.example.com;
    ssl_certificate     /etc/nginx/ssl/ops.example.com.crt;
    ssl_certificate_key /etc/nginx/ssl/ops.example.com.key;
    ssl_protocols       TLSv1.2 TLSv1.3;

    # 访问控制：只放办公网段 / VPN（按你的网段改）；放行本机，否则在服务器上 curl 自检会被自己拦成 403
    allow 127.0.0.1;
    allow 10.0.0.0/8;
    deny  all;

    location / {
        proxy_pass http://127.0.0.1:8018;
        # 必须保持浏览器看到的 Host（含非标准端口）：写接口会校验 Origin 与 Host 一致，否则 403「拒绝跨站请求」
        proxy_set_header Host              $http_host;
        # 带上它，平台才会给会话 Cookie 加 Secure
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_read_timeout 120s;
    }
}
```

```bash
sudo nginx -t && sudo systemctl enable --now nginx
sudo firewall-cmd --permanent --add-service=http --add-service=https && sudo firewall-cmd --reload
```

证书路径（`ssl_certificate`）是占位的，nginx 报 `cannot load certificate … No such file` 就是路径没改成真实证书文件。`allow` 要把本机和你自己的来源网段都写进去（只写 10.0.0.0/8 时，在服务器本机 `curl` 或从其他网段访问都会被 nginx 自己返回 403，错误日志里是 `access forbidden by rule`）。

验证：

```bash
curl -sI https://ops.example.com/api/health | head -3                       # HTTP/1.1 200
curl -s -o /dev/null -D - -X POST https://ops.example.com/api/auth/login \
  -H 'Content-Type: application/json' -H 'Origin: https://ops.example.com' \
  -d '{"user":"nobody","password":"x"}' | grep -i "^HTTP"                   # 401（不是 403，说明 Host/Origin 一致）
ss -lntp | grep 8018                                                        # 仍然只在 127.0.0.1
```

> 已知限制：平台取**连接对端地址**作为来源 IP，不读 `X-Forwarded-For`；经 nginx 时它看到的永远是 127.0.0.1，所以「按 IP 限速 / 锁定」会让所有人互相连累（同一用户名连续输错 5 次锁 10 分钟不受影响）。

### 5.5 通知渠道

「环境与设置 → 通知渠道」（需要 settings.edit；**每个集群各自一份**，先在顶栏切到对应集群）：企业微信 / 钉钉 / 飞书 / Lark / Slack / 其他（通用 JSON Webhook）。要点：

- Webhook 只允许 https 与官方域名；内网网关与「其他」渠道的主机要由管理员在启动参数 `--notify-allow-hosts` 里加入（用 drop-in 覆盖 `ExecStart` 加这个参数，见 5.4 的方法）；目标为内网 / 回环 IP 一律被拒。
- URL 与密钥用 `master.key` 加密保存，页面只显示末 4 位。
- 每渠道每分钟最多 20 条，相同事件 10–30 分钟内去重，失败重试 3 次。
- 保存后一定点「发送测试」确认连通。详细的机器人创建步骤见页面帮助「环境与设置」。

### 5.6 部署一套新的 TiDB 集群（需要时）

平台本身不安装 TiDB 的各个组件，集群由包内的 `tidb-deploy` 工具部署，有两种用法：**手工**在中控机上执行（5.6.1，所有形态通用），或在平台页面「安装部署」里**一键执行**（5.6.2，仅形态 C）。集群建好之后：形态 C 在页面「从本机 tiup 导入」，形态 A 手工接入（5.10）。

用一键部署时，中控机 root 密码要经过平台（只用一次，不落盘）；不想让平台经手 root 密码，就用手工方式。

#### 5.6.1 手工部署（用包里的 tidb-deploy）

平台只提供工具；**要在页面里一键执行见 5.6.2**（仅中控机模式）。手工方式：包内 `deploy/tidb-deploy/tidb-deploy-0.3.1.tar.gz`（页面「安装部署」也能生成 `deploy.conf` 与命令、下载离线包）。**在要作为 tiup 中控机的机器上**执行：

**先完成前置条件（脚本只检查，不再替你创建用户、配 sudo 或分发公钥，也不再用 root 登录）**：所有部署机器和中控机上都要有部署用户（默认 `tidb`）、该用户有 sudo 免密、且该用户能用密钥免密 SSH 登录集群所有机器，**包括中控机自己**。

```bash
# ① 每台机器（含中控机），root 执行
useradd -m -s /bin/bash tidb && echo '<口令>' | passwd tidb --stdin
echo 'tidb ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/tidb && chmod 440 /etc/sudoers.d/tidb && visudo -cf /etc/sudoers.d/tidb
# ② 分发公钥要用口令登录：若 sshd 禁用了口令登录，先临时打开（每台机器，root 执行；已允许可跳过）
sed -i -E 's/^#?PasswordAuthentication .*/PasswordAuthentication yes/' /etc/ssh/sshd_config && systemctl restart sshd
# ③ 中控机，以 tidb 身份生成密钥并分发到集群每台机器（含中控机自己，每台输入一次 tidb 的口令）
sudo -iu tidb
ssh-keygen -t rsa -b 4096 -N '' -f ~/.ssh/id_rsa
ssh-copy-id -i ~/.ssh/id_rsa.pub tidb@<每台机器IP>
# ④ 复测（每台都不输入口令、不报错才算通过）
ssh tidb@<IP> 'sudo -n true' && echo OK
# ⑤ 全部分发完后，如第 ② 步改过 sshd，改回（每台机器，root 执行）
sed -i -E 's/^#?PasswordAuthentication .*/PasswordAuthentication no/' /etc/ssh/sshd_config && systemctl restart sshd
```

`-E` 不要写成 `-ir`（`-i` 后紧跟的字母会被当成备份后缀）。口令请自己定强口令。不满足时 `check` / `deploy` 在最开始就中止，并列出哪台机器缺哪一项和修复命令。

```bash
tar xzf tidb-deploy-0.3.1.tar.gz && cd tidb-deploy-*
./tidb-deploy.sh wizard                                  # 交互生成 deploy.conf（或 cp deploy.conf.example 手改）
sudo ./tidb-deploy.sh check  --config deploy.conf        # 只读环境检查（先检查前置条件；需要 root 才能以 tidb 身份检查）
sudo ./tidb-deploy.sh deploy --config deploy.conf --dry-run   # 预览完整流程，不改动
./tidb-deploy.sh show-prep --config deploy.conf | less   # 审阅将经 sudo 在各机器上执行的初始化脚本
sudo ./tidb-deploy.sh deploy --config deploy.conf        # 正式部署
```

要点：

- **CentOS 7 的版本限制**：glibc 2.17，TiDB 只能装 **≤ v8.3**（推荐 `v7.5` 或 `v8.1`），v8.4 起官方不再支持 CentOS 7，脚本会拒绝。`TIDB_VERSION=auto` 会按系统自动选。
- CentOS 7 官方 yum 源已失效，需要时在 `deploy.conf` 里设 `REPO_FIX=auto`（备份 repo 并切到 vault 镜像）；**脚本会修改系统**，务必先 `--dry-run` 与 `show-prep`。
- 无外网：在联网机器上用 `download-offline` 下载离线包，部署时用 `--offline-dir`（页面「安装部署」里有现成命令）。
- 最小单机测试配置：`CLUSTER_NAME=tidb-test` 与 `DEPLOY_MODE=single` 两行（仅开发测试，无高可用）。
- 部署完成后，用 `tiup cluster display <集群名>` 查看各组件地址（PD / Prometheus / Alertmanager / Grafana / TiDB），再按 5.10接入平台。

---

#### 5.6.2 在页面里一键部署（中控机模式）

仅形态 C（`--local-tiup`）、平台管理员（`settings.edit`）可用，且需要**两名管理员**。执行的仍是上面5.6.1的 `tidb-deploy.sh`，页面替你启动并回传全部输出。**前置条件与5.6.1完全一样**（部署用户、sudo 免密、SSH 免密）。

```
页面「安装部署」填表 → 「保存配置」（服务端校验并存为 v1、v2…）
 → 「一键部署」标签：输入中控机 root 密码 → 预检（只读，实时自检日志）
 → 预检通过 → 提交部署申请 → 另一名管理员批准
 → 输入集群名 + root 密码 → 开始部署（实时输出，可取消）
 → 成功：访问信息 + 新集群 TiDB root 随机密码（只显示一次）；失败：从断点继续
```

**离线环境（内网没有外网）**：一键部署**不传离线包**，离线包要先放到中控机上；页面第 4 步选「离线安装」并填目录（`OFFLINE_PKG_DIR`），其余流程不变。

```bash
# ① 联网机器：下载离线包（含校验 SHA-256）；--tidb-version 要完整版本号，--target-os 提前检查版本与目标系统兼容性
./tidb-deploy.sh download-offline --tidb-version v7.5.4 --dir /opt/tidb-pkg --arch amd64 --with-toolkit --target-os rocky9
# ② 拷到中控机并核对
scp -r /opt/tidb-pkg 中控机:/opt/tidb-pkg && ssh 中控机 'cd /opt/tidb-pkg && sha256sum -c *.sha256'
# ③ 页面：安装部署 → 4 · 安装方式 选「离线安装」→ 填 /opt/tidb-pkg → 保存配置 → 一键部署 → 预检 …
```

- 目录必须是中控机上的绝对路径，只含字母数字和 `_ . / -`（保存配置时服务端会拒绝其他字符）；目录由中控机上的 root 读取，**不需要**给 `tidbops` 授权。
- **预检会检查离线包**：目录不存在 / 不可读 / 没有所选版本的 `tidb-community-server-v<版本>-linux-<架构>.tar.gz`，输出里会报错并列出目录里已有的版本。选「自动」或「指定系列」取目录里该系列的最高版本；选完整版本时必须有这一版。
- TiUP 也从离线包装（`local_install.sh`），有 toolkit 包时再 `tiup mirror merge`；中控机不需要外网。
- 目标机没有可用 yum 源时，需要自备依赖 rpm（`numactl`、`chrony`、`libaio` 等），页面里填「依赖 rpm 所在目录」；脚本是在**每台目标机上**检查这个目录并 `rpm -Uvh`，所以要把 rpm 拷到每台机器的同一路径。**这一点按脚本实现推断，未在真实环境验证。**
- 页面不检查目录是否存在（路径在中控机上，页面看不到），以预检结果为准。

**它在服务器上做了什么：**

| 步骤 | 实际行为 |
|---|---|
| 保存配置 | 校验后写 `/var/lib/tidbops/deploy/configs/<集群名>/v<N>.conf`（权限 600）与 `index.json`；deploy.conf 由服务端渲染，**不接受前端传来的原始文本**（脚本会以 root source 它） |
| 预检 / 部署 | 把包内 `tidb-deploy-<版本>.tar.gz` 解到 `/var/lib/tidbops/deploy/jobs/<任务号>/tool/`（校验 .sha256），生成 `run.sh`，然后 `su - root -c "systemd-run --unit=tidbops-deploy-<任务号> /bin/bash <任务目录>/run.sh"`；root 密码经伪终端交给 su，只用一次 |
| 输出 | `run.sh` 把脚本的标准输出 / 错误追加到 `<任务目录>/out.log`，退出码写进 `<任务目录>/exit`；页面轮询读取。**任务由 systemd 管理，平台重启不会中断它** |
| TiDB root 密码 | 脚本自己把它写在 `/var/lib/tidb-deploy/<集群名>/root-password.txt`（600）；成功后 `run.sh` 复制一份到任务目录，页面点「显示」时读取并**删除这一份**（中控机上的原文件不动） |
| 取消 | `su - root -c "systemctl stop tidbops-deploy-<任务号>"`（再输一次 root 密码）；已完成的阶段保留，脚本的 `state` 文件记着，可从断点继续 |

**为什么要用 `systemd-run`**：平台服务的单元带 `ProtectSystem=strict` 等沙箱，从它里面起的任何进程（包括 root）都不能写 `/etc`、改内核参数、装软件包；`systemd-run` 让 systemd 主进程在沙箱**外**起一个临时单元，所以脚本能做它该做的事。

**安全要点：**

- root 密码**不落盘**（配置、任务目录、输出、审计、下载文件里都没有；有测试检查这一点）；只经 HTTPS 传到后端；连续输错 3 次锁定该账号 10 分钟；输错时没有任何命令被执行。
- 高风险：需要两名不同的管理员；申请时锁定配置版本与 sha256；集群名二次确认；同一时间只有一个任务。
- 审计动作：`deploy.config.save`、`deploy.precheck`、`deploy.request`、`deploy.approve`、`deploy.start`、`deploy.cancel`、`deploy.secret.reveal`（摘要里没有任何口令）。系统的 `/var/log/secure` 也会有 `su` 与 `systemd-run` 的记录，两边可以对上。
- 任务目录在数据目录里（属主 tidbops，0700）：`out.log`、`exit`、`run.sh`、`deploy.conf` 的副本、工具包解压目录。需要清理旧任务时直接删对应的任务目录（失败任务的排查材料也在这里）。

**常见问题：**

| 现象 | 原因 | 处理 |
|---|---|---|
| 页面没有「一键部署」标签 | 不是中控机模式 / 账号没有 `settings.edit` / 不是 Linux / 包里没有工具包 | 标签不出现时，打开 `GET /api/deploy/status` 看 `reason` |
| 点预检提示「su 执行失败：…」 | `su` 被限制（`pam_wheel`、`securetty`）、或服务单元的 `RestrictAddressFamilies` 挡住了 PAM 的审计模块（netlink） | 先在中控机上以 `tidbops` 身份手工 `su - root -c id`（`sudo -u tidbops` 无 shell 时用 `runuser -u tidbops -- su - root -c id`）看真实报错；需要 netlink 时用 drop-in 给服务加 `RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK` |
| 预检报「离线包目录不存在或不是目录」「没有找到 … 对应的离线安装包」 | 离线包没放到中控机的这个目录，或版本 / 架构不对（文件名要形如 `tidb-community-server-v7.5.4-linux-amd64.tar.gz`） | 报错里会列出目录已有的版本；把对应的包放进去，或改页面里的版本 / 架构（架构按中控机自动判断） |
| 提示「root 密码不正确」但确定是对的 | 用了 `tidb` 等其他账号的密码；或 root 被设置为禁止密码登录 / 锁定（`passwd -S root`） | 手工 `su -` 验证 |
| 任务启动后很久没有输出 | `systemd-run` 起来了但脚本卡在某个阶段（例如等 SSH） | 在中控机上看 `journalctl -u tidbops-deploy-<任务号>` 与任务目录里的 `out.log` |
| 页面显示「状态未知」 | 超过 6 小时没有退出码：任务被外部杀掉，或中控机重启过 | 看任务目录与 `systemctl status tidbops-deploy-<任务号>`；确认没有在跑后重新预检 / 继续 |
| 点「开始部署」提示「已有部署任务在运行」 | 同一时间只允许一个 | 等它结束，或先取消 |

> ⚠️ 验证状态：各环节有单元测试与假执行器下的端到端测试；**没有在真实 CentOS 7.9 / Rocky 上跑过**（特别是 `su` 经伪终端、沙箱里的 PAM、`systemd-run` 219 的行为）。首次使用请在测试机上完整走一遍（包括输错密码、取消、从断点继续）。

### 5.7 验收 Runbook

每一项都要有**命令输出或截图**作为证据，留档。

| # | 验收项 | 怎么验证 | 通过标准 |
|---|---|---|---|
| 1 | 包完整 | `sha256sum -c *.sha256` | OK |
| 2 | 服务运行 | `systemctl is-active tidbops`；`curl -s 127.0.0.1:8018/api/health` | `active`；`"ok":true` 且版本正确 |
| 3 | 以非 root 运行 | `ps -o user= -C tidbops-server` | `tidbops` |
| 4 | 数据文件权限 | `sudo ls -l /var/lib/tidbops` | `master.key`、`users.json`、`audit.log` 等为 `-rw-------`，目录 0700 |
| 5 | 监听范围符合预期 | `ss -lntp \| grep 8018` | 直接监听：`0.0.0.0:8018`，防火墙只对可信网段开放；走反向代理：仅 `127.0.0.1:8018` |
| 6 | HTTPS 与 Secure Cookie | `curl -s -o /dev/null -D - -X POST https://… /api/auth/login …` 登录成功时看 `Set-Cookie` | 含 `Secure; HttpOnly; SameSite=Strict` |
| 7 | 初始口令已改、有 ≥ 2 名管理员 | 页面「部署自检」 | 没有红色项 |
| 8 | 集群已接入且数据完整 | 「集群接入 → 测试」；总览有数据 | 各项通过；「N 项数据缺失」都能解释 |
| 9 | 审计可用 | 「权限与审计 → 审计日志」有登录 / 接入记录；点「校验哈希链」 | 通过 |
| 10 | **执行器全链路**（B / C） | 见下 | 返回 JSON 行，不是 sudo 报错 |
| 11 | sudoers 正确（B / C） | `sudo visudo -cf /etc/sudoers.d/tidbops` | `parsed OK` |
| 12 | 紧急熔断可用（B / C） | `sudo touch /etc/tidb-ops/READONLY`，页面写开关标签变「强制只读」；`sudo rm /etc/tidb-ops/READONLY` | 能触发也能恢复 |
| 13 | 备份可做、可恢复 | 第 9 章的演练 | 在测试机恢复成功 |

第 10 项（执行器全链路，形态 B / C）：

```bash
# 平台用户 → sudo → tidb 用户的受限入口；只读命令 cluster.list
echo '{"cmd":{"kind":"cluster.list"}}' | \
  sudo -u tidbops sudo -n -u tidb /opt/tidbops/current/bin/tidbops-server exec --stdin
# 预期：若干 JSON 行，其中含 tiup cluster list 的表格输出，最后一行带 "exit":0
# 若出现 "a password is required" / "not allowed"：sudoers 路径不匹配，见 11.4
# 若返回「校验失败」类 JSON：策略 / 参数问题，读 JSON 里的 err 字段
```

### 5.8 开启某个集群的写操作（两名管理员）

写操作开关**每个集群各自一个**，新导入的集群默认关闭；要两名**不同**的管理员依次确认。开之前先把前置条件理顺：

1. **环境选对**：集群的环境（测试 / 预发 / 生产）决定审批级数，生产高风险变更需要 2 级审批并要求申请人 MFA。环境不对先在「集群接入 → 编辑」改，再开开关。
2. **通过部署自检**：「权限与审计 → 部署自检」没有红色项（自检不会自动拦住开关，要自己看）；执行器全链路已按 5.7 验证。
3. **准备能发起变更的账号**：管理员角色**不能发起集群变更，也不能审批变更**。要做扩缩容、改配置等，另建带 dba / sre 角色的账号，再配 approver 审批人，**按环境**授予。

开启步骤：

1. 管理员 A：顶栏选好目标集群 → 「权限与审计 → 写操作开关」→ 核对卡片标题里的集群与环境 → 点「提议开启写操作」。提示「已提议开启，等待另一名管理员确认」，此时写操作**仍未开启**。
2. 管理员 B：**1 小时内**用自己的账号登录（另一个浏览器或无痕窗口，同一个人换浏览器不行），顶栏切换到**同一个集群**，进同一标签页，点「确认开启写操作」。右上角由「只读」变为「写操作已开启」。
3. 超时未确认提议作废，需重新发起；A 可在期间「撤销提议」。

开启之后：先从低风险变更试起；平台**没有**对已完成变更的一键回滚；紧急时一名管理员点「立即关闭（熔断）」，或在中控机 `touch /etc/tidb-ops/READONLY` 强制全局只读。「当前不能写的原因」那一行会写明还差哪个条件。

**写开关开不了时的排查**（先到「权限与审计 → 写操作开关」看「当前不能写的原因」那一行，它会直接写出还差哪一条）：

| 现象 / 提示 | 原因 | 处理 |
|---|---|---|
| 提示「需要另一名管理员确认」 | 两次点的是同一个账号 | 必须换另一个**不同**的管理员账号确认 |
| 没有「写操作开关」页，或按钮点了无反应 | 当前账号不是管理员 | 用有管理员角色的账号 |
| 过了很久才确认，变成了新的提议 | 提议 1 小时内有效，超时后第二个人点的会变成新提议 | 让第一个人再确认一次，两次点击间隔在 1 小时内 |
| 只有一名管理员 | 没有第二个人能确认 | 安装器默认建了 `admin` 与 `admin2`；没有的话按 5.1 步骤 6 用命令行补建 |
| 写开关已开，但集群仍显示「只读」 | 开的是别的集群的开关（写开关每个集群各自一个）；或该集群是手工接入的远程集群；或还有别的闸门没满足 | 顶栏选中目标集群后再看；手工接入的集群需删除后改用「从本机 tiup 导入」 |
| 「当前不能写的原因」写着执行器 / 版本 / 强制只读 | 对应闸门没满足 | 见 11.4 的对照表 |

### 5.9 重装、卸载残留与从旧形态转换

**卸载保留什么**：`admin uninstall` 默认只删服务、systemd 单元、sudoers、systemd drop-in 和 `current` 链接，**保留**数据目录 `/var/lib/tidbops`、配置目录 `/etc/tidb-ops`、备份、`releases` 与 `tidbops` 账号。这些残留会影响重新安装：

| 残留 | 影响 | 处理 |
|---|---|---|
| `/etc/tidb-ops/exec.json`（旧安装器生成） | 安装器保留它，不生成新版默认值：`tempDir` 仍在 `/tmp`、`clusters` 仍是旧集群名 | 要新默认值就先改名：`sudo mv /etc/tidb-ops/exec.json{,.old}` |
| `/var/lib/tidbops/users.json` | 安装器**不再生成初始密码**，沿用原来的 `admin` 与口令 | 想全新开始就把整个数据目录改名挪走（`mv /var/lib/tidbops /var/lib/tidbops.old`），确认没问题再删；**不要只删部分文件**（`master.key` 加密着集群口令，`audit.log` 是哈希链） |
| 针对 `tidbops` 的 ACL（5.1 前置第 12 项） | 账号重建后用户 ID 变了，授权失效，配置变更读不到 `meta.yaml` | 重做第 3 章第 12 项的 `setfacl` |

**重装同版本**：`sudo ./install.sh … --force --start`。`--force` 不覆盖密钥与配置文件，会按新参数重写服务单元；**但对已经在运行的服务不会重启**（`--start` 用的是 `enable --now`），所以装完要手工 `sudo systemctl daemon-reload && sudo systemctl restart tidbops`，并用 `systemctl cat tidbops | grep ExecStart` 确认参数已变。`--force` 只能用于同版本，版本不同请用 `admin upgrade`。

**从旧的「同机部署」（形态 B，装的时候带了 `--cluster`）转换为中控机模式（形态 C）**：症状是「集群接入」里有一个标「主」的集群、没有「从本机 tiup 导入」按钮，编辑它时连接信息被禁用（提示「主集群的环境与连接配置来自平台启动参数」）——这是形态 B 的设计，不是故障。转换步骤：

```bash
# 1. 备份（数据目录含 master.key，备份文件按机密对待）
sudo /opt/tidbops/current/bin/tidbops-server admin backup --out /backup/tidbops/before-localtiup-$(date +%F-%H%M).tar.gz
# 2. 在发布包目录里（确认 deploy/tidb-deploy/ 里没有多出来的解压目录，见 4.2）
sudo ./install.sh --local-tiup --env prod --force --dry-run
sudo ./install.sh --local-tiup --env prod --force --start
# 3. 服务已在运行，必须手工重启
sudo systemctl daemon-reload && sudo systemctl restart tidbops
# 4. 确认已转换
systemctl cat tidbops | grep -E "ExecStart|--standalone|--local-tiup|--cluster"   # 有 --standalone --local-tiup，没有 --cluster
curl -s http://127.0.0.1:8018/api/health | grep -o '"localTiup":[a-z]*'            # "localTiup":true
```

之后浏览器 `Ctrl+F5`、重新登录，在「集群接入」点「从本机 tiup 导入」。旧的 `exec.json` 里集群白名单是原来那一个集群名，导入别的集群前要改成 `["*"]` 或追加集群名。

> ⚠️ 未验证：从形态 B 原地转换为形态 C 没有在真实环境跑过，原来的变更记录、审计与该集群的关联是否完整接续也未实测，所以**备份不能省**；结果不对用 `admin restore` 恢复。

### 5.10 接入集群：字段说明与完整性检查

#### 在集群里建只读 SQL 账号

平台用它读慢 SQL、`cluster_info`、配置等，**只给只读**（运维账号见 5.1 步骤 5）：

```sql
-- 在目标 TiDB 集群里执行（host 限定为平台所在机器的 IP）
CREATE USER 'tidbops_ro'@'10.0.9.10' IDENTIFIED BY '强口令';
GRANT SELECT ON *.* TO 'tidbops_ro'@'10.0.9.10';
```

> ⚠️ 未验证：个别系统表（慢日志、statement summary 等）可能还需要额外权限（如 `PROCESS`）。接入后若「数据缺失」里出现权限类错误，按报错补权限。

#### 手工接入（形态 A，或形态 C 里的远程集群）

用管理员登录 → 侧栏「平台配置 → 集群接入」→「＋ 接入集群」：

| 字段 | 说明 |
|---|---|
| 集群 id | 平台内唯一，小写字母开头，只含小写字母 / 数字 / 连字符，2–32 位，**创建后不可改**（如 `prod-sh`） |
| 显示名 / 描述 | 顶栏下拉里显示的名字 |
| 集群名 name | 仅作标识与审计记录（独立部署时与 tiup 无关），默认同 id |
| **环境** | 测试 / 预发 / 生产。**决定用户在该集群里的角色权限与审批强度**，生产更严格 |
| 模式 | 选 **live 实时** |
| **PD 地址** | **必填**，如 `http://10.0.1.21:2379` |
| TiDB status / Prometheus / Alertmanager | 建议都填 |
| MySQL host / 端口 / 用户 / 密码 | 只读 SQL 账号；密码用平台的 `master.key` 加密保存，接口不回显 |

提交后在列表里点「**测试**」：逐项只读探测 PD 健康、TiDB status、Prometheus、Alertmanager、SQL。全部通过后回到「集群总览」，在顶栏下拉里选这个集群。**手工接入的集群恒为只读**，即使开了写开关也不能执行变更。

#### 接入后怎么看是否完整

页面顶部横幅会显示「N 项数据缺失」，展开可看原因。常见对应关系：

| 缺失提示 | 原因 | 处理 |
|---|---|---|
| 缺少 Prometheus 节点指标 | 没填 Prometheus，或未部署 node_exporter | 补地址 / 部署 node_exporter |
| 总览的 QPS 显示「—」、趋势图停在过去某个时刻 | 1.6.14 之前的版本把 Prometheus 查询窗口在服务启动时固定了，之后一直不前移 | 升级到 1.6.14 及以上；临时办法是重启服务 |
| 智能诊断里没有 PD 副本 / TiKV 写入链路 / 主机层的诊断项（集群确有问题时） | 对应的 Prometheus 指标没有数据（指标名或标签与版本不同、node_exporter 太旧、Prometheus 没抓到该组件）；规则取不到数据时**不触发也不报错** | 在 Prometheus 里查对应指标，清单见 12.8 |
| 智能诊断里没有「执行计划突变 / 统计信息缺失 / 锁冲突」等慢日志对比项 | 只读账号没有 `cluster_slow_query` 权限，或该 TiDB 版本缺 `Stats` / `Backoff_types` 等列，或最近 2 小时没有慢日志；原有慢 SQL 列表不受影响 | 用只读账号手工执行 `SELECT Digest, Plan_digest, Stats, Backoff_types FROM information_schema.cluster_slow_query LIMIT 1;` 看报错 |
| 告警不可用 | 没填 Alertmanager 或不通 | 补地址 / 查网络 |
| 慢查询 / 配置读取失败 | 没填 SQL 账号、口令错、权限不足 | 查本节第一段与 5.2 的自测命令 |
| 远程接入：拓扑由 PD / cluster_info 推导 | 没有 tiup，**正常现象** | 无需处理（不含 TiCDC 等组件，也没有实例目录） |

「环境与设置」里中控机 / tiup 相关项显示「未探测到」也是手工接入的正常现象。

#### 版本支持

TiDB 版本在 v7.5 / v8.1 / v8.5 系列内为「已验证范围」；其他版本显示「未验证」，只读功能仍可用，写操作会自动降级为不可用。

#### 总览数据对不上时怎么排查（以 QPS 为例）

平台不直接数 SQL，QPS 来自 Prometheus 的 `sum(rate(tidb_server_query_total[1m]))`。数据对不上时，在**平台所在机器**上按顺序查：

```bash
PROM=10.0.0.9:9090      # ← 换成「集群接入」里该集群的 Prometheus 地址

# 1) Prometheus 有没有采到 TiDB、有没有这个指标
curl -sS "http://$PROM/api/v1/query" --data-urlencode 'query=up{job="tidb"}'
curl -sS "http://$PROM/api/v1/query" --data-urlencode 'query=sum(rate(tidb_server_query_total[1m]))'
curl -sS "http://$PROM/api/v1/query" --data-urlencode 'query=count(tidb_server_query_total)'

# 2) 两台机器时钟是否一致（平台按自己的时钟取最近 60 分钟）；前后两个 date 之间应夹着 Prometheus 的 time()
date +%s; curl -sS "http://$PROM/api/v1/query" --data-urlencode 'query=time()'; echo; date +%s

# 3) 平台自己拿到了什么：登录后在浏览器打开（集群 id 换成你的）
#    http://<平台地址>:8018/api/snapshot?cluster=<集群 id>     看 overview.qps 与 errors
```

结论对照：第 1 步没有数据 → Prometheus 没抓到 TiDB（看 `http://$PROM/targets`）；第 1 步有数据但页面是 0 → 看第 2 步的时钟，以及 `/api/snapshot` 里的 `errors`；页面按一位小数显示，低于 0.05 的 QPS 会显示成 0。

### 5.11 在页面里向中控机发命令（「中控机命令」，中控机模式）

仅形态 C（`--local-tiup`，执行器为 `--exec sudo`）。侧栏「运维 → 中控机命令」：被授予 `shell.run` 的用户可以在页面里向中控机发命令、实时看输出，被授予 `shell.approve` 的用户批准他人的变更申请；只有 `audit.view` 的账号（如审计员）只能看历史与输出。权限在「角色权限」页授予自定义角色再分配给用户，内置「管理员」角色默认两项都有。**1.6.0 起不需要输入中控机 root 密码**（「一键部署」仍需要）。**它是一个能在中控机上执行命令的功能，上线前请在测试机上完整走一遍。** 使用步骤、分级规则和限制见帮助「中控机命令」。

**它在服务器上做了什么**（执行方式与 10A 的 `su` 不同：一律以 `tidb` 身份，经 sudoers 放行的固定入口）：

| 步骤 | 实际行为 |
|---|---|
| 判定 | 服务端按规则把每条命令分成只读 / 变更 / 禁止；禁止的直接拒绝（不调用执行通道）；变更要先有另一名持有 `shell.approve` 的用户批准的申请，批准锁定「命令 + 目录 + 超时 + 遇错停止」的哈希，只能用一次、1 小时内有效。需要 root 权限的命令在整行开头写 `sudo`（只认 `sudo 命令` / `sudo -n 命令`；`-u / -i / -s`、管道后面的 `sudo` 拒绝），级别仍按命令本身判定 |
| 执行（只读） | 平台进程执行 `sudo -n -u tidb /opt/tidbops/current/bin/tidbops-server shell-exec --stdin`（`/etc/sudoers.d/tidbops` 里放行的第二条固定命令），请求 JSON 经 stdin 传入、不落盘；子进程**自己再判定一遍**，不是只读就拒绝，然后逐条 `bash -c` 执行（`env -i` 最小环境、进程组、超时），输出在子进程里逐行遮蔽后回传；仍在平台服务沙箱里（文件系统只读）；`sudo` 行由 tidb 再 `sudo -n` 执行。平台服务重启时这类任务中断 |
| 执行（变更） | 同一个入口用 tidb 的 sudo 免密执行 `sudo -n systemd-run --unit=tidbops-shell-<任务号> /bin/bash <任务目录>/run.sh`，在沙箱之外以 root 起临时单元；命令文本只写进 `<任务目录>/cmds.txt`（数据），**从不拼进脚本**；脚本逐条用 `runuser -u tidb -- env -i … bash -c 'cd "$0" && exec bash -c "$1"'` 执行，整行以 `sudo` 开头的行去掉前缀直接以 root 执行。设置里勾选「只读命令也用临时单元执行」后，只读也走这条路径 |
| 输出 | 只读：子进程流式遮蔽口令 / 密钥 / 手机号 / 身份证号 / 银行卡号后逐行回传，平台追加进 `<任务目录>/out.log`；变更：脚本把输出经 `sed -u -E -f mask.sed` 遮蔽再 `head -c 1048576` 追加进 `out.log`，结束写 `exit`、删除 `cmds.txt`。页面读取时再遮蔽一遍 |
| 取消 | 只读任务：关掉子进程 stdin 并发 SIGTERM，子进程终止整个进程组（退出码 143）；变更任务：经同一入口 `sudo -n systemctl stop tidbops-shell-<任务号>`。都不需要密码 |
| 存放与清理 | `/var/lib/tidbops/shell/jobs/<任务号>/`（0700）；默认 30 天后自动删除，可选「只存摘要」；设置在 `/var/lib/tidbops/shell/settings.json`，页面「策略设置」修改并写审计 |

**权限与审计**：执行 / 提交申请 / 取消 = `shell.run`；批准 = `shell.approve`；改策略设置 = `settings.edit`（仅内置管理员）；看历史与输出 = `shell.run`、`shell.approve` 或 `audit.view`，**看别人的输出要 `audit.view` 并记 `shell.output.view`**，下载记 `shell.output.download`。审计动作：`shell.run`（含被拒绝的，摘要含执行路径）、`shell.request`、`shell.approve`、`shell.cancel`、`shell.settings`、`shell.output.view`、`shell.output.download`；命令在审计里已打码。

**数据安全要点**：① 一律以 `tidb` 身份执行（需要 root 权限的行写 `sudo`，依赖 tidb 的 sudo 免密）；② 拦截读密钥与口令、读业务数据、数据外传、命令替换 / here-doc；③ 读文件内容的命令只允许日志与系统信息目录，数据目录不允许，部署目录不是 `/tidb-deploy` 时在「策略设置」追加；④ 输出遮蔽 + 留存期限；⑤ **`shell.run` 等同于中控机上 tidb（有 sudo 免密）的权限，只授予确实需要的人；想让执行的人和批准的人不同，就放进不同的角色**；⑥ 这是**基于命令文本的护栏，不是对抗恶意管理员的边界**——真正的边界是 sudoers 放行了谁、谁被授予了 `shell.run` / `shell.approve`、两人审批和审计。不需要这个功能的环境，从 `/etc/sudoers.d/tidbops` 删掉 `shell-exec` 那一行即可关闭（页面显示通道不可用）。

**常见问题**：

| 现象 | 原因 | 处理 |
|---|---|---|
| 侧栏没有「中控机命令」 | 不是中控机模式 / 账号没有 `shell.run`、`shell.approve`、`audit.view` 任一项 / 不是 Linux / 执行器不是 `--exec sudo` / 数据目录没配置 | 用管理员账号看 `GET /api/shell/status` 的 `reason`；在「角色权限」给相应角色授予能力 |
| 页面顶部红色「执行通道不可用」 | `/etc/sudoers.d/tidbops` 没有放行 `shell-exec --stdin`（手工维护 sudoers 的机器升级后没有补上第二条） | `sudo cat /etc/sudoers.d/tidbops` 核对；`sudo ./install.sh --force` 重新生成，或手工补上 `tidbops ALL=(tidb) NOPASSWD: /opt/tidbops/current/bin/tidbops-server shell-exec --stdin` 后 `visudo -cf` 校验 |
| 页面顶部黄色「tidb 没有 sudo 免密」；`sudo` 行与变更命令失败 | 中控机上 tidb 没有 `NOPASSWD` sudoers（tidb-deploy.sh 的前置条件之一） | `echo 'tidb ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/tidb && chmod 440 /etc/sudoers.d/tidb && visudo -cf /etc/sudoers.d/tidb`；验证：`sudo -u tidb sudo -n true` |
| `ss` / `ip addr` 报 netlink 不支持 | 只读命令在平台进程内执行，受服务单元沙箱限制；1.6.0 的单元模板已放开 `AF_NETLINK`，旧单元没有（升级后单元会重新生成，未重启则未生效） | `systemctl cat tidbops` 确认含 `AF_NETLINK` 并重启服务；临时办法：策略设置勾选「只读命令也用临时单元执行」 |
| 判定「禁止」但命令看起来正常 | 命中护栏（路径不在可读目录、相对路径、`$(…)`、`env` 等） | 看判定里的逐条原因；用绝对路径；追加可读目录 |
| 任务显示「状态未知」/「平台服务重启，任务中断」 | 状态未知：超过 6 小时没有退出码；中断：平台进程内执行的任务随平台服务重启而结束 | 变更任务看 `systemctl status tidbops-shell-<任务号>` 与任务目录；中断的任务重新执行 |
| 输出里仍有敏感信息 | 遮蔽靠格式识别，有漏报（如 15 位身份证、非标准口令写法） | 缩短保留天数或选「只存摘要」；对应用日志本身减少记录业务字段 |

> ⚠️ 验证状态：分级 / 遮蔽 / 审批 / 接口权限 / 子进程协议有单元测试；子进程的执行（遮蔽、遇错停止、取消）与临时单元脚本在 bash 里跑通过；**沙箱里经 sudo 起子进程、tidb 的 `sudo -n systemd-run`、CentOS 7 的 systemd 219 没有在真实机器上验证**。


---

## 6. 配置参考

### 6.1 systemd 单元（由安装器按形态渲染）

位置 `/etc/systemd/system/tidbops.service`；**每次升级都由包内模板重新生成**，手工改动会被覆盖——自定义项一律放 `/etc/systemd/system/tidbops.service.d/*.conf`。

形态 C 渲染结果的关键行：

```ini
[Service]
User=tidbops
Group=tidbops
ExecStart=/opt/tidbops/current/bin/tidbops-server \
  --web /opt/tidbops/current/web --listen 0.0.0.0:8018 --allow-remote --data-dir /var/lib/tidbops --env prod \
  --standalone --local-tiup \
  --exec sudo --exec-sudo-user tidb --exec-bin /opt/tidbops/current/bin/tidbops-server \
  --exec-policy /etc/tidb-ops/exec.json --key-file /etc/tidb-ops/exec.key --home /home/tidb
EnvironmentFile=-/etc/tidb-ops/env
Restart=on-failure
RestartSec=3
NoNewPrivileges=no          # 纯独立部署为 yes；sudo 需要提权
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=yes
ReadWritePaths=/var/lib/tidbops
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK   # AF_NETLINK：中控机命令里 ss / ip 等要用；1.6.0 起加入
LimitNOFILE=65536
MemoryMax=1G
```

用 drop-in 修改的常见项：

```ini
# /etc/systemd/system/tidbops.service.d/override.conf
[Service]
MemoryMax=2G                                    # 调大内存上限
ReadWritePaths=/home/tidb/.tiup /var/lib/tidbops-exec   # 追加可写路径（形态 B / C，见 5.1 步骤 4）
```

> 要改 `ExecStart`（加 `--notify-allow-hosts`、`--secure-cookie` 等）必须先清空再整条重写（`ExecStart=` 空行 + 新的 `ExecStart=…`），并在以后升级时对照新包里 `deploy/ops/tidbops.service` 自行同步模板变化（见 5.4）。改完 `systemctl daemon-reload && systemctl restart tidbops`。

### 6.2 `tidbops-server` 启动参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `--standalone` | 否 | 独立部署：平台本身不代表任何集群（需 `--data-dir`） |
| `--local-tiup` | 否 | 与 `--standalone` 同用：本机是 tiup 中控机；需要 `--exec sudo`（推荐）或 `direct` |
| `--data-dir` | 空 | 数据目录；设置后才有登录 / 审批 / 审计 / 集群接入，不设则是只读演示 |
| `--listen` | `127.0.0.1:8080`（程序自己的默认；`install.sh` 安装时传 `0.0.0.0:8018`） | 监听地址；**非回环必须加 `--allow-remote`** |
| `--allow-remote` | 否（安装器生成的单元默认带它） | 允许监听非本机地址；明文 HTTP，生产建议放在带 TLS 与访问控制的反向代理之后 |
| `--env` | `prod` | 平台自身所属环境：`test` / `staging` / `prod` |
| `--exec` | 空 | 执行器：`sudo`（推荐，特权分离）/ `direct`（本进程直接执行，不推荐）/ `none` / `sim`（模拟，仅演示） |
| `--exec-sudo-user` / `--exec-bin` | `tidb` / 当前二进制 | sudo 模式下以谁的身份、调用哪个二进制 |
| `--exec-policy` / `--key-file` | 空 / `<data-dir>/exec.key` | 策略文件与令牌密钥（安装器指向 `/etc/tidb-ops/`） |
| `--home` | 当前用户家目录 | tiup 用户主目录（读 `meta.yaml`） |
| `--public-url` | 空 | 对外地址，通知里的链接用 |
| `--secure-cookie` | 否 | 强制会话 Cookie 带 Secure（经 HTTPS 代理并带 `X-Forwarded-Proto: https` 时已自动加） |
| `--readonly-file` | `/etc/tidb-ops/READONLY` | 熔断文件路径 |
| `--config` | `/etc/tidb-ops/platform.json` | 平台配置文件（可选，不存在则忽略）；目前只有 `icp`（备案号），见 6.6 |
| `--notify-allow-hosts` | 空 | 额外允许的 Webhook 主机（逗号分隔） |
| `--no-mfa` | 否 | 关闭生产高风险变更的申请人 MFA（仅限测试 / POC） |
| `--timeout` / `--cache-ttl` / `--snapshot-ttl` | 20s / 15s / 10s | 单个数据源请求超时 / 数据源响应缓存 / 快照缓存 |

`tidbops-server -version` 打印版本。`--dev-seed` 只用于回放演示，**生产不要加**。

### 6.3 `/etc/tidb-ops/exec.json`（执行器策略，形态 B / C）

由 root 持有（0644），平台改不了它。特权入口**每条命令都重新读**；平台进程只在启动时读一次。

| 字段 | 含义 | 建议 |
|---|---|---|
| `clusters` | 允许操作的 tiup 集群名白名单；`["*"]` = 本机 tiup 管理的任何集群 | 形态 C 默认 `["*"]`；要收紧就列出明确的名字。`"*"` 只放宽「集群名」这一层，节点 / 角色 / 参数仍逐项校验，写命令仍要审批令牌 |
| `tiupBin` | tiup 可执行文件路径 | 例如 `/home/tidb/.tiup/bin/tiup` |
| `mysqlBin` | mysql 客户端路径 | `/usr/bin/mysql` |
| `mysqlUser` / `mysqlPwdEnv` | 运维账号名 / 存放口令的环境变量名 | `tidbops_ops` / `TIDBOPS_OPS_PWD`（口令在 `exec.env`） |
| `mysqlHost` / `mysqlPort` | 运维账号连的 TiDB 地址 | **多个集群共用一份策略时必须留空**（否则别的集群的 `SET CONFIG` 会指向这个地址）；留空则取各集群拓扑里的 TiDB |
| `alertURL` | Alertmanager 地址（告警静默用） | 同上：多集群时留空，改用 `perCluster` |
| `perCluster` | 按集群覆盖：`{"<集群名>": {"mysqlHost":…, "mysqlPort":…, "alertURL":…}}` | 要在中控机模式下用告警静默，必须为每个集群配 `alertURL` |
| `backupRoots` | 备份存储路径前缀白名单（BR 的 `--storage` 只能以它们开头） | 如 `local:///data/backup/`、`s3://bucket/tidb/` |
| `tempDir` | 拓扑临时文件目录 | **绝对路径、已存在、0700、非 /tmp**（见 5.1 步骤 4） |
| `minTiKV` | 缩容后至少保留的 TiKV 数 | 默认 3；小型测试环境可调低 |

多集群示例（形态 C）：

```json
{
  "clusters": ["*"],
  "tiupBin": "/home/tidb/.tiup/bin/tiup",
  "mysqlBin": "/usr/bin/mysql",
  "mysqlUser": "tidbops_ops",
  "mysqlPwdEnv": "TIDBOPS_OPS_PWD",
  "backupRoots": ["local:///data/backup/", "s3://tidb-backup/prod/"],
  "tempDir": "/var/lib/tidbops-exec",
  "minTiKV": 3,
  "perCluster": {
    "prod-sh": {"alertURL": "http://10.0.1.31:9093"},
    "prod-bj": {"alertURL": "http://10.1.1.31:9093"}
  }
}
```

改完先验证 JSON：`python3 -m json.tool /etc/tidb-ops/exec.json >/dev/null && echo OK`（没有 python 就用 `jq .`）。JSON 不合法时执行器直接报「读取策略失败」，**所有写 / 读命令都会失败**。

### 6.4 `/etc/tidb-ops` 下的文件

| 文件 | 属主 / 权限 | 谁读 | 内容 |
|---|---|---|---|
| `exec.json` | root:root 0644 | 平台（启动时）+ 特权入口（每条命令） | 策略 |
| `exec.key` | tidbops:tidb 0640 | 平台（签发令牌）+ 特权入口（校验令牌） | 令牌密钥（64 位十六进制） |
| `exec.env` | root:tidb 0640 | 特权入口 | `KEY=VALUE` 每行一条，如 `TIDBOPS_OPS_PWD=…` |
| `env` | root:tidbops 0640 | systemd（`EnvironmentFile`） | 平台进程的环境变量，如 `TIDBOPS_MYSQL_PWD=…`（形态 B 主集群只读账号） |
\1

### 6.5 日志

没有独立日志文件，全部进 journald：

```bash
journalctl -u tidbops -f                              # 实时
journalctl -u tidbops --since "1 hour ago" --no-pager
journalctl -u tidbops -p warning --since today        # 只看警告及以上
```

注意：经 sudo 起的执行入口（`exec --stdin`）**不单独写日志**——它的结果、被拒绝的原因和错误输出都回传给平台，体现在变更详情的步骤输出里，以及 `audit.log` 的记录中（`change.command` 等，被拒绝的请求也会记）。业务审计不在 journald，而在 `audit.log`（页面「权限与审计 → 审计日志」，可导出）。journald 默认可能只保存在内存（`/run/log/journal`），重启即丢：生产建议 `sudo mkdir -p /var/log/journal && sudo systemctl restart systemd-journald` 持久化，并设置 `SystemMaxUse`。

### 6.6 平台配置文件与备案号（`platform.json`）

平台有一个可选的配置文件 `/etc/tidb-ops/platform.json`（启动参数 `--config` 可改路径），目前只有页面展示用的一个字段：`icp`（网站备案号）。对外提供访问的站点需要在页面上展示 ICP 备案号：配置后，登录页表单下方和主界面页脚会显示它，并链接到工信部备案系统（`https://beian.miit.gov.cn/`）。

发布包自带这个文件（`deploy/ops/platform.json`），`install.sh` / `upgrade.sh` 会在 `/etc/tidb-ops/platform.json` **不存在时**自动放一份，已存在**不会覆盖**你改过的内容；`--dry-run` / `--help` 不会放。你只需要直接改这个文件：

```bash
sudo vi /etc/tidb-ops/platform.json          # 把 icp 改成你自己的备案号，如 "京ICP备12345678号-1"
sudo systemctl restart tidbops               # 只在启动时读取，改完需要重启
```

文件内容是 `{"icp": "", "_说明": "…"}`：只有 `icp` 有效，`_说明` 是给人看的备注，程序忽略；`icp` 为空则页面不显示。

- 文件格式不对、备案号含换行或超过 64 个字符：服务照常启动，只在日志里打一条警告并忽略（`journalctl -u tidbops | grep 平台配置文件`）。
- 文件要让 `tidbops` 用户可读（脚本放的是 0644）；升级与回滚不会改它。
- 备案号随 `/api/health` 的 `icp` 字段下发（未登录也可读，登录页需要）。
- 已经装好的机器执行一次 `upgrade.sh`（或手工 `cp deploy/ops/platform.json /etc/tidb-ops/`）即可得到这个文件。


---

## 7. 日常运维

### 7.1 服务管理

```bash
sudo systemctl status|start|stop|restart tidbops
sudo systemctl enable tidbops                       # 开机自启（install.sh --start 已 enable --now）
sudo /opt/tidbops/current/bin/tidbops-server admin status         # 版本、服务状态、数据 schema、升级历史、备份列表
sudo /opt/tidbops/current/bin/tidbops-server admin status --json
```

重启服务的影响：约几秒不可用；**执行中的变更会被标记为「失败（被中断）」并要求人工核对集群状态**——重启前先确认没有执行中的变更（页面「变更排期」）。会话保存在内存里，**服务重启后所有人都要重新登录**（演示环境实测：重启后页面提示「会话已过期，请重新登录」）。

### 7.2 每日检查（5 分钟）

| 项 | 命令 / 位置 | 正常 |
|---|---|---|
| 服务与健康 | `systemctl is-active tidbops`；`curl -s 127.0.0.1:8018/api/health` | `active`；`"ok":true` |
| 磁盘 | `df -h /var/lib/tidbops /var/backups/tidbops` | 使用率 < 80% |
| 告警 / 错误日志 | `journalctl -u tidbops -p err --since yesterday --no-pager` | 无或可解释 |
| 集群采集状态 | 页面「集群接入」列表的「状态」列 | 全部「正常」「可达」；记录「N 项数据缺失」的变化 |
| 待处理事项 | 页面顶栏 / 「变更排期」 | 没有长时间卡住的变更 |
| 中控机命令 / 一键部署 | 「中控机命令 → 执行历史」「安装部署 → 一键部署」 | 没有长时间「运行中」或「状态未知」的任务；没有不认识的人执行的变更命令 |

可以把第 1 行做成监控探针（Zabbix / Prometheus blackbox / cron 均可）：

```bash
#!/bin/bash
# /usr/local/bin/tidbops-check.sh —— 退出码：0 正常 / 2 严重
URL=${1:-http://127.0.0.1:8018/api/health}
out=$(curl -s --max-time 5 "$URL") || { echo "CRIT: 无法访问 $URL"; exit 2; }
echo "$out" | grep -q '"ok":true' || { echo "CRIT: health 异常: $out"; exit 2; }
systemctl is-active --quiet tidbops || { echo "CRIT: 服务不是 active"; exit 2; }
use=$(df --output=pcent /var/lib/tidbops | tail -1 | tr -dc 0-9)
[ "$use" -lt 90 ] || { echo "CRIT: 数据盘 ${use}%"; exit 2; }
echo "OK"
```

> 平台**没有 `/metrics` 端点**，自身只有 `/api/health` 与 systemd 状态可监控。

### 7.3 每周检查（15 分钟）

| 项 | 做法 |
|---|---|
| 审计链校验 | 「权限与审计 → 审计日志」→「校验哈希链」，必须通过；失败 = 有人改过 / 删过 `audit.log`，立即按 10.4 处置 |
| 部署自检 | 「权限与审计 → 部署自检」没有红色项 |
| 账号复核 | 「用户管理」：离职 / 调岗人员已禁用；管理员仍 ≥ 2 人；`admin` 账号没有被日常使用（看审计） |
| 中控机命令复核 | 导出审计，筛选 `shell.run`（高风险）与 `shell.approve`：变更命令都有原因和批准人；被拒绝（denied）的记录没有异常集中在某个账号；「策略设置」仍是预期档位（生产建议「标准」或「严格」） |
| 备份检查 | `ls -lt /var/backups/tidbops /backup/tidbops \| head`：最近一份在预期时间内、大小合理 |
| 通知渠道 | 「环境与设置 → 通知渠道」：「最近投递记录」没有持续失败；必要时「发送测试」 |
| 升级信息 | 看是否有新版本包（`CHANGELOG.md`），评估是否排升级窗口 |

### 7.4 每月 / 每季度

- **恢复演练**（第 9.4 节）：在测试机用最近的备份完整恢复一次，记录用时（RTO）与问题。
- 口令与密钥：按 10.3 的节奏轮换。
- 容量复盘：数据目录与备份目录增长趋势；`audit.log` 大小。
- 权限复盘：导出审计，按 `auditor` 角色抽查高风险操作与「写操作开关」的开关记录。
- 清理：`/opt/tidbops/releases/` 旧版本（保留 `current` 与上一个版本以及最近 3 个）、`/var/backups/tidbops` 里不再需要的 `-manual` 备份。

### 7.5 账号运维

日常的增删改（建用户、改角色、禁用 / 解锁、重置口令与 MFA）**都在页面「平台配置 → 用户管理」里做**，改动立即生效并让相关会话失效。

命令行用于两种情况：**页面进不去**（忘了所有管理员口令），以及**只有一名管理员时创建第二名管理员**（页面里授予管理员需要另一名管理员确认，一个人完成不了，见 5.1 步骤 6）。**必须先停服务**，改完再启动：

```bash
sudo systemctl stop tidbops
sudo -u tidbops /opt/tidbops/current/bin/tidbops-server user list --data-dir /var/lib/tidbops
sudo -u tidbops env TIDBOPS_USER_PASSWORD='新强口令' \
  /opt/tidbops/current/bin/tidbops-server user passwd --data-dir /var/lib/tidbops --name admin
sudo -u tidbops env TIDBOPS_USER_PASSWORD='强口令' \
  /opt/tidbops/current/bin/tidbops-server user add --data-dir /var/lib/tidbops --name bob --display "Bob" --role admin
sudo -u tidbops /opt/tidbops/current/bin/tidbops-server user disable    --data-dir /var/lib/tidbops --name bob
sudo -u tidbops /opt/tidbops/current/bin/tidbops-server user totp-reset --data-dir /var/lib/tidbops --name bob
sudo systemctl start tidbops
```

要点：必须以 `tidbops` 身份执行（保证文件属主正确）；口令从环境变量 `TIDBOPS_USER_PASSWORD` 读，不要写进命令行；需要数据目录已存在。

### 7.6 紧急只读（熔断）

形态 B / C：立即禁止全部写操作（不需要登录平台）：

```bash
sudo touch /etc/tidb-ops/READONLY       # 页面写开关标签变「强制只读」；已在执行的变更不会被它中断
sudo rm /etc/tidb-ops/READONLY          # 恢复（恢复后写开关仍需要两名管理员重新确认才会开）
```

另外：页面里**任何一名管理员**都可以「立即关闭写开关」（熔断）；开启则需要两名不同的管理员依次确认。

### 7.7 接入新集群 / 下线集群

- **新增**：形态 C 在页面「从本机 tiup 导入」；形态 A 手工接入（见 5.2）。每个新集群都要走一遍「测试」→ 联调 → 再开写开关。
- **下线**：「集群接入」→ 删除（高危确认，要输入集群 id）。只是**移除纳管**：不会停止或删除 TiDB 集群；该集群在平台里的运行数据目录会**改名保留**为 `clusters/.removed-<id>-<时间戳>/`，不会物理删除；有执行中 / 待审批的变更时会被拒绝。确认不再需要后手工删除保留的目录。
- **修改**：改 PD 地址 / 环境 / 名称等会重建该集群的实例；有执行中的变更时改 env / 名称会被拒绝。

---

## 8. 升级与回滚 Runbook

### 8.1 机制要点

- 升级是**原地换版本**：新版本装进 `/opt/tidbops/releases/<新版本>`，原子切换 `current` 链接，重启服务；平台约 **10–30 秒不可用**（不影响 TiDB 集群）。
- 流程：预检（root → 已安装 → 包与清单校验 → 架构 → 版本比较 → `minFrom` → 数据 schema → 磁盘空间 → **有执行中的变更则拒绝**）→ 拿锁 → **停服务** → **备份数据目录** → 安装新 release → 按新包模板重渲染单元 / sudoers → 数据迁移 → 切换 `current` → `daemon-reload` + 启动 → 健康检查（轮询 `/api/health` 直到版本等于新版本）→ 任一步失败**自动回滚**（停服务、必要时恢复备份、切回旧链接、启动旧版本并健康检查）→ 写 `upgrades.jsonl`。
- 单元与 sudoers 每次升级都按包内模板重新生成；**drop-in 不受影响**。
- 中控机 / 同机部署的升级会补齐旧版安装器没建的执行器临时目录与沙箱 drop-in（已存在的一律保留，1.6.13 起）；备案号配置文件 `platform.json` 不存在时也会放一份（6.6）。
- 数据迁移只在 schema 变化时发生；旧程序拒绝读取更新 schema 创建的数据，所以升级后再回滚可能需要还原备份（会丢失升级之后产生的审计 / 变更 / 用户数据）。

### 8.2 升级窗口前（T-1 天）

- [ ] 在**测试机**用同一个包完整走一遍：`upgrade --dry-run` → 升级 → 登录验证 → 回滚。
- [ ] 读新版本 `CHANGELOG.md` 与 `minFrom`：当前版本是否满足；有无配置 / 接口的不兼容变化；是否带数据迁移。
- [ ] 包已校验：`sha256sum -c`；架构与目标机一致（`uname -m`）。
- [ ] `/var/backups/tidbops` 与 `/opt/tidbops` 所在磁盘剩余 ≥ 数据目录大小的 2 倍（`du -sh /var/lib/tidbops`）。
- [ ] 通知相关人员：升级期间平台约 10–30 秒不可用；**窗口内不要安排变更**。
- [ ] 手上有旧版本的包（回滚演练 / 兜底用）。

### 8.3 升级（窗口内）

```bash
# 1. 确认没有执行中 / 已审批待执行的变更与进行中的主机维护（页面「变更排期」）
# 2. 做一份手动备份并移到异地（升级会自动备份，这是额外保险）
sudo /opt/tidbops/current/bin/tidbops-server admin backup --out /backup/tidbops/pre-upgrade-$(date +%F-%H%M).tar.gz

# 3. 解压新包并校验、预演
sha256sum -c tidbops-<版本>-linux-amd64.tar.gz.sha256
tar xzf tidbops-<版本>-linux-amd64.tar.gz && cd tidbops-<版本>-linux-amd64
./bin/tidbops-server admin upgrade --dry-run          # 只检查，不改动；有「拒绝」类输出就停下

# 4. 正式升级
sudo ./upgrade.sh
```

预期：逐步输出，最后「升级完成」类提示并写入升级历史。

升级后验证（**五件事，缺一不可**）：

```bash
systemctl is-active tidbops                                   # active
curl -s http://127.0.0.1:8018/api/health | grep -o '"version":"[^"]*"'    # 新版本
sudo /opt/tidbops/current/bin/tidbops-server admin status     # 版本、schema、最近升级结果
journalctl -u tidbops -n 30 --no-pager                        # 无错误
```

再登录页面：「环境与设置 → 关于与升级」确认版本与 schema；「权限与审计 → 部署自检」没有红色项；「校验哈希链」通过；「集群接入」各集群状态正常；（形态 B / C）重新跑 5.7 第 10 项的执行器全链路验证。

> 升级会按新包的模板**重新生成单元**。如果你之前为 `ExecStart` 写过 drop-in，请对照新包 `deploy/ops/tidbops.service` 同步新增的参数。


### 8.4 回滚决策

| 情况 | 动作 |
|---|---|
| 升级命令自己报失败 | 已自动回滚；看输出与 `journalctl -u tidbops`，确认服务是旧版本且健康，修复原因后重试 |
| 升级成功但新版本有严重问题（登录不了、关键功能不可用） | 立即回滚（下） |
| 小问题 / 不影响值班 | 不回滚，记录问题，等补丁版本 |
| 升级成功后又产生了重要的新数据（审批 / 审计） | 回滚前先评估：若升级迁移了 schema，回滚要**还原备份**，新数据会丢——此时优先考虑前滚修复 |

### 8.5 回滚

```bash
# 先看会做什么
sudo /opt/tidbops/current/bin/tidbops-server admin rollback --dry-run
# 执行（默认回到「把当前版本升上来的那次升级」的来源版本）
sudo /opt/tidbops/current/bin/tidbops-server admin rollback
# 或在旧版本的包目录里
sudo ./rollback.sh
```

要点：

- 只有该次升级**迁移过 schema**（或你显式加 `--restore-data`）才会恢复备份，并明确警告「会丢失升级之后产生的审计 / 变更记录」；`--no-restore-data` 在目标版本读不了当前数据 schema 时会被拒绝。
- 回滚前会另存当前数据；回滚后同样做 8.3 的「五件事」验证。
- `--to <版本>` 可指定回到更早的已安装版本。

### 8.6 升级失败与异常速查

| 现象 | 原因 | 处理 |
|---|---|---|
| 「有执行中的变更」拒绝升级 | 有变更 `running` / `approved` | 等其结束或在页面中止；**不要**手工 kill 平台进程；确需忽略用 `--force`（该变更会被标记为中断） |
| 「架构不匹配」 | 包与 `uname -m` 不一致 | 换对应架构的包 |
| 「当前版本低于 minFrom」 | 跨度太大 | 先升到包说明里的中间版本 |
| 「磁盘空间不足」 | 备份需要空间 | 清理 `/var/backups/tidbops` 旧备份或扩容 |
| 升级后自动回滚 | 新版本起不来 / 健康检查失败 | `journalctl -u tidbops`；「关于与升级」看失败原因；修好后再升 |
| 锁文件错误 | 上次进程被强杀，`/var/lock/tidbops-admin.lock` 残留 | 确认没有 `admin` 进程在跑（`ps aux \| grep "tidbops-server admin"`）后手工删除 |
| 回滚后服务起不来，提示数据 schema 太新 | 回到了读不了当前数据的旧版本 | 用升级前的备份还原：`admin restore --file <备份>`（见第 9 章） |

---

## 9. 备份、恢复与灾备

### 9.1 备份什么、在哪

| 对象 | 位置 | 由谁备份 | 说明 |
|---|---|---|---|
| 数据目录 | `/var/lib/tidbops` | `admin backup`；升级时自动 | **含 `master.key`**，备份文件必须按机密对待；形态 C 还含 `deploy/`、`shell/` 下的任务输出（已遮蔽，但可能含主机信息与业务日志片段） |
| 配置目录（形态 B / C） | `/etc/tidb-ops/` | **你自己**（不在 `admin backup` 里） | `exec.json`、`exec.env`、`env`、`exec.key` |
| systemd drop-in | `/etc/systemd/system/tidbops.service.d/` | 你自己 | 自定义项 |
| nginx 配置与证书 | `/etc/nginx/` | 你自己 | |
| 安装包 | 制品库 | 你自己 | 回滚 / 重建用 |

备份文件命名：`/var/backups/tidbops/<时间>-<版本>.tar.gz`（升级自动备份）、`…-manual.tar.gz`（`admin backup`）、`…-pre-restore.tar.gz`（`admin restore` 前自动另存）。

**自动清理规则**（以代码为准）：每次**升级成功后**，自动备份只保留最近 10 个，更早的自动备份会被删除；**`-manual` 与 `-pre-restore` 备份永不自动删除**；用 `--out` 写到备份目录之外的文件从不被清理。

### 9.2 日常备份策略（建议）

| 频率 | 内容 | 保留 | 去向 |
|---|---|---|---|
| 每天 | 数据目录（`admin backup --out`） | 14 天 | 本机 `/backup/tidbops`，并同步到异地 / 对象存储 |
| 每次升级前 | 手动备份（8.3 第 2 步）+ 升级自动备份 | 到下次成功升级后再留 30 天 | 同上 |
| 每次改 `exec.json` / drop-in / nginx 前后 | `/etc/tidb-ops`、drop-in、nginx 配置 | 版本化（git 或带日期的 tar） | 配置管理库 |

cron 示例（`/etc/cron.d/tidbops-backup`，每天 02:30，保留 14 天；`--out` 的文件**必须不存在**且**不能放在数据目录里面**，所以用带日期的名字；cron 里 `%` 要转义）：

```cron
30 2 * * * root /opt/tidbops/current/bin/tidbops-server admin backup --out /backup/tidbops/tidbops-$(date +\%F).tar.gz >> /var/log/tidbops-backup.log 2>&1 && find /backup/tidbops -name 'tidbops-*.tar.gz' -mtime +14 -delete
15 3 * * * root tar czf /backup/tidbops/etc-$(date +\%F).tar.gz -C / etc/tidb-ops etc/systemd/system/tidbops.service.d etc/nginx 2>>/var/log/tidbops-backup.log && chmod 600 /backup/tidbops/etc-*.tar.gz
```

> **一致性说明（推断，未在高并发下验证）**：`admin backup` **不停服务**；数据文件多数是临时文件 + 改名的原子写，`audit.log` 是追加写，极小概率备份到最后半行记录，恢复后「校验哈希链」可能在末尾报异常。**关键备份（升级前、迁移前、演练基线）请先 `systemctl stop tidbops` 再备份**，备份完再启动。

备份文件含 `master.key` 与全部口令哈希：目录权限 0700、文件 0600，异地存放要加密，传输走内网 / 加密通道。

### 9.3 恢复 Runbook（就地恢复）

场景：数据目录损坏 / 误操作 / 需要回到某个时间点。

```bash
# 0. 确认要恢复的备份文件；知道「备份之后产生的审计 / 变更 / 用户数据会被覆盖」
ls -lt /var/backups/tidbops /backup/tidbops | head

# 1. 恢复（会：校验归档 → 停服务 → 另存当前数据为 -pre-restore → 清空并恢复 → 启动 → 健康检查）
sudo /opt/tidbops/current/bin/tidbops-server admin restore --file /backup/tidbops/tidbops-2026-10-08.tar.gz
```

恢复后：

1. 若输出警告「恢复的数据 schema 高于当前版本支持的 …，服务将拒绝启动」：数据来自更新的版本，**服务保持停止**。先 `admin rollback` 或安装对应版本，再启动。
2. 登录验证；「校验哈希链」；「集群接入」各集群状态；`master.key` 随备份一起恢复，加密的口令仍可解开。
3. 写开关不会自动开：恢复的是备份时刻的设置，按需由两名管理员重新确认。
4. 恢复前的现状在 `…-pre-restore.tar.gz` 里，确认无误后再决定是否删除。

### 9.4 恢复演练（每季度一次，在测试机）

| 步骤 | 做法 | 记录 |
|---|---|---|
| 1 | 准备一台与生产同 OS 的测试机，装**同版本**（或更新）的包：`install.sh --standalone`（演练不需要 tiup） | 用时 |
| 2 | 把生产最近一份备份 + `/etc/tidb-ops` 配置包拷过来 | 传输用时 |
| 3 | `sudo systemctl stop tidbops && sudo ./bin/tidbops-server admin restore --file …` | 恢复用时 |
| 4 | 登录（用生产账号），检查：用户 / 角色在、审计链通过、集群注册表在、**集群「测试」能通**（说明 `master.key` 恢复后密码仍可解密） | 问题清单 |
| 5 | 记录 RTO（总用时）与发现的问题，更新本手册 | 归档 |

> 演练机要**断开生产通知渠道**，否则测试会真发消息：恢复后先在「通知渠道」里全部停用，或演练机网络不通外网 Webhook。

### 9.5 整机重建 / 迁移到新机器

场景：平台机故障、机房迁移、系统升级换机。数据都在 `/var/lib/tidbops`，所以迁移 = 装同样的版本 + 还原数据 + 还原配置。

```bash
# —— 旧机（若还能访问）——
sudo systemctl stop tidbops
sudo /opt/tidbops/current/bin/tidbops-server admin backup --out /tmp/tidbops-migrate.tar.gz
sudo tar czf /tmp/tidbops-etc.tar.gz -C / etc/tidb-ops etc/systemd/system/tidbops.service.d etc/nginx

# —— 新机 ——
# 1) 满足第 3 章前置；形态 B / C 需要同名的 tidb 用户、tiup 与集群元数据（~tidb/.tiup）已就位
# 2) 装同一版本（或更新版本）：安装器会创建账号、目录、sudoers、生成新的 exec.key
sudo ./install.sh --local-tiup --env prod --public-url https://ops.example.com      # 先不加 --start
# 3) 还原数据与配置
sudo /opt/tidbops/current/bin/tidbops-server admin restore --file /tmp/tidbops-migrate.tar.gz
sudo tar xzf /tmp/tidbops-etc.tar.gz -C /        # 还原 exec.json / exec.env / drop-in / nginx；exec.key 可保留新生成的（令牌是短期的）
sudo systemctl daemon-reload && sudo systemctl enable --now tidbops
```

迁移注意：

- **`master.key` 随数据目录走**，不能丢；丢了所有加密保存的口令（集群数据库密码、通知渠道 URL）作废，要在页面重新填写。
- 形态 B / C：新机 `tidb` 用户的 SSH 密钥 / tiup 元数据要与旧机一致，否则 tiup 管不了集群（这是 tiup 自己的备份迁移问题，不在平台里）。
- 访问地址变了：改 `--public-url`（通知链接）、nginx、DNS；已有会话需重新登录。
- 迁移后跑一遍 5.7 的验收。旧机确认不用后再 `admin uninstall`。

### 9.6 灾难场景速查

| 场景 | 影响 | 恢复 |
|---|---|---|
| 平台进程挂了 / 机器宕机 | 不能用平台；**TiDB 集群本身不受影响**；执行中的变更重启后被标记「失败（被中断）」 | 起服务；到集群里（`tiup cluster display`）**人工核对**被中断变更的实际状态 |
| 数据目录损坏 | 平台起不来或数据异常 | 9.3 就地恢复（用最近备份） |
| 误删用户 / 误改设置 | 局部丢失 | 能从备份恢复就恢复；否则手工重建 |
| 忘记所有管理员口令 | 登录不了 | 7.5：停服务 → `user passwd` → 启动 |
| `master.key` 丢失且没备份 | 加密口令不可解 | 在页面重新填集群数据库密码 / 通知渠道 URL；用户口令哈希不受影响 |
| 备份被泄露 | 含口令哈希与 `master.key` | 视为凭据泄露：轮换所有账号口令与集群数据库口令、重建通知 Webhook，见 10.4 |
| 中控机宕机（形态 B / C） | 平台与 tiup 都不可用 | 先恢复中控机（tiup 元数据与 SSH 密钥是它的命脉，要单独备份 `~tidb/.tiup`），再恢复平台 |

---

## 10. 安全运维

### 10.1 加固清单（逐项打勾）

- [ ] 生产环境：平台只监听 `127.0.0.1:8018`，对外只有 nginx 的 443（80 跳转），防火墙**不开 8018**；直接监听 `0.0.0.0:8018` 时（明文 HTTP）只对可信网段开放。
- [ ] TLS 证书有效、自动续期（或到期提醒）；`ssl_protocols TLSv1.2 TLSv1.3`；nginx 有来源网段白名单 / VPN / 堡垒机。
- [ ] 平台以 `tidbops` 运行，非 root；`/var/lib/tidbops` 为 0700；`/etc/tidb-ops/exec.json` 为 root 属主（平台改不了策略）。
- [ ] 初始口令已改；管理员 ≥ 2；个人账号，不共享；离职即禁用。
- [ ] 角色最小权限、**按环境**授予（生产与测试分开）；开发角色只给被授权的库。
- [ ] 数据库账号：只读账号只有 SELECT；运维账号只有 `SET GLOBAL` / `SET CONFIG`；来源主机限定为平台机 IP。
- [ ] 形态 B / C：SSH 私钥目录 `ssh/` 对 `tidbops` 不可读（3 第 12 项验证）；`exec.json` 的 `clusters` 能收紧就别用 `"*"`。
- [ ] sudoers 只放行 `tidbops → tidb` 的固定命令：`/etc/sudoers.d/tidbops`（`visudo -cf` 通过）；形态 B 是 `exec --stdin` 一条，形态 C 多一条 `shell-exec --stdin`（不需要「中控机命令」的环境可以删掉第二条）。
- [ ] 开启页面水印；会话策略保持默认（空闲 30 分钟 / 绝对 8 小时）。
- [ ] 写开关：**默认关闭**，用到才由两名管理员开启，用完可关；生产环境高风险变更保持审批与 MFA（不要加 `--no-mfa`）。
- [ ] 时间同步；系统补丁；SELinux 不要为了省事关掉（见 11.7）。
- [ ] 审计：每周校验哈希链；有条件把 journald 与审计导出定期外送到集中日志（平台代码为审计预留了外送接入点，但当前启动时**未接入**，需要自行定期导出审计）。
- [ ] 形态 C 的「一键部署」「中控机命令」：`settings.edit`（一键部署，等于能以 root 密码在中控机上执行部署）与 `shell.run` / `shell.approve`（中控机命令，等于 tidb 的 sudo 免密权限）只给确实需要的人，并让执行的人与批准的人不是同一批人；「中控机命令 → 策略设置」生产环境用「标准」或「严格」，不要用「宽松」；输出保留天数按合规要求设置（默认 30 天），输出敏感时选「只存摘要」；`/var/log/secure` 里的 `su` 记录（一键部署）与平台审计 `shell.*` / `deploy.*` 定期对照。
- [ ] 中控机的 root 密码按公司策略轮换；输入过它的管理员离职时立即轮换（平台不保存，但人知道）。

### 10.2 平台能做 / 不能做什么（让安全评审心里有数）

- 平台**不能**：执行 `destroy / clean / patch / ssh / bash -c`；绕过白名单；在没有审批令牌时执行写命令；读取 SSH 私钥；修改 `exec.json` / `sudoers`；手工添加的远程集群上执行任何变更。
- **形态 C 的例外（必须让安全评审知道）**：① 页面「一键部署」能让 `settings.edit` 的管理员**以中控机 root 密码经 `su` 在中控机上执行部署脚本**；② 页面「中控机命令」能让被授予 `shell.run` 的用户**以 tidb 身份（tidb 有 sudo 免密，等同于中控机 root）在中控机上执行命令**，不需要 root 密码：经 sudoers 放行的 `shell-exec --stdin` 入口执行，变更命令由它用 tidb 的 sudo 免密起 `systemd-run` 临时单元（不受平台服务沙箱和执行器白名单约束）。约束它们的是：一键部署的 root 密码每次输入、只用一次、不落盘；命令按文本分级，禁止读密钥 / 读业务数据 / 外传 / 命令替换；子进程二次判定；变更命令要另一名持有 `shell.approve` 的用户批准且一次性；全部进审计。**这是基于命令文本的护栏，不是对抗恶意管理员的边界**——真正的边界是 `settings.edit` / `shell.run` / `shell.approve` 给谁、sudoers 放行了什么、两人审批和审计。不需要这些功能的部署，可以不用形态 C（形态 A 没有它们），或从 sudoers 删掉 `shell-exec` 那一行，或不把相应能力授予日常运维账号。
- 写操作总闸（任一不满足即拒绝）：`READONLY` 文件不存在 → 写开关已由两名管理员开启 → 执行器已配置 → 集群版本在支持矩阵内（v7.5 / v8.1 / v8.5 系列）→ 通过审批级别与变更窗口 / 冻结期规则。
- 已知残余风险：令牌密钥由 Web 进程与执行入口共用，Web 进程被攻破后可为**白名单内**的命令签发令牌（做不了白名单外的事）；审计链能发现篡改与删除，但无法发现攻击者离线重算整条链；没有 SSO / LDAP。

### 10.3 口令与密钥轮换

| 对象 | 频率 | 做法 |
|---|---|---|
| 个人账号口令 | 按公司策略 | 用户自己在「我的账号」改；管理员可在「用户管理」重置 |
| 数据库口令（`tidbops_ro` / `tidbops_ops`） | 每 6–12 个月 / 人员变动 | 先在 TiDB 里 `ALTER USER … IDENTIFIED BY`，再改：只读 → 页面「集群接入 → 编辑」填新密码；运维 → `/etc/tidb-ops/exec.env` 的 `TIDBOPS_OPS_PWD`（立即生效，每条命令重读） |
| 通知渠道 Webhook / 密钥 | 泄露即换 | 在 IM 平台重建机器人，再在渠道里更新 URL / 密钥 |
| `exec.key`（令牌密钥） | 年度 / 怀疑泄露 | ⚠️ 未验证：停服务 → 写入新的 64 位十六进制串（`head -c32 /dev/urandom \| xxd -p -c64 > /etc/tidb-ops/exec.key`，保持 `tidbops:tidb 0640`）→ 启动。在途令牌作废，执行中的变更不要换（先等结束） |
| `master.key` | 不支持在线轮换 | 丢失 / 泄露视为事故：见 10.4。**不要**手工替换它，否则已加密的口令全部解不开 |

### 10.4 安全事件处置

| 事件 | 立即动作 | 后续 |
|---|---|---|
| 怀疑平台被攻破 | ① `sudo touch /etc/tidb-ops/READONLY`；② 页面「立即关闭写开关」；③ 隔离机器（防火墙断开入站） | 保全 `journalctl`、`audit.log`、`/var/lib/tidbops`；校验哈希链；到集群里核对 `tiup cluster display`、各实例是否被改；重建机器 + 轮换全部凭据（用户口令、数据库口令、Webhook、`exec.key`），`master.key` 视为泄露 |
| 审计哈希链校验失败 | 同上 ①②，并停止信任该时间段之后的记录 | 对照外送 / 导出的审计与 journald；定位被改 / 删的位置 |
| 备份文件泄露 | 同「备份被泄露」（9.6） | 轮换口令与 Webhook；评估 `master.key` 暴露范围 |
| 账号被盗 | 管理员在「用户管理」禁用账号（会话立即失效），重置口令与 MFA | 看审计该账号做过什么，必要时回滚相关变更 |
| 执行器收到异常命令 | 在页面「权限与审计 → 审计日志」按动作 / 结果筛选被拒绝的记录（执行入口自己不写日志，拒绝原因随结果回传并进审计） | 分析来源；必要时收紧 `exec.json` 的 `clusters` |
| 中控机命令 / 一键部署被滥用（审计里出现不该有的 `shell.run` / `deploy.start`） | ① `sudo touch /etc/tidb-ops/READONLY`（不影响已起的临时单元，需 `systemctl stop tidbops-shell-*`）；② 「用户管理」禁用相关账号，「角色权限」收回 `shell.run` / `shell.approve`；③ 紧急关闭中控机命令：从 `/etc/sudoers.d/tidbops` 删掉 `shell-exec` 一行；一键部署相关的轮换中控机 root 密码 | 对照审计 `shell.*` / `deploy.*`（一键部署另对照 `/var/log/secure` 的 `su` 记录）与 `journalctl -u tidbops-shell-<任务号>`；看任务目录 `out.log`；评估被执行命令的影响 |

---

## 11. 故障处置手册

> 方法：先看 `systemctl status` → `journalctl -u tidbops -n 100` → `/api/health`，把现象归到下面某一类。

### 11.1 服务起不来 / 反复重启

```bash
systemctl status tidbops --no-pager
journalctl -u tidbops -n 80 --no-pager
sudo -u tidbops /opt/tidbops/current/bin/tidbops-server -version        # 程序本身能运行
ss -lntp | grep 8018                                                    # 端口被占用？
```

| 日志特征 | 原因 | 处理 |
|---|---|---|
| 「拒绝监听非本机地址 …：请加 --allow-remote」 | 用 drop-in 覆盖 `ExecStart` 时，`--listen` 是非回环地址却漏了 `--allow-remote` | 补上 `--allow-remote`，或改回 `127.0.0.1:8018`（5.4） |
| 「address already in use」 | 8018 被占用 | 换端口（drop-in 改 `--listen`，并同步 nginx）或停掉占用者 |
| 「数据目录由更新版本创建（schema N，本程序支持到 M）…」 | 数据 schema 比程序新（回滚过头 / 装了旧包） | 装回对应版本，或用旧备份还原（第 9 章） |
| 「数据 schema N 需要破坏性迁移 … 请用 admin upgrade（带备份）或 admin migrate」 | 新版本含不能在启动时自动做的迁移 | 按提示用 `admin upgrade`（带备份）升级，不要直接替换二进制 |
| 「读取 clusters.json 失败 / 不是合法 JSON」 | 数据文件被手工改坏 | 从备份还原该文件；**不要**手改数据文件 |
| 权限相关（permission denied） | 属主 / 权限被改 | `sudo chown -R tidbops:tidbops /var/lib/tidbops && sudo chmod 700 /var/lib/tidbops` |
| `--standalone 需要 --data-dir` / `--local-tiup 需要 --exec sudo…` | drop-in 写错了启动参数 | 对照 6.1 修正 |
| 无日志、状态 `failed (Result: exit-code)` | 二进制损坏 / 架构不对 | `file /opt/tidbops/current/bin/tidbops-server`；重新装包 |
| 被 OOM 杀（`Out of memory` / `MemoryMax`） | 内存超过 `MemoryMax=1G` | drop-in 调大（6.1），并排查接入集群数 / 规模 |

### 11.2 页面打不开 / 访问异常

| 现象 | 诊断 | 处理 |
|---|---|---|
| 浏览器连不上 | `curl -s 127.0.0.1:8018/api/health`；`systemctl status nginx` | 平台没起 → 11.1；nginx 没起 → 起 nginx；防火墙 / 安全组没放行 8018，或监听被改成了 127.0.0.1 → 见 5.4 |
| nginx 502 | `curl 127.0.0.1:8018/api/health`；`getenforce`；`sudo tail /var/log/nginx/error.log` | 平台没起；SELinux：`setsebool -P httpd_can_network_connect 1` |
| 登录后操作报 403「拒绝跨站请求」 | 代理是否改写了 `Host` | nginx 设 `proxy_set_header Host $http_host;`，不要用 `$host` 配非标准端口 |
| 登录成功又立刻被踢回登录页 | 时钟漂移 / Cookie 被浏览器拒绝（HTTPS 下缺 `Secure`，或混合内容） | 校时；确认 nginx 带 `X-Forwarded-Proto` 且全程 HTTPS |
| nginx 启动 / 重载报 `cannot load certificate … No such file or directory` | `nginx -T \| grep ssl_certificate`；`ls -l /etc/nginx/ssl/` | 示例里的证书路径是占位的，换成真实证书文件的路径（或先自签一张测试用）；`nginx -t` 通过后再 reload |
| 访问出现 nginx 自己的 403 页（`nginx/x.y.z`，153 字节，不是平台的 JSON） | `sudo tail /var/log/nginx/error.log`，找 `access forbidden by rule, client: <IP>` | 示例配置的 `allow` 只放行了某网段，来源 IP 不在其中（包括在服务器本机 `curl` 的 `127.0.0.1`）：把该 IP / 网段加进 `allow`，本机自检要加 `allow 127.0.0.1;` |
| 提示被锁定 | 同一用户名连续输错 5 次锁 10 分钟 | 等待，或管理员在「用户管理」解锁 |
| 所有人都被限速 / 锁定 | 经代理时来源 IP 都是 127.0.0.1（已知限制） | 减少共享出口的尝试；管理员解锁 |

### 11.3 集群数据缺失 / 状态异常

先看「集群接入」列表该集群的状态与「测试」结果。

| 现象 | 原因 | 处理 |
|---|---|---|
| 状态「异常」、写着连接信息有误 | PD 地址错 / 不通 | 在平台机用第 5.2 第 3 步的 curl 自测；改地址（「编辑」） |
| 「不可达」 | 最近一次采集连不上组件 | 网络 / 防火墙；PD 是否全挂 |
| 「N 项数据缺失」：缺 Prometheus | 没填 Prometheus 或没部署 node_exporter | 补地址 / 部署 |
| 智能诊断缺少 PD 副本 / TiKV 写入链路 / 主机层诊断项 | 对应 Prometheus 指标没有数据：指标名 / 标签与版本不同、node_exporter 太旧、组件没被抓取；规则取不到数据时不触发、不报错 | 在 Prometheus 查对应指标（清单见 12.8）；Grafana 里能看到而平台没有，说明表达式需要按你的版本调整 |
| 智能诊断缺少「执行计划突变 / 统计信息缺失 / 锁冲突」等慢日志对比项 | 只读账号无 `cluster_slow_query` 权限、TiDB 版本缺 `Stats` / `Backoff_types` 列，或最近 2 小时没有慢日志；原有慢 SQL 列表不受影响 | 手工执行 `SELECT Digest, Plan_digest, Stats, Backoff_types FROM information_schema.cluster_slow_query LIMIT 1;` 看报错 |
| 页面「一键部署」点预检 / 部署报「su 执行失败」 | `su` 被 `pam_wheel` 等限制，或服务单元的 `RestrictAddressFamilies` 挡住 PAM 审计（需 AF_NETLINK） | 见 5.6.2 的常见问题；先手工 `runuser -u tidbops -- su - root -c id` 看真实报错 |
| 「一键部署」任务显示「状态未知」 | 超过 6 小时没有退出码 | 看任务目录 `/var/lib/tidbops/deploy/jobs/<任务号>/` 与 `systemctl status tidbops-deploy-<任务号>` |
| 侧栏「中控机命令」页顶部红色「执行通道不可用」/ 黄色「tidb 没有 sudo 免密」/ 任务显示「状态未知」 | sudoers 没放行 `shell-exec --stdin`（升级后手工维护的 sudoers 没补）；tidb 没有 sudo 免密；变更任务超过 6 小时没有退出码 | 见 5.11 的常见问题；任务目录 `/var/lib/tidbops/shell/jobs/<任务号>/`，变更任务的单元 `tidbops-shell-<任务号>` |
| 缺：告警不可用 | Alertmanager 没填 / 不通 | 补地址 |
| 缺：慢查询 / 配置读取失败 | SQL 账号没填 / 口令错 / 权限不足 / 没装 `mysql` | `which mysql`；手工 `mysql -h … -u tidbops_ro -p`；补权限 |
| 缺：慢查询，错误是 `ERROR 2059 (HY000): Authentication plugin 'mysql_native_password' cannot be loaded` | 平台机上的 `mysql` 是 MySQL 官方 9.x 客户端，已移除该插件，连不上 TiDB（手工 `mysql` 命令同样报错） | `sudo dnf install -y mariadb`（`mariadb --version`），用 `MYSQL_PWD='…' mariadb -h <TiDB> -P 4000 -u <账号> -e 'select version()'` 试连；通了就在「集群接入 → 编辑」把「mysql 可执行文件」填成 `mariadb`（只接受 `mysql` / `mariadb` 两个文件名），点「立即刷新」。备选：把 TiDB 里的只读账号改成 `caching_sha2_password`（需 TiDB 版本支持，改账号前先在测试环境试） |
| 总览横幅 `tiup cluster display: tiup 执行失败: exec: "tiup": executable file not found in $PATH`，拓扑退化为「从 PD / cluster_info 推导」 | 旧版本在 Web 进程（`tidbops` 账号）里直接执行 tiup：PATH 里没有 tiup，也进不去 `tidb` 的家目录 | 升级到包含修复的版本（导入的集群的 `tiup cluster display / list` 改走受限执行器）；该类集群上 `tiup --version` / `tiup cluster --version` 两项仍显示为缺失，只影响版本号显示 |
| 页面提示「尚未接入 / 尚未导入集群」 | 没有集群，或当前账号在所有集群里都没查看权限 | 管理员去「集群接入」；或给账号在对应环境授予角色 |
| 数据库密码「无法解密」（集群条目带 loadError） | `master.key` 变了 / 数据被换 | 在「集群接入」重新填写密码 |
| 版本显示「未验证」 | 集群版本不在 v7.5 / v8.1 / v8.5 系列 | 只读功能仍可用，写操作自动降级为只读 |

### 11.4 写操作被拒绝 / 执行器问题（形态 B / C）

**先看拒绝原因**——变更预览里的「闸门」和提交时的报错会写明：

| 报错 / 闸门文字 | 含义 | 处理 |
|---|---|---|
| 平台处于强制只读（存在 …/READONLY） | 熔断文件在 | `sudo rm /etc/tidb-ops/READONLY` |
| 写操作开关未开启：由两名管理员确认后才能对该环境执行变更 | 开关没开 | 两名**不同**管理员依次在「权限与审计 → 写操作开关」确认 |
| 该集群是手工接入的远程集群：平台只读，不能执行变更 | 手工添加的集群恒只读 | 删除后用「从本机 tiup 导入」 |
| 导入时：读取本机 tiup 集群列表失败：退出码 1 | 执行器链路通，但 `tiup cluster list` 在服务的 systemd 沙箱里以 1 退出；最常见的是 `~tidb/.tiup` 在沙箱里只读 | 先按下面「退出码 1 的定位」判断；新装已自动放开，旧安装见 5.1 步骤 4 |
| 未配置执行器，无法执行变更 | 执行器是 `none`（纯独立部署，或 `--exec` 没配） | 用 `--local-tiup` 安装；或检查单元里的 `--exec sudo` |
| 集群版本 … 不在平台支持范围内，写操作已降级为只读 | 版本不在 v7.5 / v8.1 / v8.5 | 升级集群到受支持版本，或接受只读 |
| 集群 "x" 未接入（执行器拒绝） | `exec.json` 的 `clusters` 不含该集群 | 加进去（或用 `"*"`） |
| 未配置有效的 Alertmanager 地址 | 告警静默缺 `alertURL` | 在 `exec.json` 的 `perCluster` 里配 |
| 未配置有效的 TiDB 连接地址 / 运维账号 | `SET CONFIG/GLOBAL` 缺地址或 `mysqlUser` | 配 `mysqlUser`；地址留空则取拓扑，拓扑读不到时才需要手配 |
| 临时目录不可用 / 临时目录权限过宽（必须为 0700） | `tempDir` 不存在 / 权限不对 | 5.1 步骤 4 |
| 读取策略失败 | `exec.json` 不是合法 JSON | 修正（6.3 的校验命令） |

**sudo / 执行入口本身出问题**：

```bash
# 1) 看 sudoers 是否合法、路径是否匹配
sudo visudo -cf /etc/sudoers.d/tidbops
sudo cat /etc/sudoers.d/tidbops            # 应为两行（形态 C）：tidbops ALL=(tidb) NOPASSWD: /opt/tidbops/current/bin/tidbops-server exec --stdin 和 … shell-exec --stdin
# 2) 以平台账号走一遍完整链路
echo '{"cmd":{"kind":"cluster.list"}}' | sudo -u tidbops sudo -n -u tidb /opt/tidbops/current/bin/tidbops-server exec --stdin
# 3) 以 tidb 用户直接跑 tiup，排除 tiup 自身问题
sudo -u tidb tiup cluster list
```

| 现象 | 原因 | 处理 |
|---|---|---|
| `a password is required` / `not allowed to execute` | sudoers 命令路径与实际调用不一致（`current` 链接路径、`--exec-bin`）、或用户名不对 | 重新渲染：`sudo ./install.sh --force`（保留数据与密钥）或升级一次；确认 `--tidb-user` |
| `sorry, you must have a tty to run sudo` | `requiretty` | sudoers 已含 `Defaults:tidbops !requiretty`；确认文件没被改 |
| `读取密钥失败` / `密钥格式不正确` | `exec.key` 不可读（`tidb` 用户需要读）/ 内容不是 hex | `ls -l /etc/tidb-ops/exec.key` 应为 `tidbops:tidb 0640`；内容 ≥ 32 位十六进制 |
| tiup 报 `read-only file system` | systemd 沙箱挡住了写入 | 5.1 步骤 4：把报错里的路径加进 drop-in 的 `ReadWritePaths=` |
| `Permission denied (publickey)` / ssh 失败 | `tidb` 用户的 SSH 密钥问题（与平台无关） | 手工 `sudo -u tidb tiup cluster display <集群>` 复现；修 tiup / SSH |
| 命令超时 | 各类命令有超时（display 30s、check 120s、重启 600s、升级 3 小时…） | 看集群状态；超时只会杀直接子进程，tiup 的孙进程在 Linux 上**可能继续运行**，需人工核对 |

**「退出码 1」的定位**（导入 / 采集时 `tiup` 经执行器运行失败）：手工在服务之外运行能成功，说明问题在服务的沙箱里。先逐项排除，再用 `systemd-run` 模拟沙箱对比：

```bash
grep -E '"tiupBin"|"clusters"' /etc/tidb-ops/exec.json                    # tiupBin 路径对不对、白名单是否包含该集群
sudo -u tidb env | grep -E '^(HOME|PATH|TIUP_HOME)='                      # sudo 切到 tidb 后的环境，HOME 应是 /home/tidb
sudo -u tidb /home/tidb/.tiup/bin/tiup cluster list                        # 在这个环境里直接跑，看 tiup 自己的报错
sudo -u tidbops sudo -n -u tidb /opt/tidbops/current/bin/tidbops-server exec --stdin </dev/null
#   应输出 {"t":"result","exit":-1,"err":"请求格式错误: EOF"…}：说明 tidbops→tidb 的 sudo 链路是通的

# 模拟服务沙箱：A 不放开 ~/.tiup，B 放开。A 报错（如 read-only file system）、B 正常 = 沙箱挡住了写入
sudo systemd-run --wait --pipe -p User=tidb -p ProtectSystem=strict -p ProtectHome=read-only -p PrivateTmp=yes /home/tidb/.tiup/bin/tiup cluster list
sudo systemd-run --wait --pipe -p User=tidb -p ProtectSystem=strict -p ProtectHome=read-only -p PrivateTmp=yes -p ReadWritePaths=/home/tidb/.tiup /home/tidb/.tiup/bin/tiup cluster list
```

确认是沙箱后，按 5.1 步骤 4 补 drop-in 与 `tempDir`，`daemon-reload` 并重启服务。

### 11.5 平台被重启 / 变更被中断后怎么办

平台重启时，**执行中的变更会被标记为「失败（被中断）」**，不会自动续跑，也不会自动回滚。处置：

1. 页面「变更排期」找到该变更，看它执行到了第几步（每步的命令与结果都在详情与审计里）。
2. 到集群里核对真实状态：`sudo -u tidb tiup cluster display <集群>`；PD 里 `evict-leader-scheduler` 是否残留（页面「热点与调度」或 `pd-ctl scheduler show`）；扩缩容是否停在中间态（Tombstone 未 prune 等）。
3. 补救后再决定重新提交变更；需要的话由 DBA 手工完成剩余步骤。

### 11.6 升级 / 恢复相关

见 8.6 与 9.3。

### 11.7 SELinux（CentOS 7 默认 Enforcing）

⚠️ 未验证：在真实 CentOS 7.9 + SELinux Enforcing 下，从 `/opt/tidbops/current/bin/` 启动服务、`sudo` 切换到 `tidb`、写数据目录等是否都不被拦截。

排查方法：

```bash
getenforce
sudo ausearch -m avc -ts recent | tail -30                 # 看有没有 avc denied
sudo ausearch -m avc -ts recent | audit2allow              # 生成建议规则（先审阅，别直接装）
```

做法顺序：先用 `setenforce 0` **临时**验证是否 SELinux 引起（只用于排查）；确认后为具体拒绝项写最小策略（`audit2allow -M tidbops && semodule -i tidbops.pp`）或修正文件上下文（`restorecon -Rv /opt/tidbops /var/lib/tidbops`）。**不要**长期关闭 SELinux。

### 11.8 磁盘满

| 位置 | 处置 |
|---|---|
| `/var/backups/tidbops` | 删除旧的自动备份与不再需要的 `-manual`、`-pre-restore`（确认已异地留存） |
| `/var/lib/tidbops` | 先看谁大：`du -sh /var/lib/tidbops/* \| sort -h \| tail`。`audit.log` 大属正常增长：**不要截断**（破坏哈希链），应扩容或迁移数据盘；`clusters/.removed-*` 是下线集群保留的数据，确认无用后可删 |
| journald | `journalctl --vacuum-size=500M` |

数据盘写满时平台写文件会失败，可能出现异常；清理后重启服务并「校验哈希链」。

---

## 12. 附录

### 12.1 命令速查

```bash
# —— 安装 / 升级 / 回滚 / 状态 ——
sudo ./install.sh --local-tiup|--standalone|--cluster <名> --env prod [--tidb-user tidb] [--public-url URL] [--start] [--dry-run] [--force]
sudo ./upgrade.sh                                   # 在新包目录
sudo ./bin/tidbops-server admin upgrade --dry-run   # 预演
sudo /opt/tidbops/current/bin/tidbops-server admin rollback [--dry-run] [--to VER] [--restore-data|--no-restore-data] [--backup FILE]
sudo /opt/tidbops/current/bin/tidbops-server admin status [--json]
sudo /opt/tidbops/current/bin/tidbops-server admin backup [--out FILE]
sudo /opt/tidbops/current/bin/tidbops-server admin restore --file FILE
sudo /opt/tidbops/current/bin/tidbops-server admin uninstall [--dry-run] [--purge-data --yes]
/opt/tidbops/current/bin/tidbops-server admin migrate --data-dir /var/lib/tidbops     # 数据迁移（升级自动调用）

# —— 服务 / 日志 ——
sudo systemctl status|start|stop|restart tidbops
journalctl -u tidbops -f
curl -s http://127.0.0.1:8018/api/health

# —— 紧急 ——
sudo touch /etc/tidb-ops/READONLY          # 熔断
sudo rm    /etc/tidb-ops/READONLY          # 恢复

# —— 账号（先停服务！）——
sudo -u tidbops env TIDBOPS_USER_PASSWORD='…' /opt/tidbops/current/bin/tidbops-server user passwd --data-dir /var/lib/tidbops --name admin
```

`admin` 公共参数：`--package DIR`（默认取二进制上两级，须有 `MANIFEST.json`）、`--root DIR`（演练用，默认 `/`）。退出码：成功 0、运行失败 1、用法错误 2。`admin uninstall` 默认保留数据、备份与 releases，会删除服务单元、sudoers 与 `current` 链接。

### 12.2 路径与权限总表

| 路径 | 内容 | 属主 / 权限 |
|---|---|---|
| `/opt/tidbops/releases/<版本>/` | 各版本程序、前端、模板 | root，0755 |
| `/opt/tidbops/current` | 当前版本符号链接 | root |
| `/var/lib/tidbops/` | 数据目录 | tidbops，0700（文件 0600） |
| `/var/backups/tidbops/` | 备份 | root，0700 |
| `/etc/tidb-ops/` | 配置目录 | root，0755 |
| `/etc/tidb-ops/exec.json` | 策略 | root:root 0644 |
| `/etc/tidb-ops/exec.key` | 令牌密钥 | tidbops:tidb 0640 |
| `/etc/tidb-ops/exec.env` | 运维账号口令 | root:tidb 0640 |
| `/etc/tidb-ops/env` | 平台环境变量 | root:tidbops 0640 |
| `/etc/systemd/system/tidbops.service` | 服务单元（升级重写） | root，0644 |
| `/etc/systemd/system/tidbops.service.d/` | 你的 drop-in（升级不动） | root |
| `/etc/sudoers.d/tidbops` | 受限 sudo 入口（形态 B / C；C 含 `exec --stdin` 与 `shell-exec --stdin` 两条固定命令） | root，0440 |
| `/var/lock/tidbops-admin.lock` | 安装 / 升级互斥锁 | root |
| `/var/lib/tidbops-exec` | 执行器临时目录（新装默认） | tidb，0700 |

### 12.3 版本与兼容

- 版本号：`主.次.补丁`；补丁只修 bug，次版本可带数据迁移，主版本可能不兼容（看 `CHANGELOG.md` 与 `minFrom`）。
- 数据 schema 是独立整数：新程序能迁移旧数据，旧程序拒绝读新 schema 数据。
- TiDB 版本支持矩阵：v7.5 / v8.1 / v8.5 系列为「已验证范围」，其他版本只读功能可用、写操作降级。
- CentOS 7 上部署 TiDB 集群只能装 ≤ v8.3（官方 glibc 限制），见 5.6.1。

### 12.4 本文中**未验证**项汇总（上线前逐项在测试环境确认）

| # | 项 | 在哪 | 确认方法 |
|---|---|---|---|
| 1 | `useradd --system` 是否同时建 `tidbops` 组；各文件属主 / 权限 | 5.1 | 5.7 第 3、4 项 |
| 2 | sudoers 对 `current` 符号链接路径的匹配、`.` 开头文件是否被忽略 | 5.7 第 10、11 项 | 执行器全链路命令 |
| 3 | `ProtectSystem=strict` / `ProtectHome=read-only` 下，执行器 / tiup 能否写 `~tidb/.tiup` 与 `tempDir`；`ReadWritePaths` drop-in 是否足够 | 5.1 步骤 4 | 在测试集群做一次扩容预检与 `edit-config` |
| 4 | `exec.json` 的 `tempDir` 在 `PrivateTmp=yes` 下的行为（本文建议用非 /tmp 目录） | 5.1 步骤 4 | 同上 |
| 5 | `tiup cluster display` 的输出格式能否被正确解析（导入） | 5.1 步骤 7 | 导入测试集群 |
| 6 | `meta.yaml` 读取权限的放开方式 | 3 第 12 项 | 生成一次配置变更方案 |
| 7 | 数据库账号权限（`SYSTEM_VARIABLES_ADMIN`、`CONFIG`、只读账号是否需 `PROCESS`） | 5.1 步骤 5 | 在测试集群 `SET CONFIG` / 读慢 SQL |
| 8 | 真实环境的内存占用与 `MemoryMax=1G` 是否够 | 2.1 | 观察 `systemctl status` |
| 9 | 备份不停服务时的一致性（`audit.log` 末行） | 9.2 | 备份后恢复并校验哈希链 |
| 10 | `exec.key` 轮换流程 | 10.3 | 测试机演练 |
| 11 | SELinux Enforcing 下的行为 | 11.7 | `ausearch -m avc` |
| 12 | 升级 / 回滚在真实机器上的完整演练（锁文件、`systemctl stop` 后 `is-active` 等待、健康检查地址） | 第 8 章 | 测试机完整走一遍 |
| 13 | 所有对真实 TiDB 集群的写命令（重启、扩缩容、配置、维护、备份、升级） | 12.7 | 测试集群按顺序联调 |
| 14 | 用 ACL（`setfacl`）放开 `meta.yaml` 读取，以及 tiup 重写 `meta.yaml` 后 ACL 是否丢失 | 3 第 12 项 | 做一次扩容 / `edit-config` 后重新验证读取 |
| 15 | 从形态 B 原地转换为形态 C（`--local-tiup --force`），原有变更 / 审计与集群的关联是否完整接续 | 5.9 | 测试机先演练；先备份 |
| 16 | MariaDB 客户端连 TiDB 做慢 SQL / `cluster_info` 采集的兼容性 | 11.3 | 页面「立即刷新」后看缺失项 |
| 17 | 中控机模式下导入集群的 `tiup cluster display / list` 经执行器采集（替代直接执行 tiup） | 11.3 | 导入后看总览横幅与拓扑是否完整 |
| 18 | `ReadWritePaths` 里带 `-` 前缀的路径在 tiup 装好之前启动服务、装好后是否需要重启才生效 | 5.1 步骤 4 | 先装平台后装 tiup 的场景测一次 |

### 12.5 术语

| 术语 | 含义 |
|---|---|
| 平台 | 本软件（`tidbops-server`） |
| 中控机 | 装了 tiup、能 SSH 到所有集群节点的机器 |
| 执行器 / 受限入口 | `tidbops-server exec --stdin`，以 `tidb` 用户运行，校验后才调用 tiup / pd-ctl / br / mysql |
| 写开关 | 每个集群各自的总闸，**开启需要两名不同管理员确认**，关闭一名即可 |
| 熔断 / READONLY | `/etc/tidb-ops/READONLY` 文件，存在即强制只读 |
| 导入 / 手工接入 | 形态 C 下，「导入」产生可运维的本机 tiup 集群；「手工接入」产生只读的远程集群 |
| 环境 | 集群属性（测试 / 预发 / 生产），决定用户在该集群里的角色与审批强度 |

### 12.7 写操作联调清单（在测试集群上逐项确认）

这些点在回放数据和模拟执行器里验证不了，**必须在真实环境确认**（代码按官方文档和经验写成，可能需要小调整）。形态 B / C 启用写操作前，先在测试集群完整走一遍。

**要确认的点：**

- [ ] `tiup cluster display` 的输出格式（解析 Role / Status，用于拓扑、等待实例 Up、执行器的节点校验）
- [ ] `tiup cluster edit-config <集群> --topology-file <文件>` 在目标 tiup 版本上是否存在、行为是否符合预期（平台用它做配置持久化）
- [ ] 平台从 `~/.tiup/storage/cluster/clusters/<集群>/meta.yaml` 取出 `topology` 作为 edit-config 的基础，与 `edit-config` 编辑器里看到的一致，补丁后 diff 只有目标项
- [ ] `tiup cluster scale-out <集群> <文件> -y`、`cluster check <集群> <文件> --cluster` 的参数写法与输出列（修复建议的匹配依赖它）
- [ ] pd-ctl 子命令：`config set`、`scheduler add evict-leader-scheduler <id>`、`scheduler remove evict-leader-scheduler-<id>`、`store limit all <n>`、`member leader transfer <name>`、`region check …` 在目标版本上的写法与输出
- [ ] PD HTTP：`/pd/api/v1/store/<id>`（leader_count、state_name）、`/pd/api/v1/regions/check/{down-peer,pending-peer}`
- [ ] `SET CONFIG` / `SET GLOBAL` 经 `mysql` 客户端执行（运维账号权限最小化：`SYSTEM_VARIABLES_ADMIN`、`CONFIG`）；`SET CONFIG` 里 size 用字符串、`SET GLOBAL` 里 size 用字节数
- [ ] BR：`tiup br backup full / restore full / log start|stop|status` 的参数与存储路径写法；`br:<ver>` / `cdc:<ver>` 这类组件版本写法；S3 凭据靠 `tidb` 用户环境，平台不传
- [ ] Alertmanager `POST /api/v2/silences` 的请求体；告警里的 `instance` 标签是业务端口还是状态端口取决于 exporter 配置
- [ ] `sudo -u tidbops sudo -n -u tidb /opt/tidbops/current/bin/tidbops-server exec --stdin` 全链路：sudoers、策略与密钥文件权限、`/etc/tidb-ops/exec.env` 里的运维账号口令
- [ ] 「权限与审计 → 部署自检」全绿；两名管理员完成写开关双人确认
- [ ] `edit-config` 的 YAML 白名单偏保守：真实 `meta.yaml` 里白名单之外的键会被拒绝，需要对照实际拓扑补全

**建议的联调顺序**（由低风险到高风险，每步观察 Grafana）：

1. 只读：登录、总览、巡检（含主机检查）、PITR 状态、参数建议、时间线
2. 告警静默
3. `inst.op`：对一个 TiDB 做「重启」（有负载均衡时先摘除）；再对一个 TiKV 做「驱逐 Leader → 恢复」
4. `config.set`：在线项（`log.level`）→ 需 reload 项（单个 TiKV）
5. `scale.out`：加一个 TiDB；再加一个 TiKV（观察 Region 均衡）
6. `maint.enter / exit`：对一台测试 TiKV 主机，计划 10–20 分钟
7. `scale.in`：下线刚加的 TiKV（观察 Tombstone 与 prune）
8. 故障注入：执行中杀掉平台进程 → 重启后变更应显示「失败（被中断）」；驳回 / 取消 / 过期；`touch /etc/tidb-ops/READONLY` 熔断
9. 审计：「校验哈希链」通过；手工改一行后应校验失败（只在测试机做）

**执行器的实现要点**（联调时知道它们能少走弯路）：令牌必须是规范编码且不可带尾部换行，有效期 15 分钟、期内可重复使用；最终命令行里除固定选项外任何以 `-` 开头的参数一律拒绝；临时目录须为绝对路径且权限不宽于 0700；拓扑 YAML 拒绝锚点 / 别名 / 自定义标签 / 重复键，并禁止会让 tiup 读取本机路径的键（`rule_dir`、`dashboard_dir`、`config_file`、`additional_scrape_conf` 等）；超时 / 取消只杀直接子进程，tiup 的子孙进程在 Linux 上不会一并结束；sudo 会重置环境，所以运维账号口令放在 `exec.env`。

**通知渠道联调**：

- [ ] 平台机到各官方域名（企业微信、钉钉、飞书、Lark、Slack）443 可达；真实 DNS 解析不会被误判为内网
- [ ] 企业微信 markdown 渲染；钉钉加签（时间戳与服务器偏差超过 1 小时会被拒）、机器人开了「自定义关键词」需包含 “TiDB”；Lark / 飞书的签名校验与两个域名
- [ ] 飞书错误码含义：19021 = 签名错误或时间戳相差超过 1 小时；19022 = 请求 IP 不在白名单；19024 = 消息不含关键词；错误原文在测试结果与「最近投递记录」里可见
- [ ] Slack：Incoming Webhook 发测试返回 HTTP 200 + `ok`；填签名密钥应被拒绝
- [ ] 「其他」类型：未加入 `--notify-allow-hosts` 的主机应被拒绝；加入后在自建接收端验证请求体字段与 2xx 判定，带密钥时按 HMAC-SHA256(密钥, 时间戳 + "." + 请求体) 校验 `X-TiDBOps-Signature` 与 `X-TiDBOps-Timestamp`

**中控机命令（5.11）在真实中控机上**：

- [ ] `sudo -u tidbops sudo -n -u tidb /opt/tidbops/current/bin/tidbops-server shell-exec --stdin` 不报 sudo 错误，页面状态里 `sudo` 探测为可用
- [ ] 只读命令（`tiup cluster display`、`df -h`、`ss -ltnp`）以 `tidb` 身份执行、输出实时回传；日志里含手机号 / 密码 / 身份证号的行已被遮蔽
- [ ] 被拒绝的命令（`cat ~/.ssh/id_rsa`、`env`、`mysql …`、`scp`、`$(…)`、`sudo -i`）确实没有调用执行通道，审计里有 denied 记录
- [ ] 变更命令：申请 → 另一名持有 `shell.approve` 的用户批准 → 执行 → 再执行同样命令被拒（一次性）；改一个字被拒
- [ ] 超时（`sleep 60` 配 30 秒）退出码 124；取消：只读任务进程组被杀（143），变更任务单元被 `systemctl stop`；输出超过 1 MiB 被截断并注明
- [ ] 平台服务重启时进程内任务标记为中断，临时单元不受影响

**一键部署（5.6.2）在真实中控机上**：

- [ ] 以 `tidbops` 身份 `su - root -c id`（经伪终端）能否成功，沙箱里的 PAM 是否报 netlink 错误
- [ ] `systemd-run --unit=… /bin/bash run.sh` 在 systemd 219（CentOS 7）上能起，输出写进 `out.log`，`exit` 能被 `tidbops` 读到
- [ ] 输错 root 密码：提示、计数、3 次锁定；预检失败 / 部署中途失败 / 取消 / 从断点继续各走一遍
- [ ] 成功后「显示」TiDB root 密码只能一次，且审计、任务目录、下载输出里都搜不到任何口令；平台服务重启时部署任务不被中断

### 12.8 诊断指标核对清单

智能诊断的部分规则依赖 Prometheus 指标和慢日志列。规则取不到数据时**不触发也不报错**，所以「集群确有问题而诊断里没有」时，先核对下面这些指标在你的版本上是否存在、标签是否一致（在 Prometheus 里逐条执行）：

| 类别 | 指标 |
|---|---|
| PD 副本 / etcd | `pd_regions_status`、`etcd_disk_wal_fsync_duration_seconds_bucket`、`etcd_network_peer_round_trip_time_seconds_bucket`、`pd_scheduler_store_status{type="store_slow_score"}` |
| TiKV 写入链路 | `tikv_engine_write_stall`、`tikv_raftstore_append_log_duration_seconds_bucket`、`tikv_raftstore_apply_log_duration_seconds_bucket`、`tikv_scheduler_latch_wait_duration_seconds_bucket` |
| 主机（node_exporter） | `node_filesystem_files*`、`node_filesystem_readonly`、`node_disk_*_time_seconds_total`、`node_load5`、`node_netstat_Tcp_CurrEstab`；`node_os_info` 较旧版本没有 |
| DB Time 分解 | `tidb_session_*_duration_seconds_sum`（是否带 `sql_type`；get_token 单位为微秒） |

标签（`instance`、`address`、`mountpoint`、`device`）要与你的版本一致。Grafana 里能看到而平台没有，说明表达式需要按你的版本调整。

慢日志对比项依赖 `information_schema.cluster_slow_query` 里的 `Stats`、`Backoff_types`、`Plan_from_cache`、`Resolve_lock_time`、`Rocksdb_delete_skipped_count`、`Exec_retry_count` 列：用只读账号手工查一条，确认 v7.5 / v8.1 / v8.5 上这些列都存在。缺列时整条失败，只影响「慢日志对比」，原有慢 SQL 分析不受影响。

### 12.9 打包与发布（面向发布者）

在开发机项目根目录运行，需要本机装有 Go；不需要 node。产物是 Linux 的 tar.gz，Windows 上也能交叉编译。

```bash
./build-release.sh                                  # 版本取项目根目录的 VERSION 文件，默认同时打 amd64 与 arm64，输出到 backend/dist/release
./build-release.sh -v 1.2.0 -a amd64 -n notes.md -m 1.0.0 -o /tmp/release
./build-release.sh --verify /绝对路径/tidbops-1.2.0-linux-amd64.tar.gz     # 校验包（注意用绝对路径）
```

| 参数 | 说明 |
|---|---|
| `-v` / `--version` | 版本号，格式 `主.次.补丁`（可带 `-rc.1` 后缀，不带 v 前缀）；缺省读 `VERSION` 文件 |
| `-a` / `--arch` | `amd64`、`arm64` 或逗号分隔（默认两个都打） |
| `-o` / `--out` | 输出目录，默认 `backend/dist/release` |
| `-n` / `--notes` | 变更说明片段（Markdown），写进包内 `CHANGELOG.md` 与清单 |
| `-m` / `--min-from` | 允许直接升级的最低版本；低于它的实例必须先升到中间版本 |
| `--deploy-tool` | 指定要带入的 `tidb-deploy` 包；缺省取 `deploy/dist` 里版本最大的一个，没有则不带 |
| `--verify <包>` | 校验一个包后退出 |

构建细节：`CGO_ENABLED=0 GOOS=linux GOARCH=<arch> go build -trimpath -ldflags "-s -w -X …buildinfo.Version=<版本> -X …Commit=<git 短哈希> -X …BuildTime=<UTC 时间>"`，所以**页面上显示的版本号来自打包时注入的值，等于 `VERSION` 文件**；开发构建（没有注入）显示 `0.3.0-dev`。`web/` 只带入白名单文件（`index.html`、`js/*.js`、`css/*.css`、`help/*.md`），文本文件统一为 LF；包里的 `docs/` 是项目 `docs/*.md`；`deploy/ops/` 带单元、sudoers、`exec.json.example`、`clusters.json.example` 与 `platform.json`；`deploy/tidb-deploy/` 与 `web/deploy/dist/` 带集群部署工具包。**打包前确认 `deploy/dist/` 里有 `tidb-deploy-<版本>.tar.gz`**（没有就用 `deploy/package.sh` 生成），否则包里没有部署工具包，页面「一键部署」不可用。

`--verify` 会把包解到临时目录（拒绝绝对路径、`..`、反斜杠、符号链接 / 硬链接 / 设备文件、多个根目录、重复条目、超大条目），检查旁边的 `.sha256`，再用清单逐个文件核对哈希、模式与是否有清单之外的文件。包被改动任意一个字节都会失败。

`install.sh` / `upgrade.sh` / `rollback.sh` 只做两件事：检查是 root、检查 `uname -m` 与包的架构一致，然后 `exec bin/tidbops-server admin install|upgrade|rollback "$@"`；`install.sh` 默认带 `--listen 0.0.0.0:8018`（你自己传的 `--listen` 会覆盖它），并在 `/etc/tidb-ops/platform.json` 不存在时放一份备案号配置文件。真正的逻辑都在 Go 实现的 `admin` 子命令里。

**版本号约定**：补丁（1.0.x）只修 bug，不改数据格式、不改对外接口；次版本（1.x.0）是向后兼容的新功能，可带数据迁移；主版本（x.0.0）是不兼容变化，在 `CHANGELOG.md` 里写清升级注意事项，可用 `--min-from` 要求先升到中间版本。**数据 schema** 是另一个独立的整数：数据文件格式发生**不兼容**变化时才加一；新程序能迁移旧数据，旧程序拒绝读取更新 schema 创建的数据（避免静默损坏），这也是升级后回滚可能需要还原备份的原因。发布版本一定要通过打包工具构建，不要手工 `go build` 后拷贝。

### 12.10 文档说明

| 文档 | 内容 |
|---|---|
| 本文《运维部署手册》 | 部署、配置、升级回滚、备份灾备、安全、故障处置 |
| 《用户手册》（`docs/用户手册.md`） | 面向使用者：各页面能做什么、变更怎么提交与审批、角色与权限、常见问题 |
| 页面「使用帮助」 | 每个页面的逐步操作、后果与常见问题（随程序发布，顶栏「? 帮助」打开当前页的说明） |
| `CHANGELOG.md`（包内） | 每个版本的变更说明、升级注意事项 |
