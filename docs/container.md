# atom 容器版设计（草案）

状态：草案，待审。日期：2026-11-20。标记沿用 [design.md](design.md)：**[定]** 已和用户确认，**[建议]** 待审，**[待定]** 还要讨论。

## 1. 范围 **[定]**

2026-11-19 用户定：

- 平台：Docker（macvlan）和 RouterOS 的 container。
- 只做单网卡旁路由，不做双网卡网关。
- 配置仍然走网页向导；compose 和 ROS 上的设置尽量少，不靠环境变量配置。
- 透明代理和 Linux 版一样用 nft redirect（TCP）+ TProxy（UDP），不用 TUN。TUN 只在以后遇到 nft 不可用的平台时再考虑。

不在本期：双网卡网关、TUN 入站、32 位 arm、Docker 的 bridge 和 host 网络、从 Linux 版迁移到容器（数据目录格式相同，手动复制 `data/` 应该可行，但不保证、不测）。

## 2. 实测依据（2026-11-20）

在测试机 `ros-test`（RouterOS 7.23.8 long-term，CHR x86_64，内核 5.6.3）上，用 Alpine 3.23 加 nftables、iproute2 和 0.2.1 包里的 sing-box 拼的探测镜像（veth `172.31.233.2/30`，测试后已全部清理）：

| 项目 | `user-ns=no` | `user-ns=yes` |
|---|---|---|
| 容器内 capabilities | 全部（`CapEff=3fffffffff`） | 全部，uid 映射到 32768 |
| 加载 atom 的 `prerouting_tcp`/`prerouting_udp`（含 `tproxy`、`redirect`、`fib`） | 成功 | 成功 |
| `ip rule fwmark 0x233 lookup 233`、表 233 `local 0.0.0.0/0 dev lo` | 成功 | 成功 |
| 写 `ip_forward`、`conf/*/` 下的 sysctl | 成功 | 成功 |
| TCP 经 redirect 进 sing-box，原目标保留 | 成功 | 成功 |
| UDP 经 TProxy 进 sing-box，原目标保留 | 成功 | 成功 |
| 真实往返（223.5.5.5 的 UDP 53 和 TCP 443，direct 出） | ROS 拿到应答 | ROS 拿到应答 |

- 7.23.8 的 `/container add` 没有 `privileged`、`cap-add`，容器默认就是 root 加全部 capabilities。
- 已有容器改 `user-ns` 后起不来（`could not create /etc/hostname`），要删掉重新导入；`user-ns` 只能在创建时定。
- 测试流量都是 ROS 自己发的，没有从 LAN 客户端经 ROS 测过。
- Docker macvlan 还没测。社区里有现成的例子（macvlan + `NET_ADMIN` + nft TProxy），但容器里加规则时宿主会不会自动加载 `nft_tproxy`、`nft_socket` 模块，要在 vm92 上实测。
- ROS 的 arm64 没测；社区项目说 RouterOS 7.21 以上的 arm64、amd64 都能用 nftables TProxy。

## 3. 和 Linux 版的差异

| 方面 | Linux 版 | 容器版 |
|---|---|---|
| 程序 | 安装器复制到 `/opt/atom/bin` | 在镜像里，就在 `/opt/atom/bin`，不复制 |
| 服务管理 | systemd / OpenRC，4 个服务 | 容器内的入口进程 `atom-installer container`（第 5 节）管 atomdns 和两个内核；没有 `atom-maintenance` |
| 依赖 | apt / apk 装 nftables、iproute2 | 在镜像里 |
| 53 端口 | 向导检测、停用占用者 | 容器有自己的网络命名空间，没有占用，跳过 |
| 环境检查 | 系统、架构、root、拒绝容器 | 改为能力探测：nft tproxy、`ip rule`、网卡（第 6 节） |
| 网络 | 写进宿主的 nft 和策略路由，卸载时按 `changes.json` 撤掉 | 只在容器的命名空间里，容器停了就没了，不需要 `changes.json` 的网络部分 |
| 拓扑 | 单网卡、双网卡 | 只有单网卡 |
| 升级 | `install.sh` 菜单、面板更新（9.7） | 换镜像后重建容器；新镜像启动时迁移数据（第 8 节）；面板不提供更新 |
| 重新配置 | `install.sh` 菜单、面板「设置 → 网络」 | 在容器里运行 `atom-installer reconfigure`（第 6.3 节）；面板的网络页只读 |
| 卸载 | `atom-installer uninstall` | 删容器；要连数据一起删就删卷 |
| 日志 | journald / `/var/log/atom` | 容器的标准输出（`docker logs`、ROS 的 `/log`），每行带服务名前缀 |

