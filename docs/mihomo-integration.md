# Atom 的 mihomo 后端

内核从官方 mihomo 的稳定发布版开始维护。首个补丁分支以 `v1.19.34` 为基线，Atom 通过 `mihomo/` 子模块固定源码提交。EDNS0 协议与 [Atom 的 sing-box 补丁](singbox-fakeip-outbound.md) 共用。 [官方版本](https://github.com/MetaCubeX/mihomo/releases/tag/v1.19.34) · [Atom 补丁分支](https://github.com/jasonxtt/mihomo/tree/atom/fakeip-outbound) · [DefaWong 参考实现](https://github.com/DefaWong/mihomo/tree/ef4f388116188e3ae4821ac72fd885c9d155b5c4)

## 配置与切换

`PUT /api/v1/atom/settings` 的 `backend` 字段接受 `mihomo` 或 `sing-box`。已有配置默认使用 sing-box。节点、组 ID、入站端口和规则由 Atom 共用，运行配置与 FakeIP 数据库分别保存在 `mihomo/` 和 `sing-box/`。 `GET /api/v1/atom/status` 的 `backend` 字段报告当前选择，`singbox` 字段为现有调用者提供活跃内核的运行状态。

切换时先校验目标配置，再启动目标服务并确认 Clash API 和各入站就绪。失败时尝试恢复原内核和选组。切换成功后客户端需要重新查询已缓存的 FakeIP，已有代理连接会随旧服务停止。现有节点中无法转换的协议或参数会在校验阶段提示具体节点。

配置生成支持手动节点、订阅节点、selector、urltest、fallback、load-balance 和 Final。FakeIP 的 SOCKS5 UDP 出站会把域名交给远端代理。其他 UDP 出站沿用 mihomo 各协议的解析方式。

## 订阅与分组

- `POST /api/v1/atom/providers` 保存 HTTP(S) 订阅。请求可带 `interval`（秒）、`filter`（按源节点名称匹配的正则）和 `download_via`（手动节点 ID，默认直连）。让下载使用独立的手动节点可以避开订阅自身依赖。读取接口只返回公开字段，`PUT /providers/{id}` 省略 `url` 时保留旧值。
- 组的 `providers` 字段接受订阅 ID，`members` 仍可加入手动节点或其他组。mihomo 用原生 `proxy-providers` 下载、缓存和健康检查。Atom 可通过 `GET /providers/{id}/nodes` 查看节点，`POST /providers/{id}/refresh` 手动刷新。节点 ID 带订阅前缀，可在 selector 组内手动选择。节点下架后 mihomo 使用现有可选成员。
- 新建或修改活跃订阅时，Atom 通过 Clash API 确认 mihomo 已加载节点。下载失败、格式不兼容或过滤后没有节点时回滚。订阅仍被组引用时，删除会返回引用它的组。只有订阅来源的空组明确使用 `REJECT`。
- 切到 sing-box 时停用订阅来源，fallback 和 load-balance 暂按 urltest 使用静态成员，没有静态成员时拒绝流量。设置 API 的 `warnings` 返回受影响的组及订阅 ID。切回 mihomo 时恢复已保存的组类型和订阅引用。（下面"切换内核的交互"改了 fallback 和 load-balance 的处理，待实现。）

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
