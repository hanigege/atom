# 开发和构建

## 仓库结构

```text
atom/
  atomdns/     子模块 jasonxtt/atom-core，分支 atom/main：DNS、控制面、管理 API、WebUI（Rust + Vue）
  sing-box/    子模块 jasonxtt/sing-box，分支 atom/fakeip-outbound（Go）
  mihomo/      子模块 jasonxtt/mihomo，分支 atom/fakeip-outbound（Go）
  installer/   安装器 atom-installer（Go 标准库，网页向导内嵌）和打包脚本
  docs/        用户指南、设计和实现文档、logo
```

父仓库用子模块固定三个组件的配套提交。改了子模块后，先在子模块里提交、推送，再在父仓库更新子模块指针。

```bash
git clone --recursive https://github.com/jasonxtt/atom.git
cd atom
```

## 构建

**atomdns 和 WebUI**：在 Linux x86_64 上用 `atomdns/scripts/build-rust-release.sh`（需要 Rust stable 和 Node.js）编出 `atomdns-linux-<变体>.tar.gz` 和 `atom-webui.tar.gz`，也可以直接用 atom-core 仓库 `release-build.yml` 工作流的产物。把它们放在同一个目录 `DIR` 里。

**打包**：sing-box、mihomo 和安装器由打包脚本按架构编译（需要 Go 1.25+）：

```bash
installer/scripts/build-package.sh --version 0.2.0 --atomdns-dir DIR
```

产物在 `installer/dist/`：四个架构和 libc 组合的 tar.gz 包（amd64、arm64、amd64-musl、arm64-musl）、`install.sh` 和 `SHA256SUMS`。用本地包安装：

```bash
ATOM_PKG=/path/to/atom-0.2.0-linux-amd64.tar.gz bash install.sh
```

sing-box 的 naive 出站要用 cgo 静态链接 cronet，只能在 Linux 上编；不带 naive 的构建只适合开发。步骤见 [installer/README.md](../installer/README.md#sing-box-的-naive-出站)。

## 测试

```bash
cd installer && go vet ./... && go test ./...                 # 安装器
cd atomdns && cargo test -p atomdns-native-host --tests       # atomdns，部分集成测试只能在 Linux 上跑
```

## 发布

正式版用固定的 Ed25519 密钥签名，面板用上一个版本里的公钥验证升级清单，所以每个版本都必须用同一把密钥签名。步骤见 [installer/README.md](../installer/README.md#发布支持在线升级的版本)。

## 设计和实现文档

| 文档 | 内容 |
|---|---|
| [design.md](design.md) | 总体设计：DNS 决策流程、规则表、节点组、内核配置生成、管理 API、部署 |
| [singbox-fakeip-outbound.md](singbox-fakeip-outbound.md) | EDNS0 65001 协议和 sing-box 的 FakeIP 出口绑定补丁 |
| [mihomo-integration.md](mihomo-integration.md) | mihomo 内核、订阅和切换内核的实现 |
| [deploy-nft.md](deploy-nft.md) | nft 规则、策略路由和两种拓扑的流量路径 |
| [installer/README.md](../installer/README.md) | 安装器、安装位置、升级时覆盖哪些文件 |
| `atomdns/docs/api/openapi.yaml` | 管理 API（`/api/v1/atom/...`） |

## 许可证

GPL-3.0。提交的代码同样以 GPL-3.0 发布。
