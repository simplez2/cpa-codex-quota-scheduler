# 1Panel 容器升级

1Panel 的“容器 → 升级”需要完整镜像名。不要把容器的镜像配置写成 `sha256:...`。

| 容器 | 目标镜像 |
| --- | --- |
| cli-proxy-api | `ghcr.io/simplez2/cli-proxy-api:latest` |
| cpa-usage-keeper | `ghcr.io/simplez2/cpa-usage-keeper-reset:latest` |

后续在 1Panel 点击“升级”，勾选“强制拉取镜像”，确认即可。保留现有挂载、环境变量和网络配置。

CPA 镜像基于官方稳定版，保留等待额度恢复和原请求续接补丁。发布流程每 6 小时检查并构建官方稳定版；兼容补丁、测试或原请求恢复验收失败时不发布。Keeper 镜像保留当前 v2.0.3 的单券选择功能。两者只发布通过验证的 `latest`。

编排文件也应使用同样的完整镜像名，避免日后从编排重建时恢复成镜像 ID。普通上游镜像不包含这些定制功能。

1Panel 1.x 的容器升级只重新连接一个网络。对于依赖多个网络和固定 IP 的现有部署，宿主运行 `network_guard.py` 及配套 systemd 服务：它在容器启动后核对 Compose 项目和挂载，仅恢复配置中列出的网络与私有地址。配置位于 `/opt/cpa-1panel/networks.json`，不会把 Docker socket 或额外权限提供给 CPA、Keeper 和插件。
