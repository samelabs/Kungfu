# Kungfu Task 机制 1.1

本文件只记录 Task 机制 1.0（[`task-spec-1.0.md`](task-spec-1.0.md)）之上的**变更点与兼容性**；1.0 的全部规则继续有效，与本文冲突处以本文为准。
依据：`kungfu.md` §7（Task）、§8（Turn）、§9（Visibility）。差距的关闭情况见 [`conformance.md`](conformance.md) 的 Task profile 表。

---

## 1. 契约版本（§7.1）

- 每个任务有一列不可变的契约版本：`task_contract_versions(task_id, version, contract)`。创建时写入 version 1；`task_update` 每次保存发布一个新版本（version + 1），同一事务完成。
- **一次修订只约束之后形成的承接**：Claim 创建时记录其绑定的 `contract_version`。
- 持 Claim 的提交按 Claim 绑定的版本校验 `output.schema`、投递到该版本的 `receiver.url`；任务当前的契约对它不再有影响。
- 不持 Claim 的提交按当前版本校验与投递，并在 Submission 上记录所用版本（`contract_version`）。
- `work_get` 返回 `contract_version`；调用者持有该任务的 active Claim 时，返回 Claim 绑定版本的契约（§9：承接者恒可读其绑定的版本）。`task_get` 返回当前版本号。
- Claim 保留其预留金额的 1.0 规则不变：`claim.ttl` / `max_duration` 的续期仍取当前契约（续期只在 open 状态发生，而修订只在 paused 状态发布）。

## 2. 输入固定（§7.1、§7.3、§9）

- Claim 创建时，为绑定版本契约 `harness_refs` 中每条**当时存在**的发布者记忆固定 revision（`claim_harness_revisions(claim_id, memory_id, revision)`）；已删除的记忆不固定（读取行为与 1.0 一致：`HARNESS_REF_NOT_FOUND`）。
- `work_harness(code, ref_id, claim_id?)`：带 `claim_id`、或调用方持有该任务的 active Claim 时，返回固定的 revision——即使发布者此后更新或撤回该记忆（§9 "input versions bound by an engagement" 对承接者恒可读）。不带 Claim 时行为同 1.0：实时读取当前内容（浏览用途）。
- 无 Claim 的提交在 Submission 上记录提交时刻各 harness 的 revision（`harness_json`）。
- 迁移前已存在的 Claim 没有固定记录，保持 1.0 的实时读取行为（向后兼容）。

## 3. 承接事实（§7.3）

- `claim.required = false` 时，无 Claim 的提交在同一事务中先建立一条**承接事实**再受理交付：复用 Claim 行、创建即 `used`（金额 = 受理时单价，预留规则不变），并同样记录 `contract_version` 与 harness 固定。
- 外部接口不变：`work_submit` 的输入输出、错误目录、状态机均与 1.0 相同。
- 账本恒等式不变：承接事实不是 active Claim，不参与 `reserved = Σ active Claim.amount + Σ inflight Submission.amount`；每个交付恰好一条结算记录的规则照旧。

## 4. 轮次融合（§8）

- `todo_list` 增加第四类事项：该 Agent 每个 **active Claim** 一项（kind = `deliver`，对象 = 任务 code + claim id，`due_at` = `expires_at`，`next_action` = `submit`）。与既有三类共用同一排序（产生时间）、同一游标与分页；Claim 转 `used` / `released` / `expired` 时同步消失。
- Task 1.0 由接收端判定，没有 Agent 侧的判定义务，不加 judge 项。`thread_list.open_items` 不变（只计线程内事项）。房间作用域（`thread_id > 0`）的 todo 不含任务事项。
- 投影不携带发布者身份（1.0 §10 第 7 条）。

## 5. 兼容性

- **既有数据**：迁移 031 为每个存量任务写入 version 1（= 迁移时的当前契约），存量 Claim 与 Submission 一律绑定 version 1——升级前承接的任务在升级后行为不变（升级演练见 `internal/service/task_version_test.go`）。
- **既有调用**：所有新字段（`contract_version`、`work_harness` 的 `claim_id`、todo 的任务事项）均为增量；不传 `claim_id` 的 `work_harness`、忽略 `contract_version` 的调用方行为与 1.0 一致。唯一行为变化是 1.0 §4 中「保存后立即对之后的每个提交生效（含既有 active Claim 携带的提交）」一句被 §1 的版本绑定取代——这是 1.1 的目的本身。
- **仍不满足**：限定受众（§7.2）与工作机会发现（§8 opportunities）不属于 1.1，见 `conformance.md`。

## 6. 迁移

| 编号 | 内容 |
|---|---|
| 031 | `tb_task.contract_version`、`task_contract_versions`、Claim/Submission 的 `contract_version` 及其外键；存量回填 version 1 |
| 032 | `claim_harness_revisions`、`tb_task_submission.harness_json` |
