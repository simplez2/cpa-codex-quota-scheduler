# Codex Quota Scheduler 生产交接手册

本文用于把当前功能分支安全交给下一位维护者。它假设 CPA、Keeper 与可选 Agent Identity sidecar 已由宿主平台部署。

## 1. 接管前必须知道

- 默认生产模式是 serial，不要改成轮询，也不要让多个会话主动分散到不同账号。
- 默认类别顺序是 5h -> weekly -> monthly；有 reset credit、已启动周期和 used% 只在类别内继续排序。
- 预热与普通调度是两条路径。预热绝不能改变已提交主账号。
- cyber_policy、cyber_abuse 和认证类 blocked 必须停止自动重试。
- 插件不应修改 CPA 官方/default 模型、OAuth、PAT、Cookie、第三方 provider 或第三方 API。
- 所有动态诊断和操作必须位于 /v0/management/...；不得把状态或特权 UI 放到 /v0/resource/plugins/...。

## 2. 版本关系

| 位置 | 含义 |
|---|---|
| main.go 的 pluginVersion | 编译进动态库的运行版本。 |
| registry.json | Plugin Store 元数据与将来 Release 资产版本。 |
| Git tag / GitHub Release | 对外正式发布版本。 |
| Draft PR | 尚未发布、供审查的功能分支。 |

当前功能分支为 v0.1.13，但正式 Release 可能仍是 v0.1.0。合并前可以测试 branch build，但不能把它称为正式 release。正式发布时必须同时校验 pluginVersion、registry、tag、资产名和 Release 说明。

## 3. 推荐生产挂载

~~~text
host/
  config.yaml
  plugins/
    linux/amd64/codex-quota-scheduler.so
  scheduler/
    state.json
    state.json.generation
    state.json.warmup.lock
    state.json.warmup.outcomes
  secrets/
    keeper_login_password
    management_key
~~~

建议：

- 插件目录在安装/升级时短暂可写，正常运行恢复只读；
- scheduler 状态目录只给 CPA 运行 UID 读写；
- secret 文件只读，禁止写入 YAML；
- 一个 state_path 只属于一个逻辑 CPA 实例；
- 1Panel 更新官方 CPA 镜像时保留这些宿主挂载。

## 4. 构建与发布前检查

~~~bash
gofmt -w .
go test ./...
go test -race ./...
go vet ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o codex-quota-scheduler.so .
git diff --check
~~~

另外必须完成：

- gitleaks detect --source . --no-banner --redact；
- 搜索公网仓库中的真实邮箱、IP、域名、auth ID、容器名和 token 片段；
- 检查完整 Git 历史，不只检查工作树；
- 使用与生产完全相同的 CPA image digest 做 canary；
- 检查动态库与准备部署文件的 SHA-256。

## 5. 安装和热更新

### 5.1 首次安装

1. 把平台对应动态库放到 CPA plugin 目录。
2. 按 SERIAL_CONFIG.example.yaml 添加配置和 secret file mount。
3. 确认 state_path 可写、secret 可读。
4. 先在独立 canary CPA 加载。
5. 检查 /quota、/bans 和 CPA 插件日志。
6. 发起一条普通低风险请求，确认所有会话选择同一 auth。
7. 验证后再切换生产挂载。

### 5.2 热替换

插件已经提供 generation ownership，但热替换仍需遵守：

1. 先构建并校验新动态库；
2. 使用同目录临时文件写入并原子 rename，避免 CPA 读到半个二进制；
3. 保留同一个 state_path；
4. 观察 runtime_generation 增加、generation_active=true；
5. 确认旧 generation 变为 superseded/released；
6. 等待至少一个 refresh interval 与 15 秒启动宽限；
7. 检查没有重复 warmup outcome。

若 CPA 宿主不支持可靠插件热重载，可以重启 CPA；不要为了零中断冒险加载损坏的共享库。

## 6. 上线验收

### 6.1 串行调度

- scheduler_mode=serial；
- 多个独立会话命中同一 serial_active_auth_id；
- 同类内 used% 更高账号优先但不会中途抢占；
- 5h 可用时不会先选 weekly/monthly；
- 5h 不可用后才进入 weekly，再到 monthly；
- provisional fallback 不增加 serial_switches。

### 6.2 429

- 429 后 /bans 出现 cooldown；
- 到期后最多一个 half-open probe；
- 成功清除，重复 429 延长而不缩短 cooldown；
- cyber/auth blocked 不被 quota reset 对账清除。

### 6.3 预热

一次预热的完整验收不是接口返回 200，而是：

