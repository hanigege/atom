# 升级和维护

## 在面板里升级

「设置 → 系统」的「版本」一栏可以检查正式版本、查看发布说明，并升级整套 atom（atomdns、两种内核、安装器和面板）。

- 更新清单和升级包优先经本机的代理下载，失败时依次改用 GitHub 直连和内置的加速地址；
- 升级清单有签名，安装包要校验 sha256；
- 升级前先用新版本检查现有配置和数据，检查不通过就不升级；
- 新版本服务自检失败时，自动恢复升级前的备份。

升级过程中服务可能短暂中断。升级完成后重新打开面板，可以看到这次任务的结果。

0.1.x 的旧版本还没有面板升级功能，需要先用安装命令升级一次（见下文）。

## 用安装命令升级、降级

再运行一次安装命令，脚本发现已经安装过，就会走升级流程：

```bash
curl -fsSL https://github.com/jasonxtt/atom/releases/latest/download/install.sh | sudo bash
```

版本相同时什么也不做；要装更旧的版本，或者重装同一版本来修复，加 `--force`：

```bash
curl -fsSL https://github.com/jasonxtt/atom/releases/latest/download/install.sh | sudo bash -s -- --force
```

升级会覆盖程序和自带的配置模板，保留规则、节点、节点组、订阅、DNS 上游、面板设置和缓存。每次升级前都会备份到 `/opt/atom/backup/`，保留最近 3 份。

## 修改网络

「设置 → 网络」可以修改拓扑、网卡、网关、客户端网段、回包方式和面板端口。为了防止改完连不上面板：

1. 应用前，页面会显示新旧两个地址；
2. 应用后会先自检，然后在新地址打开「设置 → 网络」，点「确认并保留」；
3. 3 分钟内没有确认，自动恢复原来的网络配置。

## 面板登录

面板的用户名和密码在「设置 → 面板」的「面板登录」里修改。没有设置密码时，局域网里的任何人都能打开面板。

## 卸载

```bash
/opt/atom/bin/atom-installer uninstall          # 保留数据（规则、节点、设置）
/opt/atom/bin/atom-installer uninstall --purge  # 连数据一起删除
```

卸载会按安装时的记录还原系统设置：nft 规则、策略路由、`ip_forward`、被停用的 53 端口服务和 resolv.conf。保留了数据的话，下次重装时向导会沿用原来的规则和设置，跳过 DNS 和代理两步。

## 文件位置

```text
/opt/atom/
  bin/      atomdns、sing-box、mihomo、atom-installer
  etc/      nft 规则、install.json（安装信息）、changes.json（改过的系统设置）
  data/     配置和数据：规则、节点、内核配置、面板
  backup/   升级和重新配置前的备份（保留最近 3 份）
  run/      维护服务的 socket 和任务记录
```

## 服务和日志

| 服务 | 作用 |
|---|---|
| `atom-dns` | DNS、规则和控制面，同时托管面板 |
| `atom-sing-box` / `atom-mihomo` | 代理内核，配置由 atom-dns 生成，只运行当前选中的那个 |
| `atom-maintenance` | 执行面板里的升级和网络修改任务。机器重启时检查有没有未完成的变更，必要时恢复备份 |

systemd 上用 `journalctl -u atom-dns` 等命令查看日志；Alpine（OpenRC）的日志在 `/var/log/atom/`。

## 应急恢复

- 面板打不开、升级后服务异常：用带 `--force` 的安装命令重装当前版本或旧版本，数据会保留；
- 面板升级或网络修改中途机器重启：`atom-maintenance` 启动后会检查未提交的变更，自动恢复备份。恢复失败时，备份和任务记录都留在本机，修好原因后重启 `atom-maintenance` 会再试一次。
