# Thread 开发计划

依据：docs/thread-prd.md。

目标：实现一套 Agent-first 协作协议，让任意外部 Agent 通过 API / MCP 进入 Thread、读取结构化上下文、本地执行、写回结果，并由协作关系自动形成下一轮 Todo。

## 1. 实现主线

~~~text
WO-T1 Memory 兼容演进
   ↓
WO-T2 Role / Link / Thread 数据内核
   ↓
WO-T3 Reply / Receipt / Todo
   ↓
WO-T4 Agent 工作上下文合同
   ↓
WO-T5 Protocol / API / MCP
   ↓
WO-T6 Product Surface
   ↓
WO-T7 场景与鲁棒性验收
~~~

实现围绕以下事实源展开：

~~~text
Role = existing Agent account (tb_bots)
Memory = existing tb_kungfus + revision history
Thread
RoleLink
ThreadRole
ThreadMemory
ThreadReceipt
~~~

不得新增第二套 Role / account 身份表。role_id 统一使用 tb_bots.id，role name 使用 bot_name。

Todo 是 pending ThreadReceipt 的产品投影。

现有 Task 继续保持开放雇佣 Agent 执行模型：publisher 发布并开放 Task，executor Agent 通过 work_list/work_get 发现，按合同 claim/submit，平台完成 receiver delivery 与 settlement。Thread 实现不改写 Task 状态机；回归测试必须证明两套机制并行稳定。

## WO-T1 Memory 兼容演进

### 目标

统一 Memory 的独立存储、Task live harness 与 Thread 稳定历史语义。

### 数据迁移

现有 tb_kungfus 保持 current-row 角色，新增：

~~~text
revision BIGINT NOT NULL
origin VARCHAR(...) NOT NULL  -- standalone | thread
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

迁移：

1. 全量现有 tb_kungfus 回填 origin=standalone、revision=1；
2. 为现有记录生成 revision=1 快照；
3. standalone update 在同一事务更新 current row + append revision；
4. Task harness 查询路径继续读取 current row，不改现有 live semantics；
5. ThreadMemory 保存 memory_id + revision 并读取历史快照。

### 创建 primitive

新增内部 Memory persistence primitive：

- owner = authenticated Role；
- origin；
- content 必填且有明确 size bound；
- title / tags / description 可选；
- credential scan；
- checksum；
- revision=1；
- caller-owned transaction。

standalone memory_put 继续保留现有 title / tags / content 合同，并 origin=standalone。

Thread root / reply 使用 origin=thread；可用空 title、[] tags 的内部表示，不受 standalone 50-character minimum 约束。

Thread-generated Memory 不调用 consumption.ActionStorageCreate。Store 定价与 Thread 写入定价保持两个策略边界。

### 验收

- standalone memory_list 默认只返回 origin=standalone，现有用户结果不被 Thread 消息污染；
- memory_get 已知 code 仍可读取 owner 的 thread-origin Memory；
- standalone memory_get/put/share/unshare/delete 既有字段与错误合同保持；
- Task harness_refs 始终读取 Memory current/latest；
- Thread pin revision 后不随 standalone update 改义；
- standalone soft-delete 不破坏既有 Thread history；
- Thread reply 创建 Memory revision 与 ThreadMemory 同事务；
- 一个 Memory revision 可以进入多个 Thread。

## WO-T2 Role / Link / Thread 数据内核

### Role / Role lookup

Role 直接映射 existing tb_bots。

最小能力：

- exact lookup by bot_name；
- 返回稳定 public ref + bot_name；
- 不暴露 balance、credential、register metadata。

### RoleLink

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

写入前 canonicalize pair，消除 A→B / B→A 并发双行。

生命周期：

~~~text
request → pending → accept → active
pending → decline/cancel → delete current relation
active → remove → delete current relation
~~~

规则：

- requester 不能 accept 自己的 request；
- only target can accept/decline；
- requester can cancel；
- either side can remove active；
- Link list 同时返回 active / incoming pending / outgoing pending；
- Link 不自动产生 ThreadRole 或 Todo。

### threads

~~~text
id / code
join_key_hash
join_entry_entry_id?
subject
created_by_role_id
parent_thread_id?
anchor_entry_id?
status
next_seq
revision
created_at / updated_at
~~~

结构约束：

- Root 有 subject 与 root Memory；
- Child 有直接 parent 与 anchor；
- anchor_entry_id 指向直接 parent 的 ThreadMemory entry；
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
entry_entry_id
joined_at
~~~

约束：

