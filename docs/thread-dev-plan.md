# Thread 开发计划

依据：`docs/thread-prd.md`

目标：把 Thread 协作模型落到现有 Kungfu 架构，保持 Memory、Task、Account 现有合同稳定，并让 Agent 在任一步都能明确当前状态与下一步。

## 1. 执行顺序

~~~text
T0 迁移与兼容
 ↓
T1 Memory 兼容演进
 ↓
T2 Role / Link / Thread 内核
 ↓
T3 Reply / Receipt / Todo 状态机
 ↓
T4 Agent 工作上下文
 ↓
T5 API / MCP
 ↓
T6 Product Surface
 ↓
T7 全量验收
~~~

执行使用一个 feature branch，按 T0 → T7 顺序推进。每个工作单通过验收后再进入下一单，最终只开一个实现 PR。

旧 `feat/thread-collaboration-v01` 仅供查实现经验。

当前事实源：

~~~text
Role        = tb_bots
Memory      = tb_kungfus + memory_revisions
Task / Work = 现有 Task 1.0
Thread      = threads + thread_roles + thread_memories
Todo        = pending thread_receipts 的产品投影
Link        = role_links
~~~

## 2. 全局实现约束

### 2.1 事务

以下动作必须在单事务内完成：

- Memory 更新与旧 revision 归档；
- Thread create；
- Reply；
- Branch；
- Handle；
- participant add/remove/permission change；
- close/reopen；
- join；
- join key reset/revoke；
- idempotency result 持久化。

Repository 方法继续接受 `pg.Querier`，事务归 service 所有。

### 2.2 稳定引用

协作挂载点统一使用 ThreadMemory entry：

~~~text
thread_memories.id = entry id
~~~

以下字段都引用 entry：

- `anchor_entry_id`
- `entry_id`
- `join_entry_id`
- `reply_to_entry_id`
- `input_entry_id`

Memory id 只表示信息对象，entry id 表示该信息在某个协作结构中的确定位置。

### 2.3 默认行为

正常路径使用固定默认值：

~~~text
Start
→ creator = manage
→ participant = write
→ entry = root entry

Join
→ input = join key
→ permission = write
→ entry = key-bound entry

Reply from Todo
→ 自动携带 input ref
→ 自动处理该 Todo

Branch from Todo
→ 自动携带 anchor + input ref
→ 自动处理该 Todo

Handle
→ 用于不需要 Reply / Branch 的输入
~~~

participant add 默认：

- target 来自 active Link；
- permission = write；
- entry 使用当前工作上下文给出的 entry。

高级权限、key 管理、close/reopen 放在管理动作中。

### 2.4 Agent 方向感

每个 Agent-facing 响应都要明确：

~~~text
where   当前协作与作用域
why     为什么当前 Role 在这里
what    当前需要处理的输入
next    当前允许的动作
effect  动作会改变什么
~~~

响应需要满足：

- 新进程只凭当前响应即可继续；
- 空 Todo 明确返回当前没有待处理输入；
- stale input 明确返回当前状态与重新读取入口；
- Join 后立即给出 entry 与当前动作；
- Reply / Branch / Handle 后返回写入结果与新的协作状态；
- closed / removed / permission changed 返回当前状态和可继续动作。

Agent-facing 返回不暴露 ThreadReceipt、seq、revision 等内部结构。

## 3. T0 — 迁移与兼容

### 目标

证明新结构可以安全进入当前数据库。

### 迁移

~~~text
023_memory_revision_origin.sql
024_thread_core.sql
025_thread_receipt_idempotency.sql
~~~

#### 023

`tb_kungfus` 增加：

~~~text
revision BIGINT NOT NULL
origin VARCHAR(...) NOT NULL
~~~

新增：

~~~text
memory_revisions
- memory_id
- revision
- title
- tags
- description
- content
- checksum
- created_at
PRIMARY KEY (memory_id, revision)
~~~

现有 Memory 回填：

~~~text
origin = standalone
revision = 1
~~~

现有正文不复制到 history。

#### 024

新增：

~~~text
role_links
threads
thread_roles
thread_memories
~~~

建立：

- Role pair 唯一约束；
- Thread code 唯一约束；
- parent / anchor / entry 外键与 scope 校验基础；
- `(thread_id, role_id)` 唯一；
- `(thread_id, seq)` 唯一；
- join key hash 索引。

#### 025

新增：

~~~text
thread_receipts
thread_idempotency
~~~

建立：

~~~text
UNIQUE(thread_id, input_entry_id, role_id)
UNIQUE(role_id, operation, idempotency_key)
~~~

以及 pending 查询索引。

### 验收

同时跑两条路径：

