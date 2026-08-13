# Codex Quota Scheduler 运行逻辑与状态机

本文描述当前功能分支 v0.1.13 的真实运行逻辑，供代码审查、生产验收和故障定位使用。它不表示该版本已经成为 GitHub 正式 Release。

## 1. 数据来源与可信度

调度器同时观察三类信息：

| 来源 | 用途 | 可信规则 |
|---|---|---|
| CPA scheduler candidates | 当前请求可实际选择的 auth ID、priority 与 provider | 只接受纯 codex 候选集合；混合 provider 返回 Handled=false。 |
| Keeper quota snapshot | 5h、weekly、monthly 窗口、used%、reset、allowed、reset credits | 快照和每个有效窗口都必须在 stale_after 内；过期数据不触发阈值切换或预热。 |
| 上游响应/配额头 | 429、reset-after、window class、成功响应 | 429 是权威隔离信号；无完整窗口头时进入有上限的 probation。 |

window-minutes=0 等 Primary/Secondary 占位头不覆盖 Keeper 的真实周/月窗口。显式 generate=false 的 token 计数或检查请求旁路状态变更，不会预测扣额、启动 half-open 或写入 429 隔离。

## 2. 全局串行主账号

serial 模式不读取会话 ID。所有普通请求共享一份持久化主账号状态：

~~~text
未选择
  -> 按窗口等级和同类规则选出主账号
  -> 已提交主账号
       -> 正常：持续使用同一账号
       -> 临时候选缺席：请求级 provisional fallback
       -> 阈值/429/隔离/确认缺席：正式切换
~~~

### 2.1 首次选择顺序

1. window_order，默认 5h -> weekly -> monthly；
2. 已识别窗口优先于未知窗口；
3. prefer_reset_credits=true 时，同类优先有 reset credit；
4. serial_prefer_active_cycle=true 时，同类优先已启动周期；
5. 同类优先 used% 更高者，将消耗集中到较少账号；
6. CPA priority 更高；
7. auth ID 字典序，保证结果稳定。

窗口等级具有严格优先级。当前主账号若属于较低优先级窗口，而更高优先级窗口恢复可用，代码会按 window_order 执行必要的等级抢占；同一等级内不会仅因另一个账号分数变高而抢占。

### 2.2 正式切换条件

- 任一仍有效的窗口达到 serial_switch_percent；
- 窗口 allowed=false 或 limit_reached=true；
- 上游 429 建立 quota cooldown 或 probation；
- 当前账号处于 cooldown/half-open 等不可调度状态；
- CPA 连续不再提供该候选至少 90 秒且达到 3 次确认；
- 更高优先级的窗口类别恢复并满足严格抢占条件。

### 2.3 candidate_unavailable 为什么不等于切号

CPA 在 408/5xx 后可能暂时将某 auth 从候选池移除约 60 秒。调度器先保留已提交主账号，仅为当前请求选一个稳定的 provisional auth：

- 不增加正式 switch 计数；
- 主账号回到候选池后自动恢复；
- provisional auth 在缺席期间保持稳定，避免请求之间乱跳；
- 只有超过宽限并满足确认次数才记录 candidate_unavailable_confirmed。

## 3. 429 隔离状态机

~~~mermaid
stateDiagram-v2
    [*] --> Healthy
    Healthy --> Cooldown: authoritative 429
    Cooldown --> ProbeReady: reset time reached
    ProbeReady --> HalfOpen: one global lease acquired
    HalfOpen --> Healthy: successful matching probe
    HalfOpen --> Cooldown: repeated 429
    HalfOpen --> Probation: non-429 probe failure
    Probation --> ProbeReady: retry delay reached
~~~