1. 账号在新鲜 Keeper 快照中满足全 0% 与未启动证据；
2. warmup_candidates 短暂出现；
3. 日志只出现一个 pinned auth 的执行；
4. outcome 先 pending；
5. 定向 Keeper refresh 或后续常规刷新得到新 reset anchor；
6. outcome 变为 confirmed/activated，且设置 suppress；
7. 普通流量主账号未变化。

## 7. 日常巡检

建议每次 CPA/插件更新后记录以下非敏感字段：

- plugin version、CPA image digest；
- config_generation / runtime_generation；
- generation_active；
- last_refresh、fresh_snapshots、last_error；
- serial_switches、serial_provisional_fallbacks、最近原因；
- warmup_candidates 与各类 skipped/rejected；
- keeper_refresh_requests、accepted、rejected；
- ban 数量、probe starts/successes/failures；
- 外部 reset confirmation 与 clear 计数。

不要把完整 /quota 输出直接贴到公开 issue；先删除 auth ID、内部地址和环境标识。

## 8. 常见故障

### warmup_candidates=0

按顺序看：

1. warmup_enabled；
2. fresh_snapshots 与 last_refresh；
3. warmup_auth_source / warmup_auth_last_error；
4. warmup_auth_rejected；
5. warmup_skipped_banned；
6. warmup_skipped_stale；
7. warmup_skipped_not_unstarted；
8. 当前 outcome 是否 pending、confirmed、blocked 或仍在 retry/suppress 时间内。

### 账号显示 100% 但没有预热

100% 可用不等于窗口未启动。若 reset anchor 已稳定，系统会认为周期已经存在，因此无需预热。只有 reset 缺失、过期或呈完整周期移动占位时才执行。

### candidate_unavailable

短暂出现一般是 CPA 对 408/5xx 的候选抑制。确认是否只有 provisional fallback，以及主账号是否在约 60 秒后恢复。只有 candidate_unavailable_confirmed 才代表持续缺席导致的正式切换。

### 401/403/auth_unavailable/deactivated_workspace

预热会 blocked，不自动重试。先修复 Agent Identity、PAT、workspace 或 sidecar 绑定，再调用受认证的 warmup-retry 路由。不要在工单里粘贴真实 key 或 auth ID。

### cyber_policy / cyber_abuse

这是终止类策略结果。正确行为是记录安全 code、立即停止该账号预热，不进行自动重试。不要使用 unban-all 绕过；先调查请求内容和账号状态。

### 平台批量提前重置

观察 ban_reset_pending_confirmations。第一次全 0% 快照不会立刻清 ban；系统应触发一次定向 Keeper refresh，第二个独立快照满足 reset anchor/class 变化后才清 quota cooldown。probation 与 cyber/auth 不参与。

## 9. 手工恢复操作

| 操作 | 何时使用 | 风险 |
|---|---|---|
| POST /warmup-retry | 修复了 blocked 凭据/配置后 | 可能再次发起最低成本激活请求。 |
| POST /unban | 已确认单账号上游额度恢复但自动对账无法观察 | 会允许该账号重新参与 half-open/调度。 |
| POST /unban-all | 已由管理员确认整个池的隔离均失效 | 高风险，不应作为常规刷新按钮。 |

所有操作都必须使用 CPA Management key，并在操作前保存脱敏状态快照。

## 10. 回滚

1. 保留当前动态库、SHA-256 和状态文件副本；
2. 将上一已验证动态库原子替换回 plugin 目录；
3. 保留 state.json，除非回滚版本无法读取当前格式；
4. 若必须回滚状态，先停止或隔离 CPA，避免旧 generation 写回；
5. 恢复后检查 generation owner、Keeper refresh 和 active auth；
6. 先用 canary 请求验证，避免直接对全池进行 unban-all。

## 11. 发布清单

- [ ] 版本号在 main.go、registry.json、tag 和资产名中一致。
- [ ] README 明确正式 Release 与功能分支边界。
- [ ] gofmt、test、race、vet、Linux CGO c-shared build 通过。
- [ ] gitleaks 与环境标识扫描通过。
- [ ] canary 使用目标 CPA image digest 通过。
- [ ] Management 路由无 key 不可访问。
- [ ] 无动态 /v0/resource/plugins/... 状态或特权操作。
- [ ] 预热一次一个，普通主账号不改变。
- [ ] 429 half-open 只有一个并发 probe。
- [ ] 更新 Draft PR，而不是创建重复 PR。

## 12. 禁止事项

- 不要把 Keeper/Management 密码写进 YAML、README、日志或 GitHub Actions。
- 不要把 production state 文件提交到仓库。
- 不要用真实账号做并发压力测试。
- 不要把 warmup_candidates=0 当成故障并强制全池预热。
- 不要自动重试 cyber/abuse/auth blocked。
- 不要把预热改成并行或把串行调度改成普通轮询。
- 不要为展示前端入口而注册未认证的动态 resource route。
