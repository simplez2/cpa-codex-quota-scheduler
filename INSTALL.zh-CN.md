# 安装和升级

从 [GitHub Releases](https://github.com/simplez2/cpa-codex-quota-scheduler/releases/latest)
下载正式版本。仓库里的源码 ZIP 需要自行编译；普通安装使用下面的插件 ZIP。

## 选择正确平台

| CPA 运行环境 | 安装包后缀 | 包内动态库 |
| --- | --- | --- |
| Linux x86-64 | `_linux_amd64.zip` | `codex-quota-scheduler.so` |
| Linux ARM64 | `_linux_arm64.zip` | `codex-quota-scheduler.so` |
| macOS Apple Silicon | `_darwin_arm64.zip` | `codex-quota-scheduler.dylib` |
| Windows x64 | `_windows_amd64.zip` | `codex-quota-scheduler.dll` |

按 **CPA 进程**的平台选择，Docker 部署按容器平台选择。Linux 构建以 Debian 12 glibc 为基线，
不适用于直接使用 musl 的 Alpine 容器。没有发布 Intel macOS 或 Windows ARM64 预编译包。

## 校验和安装

同时下载该版本的 `checksums.txt`，将它与插件 ZIP 放在一起。Linux 可执行：

```sh
sha256sum --ignore-missing -c checksums.txt
```

Windows PowerShell 可执行 `Get-FileHash -Algorithm SHA256 ./下载的插件.zip`，
将输出与 `checksums.txt` 中对应文件逐字比较。必须至少有一个匹配的包被实际校验。
包内 `release-manifest.json` 另记录动态库 SHA-256、版本、平台、Go 工具链和源码提交。
这些哈希用于完整性和版本核对，不替代对发布者的信任，也不是独立签名。

解压后将动态库安装到 CPA 配置对应的插件目录，常见位置为：

```text
plugins/linux/amd64/codex-quota-scheduler.so
plugins/linux/arm64/codex-quota-scheduler.so
plugins/darwin/arm64/codex-quota-scheduler.dylib
plugins/windows/amd64/codex-quota-scheduler.dll
```

仅选择与当前 CPA 匹配的一项。也可将动态库放在 CPA 接受的插件根目录。
保留包内 `LICENSE`、`NOTICE` 及权限说明。启用插件后，检查 CPA 插件列表中的版本，
再打开 **插件 → Codex 额度调度**。

## 最小配置与权限

将包内 `SERIAL_CONFIG.example.yaml` 的插件段合并进现有 CPA 配置。
保留原有提供商、账号、路由和密钥配置，根据实际容器路径设置：

- `cpa_management_url`：可信 CPA 管理地址，优先使用同容器或受保护网络的回环地址。
- `cpa_management_key_file`：CPA 管理密钥的只读文件挂载，不是普通客户端 API Key。
- `state_path`：CPA 进程可写的私有持久化目录；升级时保留状态及相邻的锁、代次和预热文件。
- `scheduler_mode`：默认 `serial`；多个会话同时分配可选择 `balanced`。

Linux 动态库通常使用 `0644`，密钥 `0400` 或 `0600`，私有状态目录 `0700`。
Windows 使用对应服务身份的 NTFS ACL。无需给插件 root、管理员、Docker socket 或 SSH 权限。
原生动态库继承 CPA 进程权限，详见包内 `CPA_PERMISSIONS.md`。

默认开启到期诊断及严格的 PAT 元数据自动修复；不希望写回认证文件时设置
`auth_expiry_auto_repair: false`。诊断仍读取凭据文档。预热默认关闭。

## 宿主与升级

凭据并发保护需要 CPA v8.0.4+。使用更旧宿主时，先关闭 `account_concurrency_enabled`。
自动长等待必须另外应用准确匹配的宿主补丁；v8.0.44 见仓库
[集成说明](https://github.com/simplez2/cpa-codex-quota-scheduler/blob/main/integration/CONTINUITY-v8.0.44.md)。
面板显示已配置并不证明宿主已实际调用该能力。

升级前保存当前动态库、配置及持久化状态。先暂停新请求并等待在途请求结束，再使用 CPA
支持的插件更新或重载方式；Windows 中被加载的 DLL 不能直接覆盖。升级后核对插件版本、
状态目录、实际并发计数和一个真实流式请求，再恢复全部入口。保留原动态库以便回退。

## 大图片与超时

反向代理可能在大图片请求上传或等待模型首包时返回 524/504。先对照客户端、代理、NewAPI
和 CPA 日志，区分上传、上游首包、额度等待与客户端取消。SSE 心跳无法保护尚未完成的上传。
插件包不包含本机代理、SSH 通道、供应商密钥或个人网络配置。
修改网络路径后，应以相同大小图片请求验证完整的 `response.completed`，不能只看健康检查。