## 4. 镜像

- **名字和标签 [定]**（2026-11-20）：Docker Hub `jasonxtt/atom` 为主（ROS 默认的仓库就是 docker.io），同时推 `ghcr.io/jasonxtt/atom`；标签有 `<版本>`、`v<版本>`、`latest`，多架构（amd64、arm64）。每次发版另出 `atom-<版本>-container-{amd64,arm64}.tar`（docker-archive 格式），挂在 Release 上。ROS 可以用 `file=` 导入这个 tar，不依赖它访问镜像仓库（`ros-test` 上 `/tool fetch` 到 Docker Hub 报 SSL 错误，vm92 上 `docker pull` 也被重置）；国内 Docker 用户也可以 `docker load`。
- **内容**：程序和 Linux 包里完全一样（atomdns、sing-box、mihomo、atom-installer、WebUI、自带配置），从同一次 `build-package.sh` 的产物组装，不另外编译。基础镜像是 `debian:trixie-slim` **[定]**，程序取 glibc 版的 `amd64`、`arm64` 包；基础系统之外只多装 nftables、iproute2、ca-certificates、tzdata。
- **目录**：
  - `/opt/atom/bin/`：程序（镜像里，只读）；
  - `/opt/atom/share/`：自带配置和 WebUI 的原始文件，首次安装和升级时从这里复制；
  - `/opt/atom/data/`：**唯一的卷**。atomdns 的配置根放在这里，`install.json`、nft 规则和备份也放在里面（`data/.atom/`），重建容器后都还在。
- **镜像里的固定设置**：`ENV ATOM_DEPLOYMENT=container`，`ENTRYPOINT ["/opt/atom/bin/atom-installer", "container"]`，`STOPSIGNAL SIGTERM`。用户不需要设置任何环境变量，`TZ` 可选。
- **体积**（2026-11-20 实测，vm92，amd64，内容和 0.2.1 正式包相同；Debian 基础用 mmdebstrap minbase 构建，按 slim 的做法去掉文档、man 和 locale，大小和官方 `debian:trixie-slim` 的 78MB 一致）：

  | | 解压后 | gzip 后（下载量） |
  |---|---|---|
  | Debian trixie slim 基础 | 78.5MB | 28.6MB |
  | Debian 基础 + nftables、iproute2、ca-certificates、tzdata | 88.7MB | 32.3MB |
  | Debian 版 atom 镜像（glibc atomdns） | 264MB | 93.4MB |
  | Alpine 3.23 基础 | 8.4MB | 3.5MB |
  | Alpine 基础 + 同样的依赖 | 14MB | 5.9MB |
  | Alpine 版 atom 镜像（musl atomdns） | 184MB | 66.1MB |

  四个程序本身约 170MB，其中 sing-box 92MB，两版差的主要是基础系统。ROS 的内置存储基本放不下（`ros-test` 的系统盘只剩 59MB），文档要写明镜像和卷都放在外接盘或数据盘上。
- **构建和发布 [建议]**：
  - 做法和 rosboard 的 `.github/workflows/docker.yml` 一样，在 atom 仓库加一个 `docker.yml` 工作流，手动触发，输入 Release 标签。流程：下载这个 Release 的配套包，按 `SHA256SUMS` 校验，用 `docker buildx` 把包里的文件装进镜像（不编译，镜像里的程序就是发布包里那几个），推到 Docker Hub 和 ghcr，再用 `docker save` 出两个 tar 和它们的 sha256，传到同一个 Release 上。
  - 发版顺序不变：先在 Mac 上打包、签名、建 Release，再跑这个工作流。私钥仍然不离开 Mac，镜像里也没有需要签名的东西。
  - Docker Hub 的登录沿用 rosboard 的两个 secret 名字：`DOCKERHUB_USERNAME`、`DOCKERHUB_TOKEN`。GitHub 的 secret 值读不出来，要用户在 `jasonxtt/atom` 仓库里再设置一次，token 可以和 rosboard 共用。ghcr 用工作流自带的 `GITHUB_TOKEN`。
  - 本地验证用的镜像在 vm92 上构建：2026-11-20 已经装了 Debian 的 `docker.io` 26.1.5 和 `docker-buildx`，是临时用阿里云源装的，系统的 apt 源没改。