- 有可信 reset 的 429 建立 quota cooldown，且重复 429 不会缩短已有期限。
- 无可信配额头的 429 使用 fallback_ban，并受 max_ban 上限约束。
- cooldown 到期不代表立即全量放行；只有一个并发请求能取得 half_open_probe_timeout 租约。
- 匹配该 probe 的成功结果才清除隔离；失败会回到 cooldown 或 probation。
- cyber_policy、cyber_abuse、认证错误和 workspace 停用不是普通 quota cooldown，不能被外部额度重置对账自动清除。

## 4. 预热候选的完整条件

预热用于启动“100% 可用但窗口尚未真正启动”的账号周期，不用于分配普通请求。一个账号只有同时满足以下条件才进入 warmup_candidates：

1. Keeper 对该 auth 的快照新鲜；
2. 存在可识别的 5h/weekly/monthly 窗口；
3. 所有仍有效且可识别窗口均为 0% used、allowed=true、未触限；
4. reset 缺失、已经到期，或呈现随观察时间移动的完整周期占位值；
5. CPA 当前存在可用的 Codex auth binding，并能稳定解析 auth ID 与 auth index；
6. Agent Identity 场景的 sidecar 标记和绑定匹配；
7. auth 未被禁用、不可用或隔离；
8. 没有该窗口的 pending、confirmed、blocked 或尚未到重试时间的结果；
9. 当前 plugin generation 仍是 owner；
10. 当前进程取得跨实例 warmup lease。

warmup_candidates=0 只说明当前轮没有可执行候选，不等于功能关闭。warmup_skipped_*、warmup_auth_rejected 和 warmup_auth_last_error 才能解释筛除原因。

## 5. 如何判断“窗口尚未启动”

仅看到 reset 时间不够，因为 Keeper 或上游可能提供“现在 + 完整周期”的移动占位值。调度器组合判断：

- 0% used；
- reset 是否缺失或已经过去；
- reset 距观察时间是否接近完整 5h、7d 或 30d；
- 相邻快照中的 reset anchor 是否稳定；
- 窗口 class 是否可信。

预热请求返回 2xx 后先写入 pending outcome。只有后续 Keeper 快照显示稳定的新 reset anchor 或真实周期证据，才标记为激活成功并设置 suppress_until。这样不会把一个返回成功但没有启动额度窗口的请求误报为已预热。

## 6. 最低成本预热请求

生产默认 warmup_execution_mode=management：

~~~json
{
  "model": "<warmup_model>",
  "input": "hello",
  "stream": false,
  "store": false,
  "max_output_tokens": 16
}
~~~

请求通过 CPA 已认证 Management api-call，携带精确 auth_index 和 pinned auth 元数据。执行前重新解析 auth binding，防止等待期间 auth 文件已被热替换。预热使用的 model 只作用于这个内部请求，不会改 CPA default 模型或正常流量模型。

账号严格一次一个执行。成功、429、SSE terminal error 或 HTTP error 都会先记录结果，再释放实例租约。

### 6.1 不自动重试的错误

下列结果会归一化成不含敏感消息的 blocked code：

- cyber_policy / cyber_abuse / abuse 类错误；
- 401、403；
- deactivated_workspace；
- invalid_refresh_token；
- auth_unavailable；
- 其它被分类为不可恢复的认证/策略错误。

插件不会自动重复触发这些账号。修复凭据、workspace 或配置后，由管理员调用 POST .../warmup-retry 显式解除；该操作不会顺便清除 quota ban。

## 7. 平台批量重置与新周期识别

当平台提前把多个账号恢复为 0% 时，旧 quota ban 不能立即相信，也不能永久保留。对账必须满足：

1. 当前 ban 确实是 quota 类型，不是 probation、cyber 或 auth；
2. Keeper 快照对该账号全部窗口均为 0% used、允许、未触限；
3. 快照自身与窗口观察时间新鲜；
4. 相对旧 ban 出现可信变化：占位 reset、reset anchor 改变或窗口类别改变；
5. 收到两个时间严格递增、彼此独立的合格快照；
6. ban 建立后没有新的 warmup 429 或其它冲突证据。

