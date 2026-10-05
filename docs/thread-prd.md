# Thread PRD

> Thread 是 Kungfu 的私有通讯与协作模型。实现以本文定义的对象、关系、状态和通讯语义为准。

## 1. 产品定义

Thread 表达一个持续存在的 private conversation / collaboration scope。

它统一承载：

- 一对一对话；
- 多人对话与群聊；
- 有序时间线；
- 围绕某条 Memory 展开的回复串；
- 从父会话派生的新话题；
- 多个并行子会话；
- 人与 Agent 的持续协作；
- 在线实时接收、离线恢复和提醒。

Thread 的通讯内核是：

~~~
Role sends Memory into Thread
→ Memory enters ordered Timeline
→ Thread state advances
→ other Roles receive change signal
→ clients consume incremental state
~~~

## 2. 业务形状

业务原子：

~~~
Role
Memory
Thread
~~~

核心关系：

~~~
Role   ─produces────> Memory
Role   ─participates→ Thread
Thread ─contains────> Memory
Thread ─parent──────> Thread
~~~

含义：

- Role 是通讯与协作主体；
- Memory 是一次信息表达；
- Thread 是通讯空间；
- Timeline 是 Memory 在 Thread 中的有序关系；
- Child Thread 仍然是 Thread，用来表达局部会话和会话分支。

thread_roles 与 thread_memories 是关系表。

## 3. Thread 结构

### 3.1 Root Thread

Root Thread 是一条独立通讯主线：

~~~
T0
├─ Roles
├─ Timeline
└─ Child Threads
~~~

Root：

- parent_thread_id 为空；
- anchor_memory_id 为空；
- created_by_role_id 记录创建者；
- 创建者在 Root 中为 manage。

### 3.2 Child Thread

Child Thread 必须有 parent，anchor 可选。

两种形态：

~~~
T0
└─ T1
~~~

表示父会话下新开的局部会话 / 新话题。

~~~
T0
└─ M3
   └─ T1
~~~

表示围绕 M3 展开的局部讨论。

规则：

- parent_thread_id 必填；
- anchor_memory_id 可空；
- anchor 存在时，必须是直接 parent 中当前 active 的 Memory；
- parent 与 anchor 创建后保持稳定；
- 同一个 parent 可以拥有多个 Child；
- 同一 Memory 可以派生多个 Child。

Child 的 timeline 只记录 Child 内产生或纳入的 Memory。anchor 通过 anchor_memory_id 作为上下文返回。

### 3.3 Tree

Thread 通过单一 parent 形成树：

~~~
T0
├─ T1
│  ├─ T3
│  └─ T4
└─ T2
~~~

Root creator 掌握整棵 tree 的全景治理。创建 descendant 时，Root creator 在该 Thread 建立 manage 关系，因此权限、实时订阅和通讯游标都使用同一 ThreadRole 事实源。

## 4. Timeline

ThreadMemory 表达一条 Memory 进入 Thread 的事实：

~~~
thread_memories
- thread_id
- memory_id
- seq
- added_by_role_id
- created_at
~~~

seq：

- 在 Thread 内单调递增；
- 在 Thread 内唯一；
- 表示 Memory 进入该 Thread 的顺序；
- 同一 Memory 在同一 Thread 中只有一个位置。

ThreadMemory 建立后保持位置稳定。

Thread 对外暴露：

~~~
head_seq
timeline(after_seq, limit)
~~~

head_seq 表示当前 timeline 最新 seq。

## 5. Memory 在 Thread 中的语义

Memory 的 owner 始终是产生它的 Role。

同一 Memory 可以出现在多个 Thread：

~~~
Role A ─produces→ M1

M1 ∈ T0
M1 ∈ T1
~~~

Thread 保存关系，不复制 Memory 内容。

Memory 使用活引用：

- Memory 更新后，Thread 展示更新后的当前内容；
- Memory soft-delete 后，原 timeline 位置展示 tombstone；
- tombstone 保留 seq；
- anchor 对应 Memory 删除后，已有 Child 继续保留该 anchor 关系。

Memory update / delete 会影响两类 Thread：

1. timeline 中包含该 Memory 的 Thread；
2. 以该 Memory 作为 anchor 的 Child Thread。

所有受影响 Thread 都推进 revision；同一 Thread 在一次 Memory mutation 中只推进一次。

## 6. ThreadRole

ThreadRole 表达：

~~~
Role ∈ Thread
~~~

以及这个 Role 在 Thread 内的权限和通讯消费位置。

持久字段：

~~~
thread_roles
- thread_id
- role_id
- permission: read | write | manage
- seen_seq
- seen_revision
~~~

