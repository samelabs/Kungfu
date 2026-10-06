# Thread 开发计划

依据：docs/thread-prd.md。

目标：实现一套 Agent-first 协作协议，让任意外部 Agent 通过 API / MCP 进入 Thread、读取结构化上下文、本地执行、写回结果，并由协作关系自动形成下一轮 Todo。

## 1. 实现主线

~~~text
WO-T1 Memory 稳定引用
   ↓
WO-T2 Link / Thread 数据内核
   ↓
WO-T3 Reply / Receipt / Todo
   ↓
WO-T4 Agent 工作上下文合同
   ↓
WO-T5 Realtime / Protocol
   ↓
WO-T6 场景与鲁棒性验收
~~~

实现围绕以下事实源展开：

~~~text
Role
Memory
Thread
RoleLink
ThreadRole
ThreadMemory
ThreadReceipt
~~~

Todo 是 pending ThreadReceipt 的产品投影。

现有 Task 继续保持开放雇佣 Agent 执行模型：publisher 发布并开放 Task，executor Agent 通过 work_list/work_get 发现，按合同 claim/submit，平台完成 receiver delivery 与 settlement。Thread 实现不改写 Task 状态机；回归测试必须证明两套机制并行稳定。

## WO-T1 Memory 稳定引用

### 目标

统一 Memory 的独立存储、Task live harness 与 Thread 稳定历史语义。

### Memory revision

新增可寻址 revision：

~~~text
memory_revisions
- memory_id
- revision
- title?
- tags?
- description?
- content
- checksum
- created_at
~~~

Memory 当前对象指向 latest revision。

规则：

- standalone update 创建新 revision；
- memory_get 默认读取 latest；
- Task harness 继续读取 latest；
- ThreadMemory pin 指定 revision；
- standalone soft-delete 后，新的 Task harness 不再取得该 Memory；
- 已经进入 Thread 的 pinned revision 继续作为既有协作历史可读。

### 创建 primitive

Memory 基础创建 primitive 支持：

- owner；
- content 必填；
- title / tags / description 可选；
- credential scan；
- checksum；
- transaction 由调用方控制。

现有 standalone memory_put 的 title / tags / content 长度等产品合同继续保留在其入口层。

Thread root / reply 使用基础 primitive，因此短协作内容不被 standalone Memory 表单约束。

### 验收

- standalone memory_list/get/put/share/unshare/delete 行为保持；
- Task harness_refs 始终读取 Memory latest revision；
- Thread pin revision 后不随 standalone update 改义；
- standalone soft-delete 不破坏既有 Thread history；
- Thread reply 创建 Memory revision 与 ThreadMemory 同事务；
- 一个 Memory revision 可以进入多个 Thread。

## WO-T2 Link / Thread 数据内核

### RoleLink

~~~text
role_links
- requester_role_id
- target_role_id
- status: pending | active
- created_at
- accepted_at?
~~~

生命周期：

~~~text
request → pending → accept → active
pending → decline / cancel
active → remove
~~~

只有 active Link 允许双方直接建立 Thread participation。

Link 不自动产生 ThreadRole 或 Todo。

### threads

~~~text
id / code
join_key_hash
join_entry_memory_id?
subject
created_by_role_id
parent_thread_id?
anchor_memory_id?
status
next_seq
revision
created_at / updated_at
~~~

结构约束：

- Root 有 subject 与 root Memory；
- Child 有直接 parent 与 anchor；
- anchor 属于直接 parent 的协作范围；
- parent / anchor 创建后保持稳定；
- code 唯一稳定；
- join key 可重置；
- Child 可继续派生 Child。

### thread_roles

~~~text
thread_id
role_id
permission
joined_by_role_id?
entry_memory_id
joined_at
~~~

约束：

- (thread, role) 唯一；
- 每个 ThreadRole 有 entry_memory_id；
- Root creator 通过 root ownership 获得 descendants 的继承式 read/manage；
- 继承治理权不自动创建 descendant ThreadRole。

### thread_memories

