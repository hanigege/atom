# Atom 的 mihomo 后端

内核从官方 mihomo 的稳定发布版开始维护。首个补丁分支以 `v1.19.34` 为基线，Atom 通过 `mihomo/` 子模块固定源码提交。EDNS0 协议与 [Atom 的 sing-box 补丁](singbox-fakeip-outbound.md) 共用。 [官方版本](https://github.com/MetaCubeX/mihomo/releases/tag/v1.19.34) · [Atom 补丁分支](https://github.com/jasonxtt/mihomo/tree/atom/fakeip-outbound) · [DefaWong 参考实现](https://github.com/DefaWong/mihomo/tree/ef4f388116188e3ae4821ac72fd885c9d155b5c4)

## 配置与切换

`PUT /api/v1/atom/settings` 的 `backend` 字段接受 `mihomo` 或 `sing-box`。已有配置默认使用 sing-box。节点、组 ID、入站端口和规则由 Atom 共用，运行配置与 FakeIP 数据库分别保存在 `mihomo/` 和 `sing-box/`。 `GET /api/v1/atom/status` 的 `backend` 字段报告当前选择，`singbox` 字段为现有调用者提供活跃内核的运行状态。

切换时先校验目标配置，再启动目标服务并确认 Clash API 和各入站就绪。失败时尝试恢复原内核和选组。切换成功后客户端需要重新查询已缓存的 FakeIP，已有代理连接会随旧服务停止。现有节点中无法转换的协议或参数会在校验阶段提示具体节点。

配置生成支持手动节点、订阅节点、selector、urltest、fallback、load-balance 和 Final。FakeIP 的 SOCKS5 UDP 出站会把域名交给远端代理。其他 UDP 出站沿用 mihomo 各协议的解析方式。

## 订阅与分组

- `POST /api/v1/atom/providers` 保存 HTTP(S) 订阅。请求可带 `interval`（秒）、`filter`（按源节点名称匹配的正则）和 `download_via`（手动节点 ID，默认直连）。让下载使用独立的手动节点可以避开订阅自身依赖。`user_agent` 设置下载时的 User-Agent（写成 mihomo 订阅的 `header`），留空用 mihomo 默认的 `clash.meta/<版本>`；改 UA 会换缓存文件，让 mihomo 重新下载。读取接口只返回公开字段，`PUT /providers/{id}` 省略 `url` 时保留旧值。
- 组的 `providers` 字段接受订阅 ID，`members` 仍可加入手动节点或其他组。mihomo 用原生 `proxy-providers` 下载、缓存和健康检查。Atom 可通过 `GET /providers/{id}/nodes` 查看节点，`POST /providers/{id}/refresh` 手动刷新。节点 ID 带订阅前缀，可在 selector 组内手动选择。节点下架后 mihomo 使用现有可选成员。
- 新建或修改活跃订阅时，Atom 通过 Clash API 确认 mihomo 已加载节点。下载失败、格式不兼容或过滤后没有节点时回滚。订阅仍被组引用时，删除会返回引用它的组。只有订阅来源的空组明确使用 `REJECT`。
- 组的成员来源有两种：`providers`（点名的订阅 ID）和 `all_providers: true`（"全部订阅"，之后新加的订阅自动包含，对应 mihomo 的 `include-all-providers`）。两者互斥，同时给会返回 400。
- 组的 `filter`（Atom 里不区分大小写）同时筛手动节点和订阅节点。手动节点由 Atom 筛出后作为 `proxies` 写入；订阅节点由 mihomo 的组 `filter` 筛（mihomo 只对 `use` 或 `include-all-providers` 的订阅节点生效）。生成时写成 `(?i)` 开头，`^` 和 `\A` 后面接订阅前缀 `p-xxxxxxxx/`，这样 `^HK` 仍匹配节点原名的开头；反引号（mihomo 用它分隔多个筛选）写成 `\x60`。mihomo 用 regexp2 读正则，语法和 Rust 的 regex 有细微差别（比如前者支持前瞻），写在筛选里的这类语法只有 mihomo 校验。
- 切到 sing-box 时订阅保留但不生效，切回 mihomo 时恢复。sing-box 下不能新建或修改出 fallback 和 load-balance 组（400）。

## 切换内核的交互（2026-11-18，用户定）**[定]**

- 订阅只用 mihomo 原生的 `proxy-providers`，AtomDNS 不做订阅下载和转换。
- 两个内核都可以在面板里切换。从 mihomo 切到 sing-box 前，如果检测到订阅或 fallback、load-balance 组，弹出警告窗口：
  - 订阅：说明订阅节点在 sing-box 下全部不可用。订阅配置保留在 Atom 中但不生效，切回 mihomo 时自动恢复。
  - fallback、load-balance 组：逐组列出，每组默认选"转换为 urltest"，旁边提供"删除"。转换是永久修改组类型，切回 mihomo 后仍是 urltest。删除按现有的删组流程处理，引用它的规则改成 Final。
  - 用户确认后才切换。后端的切换请求要带上每组的处理方式，缺少时拒绝并返回需要处理的组，不再隐式降级。
- 从 sing-box 切到 mihomo 前，在同一个确认窗口列出无法转换的节点及原因。这些节点保留在 Atom 中但不生效，切回 sing-box 时恢复。wireguard、hysteria、ssh 补上转换。
- 安装向导增加"代理内核"一步，sing-box 和 mihomo 都不预选，必须手动选择才能进入下一步。选 mihomo 时，"代理"一步可选填订阅地址，订阅放入"默认代理"组。
- 老用户升级后仍然使用 sing-box（`backend` 缺省即 sing-box）。

## 升级上游

从新的官方稳定 tag 更新补丁分支。内核回归要覆盖 EDNS0 SET/CLEAR、持久化、地址回收、flush、删组迁移，以及有绑定和无绑定的透明流量。接入测试使用同一份 Atom 状态，通过 HTTP/SOCKS mixed、TCP redirect 和 UDP TProxy 验证实际出口，再验证监听端口占用时的回滚。

## 实现说明（切换内核与订阅，2026-11-18）

- **切换前的检查**：`GET /api/v1/atom/settings/backend-check?target=mihomo|sing-box` 只读，返回 `inactive_nodes`（目标内核转换不了的节点：`id`、`name`、`type`、`reason`）、`provider_count`、`blocked_providers`（经由不生效节点下载的订阅，切到 mihomo 后改为直连下载）、`groups_to_handle`（切到 sing-box 要处理的 fallback 和 load-balance 组）、`provider_only_groups`（成员全部来自订阅的组，切到 sing-box 后没有成员）。
- **切到 sing-box**：`PUT /settings` 带 `backend: "sing-box"` 和 `group_actions`（组 ID → `urltest` 或 `delete`）。每个 fallback 和 load-balance 组都要有一项，缺任何一个返回 409 和 `groups`，什么都不改；给了不需要处理的组或别的值返回 400。`urltest` 永久改组类型（`tolerance` 取默认值，`strategy` 去掉）；`delete` 走 `DELETE /groups/{id}?confirm=true` 的同一套流程（引用它的规则改成 Final，FakeIP 绑定迁移），和切换在同一个事务里应用。
- **切到 mihomo**：有无法转换的节点时，请求要带 `confirm_inactive_nodes: true`，否则 409 和 `nodes`。这些节点留在 Atom 的状态里，但 mihomo 配置跳过它们：`ProxyState::effective()` 把它们从组和 Final 的成员里去掉（选中项落到第一个成员，空组用 `REJECT`），切回 sing-box 时原样恢复。节点 API 的 `active` 和 `inactive_reason` 标出它们，选中不生效的节点返回 400。规则只能指向组和 Final，不会直接引用节点；订阅的 `download_via` 指向不生效节点时，切到 mihomo 会在同一个事务里把这些订阅改成直连下载（`download_via` 清空），不需要额外确认；`backend-check` 的 `blocked_providers`（`id`、`name`、`download_via`、`download_via_name`）提前列出它们。设置接口的 `warnings.inactive_nodes` 在 mihomo 下列出这些节点。
- **订阅节点池**：`GET /providers/nodes` 一次返回所有订阅的节点（`id` 是 `p-xxxxxxxx/名称`，另有 `name`、`type`、`alive`、`delay`、`provider_id`、`provider_name`）；`POST /providers/nodes/delay`（`{id, url?, timeout?}`）用 mihomo 的订阅节点健康检查接口测单个节点。两者都只在 mihomo 下可用（`GET` 在 sing-box 下返回 `active: false` 和空列表，`POST` 返回 409）。
- **节点类型**：wireguard 的存储格式是 sing-box 端点（`address`、`private_key`、`peers[]`），sing-box 配置里生成到 `endpoints`，组仍按 tag 引用，没有 wireguard 节点时配置里没有 `endpoints` 键；mihomo 转成 `ip`/`ipv6` 加 `peers`。新增 snell（sing-box 版本 4 和 6，mihomo 同名协议）。mihomo 转换补上 wireguard、hysteria（第一代）、ssh、snell。sing-box 的 `multiplex`（含 `brutal`）和 mihomo 的 `smux`（含 `brutal-opts`）同是 sing-mux，两个方向都按字段转换，`tcp_fast_open` 对应 `tfo`。转换不了的情况（naive、shadowtls 独立节点、其他未映射的选项、ssh 的 `private_key_path`、snell 的 `userkey`）都带可直接展示的原因。
- **节点导入**在后端：JSON（sing-box 格式）本来就按 `NODE_TYPES` 接受新类型；Clash YAML 补上了 snell（只接受版本 4 和 6）、ssh（私钥要写内容）、hysteria、wireguard。Clash YAML 的 `smux`、`brutal-opts` 和 `tfo` 转成 sing-box 的 `multiplex` 和 `tcp_fast_open`：速率按 mihomo 的写法换算成整数 Mbps；sing-box 只有 ss、vmess、vless、trojan 能用多路复用，其他类型开着 `smux` 或带 `only-tcp` 时拒绝导入；AnyTLS 和 QUIC 类协议忽略 `tfo`。分享链接没有 wireguard、snell、ssh、hysteria 的通用格式，不新增。
- **安装器**：安装配置的 `backend` 必填（`sing-box` 或 `mihomo`，没有默认值），`subscription_url` 可选且只在 mihomo 下有效。新安装在 `stepConfig` 把初始的 `webinfo/atom_proxy.json`（含所选内核、FakeIP 网段和 mixed 端口）写好再启动 atomdns；订阅在 `stepSettings` 里创建并放进「默认代理」组。保留数据重装和重新配置沿用已有设置，忽略这两个字段。
- **内核运行时长**：`GET /status` 的 `singbox.started_at`（unix 秒）是当前活跃内核进程的启动时间，由 `/proc/stat` 的 `btime` 加进程的启动 tick 算出，systemd 和 OpenRC 都适用；内核没应答或取不到时为 null。
