# Task 1.0 开发工单

依据：`docs/task-spec-1.0.md`（下称「规范」）。规范与本文件冲突时以规范为准；规范本身有歧义时停止并报告，不自行裁量。

## 总规则（每个工单都适用）

1. **无兼容包袱**：现有任务实现（`tb_tasks`、`tb_task_submissions`、`tb_task_logs`、`/api/owner/tasks*`、`/api/testtask/*`、`work_publish`、`internal/service/{owner_task,task_submission,task_board,test_task,task_rules,task_check}.go`、`internal/repository/{task,task_submission,task_log}.go`）整体被替换，不保留旧字段、旧状态、旧接口，不写兼容分支。
2. **范围隔离**：Storage（Memory）、账户、支付（Creem）、商店、后台 RBAC 不改；仅按工单明确列出的接点调用。
3. **分支与合入**：每个工单一个分支 `task10/<wo-id>`，一个 PR；`scripts/dev.sh test` 全绿方可提交 PR；PR 描述逐条对应工单的验收项。
4. **迁移**：新增 `migrations/015_task_v1.sql` 起的迁移；删除旧任务表在同一迁移中完成；只追加，不修改已合入迁移。
5. **测试**：业务规则放在 service 层并以真实 PostgreSQL 做集成测试；规范 §10 的每条不变式必须有对应断言；不写读取源码文本的测试。
6. **命名**：状态、错误码、字段名与规范逐字一致。
7. **不做**：不改对外文案（WO-9 统一生成），不实现规范未写的能力，不引入规范未列的状态。

## 依赖顺序

```
WO-1 → WO-2 → WO-3 → WO-4 → WO-5 → WO-6
                                    ↘ WO-7 → WO-8 → WO-9 → WO-10
```

---

## WO-1 数据模型与状态机内核

**目标**：落地规范 §2 实体、§4 与 §5.4 状态机、§10 不变式，作为后续工单唯一的数据与状态来源。

**交付**
- 迁移 015：删除 `tb_tasks`、`tb_task_submissions`、`tb_task_logs` 及相关约束；新建 `tb_task`、`tb_task_version`（Contract JSONB + Harness 快照）、`tb_task_claim`、`tb_task_submission`、`tb_task_submission_event`、`tb_task_report`。金额列 BIGINT；表归属应用角色。
- CHECK 约束：状态枚举；`reserved ≥ 0`；`available ≥ 0` 由触发器或事务内断言保证；Submission 唯一键 (`agent`, `task`, `request_key`)。
- `internal/task/`（新包）：状态与转换定义为纯函数 `Transition(from, event) (to, error)`；非法转换返回错误。
- repository：按新表提供读写；所有预留/释放/结算在单事务内与 Ledger（`lock_task`、`fund_task`、`earn_task`、`refund_task`）一起完成。
- 删除旧任务相关代码与测试，保持编译通过（依赖它们的 MCP/HTTP 暂时移除注册，WO-7 恢复）。

**验收**
- 状态机表驱动测试覆盖规范 §4、§5.4 的每一条合法转换与非法转换。
- 不变式检查函数 `CheckInvariants(ctx, taskID)` 实现规范 §10 第 1、2、3、6、9 条，供后续测试调用。

## WO-2 契约校验与发布者生命周期

**目标**：规范 §3、§4（除平台暂停外）。

**交付**
- 引入 JSON Schema 校验库（draft 2020-12，纯 Go，如 `github.com/santhosh-tekuri/jsonschema/v6`）；Schema 编译结果按版本缓存。
- `ValidateContract`：规范 §3 字段约束与一致性 1–4，返回全部错误 `{field, message}`。
- `task_create`（锁定预算）、`task_update`、`task_open`（校验 → Harness 快照 → 测试投递 → 生成版本）、`task_pause`、`task_close`、`task_fund`、`task_refund`、`task_get`、`task_list` 的 service 实现。
- Harness 快照：读取发布者本人 Memory，复制内容到 `tb_task_version`；非本人或不存在的 ref 为校验错误。
- 测试投递：使用现有 `internal/delivery` 的 SSRF 防护客户端，请求格式按规范 §7.1 并带 `Kungfu-Test: 1`。