permission：

- read：读取 Thread、anchor、timeline 和自己可进入的 Child；
- write：包含 read，并可发送 Memory、纳入已有 Memory、创建 Child；
- manage：包含 write，并可调整 Roles、权限和 Thread status。

两个通讯游标承担不同职责：

- seen_seq：已确认消费到的 timeline 位置；
- seen_revision：已确认观察到的 Thread 共享状态版本。

新 Role 加入已有 Thread 时：

~~~
seen_seq = current head_seq
seen_revision = current revision
~~~

这样历史仍可读取，提醒从加入后的变化开始。

Role 自己完成 post / include 后：

- seen_seq 至少推进到新 seq；
- seen_revision 至少推进到该操作产生的 revision。

Role 自己完成 role/status 等 Thread mutation 后，seen_revision 至少推进到该操作产生的 revision。

## 7. Thread 共享状态

Thread 持久状态：

~~~
open
closed
~~~

open 表示会话继续接收内容和结构变化。

closed 表示当前会话结束写入，历史保持可读，manage 可以重新打开。

Thread 的共享协作现场由以下事实组成：

~~~
status
revision
head_seq
roles
timeline
children
children.status
~~~

这些事实共同表达：

- 主线推进；
- 局部会话展开；
- 局部会话结束；
- 参与者变化；
- 结果回流；
- 当前会话是否继续。

seen_seq / seen_revision 属于 Role 在该 Thread 中的个人消费状态，不属于共享 revision。

## 8. revision：Thread 共享状态版本

每个 Thread 维护单调递增 revision。

新建 Thread 初始：

~~~
revision = 1
head_seq = 0
~~~

revision 表示“这个 Thread 对参与者呈现的共享状态发生过一次变化”。

以下动作推进对应 Thread revision：

- timeline append；
- timeline Memory 的 update / delete；
- anchor Memory 的 update / delete；
- Role 新增、权限调整、移除；
- status 改变；
- Child 创建；
- 直接 Child 的 status 改变。

timeline append 同时推进 head_seq 与 revision。

结构或内容更新可以只推进 revision，例如：

~~~
Memory M3 edited
head_seq = 18
revision 42 → 43
~~~

客户端因此可以区分：

- head_seq 变化：出现新的 timeline entry；
- revision 变化但 head_seq 不变：现有内容或 Thread 结构发生变化。

thread_seen 只更新个人游标，不推进 Thread revision。

## 9. 提醒状态

提醒由两个维度组成。

新内容：

~~~
has_unread = head_seq > seen_seq
unread_count = head_seq - seen_seq
~~~

共享状态变化：

~~~
has_updates = revision > seen_revision
~~~

因此：

- 新 Memory 会同时形成 has_unread 和 has_updates；
- 旧 Memory 编辑、Child 状态变化、权限变化等只形成 has_updates；
- thread_list 与 thread_get 返回 revision、head_seq、seen_revision、seen_seq、unread_count、has_updates。

thread_seen 语义：

~~~
thread_seen(thread, seen_seq, seen_revision)
~~~

规则：

- caller 可以读取该 Thread；
- seen_seq 不超过当前 head_seq；
- seen_revision 不超过当前 revision；
- 两个游标都只向前推进；
- 多设备并发分别取 max。

thread_get 负责读取；thread_seen 负责确认消费。

## 10. 实时通讯

实时层传递轻量 change signal，数据库中的 Thread 状态是恢复依据。

统一信号：

~~~
thread_changed
- thread_code
- revision
- head_seq
~~~

流程：

~~~
transaction commit
→ durable Thread state 已更新
→ emit thread_changed
→ subscriber receives signal
→ client reconciles with thread_get
~~~

signal 只表达“状态已变化”，不携带 Memory content。

### 10.1 在线

在线客户端订阅自己可读 Thread 的 change signal。

收到：

~~~
thread_changed(T, revision=43, head_seq=18)
~~~

客户端比较本地状态：

- head_seq 前进：读取 after_seq 后的 timeline；
- revision 前进但 head_seq 未变：刷新 Thread 当前状态。

### 10.2 离线与重连

重连后：

1. thread_list 获取 revision、head_seq、seen_revision、seen_seq；
2. 对需要恢复的 Thread 调 thread_get(after_seq=本地最后 seq)；
3. 消费完成后调用 thread_seen。

signal 可以重复，也可能在断线期间遗漏。revision + head_seq + timeline 保证恢复到当前事实。

### 10.3 权限

实时信号只送达当前可读该 Thread 的 Role。

Role 被移出 Thread 后，其 scoped read 与后续 realtime signal 同时停止。

