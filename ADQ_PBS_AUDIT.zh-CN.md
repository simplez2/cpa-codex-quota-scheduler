# ADQ-PBS 调度审计与重构记录

日期：2026-09-10
仓库：`simplez2/cpa-codex-quota-scheduler`
当前基线：`9df0be1 fix: deduplicate warmup account status by credential identity`

## 1. 当前旧算法审计

旧实现由 `serial.go`、`balanced.go`、`pacing.go`、`quota_refresh.go` 和 `warmup.go` 共同组成。`serial` 保留一个全局 active auth，`balanced` 为多个 session 分别绑定账号；两条路径都有硬额度检查、429 quarantine、半开探测和 session 粘性。`pacing.go` 已维护 token 成本样本、P75/P90/P95 与 response header 观察，`warmup.go` 已使用 CPA Management `auth-files` 和 CPA 原生 Codex responses 路径。

当前选择流程仍是“读快照 → 排序/积分 → 记录本地预测扣减”，本地 `balancedPending` 只是估计，并不是可以跨 worker 验证的 upstream quota reservation。Usage 只能通过 `AuthID + model + RequestedAt` 做近似匹配，没有稳定 reservation id。

## 2. 当前真实 Provider 行为

插件通过 CPA 的 `api-call` 查询 `https://chatgpt.com/backend-api/wham/usage`，解析 `primary_window`、`secondary_window`、`used_percent`、`reset_at`、`reset_after_seconds`、`allowed`、`limit_reached`、`plan_type` 和 `rate_limit_reset_credits_available_count`。窗口按时长归一为 `5h`、`weekly`、`monthly`。Provider 返回的窗口和 reset timestamp 优先于配置先验。

原生 plan 映射为 `plus → plus`、`team → team_standard`、`self_serve_business_prolite → team_premium`；`pro` 等未知 SKU 不强行映射固定容量。`5h = weekly × 0.16` 只在缺少真实窗口容量时作为先验。

线上审计过的 CPA 为 `cli-proxy-api:quota-failover-v7.2.152-20260908`，插件为 `codex-quota-scheduler-v0.3.7.so`。线上曾出现多个账号同时收到 `server_is_overloaded`，随后 CPA 返回 `auth_unavailable`；这证明 provider overload 不能直接记为 quota exhausted，首包前 failover 仍属于 CPA 宿主责任。

## 3. 已验证假设

- 插件可以在请求执行前选择 auth，维护本地 quota 估计和粘性。
- 插件可以经 CPA 原生 `api-call` 查询额度，不能读取或保存 upstream token。
- 429 可以在 Usage 回调后进入 cooldown/half-open 状态。
- 余额窗口可以由 Provider telemetry 和响应 header 合并，旧的更严格证据不能被延迟数据覆盖。
- `balanced` 允许多个 session 并发；`serial` 是单 active lane。
- CPA Management 面板可以注册插件资源与管理 API，因此设置、指标和恢复操作可以集中到面板。

## 4. 被推翻假设

- `server_is_overloaded` 不等于 quota exhausted，必须进入独立 Provider overload circuit。
- 未来一次 5h reset 不等于真实可用容量；周额度不足时只能得到 phantom capacity。
- 价格中的 cached token 折扣不能直接当作产品 quota 折扣；只能由真实 quota burn 学习。
- `used_percent == 100` 不能单独证明发生了全局 reset，dormant 账号可以长期停在占位状态。
- 单纯 Max Remaining、Earliest Reset 或固定权重 score 都不能同时满足 sticky、周额度寿命、并发 reservation 和 phase 约束。
- 插件不能把已经开始输出的 SSE/WebSocket 无损迁移到另一个账号；首包前 retry/failover 需要 CPA 宿主补丁。

## 5. 新状态模型

每个账号同时维护：provider 健康、5h/weekly/monthly 窗口、有效额度、burn 统计、cache 统计、phase、warm epoch 和短期 reservation。Provider overload、quota cooldown、auth failure、warmup uncertain 是独立状态；一个状态过期不能清除其他状态。

全局维护 `scheduler_version`、`fallback_to_legacy`、状态版本、周 epoch、phase anchor mode、pool 指标、最近 routing reason 和最近 reservation。所有短期 reservation 带 TTL，重启后过期 reservation 不得重新占用额度。

## 6. ADQ-PBS 选择顺序

Fast Router 按以下字典序决策：

1. 合法 sticky 继续使用当前账号；
2. 过滤周额度为 0、5h 有效额度为 0、unhealthy、provider hard reject、reservation 冲突；
3. 以 burn P95 计算有效额度和未来容量；
4. 计算 FullWidth、EffectiveWidth、Fill Probability、Q_lock、Runway 和三个 Margin；
5. 尽量保持 FullWidth；复杂状态使用 Projected Leximin，普通状态使用 weekly waterline；
6. 只有在仍有安全候选时才考虑 phase/reset fit、cache 和轮转次数；
7. 在同级候选中使用确定性 auth id 作为最终 tie-breaker；
8. 计算和 reserve 必须在同一临界区内完成，失败重新计算。