~~~text
id
thread_id
memory_id
memory_revision
seq
author_role_id
reply_to_entry_id?
created_at
~~~

约束：

- (thread, seq) 唯一；
- seq 单调递增；
- reply_to_entry_id 指向当前 Thread 可回应的 entry；
- 普通 Reply 保持在当前 Thread。

### repository primitives

至少覆盖：

- find / lock Thread；
- allocate seq；
- increment revision；
- create / update RoleLink；
- create / remove ThreadRole；
- append ThreadMemory；
- resolve parent / root / lineage；
- direct children query；
- key lookup / reset；
- scoped Memory read。

### 并发验收

- 同 Thread 并发 append 获得不同 seq；
- Link 并发创建保持单一关系；
- Role 并发加入保持单一 ThreadRole；
- key reset 原子替换 key + join entry；
- key join 建立 write ThreadRole 并使用绑定 entry；
- active Link 才允许 direct add；
- Root governance 不污染 descendant participant / Todo；
- Tree lineage 在深层递归下保持稳定。

## WO-T3 Reply / Receipt / Todo

### thread_receipts

~~~text
thread_id
input_entry_id
role_id
reason
state
created_at
handled_at?
~~~

reason：

~~~text
entry
reply
~~~

state：

~~~text
pending
handled
withdrawn
~~~

唯一约束：

~~~text
(thread_id, input_entry_id, role_id)
~~~

### thread_create

单事务：

1. 创建 Root Thread；
2. 创建 root Memory revision；
3. append root ThreadMemory seq=1；
4. creator 建立 manage ThreadRole；
5. initial Roles 建立 ThreadRole，entry=root Memory；
6. 仅为 write / manage initial Roles 创建 entry receipt；
7. revision 提交。

### thread_reply

输入：

~~~text
thread
reply_to_entry
content
idempotency_key
~~~

单事务：

1. lock Thread；
2. 验 open + write；
3. 验 reply target；
4. create Memory revision；
5. allocate seq；
6. append ThreadMemory with reply_to；
7. 当前 Role 只把 reply_to 对应的 pending receipt 收敛为 handled；
8. parent author 当前为 write/manage participant 且不是当前 Role 时，为其创建 reply receipt；
9. revision + 1；
10. commit。

### thread_branch

输入：

~~~text
thread
anchor_entry
subject
initial_roles
idempotency_key
~~~

单事务：

1. 验 parent + anchor；
2. 创建 Child；
3. 建立 Child ThreadRole；
4. 仅为 Child write/manage participants 建立 entry receipts；
5. 当前 Role 只把 anchor 对应的 pending receipt 收敛为 handled；
6. parent revision + 1；
7. commit。

### thread_handle

输入：

~~~text
thread
input_entry
idempotency_key
~~~

效果：

- 仅允许处理当前 Role 自己的指定 pending receipt；
- pending → handled；
- 保存 handled_at；
- 不影响同 Thread 的其他 pending inputs。

### ThreadReceipt / Todo projection

查询 open Thread 中当前 Role 的 pending receipts，且 Role 当前 permission 为 write / manage，再按 Thread 聚合：

~~~text
thread
subject
pending_count
latest_pending_at
pending_input_refs[]
lineage_hint
~~~

### 验收

- Root 首轮对所有 initial Roles 自动产生 Todo；
- 多人并行 reply 同一 root Memory；
- 一个 Role 在同一 Thread 同时持有多个 pending inputs；
- reply 自动完成当前输入并把下一轮传回；
- branch 自动完成当前输入并建立 Child 首轮 Todo；
- handle 结束当前输入；
- read Role 不产生 Todo；
- role removal 把该 Role 的 pending receipts 转为 withdrawn；
- Thread close 把当前 pending receipts 转为 withdrawn；reopen 不恢复；
- Todo 表达始终按 Thread 聚合；
- Task 的 open/claim/submission/delivery/settlement 生命周期与 Thread Todo 独立；
- Thread Todo 不进入 work_list，不占用 Task claim，不参与 Task settlement。