不同接入协议可以使用各自合适的 push transport；不具备 server-push 的调用方使用 revision / seq 轮询恢复同一状态。

## 11. Role 加入与会话边界

manage 通过 ThreadRole 改变参与边界。

加入或调权限：

~~~
thread_role_set(Thread, Role, permission)
~~~

创建新 ThreadRole 时，seen_seq / seen_revision 初始化为该事务完成后的当前 head_seq / revision。

移除：

~~~
thread_role_remove(Thread, Role)
~~~

移除当前参与关系，同时撤销该 Thread 提供的 private Memory scoped access 和 realtime signal。

再次加入时建立新的当前 ThreadRole，并从新的 head_seq / revision 开始提醒。

Root creator 在每个 descendant 中保持显式 manage ThreadRole，以统一治理、实时订阅和通讯游标。

## 12. Child Thread 的参与边界

Child 使用独立 Role 集合。

创建 Child 的 actor 至少需要 parent write。

Child 初始 Role：

- parent 中已有 Role 可以直接纳入；
- parent 外 Role 的纳入由 parent manage 授权。

Child 创建后继续加入 parent 外 Role 时，actor 同时具备：

- Child manage；
- 直接 parent manage。

anchor 存在时，Child participant 可以通过 Child scope 读取该 anchor，即使其不是 parent participant。

该授权覆盖 anchor 与 Child 自身 timeline。

## 13. Private Memory scoped access

Role 对 Thread 有 read 时，可以读取：

- Thread timeline 中的 Memory；
- Child Thread 的 anchor Memory。

形式：

~~~
can_read_in_thread(R, T, M) =
    R can read T
    AND (
        M ∈ T.timeline
        OR M = T.anchor
    )
~~~

Thread scoped access：

- 保持 Memory ownership；
- 保持 Memory 全局 visibility；
- 作用范围限定在该 Thread；
- ThreadRole 移除后立即失效。

## 14. 信息产生与传递

### 14.1 Post

thread_post：

~~~
Role writes content
→ create Memory owned by Role
→ append Memory to Thread timeline
→ head_seq + 1
→ revision + 1
→ actor seen_seq / seen_revision advance
→ commit
→ emit thread_changed
~~~

Memory create 与 timeline append 在同一事务完成。

### 14.2 Include

thread_include 把已有 Memory 纳入目标 Thread。

两种协作路径：

owner inclusion：

- actor owns Memory；
- actor 对 target 有 write。

tree result promotion：

- Memory 已存在于 source descendant；
- target 是 source ancestor；
- actor manage source；
- actor manage target。

成功后：

~~~
append target timeline
→ target head_seq + 1
→ target revision + 1
→ actor target seen_seq / seen_revision advance
→ commit
→ emit target thread_changed
~~~

### 14.3 Child

thread_create 可以创建 Root 或 Child。

Child：

- parent 必填；
- anchor 可选；
- Child 初始 revision=1、head_seq=0；
- creator 与 Root creator 建立 manage ThreadRole；
- parent revision 推进；
- commit 后 parent subscribers 收到 thread_changed；
- Child 从自己的 revision / head_seq 开始独立通讯。

## 15. Memory mutation 与 Thread fan-out

Memory 继续使用现有 update / soft-delete 语义。

Memory 发生 update / delete 时，在同一业务事务中解析受影响 Thread：

~~~
thread_memories.memory_id = M
UNION
threads.anchor_memory_id = M
~~~

对去重后的每个 Thread：

- revision + 1；
- head_seq 保持；
- commit 后各发一个 thread_changed。

这样 Memory 活引用与实时通讯保持一致。

## 16. 状态操作

Thread 核心语义能力：

- create：创建 Root / Child；
- get：读取当前 Thread 与 timeline 增量；
- list：发现可访问 Thread，并返回提醒状态；
- post：产生新 Memory；
- include：纳入已有 Memory；
- role set：加入 Role 或调整权限；
- role remove：移除 Role；
- status set：open / closed；
- seen：确认消费位置；
- realtime subscribe：接收 thread_changed。

协议层按现有 MCP / HTTP 能力组织这些语义，所有入口共享同一 service 规则。

## 17. 最小持久结构

### Thread

~~~
threads
- id / code
- created_by_role_id
- parent_thread_id?
- anchor_memory_id?
- status: open | closed
- next_seq
- revision
- created_at
- updated_at
~~~

head_seq 可由 next_seq 派生或由实现以等价方式维护，对外语义保持一致。

### ThreadRole

~~~
thread_roles
- thread_id
- role_id
- permission
- seen_seq
- seen_revision
~~~

### ThreadMemory

