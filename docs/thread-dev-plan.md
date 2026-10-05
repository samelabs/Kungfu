# Thread 开发计划

依据：docs/thread-prd.md。执行顺序围绕同一个通讯模型展开：Memory 原子 → Thread 持久状态 → Thread service → realtime / protocol → 完整验收。

## 1. 执行原则

1. Role / Memory / Thread 是业务原子。
2. thread_roles 与 thread_memories 表达关系状态。
3. Thread service 是权限、seq、revision、seen cursor 的唯一业务入口。
4. repository 提供事务安全的持久化 primitive。
5. thread_post 原子完成 Memory create、timeline append、seq/revision/seen 更新。
6. Memory 活引用变更通过 Thread revision fan-out 纳入同步。
7. realtime signal 负责低延迟唤醒，revision + seq 负责可靠恢复。
8. MCP、HTTP 和 realtime transport 共享同一 Thread service 事实。

执行链：

~~~
WO-T1 Memory 原子
   ↓
WO-T2 Thread 数据内核
   ↓
WO-T3 Thread service
   ↓
WO-T4 Realtime + Protocol
   ↓
WO-T5 全量验收
~~~

## WO-T1 Memory 原子契约

### 目标

让 Memory 可以自然承载一次短通讯，同时保持现有 ownership、visibility、checksum、soft delete、Task live-reference 行为。

### 输入契约

memory_put：

- content 必填，trim 后至少 1 字符；
- title 可选，缺省保存空字符串；
- tags 可选，缺省保存 []；
- description 可选；
- 现有最大长度、敏感内容扫描、checksum 与 update 语义继续生效。

### transaction-safe create primitive

提取统一 Memory create primitive：

- 接收已验证 owner Role；
- 生成唯一 code；
- 应用现有 storage consumption policy；
- 创建 private / active Memory；
- 返回 Memory identity；
- 由外层事务决定 commit。

standalone memory_put 与 thread_post 共用它。

### 验收

- 短 content 正常创建；
- title / tags 缺省输出稳定；
- 既有 Memory create/update/share/unshare/delete 行为回归；
- Task harness_refs 继续读取当前 Memory；
- 外层事务回滚时 Memory 与消费副作用同时回滚。

## WO-T2 Thread 数据内核

### threads

~~~
id / code
created_by_role_id
parent_thread_id?
anchor_memory_id?
status
next_seq
revision
created_at / updated_at
~~~

初值：

~~~
revision = 1
head_seq = 0
~~~

结构约束：

- Root：parent=null、anchor=null；
- Child：parent 非空、anchor 可空；
- anchor 非空时由 service 验证其属于直接 parent 且当前 active；
- parent / anchor 创建后保持稳定；
- code 唯一。

### thread_roles

~~~
thread_id
role_id
permission
seen_seq
seen_revision
~~~

约束：

- (thread, role) 唯一；
- seen_seq >= 0；
- seen_revision >= 1；
- 当前值由 service 保证不超过对应 Thread head_seq / revision。

### thread_memories

~~~
thread_id
memory_id
seq
changed_revision
added_by_role_id
created_at
~~~

约束：

- (thread, memory) 唯一；
- (thread, seq) 唯一；
- seq 单调递增；
- changed_revision <= Thread revision；
- relation 保持位置稳定。

### repository primitives

至少覆盖：

- find / lock Thread；
- atomic increment revision；
- atomic allocate seq + revision；
- set / remove ThreadRole；
- monotonic advance seen_seq / seen_revision；
- append ThreadMemory 并写入 changed_revision；
- timeline after_seq pagination；
- timeline changed_after_revision 查询；
- parent / ancestor / descendant 查询；
- 查询某 Memory 作为 timeline entry 或 anchor 影响到的 Thread IDs。

### 并发验收

- 同 Thread 并发 post 获得不同 seq；
- revision 每次共享状态 mutation 精确前进；
- 同 Role 并发 seen 取 max；
- duplicate include 只形成一个 timeline relation；
- role set 并发保持单一 relation。

## WO-T3 Thread service

### Authorization

统一提供：

~~~
CanReadThread
CanWriteThread
CanManageThread
CanReadMemoryInThread
~~~

Root creator 在每个 descendant 中保持显式 manage ThreadRole。

### thread_create

Root：

- 创建 Thread revision=1 / head_seq=0；
- creator ThreadRole = manage；
- creator seen_seq=0 / seen_revision=1；
- 初始 Roles 以创建完成后的 head_seq / revision 初始化 cursor。

Child：

- parent open；
- actor 至少 parent write；
- anchor 可选；
- anchor 存在时属于直接 parent 且 active；
- parent 外初始 Role 经 parent manage 授权；
- Root creator 与 Child creator 建立 manage relation；
- Child revision=1 / head_seq=0；
- parent revision + 1。

### thread_get

返回：

- identity / status；
- parent / anchor；
- effective permission；
- Roles；
- revision / head_seq；
- caller seen_revision / seen_seq；
- has_updates / unread_count；
- timeline(after_seq, after_revision, limit)，同时返回新 entry 与后续发生变化的既有 entry；
- caller 可见的 Child summaries。