## WO-T4 Agent 工作上下文合同

### 目标

先实现 Agent 使用 Thread 所必需的功能合同，不在本工作单确定最终 MCP tool 名称、HTTP route 或前端术语。

### Pending discovery

提供当前 Role 的协作待处理投影：

- 按 Thread 聚合；
- pending_count；
- pending input stable refs；
- subject / status；
- latest pending time；
- lineage hint。

只包含当前 open Thread 中 write/manage Role 的 pending receipts。

### Thread work context

读取一个 Thread 时返回：

~~~text
thread identity
subject / status

current role
effective permission
why_here

pending inputs
- stable input ref
- source role
- content or memory ref
- reason
- created_at

context
- relevant memory refs
- lineage summary
- direct child summaries
- continuation / cursor

participants

allowed actions
- action type
- valid target ref
- required parameters
~~~

### 按需展开

- 当前 pending 与进入原因优先；
- 大体量 Memory 按 ref 读取；
- Timeline cursor 分页；
- lineage 返回必要路径；
- sibling / full tree 按需读取。

### 会话隔离

每次读写显式绑定目标 Thread。

服务返回稳定 input / memory / role / continuation ref，Agent 后续直接回传这些 ref。

内部 service 负责解析为 ThreadMemory / ThreadReceipt / Memory revision，并执行权限校验。

### 可信边界

Kungfu 生成的结构、权限、allowed actions 与导航信息是协议事实。

Memory content 是参与者协作数据。

两者在返回结构中明确分区。

### 验收

- Agent 能发现当前等待自己的协作；
- 读取 Thread 后知道事项、进入原因、pending、上下文和允许动作；
- 1000+ Memory Thread 首包仍保持有限工作集；
- 深层 Child 无需遍历 sibling 即可理解 lineage；
- Agent 不需要理解 ThreadReceipt、seq、revision 才能正确写回；
- 最终外部命名可以在不改变本功能合同的情况下独立设计。

### 表达基线

一级入口按优先级固定为：

~~~text
Todo
Link
Start
Join
Work
Hire
Store
Retrieve
~~~

映射关系：

- Todo / Start / Join / Link：Thread / Link 协作能力；
- Work：Task executor side；
- Hire：Task publisher side；
- Store / Retrieve：Memory 存取能力。

Reply / Branch / Handle 等依赖具体 Thread 上下文的动作，在当前协作内出现，不作为一级入口。

Memory / Thread 保持底层抽象，不作为一级导航名称。

## WO-T5 Realtime / Protocol

### change signal

~~~text
thread_changed
- thread_code
- revision
- head_seq
~~~

durable transaction commit 后发送。

### Todo 恢复

Todo 由 ThreadReceipt 持久化。

Agent 重连：

~~~text
query pending projection
→ select Thread
→ read current Thread work context
→ continue
~~~

不依赖实时信号历史。

### Timeline 恢复

thread_timeline 支持：

~~~text
after_seq
limit
~~~

revision 用于识别 Thread 结构变化。

### Role 加入

#### Link direct add

~~~text
thread_role_add(thread, role, entry_memory)
~~~

条件：

- caller 有 manage；
- caller 与 target Role 有 active Link；
- entry_memory 属于 Thread 协作范围。

成功：

- 建立 ThreadRole；
- permission=write/manage 时建立 entry receipt；
- pending projection 随 durable state 更新。

#### Key join

~~~text
thread_join(key)
~~~

成功：

- resolve open Thread；
- 建立 write ThreadRole；
- entry 使用 key 绑定的 join entry；
- 建立 entry receipt；
- 返回当前 Thread work context。

#### Key reset

~~~text
thread_key_reset(thread)
~~~

原子更新 join_key_hash + join_entry_memory_id 并推进 revision。

### Protocol 映射

MCP / HTTP 最终命名在功能 PRD 审核通过后确定。

协议层必须覆盖：

- pending discovery；
- Thread work context；
- create / reply / branch / handle；
- join / participant management / key reset；
- lineage / tree / timeline expansion；
- realtime change signal。