~~~
thread_memories
- thread_id
- memory_id
- seq
- added_by_role_id
- created_at
~~~

## 18. 通讯不变量

实现保持：

1. 同一 Thread 的 seq 唯一且单调；
2. head_seq 与 timeline 最新 seq 一致；
3. seen_seq 单调且不超过 head_seq；
4. revision 单调递增；
5. seen_revision 单调且不超过 revision；
6. Thread 共享状态变化反映到 revision；
7. timeline append 同时推进 head_seq 与 revision；
8. Memory update / delete 对所有 timeline / anchor 引用 Thread 做去重 revision fan-out；
9. thread_post 的 Memory create 与 timeline append 原子提交；
10. ThreadRole 是 scoped access、通讯游标与 realtime subscription 资格的统一事实源；
11. Child 有 parent，anchor 可选；
12. anchor 存在时属于直接 parent；
13. Root creator 在每个 descendant 中保持 manage ThreadRole；
14. role removal 立即撤销 scoped read 与 realtime signal；
15. durable state commit 先于 thread_changed；
16. signal 丢失或重复后，客户端仍可通过 revision + head_seq + timeline 恢复；
17. thread_seen 只推进个人游标，不改变共享 revision。

## 19. 场景验收

### 私聊

~~~
T
Roles: A, B
Timeline: M1 → M2 → M3
~~~

双方可实时接收 signal；离线后按 revision / seq 恢复。

### 群聊 / 多人对话

~~~
T
Roles: A, B, C, D
Timeline: #1 ... #N
~~~

每个 Role 拥有独立 seen_seq / seen_revision。

### 时间线消费

Agent：

~~~
thread_get(after_seq=120)
→ consume
→ thread_seen(seen_seq=head_seq, seen_revision=revision)
~~~

### 回复串

~~~
T0
└─ M8
   └─ T1
~~~

T1 participants 可以读取 M8 和 T1 timeline。

### 新话题

~~~
T0
└─ T1
~~~

T1 有 parent、无 anchor，形成父会话下独立话题。

### 并行协作

同一 parent 同时拥有多个 Child，每个 Child 独立 Roles、timeline、revision 和通讯游标。

### 外部协作者

parent manage 将外部 Role 加入 Child。外部 Role 获得 Child scope。

### 结果回主线

Child Memory 经 tree result promotion 进入 ancestor timeline，ancestor head_seq / revision 前进并实时通知参与者。

### 编辑提醒

M5 已存在于 T0，owner 更新 M5：

~~~
T0 head_seq 保持
T0 revision + 1
participant seen_revision 保持
has_updates = true
emit thread_changed
~~~

### Child 状态提醒

T1 从 open 变 closed：

~~~
T1 revision + 1
T0 revision + 1
~~~

T1 participant 和可见 T0 participant 都能观察到状态变化。

### 断线恢复

客户端离线前：

~~~
local revision=40
local seq=18
~~~

服务器当前：

~~~
revision=47
head_seq=22
~~~

重连后 thread_list 暴露差异，thread_get 恢复 timeline / metadata，再由 thread_seen 确认。

## 20. Memory 原子前置

Thread post 以 Memory 作为内容原子，因此 Memory 输入契约调整为：

- content 必填，最少 1 个字符；
- title 可选；
- tags 可选；
- description 可选；
- owner、visibility、checksum、soft delete、update 保持现有语义。

Thread post 与 standalone memory_put 复用同一个 transaction-safe Memory create primitive。

Task 对 Memory 的 live-reference 行为保持一致。

## 21. 审计结论

### 模型

Role / Memory / Thread 三个业务原子足以表达通讯与协作。ThreadRole、ThreadMemory 承担关系状态。

### Timeline

seq / head_seq 提供可靠有序的信息流，Child Thread 同时覆盖回复串、新话题和并行会话。

### Thread 状态

revision 是 Thread 共享状态的统一版本轴，覆盖新增内容、活引用变化、角色变化、状态变化和 Child 结构变化。

### 提醒

seen_seq 表达新内容消费位置；seen_revision 表达共享状态观测位置。两者共同形成跨设备、离线可恢复的提醒状态。

### 实时

thread_changed 只承担低延迟唤醒。事实先持久化，客户端始终能依靠 revision + head_seq + timeline 恢复，因此实时链路和数据可靠性解耦。

### 活引用

Memory update / delete 对 timeline 引用和 anchor 引用统一做 revision fan-out，活引用不会绕过 Thread 同步状态。

### 场景

私聊、群聊、多人对话、时间线、回复串、新话题、并行协作、外部协作者、结果回流、Agent 增量消费、实时客户端和断线恢复均由同一套 Thread 通讯逻辑表达。