~~~text
fresh:
001 → ... → 025

upgrade:
001 → ... → 022
→ seed Memory + Task
→ snapshot counts / checksums / Task contracts
→ 023 → 024 → 025
→ compare snapshot
~~~

完成条件：

- fresh migration 全绿；
- upgrade rehearsal 全绿；
- 现有 Memory / Task 事实保持；
- CI 增加 upgrade rehearsal。

T0 未通过时停止后续实现。

## 4. T1 — Memory 兼容演进

### 目标

让 standalone Memory、Task live harness、Thread pinned history 同时成立。

### 实现

当前 Memory 行继续保存在 `tb_kungfus`。

读取 revision：

~~~text
requested revision == current revision
→ tb_kungfus

requested revision < current revision
→ memory_revisions
~~~

更新：

~~~text
lock current
→ archive current revision
→ update current fields
→ revision + 1
→ commit
~~~

Thread 产生 Memory：

~~~text
origin = thread
revision = 1
content required
title / tags / description optional
credential scan
checksum
~~~

首写只保存 current row，不额外写 history。

standalone `memory_list` 默认过滤：

~~~text
origin = standalone
~~~

Task harness 继续读取 current row。

Thread 写入走独立内部 persistence primitive，不调用 `consumption.ActionStorageCreate`。

### 主要改动

~~~text
internal/model/kungfu.go
internal/repository/kungfu.go
internal/service/kungfu_manage.go
internal/service/kungfu_read.go
Memory integration tests
Task harness regression tests
~~~

### 验收

- 现有 `memory_*` 合同保持；
- Task harness 继续读取最新 Memory；
- thread-origin Memory 不进入默认 Store；
- 首次 Thread Memory 正文只存一份；
- Memory 更新后，旧 pinned revision 可读取；
- soft delete 不破坏已存在 Thread history。

T1 未通过时停止 Thread 内核实现。

## 5. T2 — Role / Link / Thread 内核

### 目标

建立协作的持久结构与权限基础。

### Role

直接使用：

~~~text
role_id   = tb_bots.id
role_name = tb_bots.bot_name
~~~

提供 exact lookup by bot_name，返回稳定 public ref 与 name。

### Link

~~~text
role_links
- role_low_id
- role_high_id
- requested_by_role_id
- status: pending | active
- created_at
- accepted_at?
UNIQUE(role_low_id, role_high_id)
~~~

写入前 canonicalize pair。

生命周期：

~~~text
request → pending → accept → active
pending → decline / cancel
active  → remove
~~~

权限：

- target accept / decline；
- requester cancel；
- active 任一方 remove。

查询返回：

- active；
- incoming pending；
- outgoing pending。

### Thread

~~~text
threads
- id
- code
- join_key_hash?
- join_entry_id?
- subject
- created_by_role_id
- parent_thread_id?
- anchor_entry_id?
- status: open | closed
- next_seq
- revision
- created_at
- updated_at
~~~

Root：

~~~text
parent_thread_id = null
anchor_entry_id = null
~~~

Child：

~~~text
parent_thread_id = direct parent
anchor_entry_id = direct parent ThreadMemory entry
~~~

### ThreadRole

~~~text
thread_roles
- thread_id
- role_id
- permission: read | write | manage
- joined_by_role_id?
- entry_id
- joined_at
UNIQUE(thread_id, role_id)
~~~

Root creator 拥有 tree-wide read + govern。

govern 能力：

- participant；
- key；
- subject；
- status；
- tree。

协作写入由当前 Thread 的 write/manage ThreadRole 授权。

权限变化：

~~~text
write/manage → read
→ 当前 pending receipts withdrawn

read → write/manage
→ 指定新 entry
→ 创建一个 entry receipt

write ↔ manage
→ 保留 pending
~~~

### ThreadMemory

~~~text
thread_memories
- id
- thread_id
- memory_id
- memory_revision
- seq
- author_role_id
- reply_to_entry_id?
- created_at
UNIQUE(thread_id, seq)
~~~

reply target 范围：

- 当前 Thread entry；
- 当前 Child 的 direct anchor entry。

### Join key

- 随机不可预测；
- 数据库只保存 hash；
- raw key 在 create/reset 成功时披露一次；
- join error 使用统一错误；
- join 受 rate limit；
- existing participant 重复 join 保持原 permission；
- participant removal 同事务 revoke 当前 key；
- 后续需要加入时显式 reset。

### 并发验收

- A/B 同时互相 request 只形成一个 Link；
- 同 Thread 并发 append 获得不同 seq；
- Role 并发加入只形成一个 ThreadRole；
- Child anchor 精确定位 parent entry + revision；
- 20 层 lineage 正确；
- Root govern 不产生 descendant participant/Todo；
- Parent close/remove 保持既有 Child 独立状态。

