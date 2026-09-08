# 独立 CPA 插件交接

源码版本为 0.2.1；测试构建不代表已经发布或部署。

CPA 面板侧栏新增“Codex 额度调度”，插件列表使用相同中文显示名，ID 仍为
`codex-quota-scheduler`。公开资源仅返回静态页面，额度仍经 CPA 管理鉴权读取。
面板每 15 秒读缓存，不额外查询上游、不改变预热开关或调度算法。

## 运行依赖与迁移

只需要 CPA Management API。插件自行查询解析上游额度，不再连接外部额度服务。
账号令牌由 CPA 注入，插件只读取 Management key 文件。
默认查询官方 wham/usage；自定义供应商必须提供同结构额度接口。

1. 使用目标平台构建的动态库，保留 state.json 与 generation 文件。
2. 按 SERIAL_CONFIG.example.yaml 设置 CPA Management 地址与 key 文件。
3. 删除旧额度服务的 URL、密码配置及密码挂载；插件不会读取这些字段。
4. 可选预热默认 native，默认关闭；开启后通过 CPA HostModel 执行。
5. 检查认证接口 /v0/management/plugins/codex-quota-scheduler/quota，
   验证 fresh_snapshots、quota_polls、quota_refresh_error 与主账号。
6. 配套宿主以线上v7.2.152为基线应用首次输出前额度失败切换修复，设置
   codex.stream-bootstrap-buffering=true、max-retry-credentials=0。
   发布部署已获授权；完成状态须由运行镜像、实际配置和请求验收确认，此文不提前宣告成功。

## 验收

- 正常账号得到额度窗口；禁用账号不再查询；暂时 unavailable 账号仍能观测重置。
- 主账号优先查询，备用账号按独立冷却轮询；某个账号失败不阻止其他账号。
- 默认sustainable按周余量扣储备后的日预算排序；同reset下80%优先40%，近reset可优先消耗较低余量。
- 预算优势20%、双方两次新原生观测、主号保持5分钟再平衡；查看serial_weekly_rebalance的metric与weekly_budget_rebalance切换原因。
- 默认5h采用429_only、reserve_5h_percent=0；98%/99%已用不因5h软阈值提前切号，硬满/禁用/429仍切备用。
- 429_only下5h排序不扣静态储备或耗速/缓存预测；runway估计只供观测。已有会话后续请求跟随新主号。
- 默认team_standard；quota_account_plans可声明1/5/20容量先验，不乘入周健康评分。
- 5h重置不能机械切到周预算明显更低的账号；周额度耗尽即使5h满额也不可用。
- 旧预留方案须显式设置reserve_aware和reserve_5h_percent=15；weekly_remaining、inherit_global、serial_soft_continuation分别保留其他旧行为。
- research/ALLOCATION_RESEARCH.zh-CN.md的离线表格是旧保留阈值方案的历史比较，不能当作当前零预留方案的成效。
- 接口故障保留原缓存，刷新退避在重启后仍生效。
- 乱序数据不回退新额度；读缓存不推动 reset；过期周窗口不掩盖新鲜 5h 硬限额。
- 热重载沿用 generation owner 与预热 lease。
- 预热默认全池 15 分钟一次、滚动 24 小时 8 次，失败计数；检查 warmup_traffic。
- 暂停原因 manual_retry_required 表示需要修复后显式调用 /warmup-retry；更换绑定不解除暂停。
- 保留 state.json 中 warmup_attempts；窗口清理、热重载与手动重试均不退还请求预算。
- 预热只使用两分钟内且高于储备线的额度；确认完成须由同绑定的新窗口观测证明。
- 流式请求首次输出前的同请求切号须验证配套CPA宿主修复、stream-bootstrap-buffering=true和max-retry-credentials=0实际生效。

配额查询不消费重置次数。已经输出的流不能靠调度插件无条件重放。
