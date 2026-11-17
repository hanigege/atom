# Atom 的 mihomo 后端

内核从官方 mihomo 的稳定发布版开始维护。首个补丁分支以 `v1.19.34` 为基线，Atom 通过 `mihomo/` 子模块固定源码提交。EDNS0 协议与 [Atom 的 sing-box 补丁](singbox-fakeip-outbound.md) 共用。 [官方版本](https://github.com/MetaCubeX/mihomo/releases/tag/v1.19.34) · [Atom 补丁分支](https://github.com/jasonxtt/mihomo/tree/atom/fakeip-outbound) · [DefaWong 参考实现](https://github.com/DefaWong/mihomo/tree/ef4f388116188e3ae4821ac72fd885c9d155b5c4)

## 配置与切换

`PUT /api/v1/atom/settings` 的 `backend` 字段接受 `mihomo` 或 `sing-box`。已有配置默认使用 sing-box。节点、组 ID、入站端口和规则由 Atom 共用，运行配置与 FakeIP 数据库分别保存在 `mihomo/` 和 `sing-box/`。 `GET /api/v1/atom/status` 的 `backend` 字段报告当前选择，`singbox` 字段为现有调用者提供活跃内核的运行状态。

切换时先校验目标配置，再启动目标服务并确认 Clash API 和各入站就绪。失败时尝试恢复原内核和选组。切换成功后客户端需要重新查询已缓存的 FakeIP，已有代理连接会随旧服务停止。现有节点中无法转换的协议或参数会在校验阶段提示具体节点。

配置生成使用现有的手动节点、selector、urltest 和 Final。订阅 provider 与 mihomo 专属分组尚未接入 Atom 管理接口。FakeIP 的 SOCKS5 UDP 出站会把域名交给远端代理。其他 UDP 出站沿用 mihomo 各协议的解析方式。

## 升级上游

从新的官方稳定 tag 更新补丁分支。内核回归要覆盖 EDNS0 SET/CLEAR、持久化、地址回收、flush、删组迁移，以及有绑定和无绑定的透明流量。接入测试使用同一份 Atom 状态，通过 HTTP/SOCKS mixed、TCP redirect 和 UDP TProxy 验证实际出口，再验证监听端口占用时的回滚。