旧 serial/balanced/pacing 路径作为 fallback；新路径由 `scheduler_version=adq-pbs` 和 `fallback_to_legacy` 控制。

## 7. Effective Quota

对账号 i：`H_i = W_i × max_quota_ratio_5h`，`h_eff = h_raw - reserved_5h - pending_5h - safety_5h`，`w_eff = w_raw - reserved_week - pending_week - safety_week`。周额度是绝对硬上限；`w_eff <= 0` 时，即使 5h full 也不能选中。

## 8. Burn 与 Cache

成本使用 model/effort 样本的 EWMA 和 P95。Cache 只影响预计 burn，不获得额外固定 score；没有 diagnostics 时按 cold 估计。`B_expected = P_cache × B_warm + (1-P_cache) × B_cold`，其中 `P_cache` 由真实 cached/read/write token 和同 session/repo 的历史 burn 学习，30 分钟只作为保守衰减窗口。

## 9. Width、Fill、Lock-in、Runway

`FullWidth = count(w_eff >= H)`，`EffectiveWidth = sum(min(1,w_eff/H))`。`p_fill_current` 估计在当前 5h reset 前耗尽当前 h_eff 的概率；`p_fill_full` 估计一个完整 5h 是否能被真实需求填满。Lock-in 使用配置化 SLA：`1-(1-p)^m >= confidence`，不写死 `p<0.5`。

`Q_lock_mean = H + ((1-p)/p) × mu_fail`，复杂状态用保守的 `Q_lock_p95`。Runway 同时计算 5h 与 weekly，最终取更小者；future recovery 必须同时满足 5h reset 和未来周额度，不把 reset timestamp 直接当救援点。

## 10. Waterline、Projected Leximin、Bottleneck

普通同规格池优先让 `w_eff/W` 同步下降；模拟每个候选扣除 `Q_lock_p95` 后，按升序比较整池 projected kappa，最大化最危险账号的 kappa。`M_week`、`M_5h`、`M_phase` 分别反映周、5h、phase bucket 的未来余量；系统 Margin 取三者最小。周约束进入 WEEK_CONSTRAINED，phase 约束进入 PHASE_CONSTRAINED，所有候选都不足时区分 BRIDGE、RISK、CAPACITY_FAILURE。

## 11. Week reset、Warmup、Phase

周 reset 通过 Provider reset timestamp、旧值显著下降后多账号同时恢复、debounce/quorum 和 state version 确认新 epoch。epoch 变化立即令旧 warmup 失效，并对每个 CPA Codex auth 最多成功 warm 一次。warmup 只走 CPA 原生路径，不解析或兼容 Codex Agent Identity 请求。

第一代 5h 允许同步；从第二代开始先验证 `FIRST_USE_AFTER_RESET` 或 `FIXED_PROVIDER_WINDOW`。可重锚定时用 15 分钟 phase bucket 做 deficit fill；不可重锚定时只做 consumption staggering。真实用户到来优先成为 Natural Warmup，高峰允许 `PHASE_RECOVERY_COMPRESSED`，流量下降后再 self-heal，不能为相位美观拒绝真实请求。

## 12. Reservation 与故障边界

reservation 记录 `reservation_id`、auth、session、model、窗口扣减、创建时间、过期时间和状态。创建、释放、超时和 Usage reconciliation 都是幂等的。Provider overload 只打开短 circuit；quota 429/limit reached 进入 quota cooldown；未知结果保留短 TTL，避免重复请求。

已经输出的数据流不能由插件重放；CPA 宿主需要在首包前执行 retry/failover。插件层可以确保下一次选择不再使用已知不可用账号。

## 13. 面板与验证

面板状态新增 `scheduler_version`、`fallback_to_legacy`、每账号 w/h/w_eff/h_eff/kappa、FullWidth/EffectiveWidth、burn、cache、p_fill、Q_lock、runway、M_week/M_5h/M_phase、weekly debt、phase、reservation 和人话 routing reason。设置继续通过 CPA 面板保存，不要求进入插件内部设置。

完成后运行旧测试、新单元测试、并发测试、前端测试、`go vet`、CGO shared build 和离线仿真。代码、构建、运行时注册、部署和终端用户流量分别验证；本轮先不修改线上服务器。

## 14. 已知剩余风险

Provider 可能延迟 telemetry、改变窗口字段或在首包后失败；插件只能保守收敛，不能保证上游容量。没有真实 burn 样本时 P95 是保守先验，Monte Carlo 只提供风险估计。全池总需求超过总容量属于 CAPACITY_FAILURE，不能归咎于调度器。