### thread_list

返回 caller 可访问 Thread 的轻量状态：

- status；
- revision / head_seq；
- seen_revision / seen_seq；
- has_updates / unread_count；
- parent / anchor 摘要。

用于入口发现、离线提醒和重连比较。

### thread_post

单事务：

1. lock Thread；
2. 验 open + write；
3. create Memory；
4. allocate seq；
5. Thread revision + 1；
6. append ThreadMemory，并令 changed_revision = new revision；
7. actor seen_seq = max(current, new seq)；
8. actor seen_revision = max(current, new revision)；
9. commit。

commit 后产生 thread_changed。

### thread_include

owner inclusion：

- actor owns Memory；
- target write；
- target open。

tree result promotion：

- source 是 target descendant；
- actor manage source；
- actor manage target；
- target open。

成功路径与 post 一样推进 target seq / revision / actor cursors。

### thread_role_set

manage 调整参与关系。

新增 Role：

- 在 mutation 后的 current head_seq / revision 初始化 seen cursor。

权限调整：

- Thread revision + 1；
- actor seen_revision 推进到结果 revision；
- target Role 保留原 seen_revision，使其能观察到权限变化。

Child 纳入 parent 外 Role 时执行 parent manage gate。

### thread_role_remove

manage 移除当前关系：

- Thread revision + 1；
- commit 后停止该 Role 的 scoped access 与 realtime delivery。

再次加入建立新的当前 relation，并从当时状态初始化 cursor。

### thread_set_status

manage 执行 open / closed。

- 当前 Thread revision + 1；
- actor seen_revision 推进；
- Child status 变化同时使直接 parent revision + 1，因为 parent 的 Child summary 已变化。

### thread_seen

输入：

~~~
thread
seen_seq
seen_revision
~~~

更新：

~~~
stored seen_seq = max(stored, requested)
stored seen_revision = max(stored, requested)
~~~

并校验 requested 不超过当前 head_seq / revision。

seen mutation 属于个人消费状态，不改变 Thread shared revision。

### Memory mutation fan-out

Memory update / soft-delete 完成时：

1. 找出 timeline 引用该 Memory 的 Thread；
2. 找出以该 Memory 为 anchor 的 Child Thread；
3. Thread IDs 去重；
4. 每个受影响 Thread revision + 1；
5. timeline 引用同步把对应 ThreadMemory.changed_revision 写为新 revision；
6. commit；
7. 每个受影响 Thread 发一个 thread_changed。

head_seq 保持。

## WO-T4 Realtime + Protocol

### Realtime contract

统一 change signal：

~~~
thread_changed
thread_code
revision
head_seq
~~~

共享状态 commit 完成后发出。

subscriber 通过 revision / head_seq 判断需要：

- 读取 after_seq 后的新 timeline entry；
- 读取 after_revision 后发生变化的既有 timeline entry；
- 刷新 roles / status / child summary。

### Delivery model

realtime 是低延迟通道，durable state 是恢复依据。

验证：

- signal duplicate 时 client 可安全重复 reconcile；
- signal missing 时 thread_list / thread_get(after_seq, after_revision) 可恢复；
- reconnect 后 revision / head_seq 能发现差异；
- role removal 后停止后续 delivery。

具体 transport 在实现时按现有服务能力选择；协议只要求 thread_changed 语义一致。

### 外部语义面

需要提供：

- create；
- get；
- list；
- post；
- include；
- role set；
- role remove；
- status set；
- seen；
- realtime subscribe。

MCP / HTTP 根据自身 transport 能力映射，业务结果由同一 service 产生。

### Rate limit

thread_post 使用独立通讯写入 bucket，阈值集中配置。

## WO-T5 完整验收

### 通讯场景

覆盖：

- 私聊；
- 群聊；
- 多人对话；
- timeline 增量读取；
- anchored reply thread；
- parent-only child topic；
- 并行 Child；
- 外部协作者；
- result promotion；
- Memory edit / delete 的 changed_revision 增量恢复；
- Child status change；
- Role add / permission change / remove；
- offline reconnect；
- multi-device seen max。

### 状态不变量

验证：

- seq / head_seq 一致；
- revision 单调；
- seen_seq / seen_revision 单调且有界；
- post 原子性；
- Memory fan-out 去重与 ThreadMemory.changed_revision；
- child status 对 parent revision 的传播；
- Root creator descendant ThreadRole；
- private scoped access；
- realtime signal 发生在 durable state commit 之后。

### 兼容回归

- Memory；
- Task live-reference；
- MCP / HTTP 同语义；
- fresh database migrations；
- 全仓测试。

### 发布门槛

验收通过后，再从最终实现生成 README / llms / skill / CHANGELOG / VERSION 等产品表达。

## 完成判据

Thread 第一版应该能直接解释成一套通讯协议：

> Role 在 Thread 中发送 Memory，Timeline 以 seq 保序，revision 表达共享状态变化，ThreadMemory.changed_revision 定位既有内容变化，ThreadRole 的 seen_seq / seen_revision 表达个人消费位置，realtime signal 提供即时唤醒，Child Thread 递归承载局部和并行会话。