## 5. 入口进程 `atom-installer container` **[建议]**

用 Go 写在安装器里，不用 s6、tini 或 shell 脚本。这样 nft 模板（`renderNft`、`renderPolicyRoute`）、`sync-nft`、向导、升级逻辑和 Linux 版是同一份代码。

### 5.1 启动顺序

1. 设好信号处理，开一个只在容器内可访问的控制 socket（`/run/atom/control.sock`）。
2. 没有 `data/.atom/install.json`，或者它的状态不是 `complete`：起向导（第 6 节），装完再接着执行第 3 步。
3. 镜像版本比 `install.json` 里的新：做数据迁移（第 8 节）；镜像版本更旧：拒绝启动，日志写明两个版本号，然后退出。
4. 加载网络：`sync-nft --load-atom`、`policy-route.sh`；勾了“回包经主路由”的话，再写 `accept_redirects=0`。这一步失败就退出，并在日志里说明缺什么（比如缺内核模块、缺 `NET_ADMIN`）。
5. 按保存的期望状态（`data/.atom/services.json`）先起内核，再起 atomdns。

### 5.2 服务管理

- 在安装器的 `initSystem` 接口（`service.go`）下加第三个实现 `containerInit`，向控制 socket 发 start、stop、restart、reload、is-active、logs。这样安装、重新配置、自检的代码不用分支。
- 三个服务 `atom-dns`、`atom-sing-box`、`atom-mihomo` 的命令行、`sync-nft --start` 和重载前的 `sync-nft` 都和 systemd 单元（`renderProxyUnit`）一样。
- 进程异常退出就重启：atomdns 等 2 秒，内核等 1 秒，和 systemd 单元的 `RestartSec` 一致。
- reload 先执行 `sync-nft --backend <内核>`，再给内核发 `SIGHUP`；内核没在运行就直接启动它，和 `reload-or-restart` 一样。
- 每个服务的期望状态（运行或停止）写进 `services.json`，容器重启后照这个恢复。Linux 版这件事由 systemd 的 enable 状态来记。
- atomdns 的 `atom_rules.yaml` 里，两个内核的 `reload`、`stop` 在安装和迁移时改写成 `[/opt/atom/bin/atom-installer, svc, reload, atom-sing-box]` 这种形式，沿用 OpenRC 的 `setReloadCommand`，不改 atomdns 子模块。
- 日志：每个服务的标准输出和标准错误加上 `[atom-dns]` 这类前缀，转到容器的标准输出；同时每个服务在内存里保留最近 200 行，给 `Logs()`（安装失败时显示）用。

### 5.3 维护 socket

atomdns 通过 `ATOM_MAINTENANCE_SOCKET` 访问维护服务（`atom_maintenance.rs`）。容器里由入口进程在同一路径提供一个最小实现：

- 概览返回 `deployment: "container"` 和当前版本；
- 发起更新或网络变更时返回明确的错误；
- 不提供检查更新。

`origin/main` 在写这份草案后加了「组件」表格和重启（`maintenance_services.go`）以及「重置所有设置」（`reset.go`，design.md 9.7）。容器里也要有这两项：

- **组件**：`GET /services` 列出 `atom-dns` 和当前内核，状态从入口进程直接拿。`POST /restart` 交给入口进程执行，判断规则和 Linux 版一样（没选中的内核不能重启，有维护任务时返回 409）。容器里没有 `atom-maintenance`，所以不列出它。
- **重置**：入口进程停掉所有服务，把 `data/`（含 `.atom/`）备份到 `data/.atom/backup/reset-<时间>/`，然后清空 `data/` 里除备份以外的内容，回到向导。向导的预填规则和 Linux 版一样，从备份里读出原来的网络设置。
- **系统更新、网络变更**：返回错误（见上）。