- (thread, role) 唯一；
- 每个 ThreadRole 有 entry_entry_id；该值是确定 ThreadMemory entry，不是裸 Memory id；
- Root creator 通过 root ownership 获得 descendants 的 inherited read + govern；
- inherited govern 允许 participant/key/subject/status/tree 管理，但不允许 reply/branch/handle；
- collaboration write 仍必须有 descendant write/manage ThreadRole。

### permission lifecycle

- write/manage → read：同事务将该 Role 当前 pending receipts → withdrawn；
- read → write/manage：要求新的 entry_entry_id，并创建一条 entry receipt；
- write ↔ manage：不改变 pending。

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
- reply_to_entry_id 只能指向当前 Thread entry 或当前 Child 的 direct anchor_entry_id；
- 普通 Reply 保持在当前 Thread。

### thread_idempotency

~~~text
role_id
operation
idempotency_key
request_hash
result_ref
created_at
UNIQUE(role_id, operation, idempotency_key)
~~~

相同 key + 不同 hash 返回 IDEMPOTENCY_CONFLICT。

### repository primitives

至少覆盖：

- find / lock Thread；
- allocate seq；
- increment revision；
- exact Role lookup；
- create / accept / delete / list RoleLink；
- create / remove ThreadRole；
- append ThreadMemory；
- resolve parent / root / lineage；
- direct children query；
- key lookup / reset / revoke-on-remove；
- exact entry lookup / scope validation；
- scoped Memory read；
- idempotency claim / result persistence。

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
withdrawn_at?
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
input_ref?        # 从 Todo 发起时携带
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
7. input_ref 存在时，对 caller 的该 receipt 执行 pending→handled CAS；非 pending 则 stale-input 且不 append；
8. input_ref 缺省时视为主动回复历史，不处理 Todo；
9. parent author 当前为 write/manage participant 且不是当前 Role 时，为其创建 reply receipt；
10. revision + 1；
11. commit。

### thread_branch

输入：

~~~text
thread
anchor_entry
input_ref?        # 从 Todo 发起时携带
subject
initial_roles
idempotency_key
~~~

单事务：

1. 验 parent + anchor_entry，并锁定 anchor 对应的 Memory revision；
2. 创建 Child，保存 parent_thread_id + anchor_entry_id；
3. actor 建立 Child manage ThreadRole，不生成 actor entry receipt；
4. 建立其他 Child ThreadRole；
5. 仅为其他 Child write/manage participants 建立 entry receipts；
6. input_ref 存在时执行 pending→handled CAS；非 pending 则整个 branch 不创建；
7. input_ref 缺省时为主动 branch，不处理 Todo；
8. parent revision + 1；
9. commit。

### thread_handle

输入：

~~~text
thread
input_entry
idempotency_key
~~~

效果：

- input_ref 必须属于当前 Role；
- 仅允许 pending → handled CAS；
- 已 handled / withdrawn 返回 stale-input；
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
- role removal 把该 Role 的 pending receipts 转为 withdrawn，并在存在 join key 时同事务 rotate key；
- Thread close 把当前 pending receipts 转为 withdrawn；reopen 不恢复；
- Parent close 不级联 close Child；
- Parent participant removal 不级联删除既有 Child membership；
- Todo 表达始终按 Thread 聚合；
- Task 的 open/claim/submission/delivery/settlement 生命周期与 Thread Todo 独立；
- Thread Todo 不进入 work_list，不占用 Task claim，不参与 Task settlement。

## WO-T4 Agent 工作上下文合同

### 目标

先实现 Agent 使用 Thread 所必需的功能合同，不在本工作单确定最终 MCP tool 名称、HTTP route 或前端术语。

### Input bounds

必须在协议层固定：

- subject length；
- Thread message content max bytes；
- participant batch max；
- pending / timeline / tree page size 与最大 page size；
- join / Link / Thread write rate limits；
- lineage 语义不设业务深度上限，但每次响应有 bounded page / continuation；
- parent / anchor cycle 永远拒绝。

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

### Link context

一级 Link 能力返回 active / incoming pending / outgoing pending，并给出当前可执行动作。

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

## WO-T5 Protocol / API / MCP

### 单一 Tool registry

现有架构已经由 mcpserver registry 同时服务：

~~~
MCP /mcp
POST /api/v1/<tool>
~~~

Thread / Link 新能力必须继续注册一次、双 transport 复用同一 handler / service，不新增第二套业务 HTTP 实现。

### Pull-first

第一版正式能力：

- Todo / pending discovery；
- Thread work context；
- create / reply / branch / handle；
- participant / permission / close / reopen；
- Link lifecycle；
- join / key reset；
- timeline / lineage / tree expansion。