所有 transport 共享同一 service 事实。

### 幂等

以下内部写操作接受 idempotency key：

- thread_create；
- thread_reply；
- thread_branch；
- thread_handle；
- thread_role_add；
- thread_join。

外部协议将同一个 idempotency key 透传到对应内部操作。

重复请求返回同一业务结果。

## WO-T6 场景与鲁棒性验收

### 双 Agent 邮件式往返

~~~text
A root
→ B Todo
→ B reply
→ A Todo
→ A reply
→ B Todo
~~~

验证 Reply 挂载与 Receipt 转移。

### 多 Agent 首轮

~~~text
A creates T with B/C/D
~~~

B/C/D 并行获得 root Todo。

### 多人回复同一点

B/C/D 同时 reply M1。

验证：

- 仍为一个 Thread；
- 三个独立 reply edges；
- A 的 Todo 按 Thread 聚合为 pending_count=3。

### 中途加入

已有长历史 Thread 在 M87 处加入 D。

验证：

- D 可读授权范围；
- entry=M87；
- write/manage D 只产生一个明确 entry Todo；
- read D 不产生 Todo；
- Thread work context 直接把 M87 作为 actionable input。

### 深层分叉

至少构造 20 层 Child。

验证：

- lineage 正确；
- Agent 无需遍历 siblings；
- 每个 Child 保持自己的 Roles / Todo / Timeline；
- Root creator 可获得完整 tree；
- Root creator 未加入某 Child 时不出现在该 Child participant / Todo。

### 大信息量

单 Thread：

- 1,000+ ThreadMemory；
- 多个大体量 Memory；
- 多个 Child。

验证：

- Thread work context 首包保持可控；
- pending / anchor 优先；
- Memory 按需读取；
- cursor 正常分页。

### 并发

覆盖：

- 同一 Memory 多 Agent 并发 reply；
- 同一 Role handle 与 reply 竞态；
- duplicate write 重试；
- role add / remove 竞态；
- key reset 与 join 竞态。

### 掉线 / 换进程

Agent 离线后收到多个 Reply，再以新进程恢复。

验证：

~~~text
pending projection
→ Thread work context
→ 正确继续
~~~

### Close / Reopen

验证：

- close 把当前 pending receipts 转为 withdrawn；
- closed Thread 停止 reply / branch；
- reopen 保留历史；
- withdrawn receipts 不恢复；
- 新输入重新生成 pending。

### Link / Key

验证：

- Link request / accept 后才 active；
- active Link 才允许 direct add；
- unlink 不移除既有 ThreadRole；
- key reset 同时绑定明确 join entry；
- key join 默认建立 write ThreadRole；
- 重复 join 不重复生成 entry receipt。

### Task 边界回归

验证现有开放雇佣机制保持完整：

- publisher create / open；
- work_list / work_get 对外发现；
- claim / renew / release；
- submit / revise；
- receiver delivery；
- settlement / rejection / failure；
- harness_refs 继续读取 publisher Memory；
- Thread Todo 不进入 Task executor surface；
- Task executor 不因处理开放 Task 自动获得 ThreadRole。

### 异构 Agent

至少以两种不同客户端协议行为验证：

~~~text
API client
MCP client
~~~

二者得到同一 Thread/Receipt/Todo 语义。

### 权限与隔离

验证：

- ThreadRole 决定当前 scope；
- lineage 只暴露协作所需 anchor；
- role removal 更新 scoped access；
- writeback 目标校验当前 Thread；
- Link 与 ThreadRole 生命周期独立。

## 发布判据

完成后，任意外部 Agent 能只依赖 Kungfu 协议完成：

~~~text
发现当前 pending
→ 读取 Thread 工作上下文
→ 理解进入原因与 lineage
→ 按需读 Memory
→ 本地执行
→ reply / branch / handle
→ 自动形成下一轮协作
~~~

产品命名、MCP tool、HTTP contract、前端表达、README 与版本说明都从审核通过的功能合同生成。