atomdns 和 WebUI 只需要改一处：看到 `deployment: "container"` 时，隐藏「系统更新」的操作按钮，把「设置 → 网络」变成只读。按项目规定，UI 里只放标签，不放解释性的段落。

### 5.4 停止

收到 `SIGTERM` 后，先停 atomdns，再停内核，然后退出，整个过程控制在 10 秒内（ROS 的 `stop-time` 默认 10 秒）。nft 规则和路由跟着网络命名空间一起消失，不需要清理。

## 6. 向导（容器模式）**[建议]**

用的是同一个 `web/index.html` 和同一个 `runInstall`。`/api/state` 返回 `deployment: "container"` 时，页面和步骤有下面这些变化。

### 6.1 端口

向导直接监听面板端口：默认 80，被占用就随机选一个。容器有自己的 IP，不需要像 Linux 版那样另用 9996。入口进程把访问地址打印在日志里。访问控制和 Linux 版一样：只接受私网来源，写操作要求同源；但容器里不设闲置超时退出，没装完就一直等。

### 6.2 各步

1. **环境**：不检查系统和 53 端口，改为探测：
   - 有没有 `NET_ADMIN`；
   - 能不能加载一张带 `tproxy` 和 `redirect` 的临时 nft 表（加载后立即删除）；
   - 能不能加删 `ip rule`；
   - 除了 `lo` 有几块网卡。

   任何一项不通过就列出原因，不能继续。原因写短格式，比如“内核缺少 nft_tproxy”“缺少 NET_ADMIN”。
2. **网络**：
   - 拓扑固定为单网卡，不给选。
   - 网卡只有一块就自动选定；有多块才让用户选。
   - 面板端口、面板登录、“回包经主路由”保留。
   - Linux 版的所有权检查（表 233 是否已被占用、已有路由）在容器里照做，几乎不会触发。
3. **DNS**、**代理内核**、**代理**：和 Linux 版一样。
4. **确认并安装**：步骤变成：配置 → 网络（写规则文件并加载）→ 启动 → 设置 → 自检。没有依赖、程序两步。自检照旧：google 得到 FakeIP，baidu 得到真实 IP。
5. **完成页**：和 Linux 版一样的两步提示（主路由加静态路由把 FakeIP 网段指向容器 IP，客户端 DNS 设为容器 IP），然后跳转到面板。

失败后不回滚到装之前：容器里没有宿主的状态要还原。只撤掉本次加载的 nft 表和策略路由，显示错误，可以返回修改后重试。

### 6.3 重新配置 **[定]**

在运行中的容器里执行 `atom-installer reconfigure`（Docker：`docker exec atom atom-installer reconfigure`；ROS：`/container shell atom` 之后执行）。它通知入口进程在面板端口之外另开一个端口跑向导，访问地址打印在终端里。网络一步按 `install.json` 预填，其余规则和 Linux 版 9.6 节一样；失败就恢复原来的规则文件并重新加载。

## 7. 部署形态

### 7.1 Docker + macvlan **[建议]**

```yaml
services:
  atom:
    image: jasonxtt/atom:0.3.0
    container_name: atom
    restart: unless-stopped
    cap_add: [NET_ADMIN]
    sysctls:
      - net.ipv4.conf.all.accept_redirects=0   # 只在勾了“回包经主路由”时需要；写上也无妨
    volumes:
      - ./data:/opt/atom/data
    networks:
      lan:
        ipv4_address: 192.168.1.250
networks:
  lan:
    driver: macvlan
    driver_opts: {parent: eth0}
    ipam:
      config: [{subnet: 192.168.1.0/24, gateway: 192.168.1.1}]
```

- 用户只需要改 `parent`、网段和容器的 IP。
- Docker 默认把 `/proc/sys` 挂成只读，入口进程写不了 sysctl，所以需要的 sysctl 写在 compose 里。单网卡旁路由不需要 `ip_forward`（被代理的流量都是交给本机的 sing-box）。“回包经主路由”只需要 `accept_redirects=0`；勾了这个选项但没在 compose 里写，自检就报错并指明要加哪一行。
- macvlan 的限制：宿主自己访问不到容器。文档里写明，不做绕过方案。
- 宿主内核要有 `nft_tproxy` 和 `nft_socket`。Debian、Ubuntu 的默认内核都有。群晖这类 NAS 不保证，向导的环境探测会拦下来。

