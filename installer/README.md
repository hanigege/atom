# atom 安装器

设计见 [../docs/design.md](../docs/design.md) 第 9.2 节，nft 规则见 [../docs/deploy-nft.md](../docs/deploy-nft.md)。

## 用户怎么装

支持 Debian 12+ / Ubuntu 22.04+（systemd）和 Alpine 3.19+（OpenRC），x86_64 或 aarch64，不支持 Docker/LXC 容器。脚本按架构和系统选包。

```bash
curl -fsSL <install.sh 的地址> | sudo bash
```

Alpine 上（脚本是 bash）：

```bash
apk add bash curl && curl -fsSL <install.sh 的地址> | bash
```

脚本下载配套包（先 GitHub，慢或不通时依次换加速前缀）、校验 sha256、解到 `/opt/atom/` 下的临时目录，起安装向导并打印地址：

```text
atom 安装器已就绪，请在浏览器打开：
  http://10.0.0.77:9996/

  =======================================
    安装码  012345
    请在浏览器输入此码。仅本次启动有效。
  =======================================
```

在网页输入终端显示的 6 位安装码，再依次设置：环境（53 端口被占时有"停用"按钮）→ 网络（单网卡旁路由 / 双网卡网关、面板端口，默认 80，可选的面板用户名和密码，留空就不需要登录）→ DNS → sing-box（FakeIP 网段、mixed 端口、可选导入节点）→ 安装。装完自动跳到面板。安装器启动 30 分钟后停止接收新任务，进行中的安装可以继续完成。

## 安装后在面板维护

- 「设置 → 系统更新」可以检查正式版本、查看发布说明和升级整套 Atom。更新清单与升级包优先通过当前 mixed 端口走本机 SOCKS5，依次回退到 GitHub 直连和内置加速源。升级前先下载、校验并检查配置，服务自检通过后提交新版本。
- 「设置 → 网络」可以编辑拓扑、网卡、网关、客户端网段、回包方式和面板端口。应用前页面显示新旧地址。自检完成后，在新地址打开「设置 → 网络」，按「确认并保留」。3 分钟内未确认会恢复原配置。
- 两项功能沿用面板现有登录模式。升级和网络试用期间，服务可能短暂中断。重新打开面板可以查看任务结果。
- 较早版本用同一条安装命令升级一次以安装维护服务。应急降级可以运行 `curl -fsSL <install.sh 的地址> | sudo bash -s -- --force`。

```bash
/opt/atom/bin/atom-installer uninstall          # 卸载，数据（规则、节点、设置）保留在 /opt/atom/data
/opt/atom/bin/atom-installer uninstall --purge  # 连数据一起删
```

脚本的环境变量：`ATOM_MIRRORS`（加速前缀，空格分隔，设成空串就只直连）、`ATOM_REPO`（owner/name）、`ATOM_PKG`（本地包文件或地址，代替下载）、`ATOM_SHA256`（包必须有的 sha256，`skip` 关闭校验）。

## 装到哪里

```text
/opt/atom/
  bin/      atomdns  sing-box  atom-installer
  etc/      atom.nft  atom-gateway.nft（网关形态）  install.json  changes.json
  lib/      policy-route.sh
  data/     配置根（atomdns -d）：config_custom.yaml  sub_config/  webinfo/  sing-box/  ui/main/ …
  backup/   升级和重配前的备份（成功后按时间保留最近 3 份）
  run/      维护服务的 Unix socket、锁和最近一次任务记录
```

OpenRC（Alpine）上还有 `lib/reload-sing-box.sh`。安装器为 `/var/log/atom/*.log` 安装轮转规则，Alpine 的定时任务检查到文件超过 5 MB 时轮转，最多保留 3 份压缩旧日志。停用 53 端口的服务后若改写了 resolv.conf，原内容在 `etc/resolv.conf.orig`。

`atom-maintenance` 在 DNS 重启期间继续执行任务。设备重启时，维护服务检查未提交的变更并恢复备份。服务日志使用 `journalctl -u atom-maintenance` 或 Alpine 的 `/var/log/atom/atom-maintenance.log`。如果恢复失败，备份和任务记录留在本机，修复原因后重启维护服务会重试。

systemd 服务定义位于 `/etc/systemd/system/`。使用"停用系统 DNS 占用"按钮时，安装器可能写入 `/etc/systemd/resolved.conf.d/atom.conf` 并修改 `/etc/resolv.conf`。OpenRC 服务定义位于 `/etc/init.d/`，日志在 `/var/log/atom/`，轮转规则在 `/etc/logrotate.d/atom-installer`，两者随卸载移除。双网卡网关还会加载 `ip atom_nat` 和 `inet atom_wan`，阻止 WAN 访问本机的面板、DNS 和代理端口。`changes.json` 记录安装器改过的系统设置。卸载前会先验证恢复记录，记录缺失或损坏时停止并保留现场。

## 升级时哪些文件被覆盖

升级和降级前，安装器会用候选版本检查即将使用的配置及已有数据，检查失败时当前服务继续运行。

升级会更新程序、单元、nft 规则、`config_custom.yaml`、`sub_config/` 中的程序配置、`rule/atom_catalog.json` 和 `ui/main/`。已有的 DNS 上游文件（`sub_config/forward_local.yaml`、`sub_config/forward_nocn.yaml`、`sub_config/forward_nocn_ecs.yaml`）会保留，缺失时才从安装包补齐。面板保存的上游和其他 `webinfo/` 数据、每日更新下载的规则和名单、缓存也会保留。新版本新增的数据文件会补上。升级后 atom-dns 起不来（或 sing-box 没有处于最新配置）就自动恢复备份。

## 开发

```bash
go vet ./... && go test ./...        # 纯函数的测试，Mac 上就能跑
scripts/build-package.sh --version 0.1.0 --atomdns-dir DIR   # 一次出四个包、install.sh、SHA256SUMS
```

DIR 里是 atomdns 的发布产物 `atomdns-linux-<变体>.tar.gz` 和 `atom-webui.tar.gz`；包与变体对应：amd64←`amd64`、arm64←`arm64-gnu`、amd64-musl←`amd64-musl`、arm64-musl←`arm64`。sing-box 和 `atom-installer` 由脚本按架构编。`install.sh.in` 是模板，`build-package.sh` 把版本和四个包的 sha256 写进去。包里的 `atom-installer` 是 Go 标准库写的单个静态文件，向导页面内嵌在里面（`web/index.html`）。

## 发布支持在线升级的版本

用固定的 Ed25519 发布密钥给连续版本签名。首次可以在发布环境中生成 PKCS#8 PEM 密钥。

```bash
openssl genpkey -algorithm Ed25519 -out /secure/path/atom-release.pem
installer/scripts/build-package.sh --version 0.2.0 --atomdns-dir DIR \
  --signing-key /secure/path/atom-release.pem
```

也可在发布环境设置 `ATOM_RELEASE_SIGNING_KEY_FILE`。私钥保存在发布环境，打包命令会把对应公钥嵌入安装器并生成 `atom-update-manifest.json`。将清单和四个原名安装包一同附在 `jasonxtt/atom` 的正式 `vX.Y.Z` Release 上，同时上传 `install.sh` 与 `SHA256SUMS`。面板通过上个已安装版本中的公钥验证清单，因此下一版须使用同一密钥签名。

开发包可省略签名参数，保留 Shell 安装和面板网络设置功能。