**验收**
- 每个转换的前置条件违反时返回规范列出的错误码（`INVALID_STATE`、`INSUFFICIENT_CREDITS`、`VALIDATION_FAILED`、`TEST_DELIVERY_FAILED`、`HAS_RESERVATIONS`）。
- sync 任务含 `judgment` 标准时校验失败；`accepted=true` 的 example 不过 schema 时校验失败。
- paused 修改后再 open 生成新版本，旧版本数据不变。
- 每个测试结束调用 `CheckInvariants`。

## WO-3 Claim

**目标**：规范 §5.2。

**交付**
- `work_claim`、`work_claim_renew`、`work_release`；到期回收器（后台循环，复用现有后台任务生命周期），到期写 expired 并释放预留。
- pause/close 后 active Claim 可提交、不可续期。

**验收**
- 同一执行者同一任务至多一个 active Claim。
- 续期不超过 `deadline`；到期后提交返回 `CLAIM_INVALID`。
- 并发：`slots = 1` 时两个执行者同时 Claim，恰一成功。
- 释放/到期后 `reserved` 回落，`CheckInvariants` 通过。

## WO-4 提交受理

**目标**：规范 §5.3 受理顺序 1–8 与 Submission 创建。

**交付**
- `work_submit` service：严格按受理顺序；幂等检查基于 `payload_hash`（规范化 JSON 的 SHA-256）。
- 凭据形态检测复用 `internal/security`，返回命中的 JSON Pointer。
- Schema 校验错误转为 `{pointer, message}` 列表。
- 创建 Submission、写首条事件、预留（或承接 Claim 预留）同一事务。

**验收**
- 受理顺序每一步各有测试，失败时不创建 Submission、不改 `reserved`。
- 同 key 同 payload 在任务关闭后仍返回原 Submission；同 key 不同 payload 返回 `IDEMPOTENCY_CONFLICT`。
- 上限计数包含进行中的 Submission。
- 携带旧版本 Claim 的提交按旧版本 schema 校验。

## WO-5 投递、判定与结算

**目标**：规范 §5.4、§6、§7。

**交付**
- 投递器：请求格式 §7.1（含 `agent_ref` = HMAC(task, agent)）；回复映射 §7.2 全表。
- `uncertain` 恢复循环：30 秒间隔、24 小时上限后 `failed`（`DELIVERY_UNRESOLVED`）；进程重启后可继续（以数据库为准）。
- `under_review`：截止时间写入；超时回收器按 `source=timeout` 结算。
- `task_verdict`、`task_submissions`；Verdict 校验（criteria 已声明、reason 长度、annotations 规则）。
- 结算：接受与 `earn_task` 同事务；驳回/失败释放预留；每次状态变迁写事件。
- 接收端连续 5 次 `failed`（协议错误或故障）自动暂停任务，`paused_reason = RECEIVER_FAULT`。

**验收**
- 规范 §7.2 每一行回复各一个测试（使用本地测试接收端）。
- 超时接受、协议错误、连续故障暂停各有测试。
- 进程在 `delivering` 中断后重启，Submission 进入 `uncertain` 并最终确定。
- 全部测试结束 `CheckInvariants` 通过，包括第 4 条时限（以可控时钟测试）。

## WO-6 统计、上限与举报

**目标**：规范 §6.3、`work_report`、§9 数据保留。

**交付**
- 任务统计（近 30 天）：`accept_rate`、`median_verdict_seconds`、`timeout_rate`、`failure_rate`；`work_list` 的本执行者计数与 `remaining`。
- `work_report` 写 `tb_task_report`。
- 保留清理：终态满 30 天清除 payload 保留 hash；任务关闭满 30 天清除版本快照内容。