### 7.2 RouterOS container **[定]**

2026-11-20 用户定：两种接法都支持，路由接法的网段不限于 /30；“回包经主路由”在两种平台、两种接法下都允许用户自己打开，默认关闭（同 Linux 版）。

| 接法 | 做法 | 回包 |
|---|---|---|
| 路由（单独网段） | veth 单独一个网段（任意大小，例子里用 /30），ROS 在 veth 上有地址，LAN 客户端经 ROS 路由访问容器 | 来回都经过 ROS，defconf 里“丢弃 invalid”的规则不会误伤，不需要“回包经主路由” |
| 网桥（LAN 网段） | veth 加进 LAN 网桥，容器拿一个 LAN 地址，和 Linux 版的单网卡旁路由一样 | 同网段客户端的回包在二层直接发回，ROS 只看到去程；遇到 invalid 被丢时，在向导里打开“回包经主路由”（主路由地址填 ROS 的 LAN 地址），或者照 deploy-nft.md 第 5 节的其他办法处理 |

- 向导不需要区分这两种接法：容器里看到的都是单网卡，网关就是默认路由。
- ROS 自己就是主路由，FakeIP 网段的静态路由直接加在 ROS 上，指向容器地址。

路由接法：

```
/interface veth add name=atom address=172.31.233.2/30 gateway=172.31.233.1
/ip address add address=172.31.233.1/30 interface=atom
/container mounts add name=atom-data src=/sata1/atom-data dst=/opt/atom/data
/container add name=atom file=sata1/atom-0.3.0-container-amd64.tar interface=atom \
    root-dir=sata1/atom mount=atom-data logging=yes start-on-boot=yes
/container start atom
/ip route add dst-address=28.0.0.0/8 gateway=172.31.233.2
/ip dhcp-server network set [find] dns-server=172.31.233.2
```

网桥接法（LAN 是 `192.168.88.0/24`，网桥 `bridge`）：

```
/interface veth add name=atom address=192.168.88.250/24 gateway=192.168.88.1
/interface bridge port add bridge=bridge interface=atom
# 其余的 mounts、container add 和路由同上，地址换成 192.168.88.250
```

- `mounts` 和 `mountlists` 的写法在不同版本之间有变化，以实测的版本为准，文档写明是在哪个版本上测的。
- 面板地址就是容器地址，比如 `http://172.31.233.2/`。
- 文档里写清：`user-ns` 只能在创建容器时定；镜像和 `root-dir` 要放在数据盘上；ROS 的版本要求为 7.21 以上，最终以实测为准。
- 测试时发现：ROS 自己发出的包会用 veth 上的 `172.31.233.1` 作源地址。所以需要防环的场景里，routing rule 只能匹配容器的单个地址，不能匹配整个 /30。正常部署不需要这类规则，只在测试文档里写一句。

## 8. 升级（换镜像）**[建议]**

用户拉取新镜像，然后重建容器：Docker 是 `docker compose pull && docker compose up -d`；ROS 是删掉容器再用新 tar 重新添加，挂载同一个数据目录。新镜像启动时，入口进程会：

1. 把 `install.json` 里的版本和镜像版本比较：版本相同就直接启动；镜像更旧就拒绝启动（见 5.1 节）。
2. 镜像更新时，先把自带配置（`config_custom.yaml`、`sub_config/`、`rule/atom_catalog.json`、`ui/main/`）备份到 `data/.atom/backup/`，保留最近 3 份。
3. 用 `share/` 里的新文件覆盖自带配置，保留 `webinfo/`、下载的规则和缓存，重新改写重载命令和面板端口，然后重新生成 nft 规则。这些步骤和 Linux 版的 `upgrade` 共用代码。
4. 正常启动；接口起不来，或者内核没有加载到最新配置，就把备份恢复回去、写日志，然后退出。退出后由 Docker 或 ROS 的重启策略决定要不要再试。镜像本身回不到旧版，要用户自己换回旧镜像。

## 9. 实施步骤 **[建议]**

