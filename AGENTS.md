# atom 项目规范

先遵守上级的 `/Users/tom/github/AGENTS.md`。下面是本项目额外的规定。

## 编译和测试放在哪里（2026-11-18，用户定）

Mac 的硬盘空间小，本项目的编译产物、缓存和临时测试文件不要放在 Mac 本地盘上。

- **源码**留在 `/Users/tom/github/atom`（包括子模块），git 操作也在这里做。
- **必须在 Mac 上做的事**（WebUI 开发、Go 交叉编译、打包、快速单元测试），产物统一放到 NAS 上的 `/Users/tom/nas/u.2/mac-use/atom/`（SMB 挂载的固态硬盘）：
  - Rust：`CARGO_TARGET_DIR=/Users/tom/nas/u.2/mac-use/atom/cargo-target`
  - Go：`GOCACHE=/Users/tom/nas/u.2/mac-use/atom/go-build`
  - 打包：`installer/scripts/build-package.sh --out /Users/tom/nas/u.2/mac-use/atom/dist ...`
  - 临时测试文件、下载的包、解包目录：`/Users/tom/nas/u.2/mac-use/atom/tmp/`
  - 这个目录不支持硬链接（cargo 会自动改用复制，不影响使用）。
- **必须在 Linux 上做的事**放到 `ssh vm92`（10.0.0.92，Debian 13，x86_64），工作目录是 `/root/atom-build`：
  - atomdns 的集成测试（`cargo test -p atomdns-native-host --tests`）：macOS 不能绑定 127.0.0.2 等地址，部分集成测试只能在 Linux 上跑；
  - atomdns 的发布构建，以及需要 cgo 或 Linux 工具链的构建（比如 sing-box 的 naive 出站）；
  - 正式发布包的打包。
  - vm92 系统自带的 Go 太旧（1.24），编译 sing-box 需要 1.25 以上，可以用 `GOTOOLCHAIN=auto` 或另装新版 Go，不要改系统的 Go。
  - 源码同步用 git（推到分支后拉取）或 rsync，不要在 vm92 上改代码后再拷回来。
- **真机安装测试**仍然在各台测试 VM 上做，不在 vm92 上做。
- 汇报构建和测试结果时，写清楚是在哪台机器、哪个目录上跑的。