T2 通过后进入状态机。

## 6. T3 — Reply / Receipt / Todo 状态机

### ThreadReceipt

~~~text
thread_receipts
- thread_id
- input_entry_id
- role_id
- reason: entry | reply
- state: pending | handled | withdrawn
- created_at
- handled_at?
- withdrawn_at?
UNIQUE(thread_id, input_entry_id, role_id)
~~~

Todo 查询：

~~~text
open Thread
+ current Role write/manage
+ pending receipt
→ Todo
~~~

### Idempotency

~~~text
thread_idempotency
- role_id
- operation
- idempotency_key
- request_hash
- result_ref
- created_at
UNIQUE(role_id, operation, idempotency_key)
~~~

规则：

- 相同 key + 相同 request hash → 原结果；
- 相同 key + 不同 hash → `IDEMPOTENCY_CONFLICT`。

join-key issue/reset 重放返回：

~~~text
already_applied
fingerprint
~~~

raw key 只在首次成功响应披露。首次响应丢失时再次 reset。

### Create

单事务：

~~~text
create Root Thread
→ create root Memory
→ append root entry seq=1
→ creator manage ThreadRole
→ initial ThreadRoles
→ write/manage initial receipts
→ persist idempotency
→ commit
~~~

creator 不获得自己的 root Todo。

### Reply

输入：

~~~text
thread
reply_to_entry
input_ref?       # Todo 路径
content
idempotency_key
~~~

单事务：

~~~text
lock Thread
→ validate open/write/target
→ if input_ref: CAS pending receipt
→ create thread-origin Memory
→ allocate seq
→ append ThreadMemory
→ create target author's receipt when eligible
→ revision + 1
→ persist idempotency
→ commit
~~~

`input_ref` 已 handled/withdrawn 时返回 stale input，不产生新写入。

无 `input_ref` 表示主动回复历史 entry，不处理 Todo。

### Branch

输入：

~~~text
thread
anchor_entry
input_ref?
subject
initial_roles?
idempotency_key
~~~

单事务：

~~~text
validate parent + anchor
→ if input_ref: CAS pending receipt
→ create Child
→ actor = Child manage participant
→ add other participants
→ create their entry receipts
→ parent revision + 1
→ persist idempotency
→ commit
~~~

actor 不获得自己的 Child entry Todo。

### Handle

输入：

~~~text
thread
input_ref
idempotency_key
~~~

动作：

~~~text
pending → handled
~~~

只处理当前 Role 的指定 receipt。

### Participant / Status

remove：

~~~text
withdraw participant pending
→ remove membership
→ revoke current join key if present
→ commit
~~~

close：

~~~text
status = closed
→ current pending receipts = withdrawn
→ commit
~~~

reopen 保留历史，不恢复旧 pending。

Parent close/remove 保持既有 Child 生命周期。

### 并发验收

同一 input：

- handle vs reply；
- reply vs branch；
- duplicate reply retry。

每组只有一个 Todo-consuming 动作成功。

同时验证：

- stale action 不产生 Memory / ThreadMemory；
- 一个 Role 可同时持有多个 pending；
- 多 Agent reply 同一点形成独立 receipts；
- read Role 无 Todo；
- permission downgrade 撤销 pending；
- Branch 建立独立 Child Todo。

最小闭环：

~~~text
A Link B
→ A Start
→ B Todo
→ B Reply
→ A Todo
→ A Handle
→ no pending
~~~

T3 未通过时不开放 API/MCP。

## 7. T4 — Agent 工作上下文

### 目标

把持久状态转换成 Agent 可直接继续执行的工作上下文。

### Todo projection

按 Thread 聚合：

~~~text
thread
subject
pending_count
latest_pending_at
pending_input_refs[]
lineage_hint
~~~

### Thread work context

~~~text
where
- thread ref
- subject
- status

why
- role
- entry
- reason

what
- pending inputs
- source role
- content / memory ref
- created_at

context
- relevant memory refs
- lineage
- direct children
- participants
- continuation

next
- allowed actions
- target refs
- required params

effect
- each action's state effect
~~~

### 分页与边界

固定：

- subject max length；
- message max bytes；
- participant batch max；
- pending page size；
- timeline page size；
- tree page size；
- maximum page size；
- write rate limits。

lineage 可递归延伸，每次返回有界。

### Agent 不迷路验收

对 Todo、Join、Reply、Branch、Handle 分别执行无本地状态测试：

1. 启动新客户端；
2. 只提供当前响应；
3. 客户端能判断 where / why / what / next / effect；
4. 执行动作；
5. 下一响应继续提供完整状态。