1. **C1 入口进程和镜像**：`atom-installer container`、`containerInit`、控制 socket、`services.json`、日志转发、`build-image.sh`（先只做 amd64）。先跳过向导，用 vm92 上 Linux 版装好的一份 `data/` 起容器，验证三个服务、重载、切换内核、`kill` 后自动拉起、`docker restart` 后状态恢复。
2. **C2 向导的容器模式**：环境探测、网络一步的简化、安装步骤的裁剪、`reconfigure`。
3. **C3 atomdns 和 WebUI**：维护 socket 的 `deployment` 字段；面板在容器模式下隐藏更新、网络页只读。这一步改子模块。
4. **C4 升级迁移**：用旧镜像装好后换新镜像；再用一个故意起不来的新镜像，验证会恢复备份。
5. **C5 Docker 实测（vm92）**：装 Docker，建 macvlan；客户端用另一个接在同一块网卡上的 macvlan netns。完整走一遍向导：google 得到 FakeIP 并经节点出去，baidu 得到真实 IP，TCP 和 UDP 都测，再测“回包经主路由”。
6. **C6 RouterOS 实测（`ros-test`）**：按 7.2 节部署，测 `user-ns` 开和关；尽量找一个 LAN 客户端经 ROS 测一遍。
7. **C7 arm64 和发布**：`build-image.sh` 加 arm64；加 `docker.yml` 工作流（第 4 节），推 Docker Hub 和 ghcr，tar 传到 Release；更新 README 和部署文档。arm64 的 ROS 没有设备的话，只测 Docker（n1 能不能装 Docker 另看）。

每一步按现有规矩走 codex 审查闭环，汇报时写清在哪台机器、哪个目录上测的。

## 10. 结论（2026-11-20）

第 10 节原来的待定项都已定：

- **基础镜像 [定]**：`debian:trixie-slim`，配 glibc 版 atomdns（`amd64`、`arm64` 两个包里的程序），和 Debian 上的 Linux 版一致，也避开 musl 的 malloc 在多线程下慢的问题（design.md 9.5）。代价是镜像比 Alpine 版大约 80MB（下载多约 27MB，见第 4 节）。
- **镜像仓库 [定]**：Docker Hub 加 ghcr（第 4 节）。
- **重新配置 [定]**：`atom-installer reconfigure`（6.3 节）。
- **ROS 接法 [定]**：两种都支持，“回包经主路由”允许打开（7.2 节）。

## 11. 实施和验证记录

### C1 入口进程和镜像（2026-11-20）

**做了什么**（代码在 `installer/`，只用标准库）：