**验收**
- 统计口径与规范公式一致的单元测试。
- 清理后 Verdict、hash、事件仍在。

## WO-7 协议表达：MCP 与 HTTP（分步：7a 注册表与执行者工具、7b 发布者工具与错误目录、7c 并发收口、7d 协议归一、7e 测试隔离）

**目标**：规范 §8。

**交付**
- 单一工具注册表：每个工具定义一次（名称、输入 schema、描述、handler），同时挂载到 MCP 与 `POST /api/v1/<tool>`（Bearer 鉴权，与 MCP 同一身份校验）。
- 执行者返回结构 §8.2；`next_action` 按 §8.3 由状态与错误码唯一决定（纯函数，表驱动测试）。
- 未受理错误：MCP `isError=true` + 同一 `structuredContent`；HTTP 4xx + 同一 JSON。
- 工具描述包含前置条件、可能状态、可能 `next_action`，由规范条目生成。
- 删除旧 `work_publish`；保留账户与 Memory 工具不变。

**验收**
- 每个工具经 MCP 与 HTTP 各调用一次，返回结构逐字段一致。
- 错误目录每个 code 至少一个经协议层触发的测试。

## WO-8 控制台

**目标**：发布者与平台的人工入口。

**交付**
- Owner 工作台「任务」重做：任务列表与状态；创建/编辑使用 Contract JSON 编辑（带校验结果展示）与字段表单二选一；开放/暂停/关闭/追加/退款；待判定队列（查看 payload、按 criteria 驳回并填写 reason、接受）；统计展示。
- `/samelabs`：任务页适配新模型（状态、版本、统计、关闭原因）；新增举报队列（查看、驳回举报、关闭任务）。
- 首页任务板改用新模型（open 且 `slots ≥ 1`）。

**验收**
- 控制台所有操作调用 WO-2/5 的 service，不另写业务规则。
- 浏览器验收：桌面与 390px 宽度下完成一次 async 任务的创建、开放、判定。

## WO-9 对外表达与参考接收端

**目标**：由规范生成全部对外文本；降低发布方搭建判定器的门槛。

**交付**
- `web/llms.txt`、`web/kungfu_skill.md`、`web/openai.json`、MCP `instructions`、Owner 任务指南：按规范重写，客观、规则化，内容只来自规范。
- `examples/receiver/`：参考接收端（Go 单文件）：按规范 §7 协议收发；内置规则判定（schema、正则、必填）与模型评分（细则 + 阈值，模型调用可配置）；输出规范 §6.1 Verdict；附部署说明。

**验收**
- 文本中出现的每个工具名、状态、错误码、字段名均可在规范中找到。
- 参考接收端通过 WO-10 的端到端测试。

## WO-10 端到端执行者旅程

**目标**：以执行者视角验证整条工作流。

**交付**：`internal/e2e/`（或 `cmd/` 下测试）以 HTTP 接入 + 参考接收端运行以下旅程，每条结束执行 `CheckInvariants`：
1. sync：发现 → 读取 Harness → 提交 → 接受 → 结算。
2. sync：驳回（带 annotations）→ `revise` 修订重交 → 接受。
3. async：提交 → `under_review` → 发布者判定接受；另一条超时接受。
4. Claim：领取 → 续期 → 提交；领取后到期 → `CLAIM_INVALID`。
5. 投递中断 → `uncertain` → 恢复确定。
6. 接收端连续故障 → 任务自动暂停 → 执行者收到 `TASK_NOT_OPEN`。
7. 上限：驳回达到 `max_rejected_per_agent` → `SUBMISSION_LIMIT`。
8. 平台预检：`SCHEMA_MISMATCH` 附 pointer → 修正后接受。

**验收**：全部旅程通过；每条旅程中执行者仅依据返回的 `next_action` 决定下一步即可完成。

## 决策记录

