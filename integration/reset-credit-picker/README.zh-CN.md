# CPA / Keeper 手动选择重置券

这组补丁只用于管理页手动兑换。调度插件的自动使用开关、时间与防重复记录保持原有行为。

## 问题与行为

Keeper 的“第 1／2／3 次”原来只是到期时间展示，无法点击选择；CPA 原生确认框也没有单券选择。
补丁把每一行改成带标签的原生单选按钮，默认按到期时间选中第一张，允许鼠标、触摸与键盘切换。
确认请求通过 `credit_id` 传入所选券，保留官方 `redeem_request_id`；没有单券明细时仍可明确使用上游选择路径。

Keeper 的 `POST /api/v1/quota/reset` 增加可选 `credit_id`，从页面、API 绑定、服务到官方请求完整传递。
Keeper 和 CPA 提交前重新读取当前账号的明细；所选券失效、已过期或不属于当前账号时拒绝兑换，避免换用另一张。
请求失败不自动重试兑换；Keeper 保留弹窗并展示错误。

## 可复现来源

| 补丁 | 上游仓库 | 基线提交 |
| --- | --- | --- |
| `keeper.patch` | [Willxup/cpa-usage-keeper](https://github.com/Willxup/cpa-usage-keeper) | `f53fec8d3f7c0f189cf046aeae807375fd9b23e4` |
| `management.patch` | [router-for-me/Cli-Proxy-API-Management-Center](https://github.com/router-for-me/Cli-Proxy-API-Management-Center) | `f154fa9380825c4fe94bc577a9abaa7e505f8875` |
| `keeper-v2.0.3.patch` | [Willxup/cpa-usage-keeper](https://github.com/Willxup/cpa-usage-keeper) | `9db74cfc380d11064af80cf1626efd6e3d068217` |

在对应基线的干净工作区使用 `git apply --check`，再 `git apply`。
Keeper 按自身 Dockerfile 构建；管理页使用上游锁定的 Bun 1.4.3，执行 `bun run verify`，发布单文件 `dist/index.html`。

Keeper v2 使用单独的 `keeper-v2.0.3.patch`，保留新版 Claude grant 和组织选择链路。
升级前备份 v1 数据库，并先用副本验证迁移；回退 v1 必须使用迁移前的数据库副本。
Codex 提交失败时保留弹窗和所选券，展示错误并等待用户处理，不自动重复兑换。

官方客户端的请求契约见
[rate_limit_resets.rs](https://github.com/openai/codex/blob/1a604d58da344c84528fecd5b5154105f597d70b/codex-rs/backend-client/src/client/rate_limit_resets.rs)：
`credit_id` 是可选字段，支持消费指定券。最终能否兑换仍由上游确认；可选择不等于已经兑换成功。

## 验收与回滚

必须验证第二、第三张实际提交的 ID，不能只看选中样式。
浏览器验收拦截兑换 POST，不用真实重置券测试界面。恢复旧页面和 Keeper 镜像即可回滚；保留原数据卷与数据库备份。
只更新管理页与 Keeper 服务，CPA 本体和模型请求进程不需要重启。