- `atom-installer container`（`container.go`）：PID 1。启动顺序是先开控制 socket 和维护 socket，再读 `install.json`，再加载网络，再按 `services.json` 起服务（内核在前，atomdns 在后）；SIGTERM 后先停 atomdns 再停内核，每个服务 SIGTERM 后最多等 4 秒再 SIGKILL。
- 三个服务的命令行、`sync-nft --start`、重载前的 `sync-nft` 和 RestartSec（atomdns 2 秒，内核 1 秒）与 systemd 单元一致，有单元测试对比。退出码为 0 的进程不重启（同 `Restart=on-failure`），被信号杀死或非 0 退出会重启；启动前的准备（`sync-nft --start`、`config.json` 在不在）失败也按同样的间隔重试。
- 加载网络放在 atomdns 的启动前（同单元里的 `ExecStartPre`），启动时先做一次，失败就退出并提示检查 `NET_ADMIN` 和内核模块。**每次都从 `install.json` 重新渲染 `atom.nft` 和 `policy-route.sh`**（偏离 5.1 节的字面：5.1 只说加载），所以没有向导的 C1 也能起来，向导（C2）直接复用 `writeNetworkFiles`。
- `services.json` 是 `{"atom-dns": "running", ...}`。`start`、`reload`、`restart` 记为 running，`stop` 记为 stopped；SIGTERM 关机不改它。没有这个文件时只有 atomdns 是 running，内核由 atomdns 自己拉起，和 systemd 版第一次安装一样。
- `containerInit`（`container_control.go`）是第三个 `initSystem`，经 `/run/atom/control.sock`（HTTP over unix）通信。`atom-maintenance` 在容器里就是入口进程自己，对它的启停是空操作。`atom-installer svc <start|stop|restart|reload|is-active> <服务>` 是同一个客户端的命令行；`reload` 先 `sync-nft` 再 SIGHUP，没运行就启动。
- 路径（`paths.go`）：`ATOM_DEPLOYMENT=container` 时 `Paths.Container` 为真；`Etc`、`Lib` 都指向 `data/.atom/`，`Backup` 是 `data/.atom/backup`，`Run` 是 `/run/atom`（`ATOM_RUN_DIR` 可改，给测试用）；`Bin`、`Shipped` 仍在 `/opt/atom/bin`、`/opt/atom/share`。`Paths.Init()` 返回 `containerInit`，安装、自检的代码没有分支；只有 `writeNetworkFiles` 在容器里不写服务定义、`serviceState`/`installedServices` 在容器里走入口进程。
- `setReloadCommand` 在命令是 `atom-installer` 时改写成 `[atom-installer, svc, reload|stop, atom-sing-box|atom-mihomo]`（`refreshConfig` 在容器里这样调用）。
- 维护 socket（`container_maintenance.go`，路径同 Linux 版 `maintenance.sock`）：`GET /` 返回 `{"version", "online": false, "deployment": "container"}`；`GET /services` 列出 atomdns 和当前内核（不列 atom-maintenance）；`POST /restart` 复用 Linux 版的判断（未选中的内核 400，有任务时 409）；`/check`、`/upgrade`、`/network` 返回 400 和说明；`/reset` 返回 400“还没有实现”（留给 C2）。
- 日志：每行加 `[atom-dns]` 之类的前缀写到标准输出，入口进程自己的行是 `[atom]`；每个服务在内存里留 200 行给 `Logs()`。
- `installer/container/Dockerfile`、`installer/scripts/build-image.sh`：输入 `build-package.sh` 的产物目录（校验 `SHA256SUMS`）或单个 glibc 包，可用 `--installer` 换掉包里的 atom-installer（开发用）；`--base` 指定已经装好依赖的基础镜像（此时不跑 apt）。输出 `atom-<版本>-container-amd64.tar`。镜像里 `PATH` 含 `/opt/atom/bin`，`docker exec atom atom-installer ...` 可以直接用。
- `installer/scripts/dev-container-seed.sh`：**只用于开发**，在向导做出来之前，把包里的配置、WebUI、`install.json`（完成、单网卡）和初始 `atom_proxy.json` 放进数据目录。

**测试环境**：

- `go vet`、`go test ./...`：vm92，`/root/atom-build/atom-c1/installer`（Go 1.24.4，linux/amd64），全部通过；新增的 `container_test.go` 连跑 5 遍通过。Mac 上 `TestStatusQueriesKeepTheDeadline` 在没有这次改动时也失败（1.2 秒对 100ms 的时限），是 Mac 上已有的计时问题，vm92 上通过。
- vm92 不能访问 GitHub、Docker Hub 和 deb.debian.org，源码用 rsync 同步（没有在 vm92 上改代码）；基础镜像用 mmdebstrap（阿里云源，minbase，去掉文档、man、locale）构建后 `docker import` 成 `atom-base:trixie-slim`（88.7MB，含 nftables、iproute2、ca-certificates、tzdata）。镜像 `atom:0.2.1` 264MB，内容是 0.2.1 正式包，只把 atom-installer 换成新编的。
- 实测：Docker 26.1.5，macvlan（parent `eth0`，10.0.0.0/24），容器 10.0.0.201，`cap_add: NET_ADMIN`，数据目录挂 `/root/atom-build/c1/data`；客户端是接在同一块网卡上的 macvlan netns（10.0.0.202）。结果见下表。

