# Codex Quota Scheduler

面向 CPA（CLIProxyAPI）的 Codex 串行配额调度与 429 自动隔离插件。

当前版本：`v0.1.3`。

## 解决的问题

旧的按会话粘性或动态评分调度，会让不同会话同时使用多个 Codex 账号。这个项目默认改为全局串行 fill-first：

1. 全部 Codex 请求固定使用同一个“当前主账号”；
2. 只在主账号达到配额阈值、收到 429、进入插件隔离或持续不可用时正式切换；
3. 备用账号保持休眠，不会因为另一个 session 或更高评分提前参与流量；
4. 429 进入持久化 cooldown / probation / half-open 状态，恢复时全局只放行一个探测请求；
5. 预热与正常流量解耦：100% 可用且尚未启动窗口的账号会一次一个顺序激活，正常请求仍只使用当前主账号。

默认切换阈值是任一活动窗口达到 `98% used`。这是为了在硬 429 前留下很小的并发余量；若你确实希望完全用尽，可把 `serial_switch_percent` 设为 `100`。

CPA 会在上游 `408/5xx` 后把账号临时移出候选池约 60 秒。插件将这种情况作为 request-local provisional fallback：固定使用一个临时备用账号，但保留原主账号且不增加正式切换次数；主账号恢复后自动回归。只有候选持续缺席至少 90 秒并经过 3 次确认，才以 `candidate_unavailable_confirmed` 正式切换。pinned 预热和半开探测竞争也不会污染全局主账号。

`v0.1.3` 还会拒绝没有正数窗口时长的 Primary/Secondary 占位配额头，防止 `window-minutes=0`、`used-percent=0` 把 Keeper 的真实周/月窗口覆盖成伪 weekly。

## 账号选择顺序

首次选择或发生切换时，插件按以下顺序选择下一个健康账号：

1. 按 `window_order` 排序，默认是 5 小时窗口、周窗口、月窗口；
2. Keeper 类型未知的账号排在已识别窗口之后；
3. 开启 `prefer_reset_credits` 时，同类中优先有 reset credit 的账号；
4. 再优先已经启动周期的账号；
5. 再优先已用比例更高的账号，以集中消耗；
6. 最后按 CPA priority 和稳定 auth ID 排序。

选定后，其他账号即使出现更高分、更高优先级或来自另一个会话，也不会抢占当前主账号。

Keeper 暂时不可用时，插件不会伪造配额百分比：已经选中的主账号会继续使用，尚未选中时按 CPA priority 和 auth ID 确定性认领一个账号，直到真实 429 或后续 Keeper 数据触发切换。

## 安全边界

插件只处理 provider 为 `codex` 的纯 Codex 候选池：

- 不修改官方/default 模型名称和模型列表；
- 不修改 OAuth、PAT、cookie 或任何凭证文件；
- 不修改第三方 provider、第三方 API 或混合 provider 路由；
- Keeper 密码和 CPA Management key 只从挂载文件读取，不进入 YAML、状态文件或日志；
- 不注册未鉴权的动态 `/v0/resource/plugins/...` 页面；所有状态和特权操作仅位于 CPA 已鉴权的 `/v0/management/...` 路由。

## 推荐配置

直接参考 [SERIAL_CONFIG.example.yaml](SERIAL_CONFIG.example.yaml)。关键配置如下：

```yaml
plugins:
  enabled: true
  configs:
    codex-quota-scheduler:
      enabled: true
      priority: 2000
      scheduler_mode: serial
      serial_switch_percent: 98
      serial_prefer_active_cycle: true

      keeper_url: http://cpa-usage-keeper:8080/keeper
      keeper_password_file: /run/secrets/keeper_login_password
      refresh_interval: 30s
      stale_after: 45m
      state_path: /var/lib/codex-quota-scheduler/state.json

      fallback_ban: 15m
      max_ban: 24h
      half_open_probe_timeout: 15m
      half_open_retry_after: 2m

      prefer_reset_credits: true

      warmup_enabled: true
      warmup_model: gpt-5.6-sol
      warmup_sidecar_url: http://codex-agent-identity-sidecar:8787/backend-api/codex
      warmup_retry_after: 15m
      cpa_management_url: http://127.0.0.1:8317/v0/management/api-call
      cpa_management_key_file: /run/secrets/management_key
```

`warmup_model` 只用于一次最小化的 pinned hello 激活请求，不会修改 CPA 的默认模型。
预热器每轮最多执行一个请求，并跳过本周期已成功或仍在重试冷却期的账号；下一轮会继续处理后续满额账号，不会并发唤醒整个账号池。Keeper 对未启动窗口也可能返回 `观测时间 + 完整周期` 的动态占位重置时间；插件会结合 `resetAfterSeconds`、窗口时长、实际用量和观测锚点识别这种漂移值，并顺序激活 5h、weekly 与 monthly 主窗口。外部重置导致旧预热记录失效时，插件会自动丢弃旧周期锚点。

## 构建

需要 Go 1.21+、CGO 和本机 C 编译器。

Linux / macOS：

```bash
./build.sh
```

Windows PowerShell：

```powershell
./build.ps1
```

生成的文件名与插件 ID 一致：

- `codex-quota-scheduler.so`
- `codex-quota-scheduler.dylib`
- `codex-quota-scheduler.dll`

## 状态与升级

默认状态文件：

```text
/var/lib/codex-quota-scheduler/state.json
```

状态文件版本 4，包含：

- 429 quarantine / probation / half-open 状态；
- 当前主账号及选择时间；
- 正式串行切换计数、临时故障转移计数和最近切换原因；
- 已执行的预热记录。

它不包含 token、PAT、cookie、Keeper 密码或 Management key。文件以 `0600` 权限原子写入。

从旧 `codex-429-autoban` 升级时，如果需要沿用旧的 ban/warmup 状态，可先把 `state_path` 显式指向旧文件；插件兼容读取旧版 v2/v3 状态，并在下次写入时升级为 v4。

## Management API

以下端点全部位于 CPA Management key 鉴权后：

```text
GET  /v0/management/plugins/codex-quota-scheduler/bans
GET  /v0/management/plugins/codex-quota-scheduler/quota
POST /v0/management/plugins/codex-quota-scheduler/unban
POST /v0/management/plugins/codex-quota-scheduler/unban-all
```

`/quota` 会显示当前主账号、切换阈值、正式切换、临时故障转移、候选缺席确认、Keeper 快照、预热和隔离状态。

## 兼容模式

为便于旧部署迁移，代码仍保留 `legacy`、`shadow` 和 `enforce` 模式；新安装默认且推荐使用 `serial`。只有 serial 模式保证不同会话不会主动分散到多个账号。

## 已知限制

CPA 当前 scheduler API 不能返回“过滤后的候选集合”或硬拒绝全部候选。如果所有账号都处于隔离状态，插件只能返回 `Handled=false`；CPA 核心后续如何处理取决于宿主版本。要实现绝对的全池硬拒绝，需要 CPA 核心扩展 API。

## 设计与安全

- [DESIGN_SERIAL_SCHEDULER.md](DESIGN_SERIAL_SCHEDULER.md)
- [SECURITY.md](SECURITY.md)
- [NOTICE](NOTICE)

## 许可证与来源

MIT。项目基于 [ysxk/codex-429-autoban](https://github.com/ysxk/codex-429-autoban) 的实现演进，保留原始版权和许可证声明；本仓库增加了全局串行调度、主账号状态持久化、active-only warmup、安全路由收敛及相关测试。
