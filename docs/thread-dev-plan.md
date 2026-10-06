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
WO-T4 Agent Context Envelope
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

## WO-T1 Memory 稳定引用

### 目标

让 Memory 同时满足：

- 可独立存储；
- 可独立读取；
- 可独立分享；
- 可进入多个 Thread；
- Thread 内一次已经发生的表达保持稳定语义。

### 契约

ThreadMemory 引用：

~~~text
memory_id
memory_revision
~~~

Memory 的独立更新产生新的可寻址版本。

Thread 读取固定 revision。

Thread 产生新表达时：

~~~text
create Memory revision
→ append ThreadMemory
→ pin revision
~~~

### 验收

- standalone Memory 正常存储与分享；
- 一个 Memory revision 可以被多个 Thread 引用；
- Memory 后续产生新 revision 后，已有 Thread 表达保持原语义；
- Thread reply 创建 Memory 与 ThreadMemory 在同一事务完成；
- 大体量 Memory 可以通过 ref 按需读取。

## WO-T2 Link / Thread 数据内核

### RoleLink

~~~text
role_links
- role_a_id
- role_b_id
- status
- created_at
~~~

约束：

- Role pair 唯一；
- active Link 表达双方直接协作信任。

能力：

- 直接建立双人 Thread；
- manage Role 将已 Link Role 直接加入 Thread。

### threads

~~~text
id / code
join_key_hash
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
- Root creator 在 descendants 中保持 manage 能力。

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
- key reset 原子替换；
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
6. 为 initial Roles 创建 entry receipt；
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
7. 当前 Role 针对 parent input 的 pending receipt 收敛为 handled；
8. parent author 当前仍在 Thread 时，为其创建 reply receipt；
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
4. 建立 Child entry receipts；
5. 当前 Role 针对 anchor 的 pending receipt 收敛为 handled；
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

- 当前 Role 的指定 pending receipt 原子变为 handled；
- 保存 handled_at；
- 作为一轮协作自然结束。

### thread_todos

查询当前 Role 的 pending receipts，按 Thread 聚合：

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
- Todo 表达始终按 Thread 聚合；
- Task 模型不会参与 Thread Todo 状态。

## WO-T4 Agent Context Envelope

### thread_open

thread_open 是 Agent 进入协作事项的主要入口。

返回：

~~~text
schema_version

identity
- current_role
- thread_code
- permission

matter
- subject
- status
- creator
- participants

position
- root
- parent
- anchor
- lineage

todos
- current Role pending inputs
- source author
- reason
- reply target

context
- anchor refs
- lineage anchor refs
- relevant Timeline index
- direct Child summaries
- pagination refs

capabilities
- reply
- branch
- handle
- add_role
- remove_role
- reset_key
- close

writeback
- valid reply targets
- valid branch anchors

agent_guidance
- collaboration scope
- actionable inputs
- writeback contract
- available next actions
~~~

### Guidance contract

agent_guidance：

- 由 Kungfu 根据当前协议状态确定性生成；
- 使用固定 schema/version；
- 与 Memory content 分区；
- 明确当前 Role、Thread、Todo 与可执行动作。

### 按需展开

默认直接提供：

- 当前 pending input；
- 当前 anchor；
- lineage anchor；
- 当前 Thread 的结构索引。

大体量 Memory 通过 memory_get(ref) 展开。

Timeline 使用 cursor 分页。

### 会话隔离

每次 Agent 操作显式携带 thread_code。

thread_open(T1) 与 thread_open(T2) 形成两个独立工作上下文。

写入校验：

- reply target 属于当前 Thread 范围；
- branch anchor 属于当前 Thread 范围；
- Role 权限来自当前 ThreadRole。

### 验收

- Agent 只凭 thread_open 返回即可知道当前事项、来源、Todo 与写回位置；
- 深层 Child 直接返回 lineage；
- 1000+ Memory Thread 仍可先读结构、再按需展开；
- Memory 内自然语言不会进入 agent_guidance 控制区；
- 切换 Thread 后所有 action 继续显式绑定目标 Thread。

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
thread_todos
→ thread_open
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
- 建立 entry receipt；
- target 的 thread_todos 立即出现该事项。

#### Key join

~~~text
thread_join(key)
~~~

成功：

- resolve Thread；
- 建立 ThreadRole；
- 建立 entry；
- 返回 Thread Context Envelope。

#### Key reset

~~~text
thread_key_reset(thread)
~~~

更新 join_key_hash 并推进 revision。

### 对外操作面

高频：

~~~text
thread_todos
thread_open
thread_reply
thread_branch
thread_handle
~~~

协作建立：

~~~text
thread_create
thread_join
thread_role_add
thread_role_remove
~~~

信任与寻址：

~~~text
role_find
role_link
role_unlink
thread_key_reset
~~~

结构读取：

~~~text
memory_get
thread_lineage
thread_tree
thread_timeline
~~~

MCP / HTTP 映射同一 service 语义。

### 幂等

以下写操作接受 idempotency key：

- thread_create；
- thread_reply；
- thread_branch；
- thread_handle；
- thread_role_add；
- thread_join。

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

- D 可读完整 Thread；
- entry=M87；
- D 只产生一个明确 entry Todo；
- thread_open 直接把 M87 作为 actionable input。

### 深层分叉

至少构造 20 层 Child。

验证：

- lineage 正确；
- Agent 无需遍历 siblings；
- 每个 Child 保持自己的 Roles / Todo / Timeline；
- Root creator 可获得完整 tree。

### 大信息量

单 Thread：

- 1,000+ ThreadMemory；
- 多个大体量 Memory；
- 多个 Child。

验证：

- thread_open 首包保持可控；
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
thread_todos
→ thread_open
→ 正确继续
~~~

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
发现 Todo
→ 打开事项
→ 理解 lineage
→ 按需读 Memory
→ 本地执行
→ reply / branch / handle
→ 自动形成下一轮协作
~~~

产品文档、MCP 描述、HTTP contract、README 与版本说明都从最终实现契约生成。