| 项目 | 结果 |
|---|---|
| 容器起来，atomdns 加 sing-box 都在运行，面板 `http://10.0.0.201/` 从局域网可访问 | 通过 |
| 容器里有 `table inet atom`、`ip rule 9233 fwmark 0x233 lookup 233` 和表 233 的 local 路由（宿主已加载 `nft_tproxy`，`nft_socket` 本期的规则用不到） | 通过 |
| `PUT /api/v1/atom/settings` 把 backend 改成 mihomo：mihomo 起来，sing-box 被 `svc stop` 停掉，`services.json` 同步；再改回 sing-box 同样 | 通过 |
| `kill -9` 内核：1 秒后拉起；`kill -9` atomdns：2 秒后拉起 | 通过 |
| `docker restart`、`docker stop` 后再 `start`：`services.json` 恢复，mihomo 和 atomdns 重新起来 | 通过 |
| `docker stop`：179 毫秒退出，退出码 0，日志有“已停止” | 通过 |
| 客户端查 `www.google.com`：FakeIP（28.0.0.x）；`www.baidu.com`：真实 IP；UDP 和 TCP 都查过；两个内核各查过一遍 | 通过 |
| `GET /api/v1/atom/maintenance` 返回 `deployment: container`；`/services` 返回两项；`/restart` 重启 atom-sing-box 和 atom-dns；`/network` 返回 400 | 通过 |
| 没有 `NET_ADMIN`：容器退出（1），日志写明“检查 NET_ADMIN 和内核的 nft_tproxy、nft_socket” | 通过 |
| 没有 `install.json`：容器退出（1），日志说明向导还没有实现 | 通过 |

没有节点，所以没测代理流量能不能出去；没测“回包经主路由”（5.1 节第 4 步里的 `accept_redirects` 逻辑只有单元测试没有实测）；没测 arm64。

**codex 审查**（三轮，`codex exec -s read-only`，静态审查，没有让它编译）：

- 第一轮 10 条：关机和启动的竞争（SIGTERM 在启动中、关机时还能启动、三个服务串行停）、只给进程组长发信号、重载在重试等待中返回成功、`services.json` 读失败当成新安装且写失败只记日志、两个内核同时记为运行、运行目录权限、`setReloadCommand` 容器改写不检查、种子脚本和 `build-image.sh` 的写法。除了“Linux 面板重启的锁等待超过 atomdns 的 8 秒”（Linux 版已有代码，不在 C1 范围）和 Mac 临时目录默认值（沿用 `build-package.sh` 的约定）外都修了，加了测试。
- 第二轮 8 条：并发启动能同时跑两个内核（1 条阻断）、控制请求和启动的竞争、关机时慢的启动脚本、重载在重试等待中取消了自动重试、`services.json` 为 `null`、写 `services.json` 失败时的一致性、状态查询丢了调用方的时限、种子脚本校验。全部修了，加了测试（含 `-race`）。
- 第三轮 7 条，其中 3 条阻断：重试中的内核重载时另一个内核能抢位置、关机会被卡在启动脚本里、关机和并发停止的次序。已修：重试循环每次都先做同组检查，内核在重试等待中仍占着位置，`end()` 让后来的调用等同一个结束并在 `stopWait` 后放弃卡住的循环，启动在操作锁内重读期望状态，重复的 `start` 写不进 `services.json` 时不动已经在跑的服务，种子脚本逐条校验四条命令；对应加了测试，在 vm92 上 `go test ./...` 和 `-race` 通过，容器里的实测重新跑了一遍。**这一轮的修复没有再送 codex 复审**（按规矩三轮为限）。
- 记为已知限制的（codex 提了，超出 C1）：客户端 90 秒超时后，排在卡住的启动脚本后面的请求仍可能执行（控制 socket 没有把请求的 context 传进监督器）；启动脚本（`sync-nft`、`policy-route.sh`）不能被取消，关机只是不再等它们；`writeJSON` 在 rename 之后同步目录失败时，`services.json` 可能已经是新值而调用报错；运行目录只做了 0700，没有校验目录属主和符号链接（容器里只有 root，`ATOM_RUN_DIR` 只给测试用）。

**已知限制**：

- 入口进程不回收孤儿子进程（僵尸进程）。要正确回收，需要接管所有子进程的 wait（`exec.Cmd` 的 wait 会和通用回收冲突）；atomdns、sing-box、mihomo 的子进程都是自己 wait 的，目前没见到孤儿。需要的话 Docker 可以加 `init: true`。
- 入口进程在 C1 没有安装向导，没有完成安装就退出；镜像版本比数据新时只打印一行日志，数据迁移留给 C4。