- WO-1：账本结算类型为 `earn_task`。后台任务治理、Owner 投递日志、首页任务板、仪表盘的 draft/paused 计数在 WO-8 按新模型重建；pinned 删除。
- WO-2a：可选数值字段用指针区分缺省与显式值；“字符”按 rune 计；schema 根必须显式声明 `type: object`；harness_refs 归属与 receiver 可达性在 WO-2b 校验。
- WO-2b：任务不存在统一为 `TASK_NOT_FOUND`；可用为 0 的退款为 `NOTHING_TO_REFUND`；budget 下限不足为 `VALIDATION_FAILED`（field=budget）；Harness 快照形状 `[{ref_id, title, description, content}]`；`draft_contract` 缺省 `'{}'`。
- WO-3：已有 active Claim 时领取幂等返回；后台周期任务统一为 `runPeriodic`，claim_expiry 每 30 秒一轮、每轮 100 条。
- WO-4：payload 规范化哈希（键排序，数字按原文）；schema 编译按 (task_id, version) 进程内缓存；非 object 的 payload 归为 SCHEMA_MISMATCH（pointer ""）；CREDENTIAL_IN_PAYLOAD 取首个命中位置。
- WO-5a：状态机补边 uncertain --delivery_failed--> failed；投递结果写入使用不随请求取消的上下文；202 与其他 2xx 以响应码区分。
- WO-5b：恢复租约为 UPDATE…SKIP LOCKED 并刷新 updated_at；delivering 超 15 秒、uncertain 每 30 秒重投、满 24 小时 failed(DELIVERY_UNRESOLVED)；agent_ref 密钥取 SESSION_SECRET（轮换会改变 agent_ref，部署说明需写明）；判定已过期时先超时结算再返回 NOT_UNDER_REVIEW。
- WO-6a：work_list 至多 100 条、候选窗口 500；my.remaining 为两类上限剩余的较小值，全不限为 null；统计窗口按进入终态时间；harness bytes 为快照内容字节数。已知优化项：ListWork 逐任务查询统计，规模增长后改为批量查询。
- WO-7a：协议层统一 code→HTTP 状态表（429 仅 RATE_LIMIT）；retry_after：delivering 5、uncertain 30、under_review 60、failed 60 秒；未知工具 UNKNOWN_TOOL(404)。
- WO-7b：执行者与发布者共用一个注册表；发布者工具 next_action 恒为 null；/api/v1 请求体超限为 PAYLOAD_TOO_LARGE(413)。
- WO-7c：全局锁序 Task → Claim → Submission；无 Claim 提交以锁内版本为准；OpenTask 以草稿内容比较防并发修改；过期未清理的 Claim 在领取时就地过期；续期 TTL 取 Claim 所属版本；delivering 转 uncertain 后按 30 秒节奏重投；故障暂停在锁内按失败原因判定。
- WO-7d：账户与存储工具并入注册表，公开工具由 ToolDef.Public 标记；鉴权失败统一 UNAUTHORIZED；状态码单一来源 internal/errors.StatusFor；工具结果为类型化 ToolResult。
- WO-8a：Owner 控制台经 /api/owner/tool/{tool} 复用同一注册表，仅开放发布者工具；控制台不含独立业务接口。
- WO-6b：举报幂等（同执行者同任务 open 举报返回原记录）；payload 按进入终态时间满 30 天清理；快照按任务 updated_at 满 30 天清理；retention 每 6 小时一轮、每轮 500。
- WO-8b：平台关闭与发布者关闭共用 Claim/预留处理；处置举报即关闭任务时该任务全部 open 举报置 actioned；首页任务板单查询、至多 20 条。
- WO-7e：测试包串行（-p 1）共用一个测试库；列表查询以主键作为最终次序键。
- 门禁：凡改动迁移或被多包依赖的代码，PR 前必须跑全仓 `scripts/dev.sh test`。
- 部署：WO-7 完成前不部署生产。