第一次确认会触发一次有跨实例 cooldown 的定向 Keeper refresh，用独立观测完成第二次确认。确认后只清除对应 quota cooldown，并清理旧 warmup 状态，使账号可以在同一刷新循环重新进入预热候选。

历史版本曾可能把周/月级 reset 统一保存成 `Window: 5h`。对账不会直接相信这个标签，而会先用 `BannedAt -> ResetAt` 的完整跨度修复可证明的低估分类；若新 Keeper 计划已经变成更短且完整的 weekly-only Team 窗口，则仍须两个独立新鲜观测才能确认旧 monthly cooldown 已失效。第二次确认完成后，当前这份 weekly 快照会在同一轮参与预热候选判断，不需要等待下一次全局刷新。

## 8. 热加载与 generation ownership

CPA 热重载可能让新旧动态库 generation 短时间同时存在。v0.1.13 使用两层互斥：

- Generation record：state_path.generation 记录当前 owner。新 generation 原子认领后，旧 generation 发现被替代便停止刷新、调度副作用和完整状态写入。
- Warmup instance lease：state_path.warmup.lock 使用 OS 文件锁，保证跨进程同时只有一个预热执行者。

新 generation 启动后有 15 秒预热宽限，用于合并旧实例刚完成的 outcome journal，避免同一账号在热替换边界被重复请求。被替代实例唯一允许的尾部写入是自己已经持有租约的 warmup outcome，且写到单独 journal；新 owner 合并后清空 journal。

## 9. 持久化状态

state_path JSON 当前包含：

- bans 与 half-open/probation 元数据；
- warmup outcome；
- 外部 reset 双快照确认进度；
- serial_active_auth_id、选择时间、正式切换/临时 fallback 计数和最近原因；
- 保存时间和格式版本。

状态不包含 Keeper 密码、CPA Management key、PAT、OAuth token、Cookie 或原始错误正文。目录以 0700 创建，文件以 0600 临时写入、fsync，再在 generation fence 内原子 rename。

辅助文件：

~~~text
state.json
state.json.generation
state.json.warmup.lock
state.json.warmup.outcomes
~~~

这些文件应位于同一持久卷，不应由两个互不相关的 CPA 实例共享。

## 10. Management 状态判读

GET /v0/management/plugins/codex-quota-scheduler/quota 的关键字段：

| 字段 | 正常含义 |
|---|---|
| scheduler_mode | 生产应为 serial。 |
| serial_active_auth_id | 当前全局主账号，仅用于已认证管理诊断。 |
| serial_switches | 正式切换次数，不包含 provisional fallback。 |
| serial_provisional_fallbacks | 临时候选缺席导致的请求级备用次数。 |
| generation_active | 当前实例是否仍为 generation owner。 |
| fresh_snapshots | 可用于真实策略判断的新鲜 Keeper 快照数量。 |
| warmup_candidates | 当前轮真正可执行的候选数。 |
| warmup_auth_rejected | CPA auth 绑定筛除分类，不含 secret。 |
| ban_reset_pending_confirmations | 正在等待第二个独立快照的旧 quota ban 数。 |
| last_ban_clear_reason | 最近一次安全清除旧 quota ban 的证据类型。 |

正常稳定状态不要求 warmup_candidates 大于 0；若全部周期已经启动或候选仍在 suppress/confirmed 状态，0 是正确值。

## 11. 不变量

- 正常流量只提交一个全局 auth；预热不会改变它。
- 5h -> weekly -> monthly 是类别优先级，不是账号轮询顺序。
- 100% 可用不自动等于“未启动”；必须有 reset anchor 证据。
- HTTP 2xx 不自动等于预热成功；必须由后续 Keeper 快照确认。
- cyber_policy 原样归类为 blocked code，绝不进入自动重试。
- 插件不接管 OAuth、PAT 保存、模型列表或第三方 API。