现有 MCP 为 stateless Streamable HTTP、无 server push。第一版不依赖 realtime。

### 可选 wake-up

如后续加入 SSE/WebSocket/其他 notification adapter：

~~~
commit durable state
→ emit lightweight change signal
→ client wakes
→ re-read durable state
~~~

signal 不是事实源，不影响第一版验收。

### Key / Link security

- join key 只持久化 hash；
- raw key 只在 create/reset 时一次性返回；
- join error 不泄露 Thread 是否存在；
- join / Link request 受 rate limit；
- key reset 原子替换 key + join entry；
- participant removal 在存在 join key 时同事务 revoke，旧 key 立即失效；
- existing participant 重复 join 不升级 permission；
- role lookup 只暴露最小 public identity。

### 幂等

Thread 写操作：

- create；
- reply；
- branch；
- handle；
- role add / permission change；
- join；
- close / reopen；
- key reset。

普通 Thread 写操作统一使用 thread_idempotency；同 key 不同 request hash → IDEMPOTENCY_CONFLICT。

join-key issue/reset 同样记录 idempotency，但重放只返回 already_applied + fingerprint，不重复返回 raw secret；首次响应丢失时需再次 reset。

### 向后兼容

回归现有工具：

~~~
memory_*
work_*
task_*
account_*
~~~

不得因 Thread / revision / origin 改造改变现有必填字段、返回字段、Task live harness、Task settlement 或 HTTP/MCP envelope。

## WO-T6 Product Surface

产品表面按已定优先级实现：

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

原则：

- Memory / Thread 保持底层抽象，不直接作为一级导航；
- Todo 只汇总 Thread pending；
- Reply / Branch / Handle 在具体协作上下文中出现；
- Work 复用 executor Task 能力；
- Hire 复用 publisher Task 能力；
- Store / Retrieve 使用 standalone Memory surface；
- thread-origin Memory 不进入默认 Store 列表。

Web 第一版必须至少让人类 owner 能检查 Todo、Link、Start、Join 与当前 Thread 状态，同时保留现有 Work/Hire/Store 功能。

Agent surface 与 Web surface 使用同一 service 事实，不要求名称完全相同。

### 验收

- 首页/主入口不平铺 Role / Memory / Thread / Task 数据模型；
- Todo 打开后直接进入可处理上下文；
- Link pending request 能完成 accept / decline；
- Start 对 active Link 可直接建协作，对非 Link 可生成 join path；
- Join 后立即看到明确 entry；
- 高频 Thread 回复不出现在 Store 默认列表；
- Work / Hire 现有路径不退化。

## WO-T7 场景与鲁棒性验收

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

- lineage 的每一级 anchor 都指向确定 ThreadMemory occurrence / revision；
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

- A/B 同时互相 request 只能形成一个 canonical pair；

- Link request / accept 后才 active；
- active Link 才允许 direct add；
- unlink 不移除既有 ThreadRole；
- key reset 同时绑定明确 join entry；
- raw key 不可再次读取，只能 reset；
- key join 默认建立 write ThreadRole；
- existing read participant 用 key 不升级 permission；
- 重复 join 不重复生成 entry receipt；
- participant removal 后旧 key 不能重新加入，Thread 暂时没有 join key；
- join 枚举 / brute-force 防护生效。

### Memory 边界回归

验证：

- migration backfill 后现有 memory_list 结果数量与内容保持；
- thread-origin Memory 不进入 default memory_list；
- Task harness 仍实时读取 current Memory；
- Thread pinned revision 在 standalone update/delete 后保持；
- Thread write 不走 standalone Store consumption action。

### Todo race

验证同一 input 的并发：

- handle vs reply；
- reply vs branch；
- duplicate reply retry。

只有一个 Todo-consuming 动作成功；stale 动作不产生额外 Thread write。

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

- ThreadRole 决定 participant scope；
- Root ownership 只派生 read/govern，不派生 descendant collaboration write；
- lineage 只暴露协作所需 anchor；
- role removal 更新 scoped access；
- writeback 目标校验当前 Thread；
- Link 与 ThreadRole 生命周期独立；
- write/manage → read 时 pending withdrawn；
- read → write/manage 时以新 entry 产生单一 Todo。

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

发布前同时通过：migration/backward-compat、Thread state-machine、Todo race、Link/Key、Task regression、Agent protocol、Web Product Surface。

README / llms.txt / MCP descriptions / HTTP contract / Web 文案从最终事实合同同步更新。
