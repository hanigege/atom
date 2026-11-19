# atom

DNS 和代理一体的分流网关：AtomDNS 决定每个域名走直连还是哪个节点组，sing-box 或 mihomo 按决定执行。这是 [atom](https://github.com/jasonxtt/atom) 的容器版，单网卡旁路由，透明代理用 nft redirect + TProxy。

- 架构：`linux/amd64`、`linux/arm64`
- 标签：`latest`、`<版本>`（如 `0.3.0`）、`v<版本>`
- 数据卷：`/opt/atom/data`（规则、节点、设置都在这里）

## Docker Compose（macvlan）

```yaml
services:
  atom:
    image: jasonxtt/atom:latest
    container_name: atom
    restart: unless-stopped
    cap_add: [NET_ADMIN]
    sysctls:
      - net.ipv4.conf.all.accept_redirects=0
      - net.ipv4.conf.eth0.accept_redirects=0
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

改 `parent`（宿主网卡）、网段和容器 IP 后：

```bash
docker compose up -d
docker logs atom    # 显示安装向导地址和 6 位安装码
```

浏览器打开向导，输入安装码，装完跳到面板。然后在主路由上：

- 加静态路由：FakeIP 网段（默认 `28.0.0.0/8`）→ 容器 IP；
- 把客户端的 DNS 设成容器 IP。

## 升级

```bash
docker compose pull && docker compose up -d
```

新版本启动时自动迁移数据，失败会恢复原数据并退出。不能换回比数据更旧的镜像。

重新配置网络：`docker exec -it atom atom-installer reconfigure`。

## RouterOS

每个版本的 [Release](https://github.com/jasonxtt/atom/releases) 附带 `atom-<版本>-container-{amd64,arm64}.tar`，用 `/container add file=...` 导入。接法见 [docs/container.md 第 7.2 节](https://github.com/jasonxtt/atom/blob/main/docs/container.md#72-routeros-container-定)。

## 限制

- 宿主内核需要 `nft_tproxy`、`nft_socket` 模块。
- macvlan 下宿主本机访问不到容器。
- 需要 `NET_ADMIN`；不需要 `privileged`。