必须覆盖：

- 空 Todo；
- Join 后首次进入；
- stale input；
- closed；
- removed；
- permission changed；
- 多 pending；
- 20 层 Child；
- 1,000+ ThreadMemory。

T4 通过后冻结上下文合同。

## 8. T5 — API / MCP

### 架构

继续使用现有单一 registry：

~~~text
ToolDef
→ handler
→ service
→ MCP /mcp
→ POST /api/v1/<tool>
~~~

Thread/Link 能力注册一次，两个 transport 共用同一 service 事实。

### 能力范围

- Todo discovery；
- Thread work context；
- Start；
- Join；
- Reply；
- Branch；
- Handle；
- Link lifecycle；
- participant management；
- permission；
- close/reopen；
- key reset；
- timeline；
- lineage；
- direct children。

第一版使用 durable pull。实时 signal 留作后续 wake-up 优化。

### 命名冻结

T4 通过后只做一次 tool naming freeze。

名称要求：

- 与实际动作一致；
- 输入副作用明确；
- tool description 写明调用时机；
- 返回携带 next-action guidance。

### 双端验收

使用两个不同客户端：

~~~text
API client A
MCP client B

A Start
→ B Todo
→ B Reply
→ A Todo
→ A Branch / Handle
~~~

验证：

- 状态一致；
- 权限一致；
-错误语义一致；
- next-action guidance 一致。

同时回归：

~~~text
account_*
memory_*
work_*
task_*
~~~

T5 通过后进入 Web/Product Surface。

## 9. T6 — Product Surface

一级优先级：

~~~text
1. Todo
2. Link
3. Start
4. Join
5. Work
6. Hire
7. Store
8. Retrieve
~~~

Thread 内动作：

~~~text
Reply
Branch
Handle
Participants
Close / Reopen
Join key
~~~

### 正常路径

linked：

~~~text
Start
→ target Todo
~~~

non-linked：

~~~text
Start
→ join path
→ target Join
→ target Todo
~~~

Todo：

~~~text
open
→ Reply / Branch / Handle
~~~

Link：

~~~text
request
→ peer accept
~~~

Web 继续使用 Owner session 与 unified tool bridge。

### 验收

- Todo 首页可直接判断当前输入；
- Todo 打开后直接执行 Reply / Branch / Handle；
- linked Start 一次产生对方 Todo；
- non-linked Start 同一意图返回 join path；
- Join 只输入 key；
- participant add 默认隐藏 permission/entry 配置；
- Link request/accept/decline/cancel/remove 完整；
- Store 默认列表无 thread-origin Memory；
- Work/Hire 保持现有流程；
- 页面一级入口不使用底层数据表结构组织。

## 10. T7 — 全量验收

### 数据与兼容

- fresh migrations；
- upgrade rehearsal；
- Memory backward compatibility；
- Task 1.0 full regression；
- Task harness live current；
- Thread pinned revision；
- thread-origin Memory 隔离。

### 状态机

- Todo race；
- duplicate retry；
- stale input；
- permission transition；
- close/reopen；
- participant remove；
- key revoke/reset/join；
- deep lineage；
- large Thread。

### 协议

- API / MCP parity；
- Agent 无本地状态恢复；
- error + next action；
- rate limit；
- join-key enumeration protection。

### Product Surface

- Todo；
- Link；
- Start；
- Join；
- Work；
- Hire；
- Store；
- Retrieve；
- Owner Web。

### 工程检查

~~~text
gofmt -l .
go vet ./...
go test -p 1 ./...
go build ./...
fresh migration chain
upgrade rehearsal
container build
healthz / readyz / SIGTERM smoke
~~~

全部通过后更新：

~~~text
README
llms.txt
kungfu_skill.md
MCP descriptions
HTTP contract
release notes
~~~

## 11. 可行性结论

| 部分 | 判断 | 关键验证 |
| --- | --- | --- |
| Memory revision | 可实施 | T0 / T1 migration + revision resolver |
| Thread kernel | 可实施 | T2 schema / seq / lineage / permission |
| Todo state machine | 可实施，风险最高 | T3 PostgreSQL concurrency tests |
| Agent context | 可实施 | T4 no-local-state tests |
| API / MCP | 可实施 | T5 shared registry parity |
| Product Surface | 可实施 | T6 normal-path friction |
| Task coexistence | 可实施 | T1 / T7 regression |
| Realtime | 第一版无需依赖 | durable pull 已覆盖协作闭环 |

实施重点：

~~~text
T0 兼容迁移
T3 状态机并发
T4 Agent 不迷路
T6 正常路径摩擦
~~~

这四项通过后，Thread 具备发布条件。
