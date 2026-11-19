# atom

atom 的容器版：AtomDNS 加 sing-box / mihomo 的透明分流网关，一个镜像、一个数据卷（`/opt/atom/data`），支持 linux/amd64 和 linux/arm64。

- Docker Compose 部署：见 [docs/container.md 第 7.1 节](https://github.com/jasonxtt/atom/blob/main/docs/container.md)
- MikroTik RouterOS（容器）部署：见 [docs/container.md 第 7.2 节](https://github.com/jasonxtt/atom/blob/main/docs/container.md)
- 每个版本的 Release 附带 `atom-<版本>-container-{amd64,arm64}.tar`，可 `docker load` 或在 ROS 上 `file=` 导入。

项目主页：https://github.com/jasonxtt/atom
