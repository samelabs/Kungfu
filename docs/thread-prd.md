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
- 在线实时接收、离线恢复和未读提醒。

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

Root creator 掌握整棵 tree 的全景治理。创建 descendant 时，Root creator 在该 Thread 建立 manage 关系，因此权限、实时订阅和 seen_seq 都使用同一 ThreadRole 事实源。

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

Memory 的更新和删除属于 Thread 可见状态变化，因此所有包含该 Memory 的 Thread 都推进 revision，并产生实时 change signal。

## 6. ThreadRole

ThreadRole 表达：

~~~
Role ∈ Thread
~~~

以及这个 Role 在 Thread 内的权限与通讯消费位置。

持久字段：

~~~
thread_roles
- thread_id
- role_id
- permission: read | write | manage
- seen_seq
~~~

permission：

- read：读取 Thread、anchor、timeline 和自己可进入的 Child；
- write：包含 read，并可发送 Memory、纳入已有 Memory、创建 Child；
- manage：包含 write，并可调整 Roles、权限和 Thread status。

seen_seq 是通讯游标：

- 表示该 Role 已确认消费到的 timeline seq；
- 单调前进；
- 不超过 head_seq；
- 新增 Role 时初始化为当前 head_seq，因此历史可读但不自动形成历史未读；
- Role 自己 post / include 成功后，seen_seq 至少推进到刚写入的 seq。

未读是派生量：

~~~
unread_count = head_seq - seen_seq
has_unread = head_seq > seen_seq
~~~

thread_list 与 thread_get 返回 caller 的 seen_seq、head_seq、unread_count。

## 7. Thread 状态

Thread 持久状态：

~~~
open
closed
~~~

open 表示会话继续接收内容和结构变化。

closed 表示当前会话结束写入，历史保持可读，manage 可以重新打开。

Thread 的当前协作现场由以下事实组成：

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

## 8. revision：Thread 状态版本

每个 Thread 维护单调递增 revision。

revision 表示“这个 Thread 对可见参与者呈现的状态发生过一次变化”。

以下动作推进对应 Thread revision：

- timeline append；
- Thread 内 Memory 的 update / delete；
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

这样客户端能区分：

- head_seq 变化：有新的 timeline entry；
- revision 变化但 head_seq 不变：现有内容或 Thread 结构发生变化。

## 9. 实时通讯

实时层传递轻量 change signal，事实仍以数据库中的 Thread 状态为准。

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
→ Thread durable state 已更新
→ emit thread_changed
→ subscriber receives signal
→ thread_get(after_seq=...)
~~~

信号可以重复，也允许连接期间遗漏；客户端通过 revision、head_seq 和 timeline 恢复。

因此实时链路故障不会造成通讯事实丢失。

### 9.1 在线

在线客户端订阅自己可读 Thread 的 change signal。

收到：

~~~
thread_changed(T, revision=43, head_seq=18)
~~~

客户端比较本地状态：

- head_seq 前进：读取 after_seq 后的 timeline；
- revision 前进但 head_seq 未变：刷新 Thread metadata / 当前 Memory 状态。

### 9.2 离线与重连

重连后：

1. thread_list 获取各 Thread 的 revision、head_seq、seen_seq；
2. 对需要恢复的 Thread 调 thread_get(after_seq=本地最后 seq)；
3. 消费完成后调用 thread_seen。

实时信号负责低延迟，revision + seq 负责恢复。

### 9.3 权限

实时信号只送达当前可读该 Thread 的 Role。

Role 被移出 Thread 后，其 scoped read 与后续实时信号同时停止。

signal 本身不携带 Memory content。

## 10. 提醒与已读

提醒直接由 ThreadRole.seen_seq 与 Thread.head_seq 计算。

例：

~~~
head_seq = 31
seen_seq = 27
unread_count = 4
~~~

这是一条稳定的跨设备状态。

thread_seen 语义：

~~~
thread_seen(thread, seq)
~~~

规则：

- caller 必须可以读取该 Thread；
- seq 不超过当前 head_seq；
- seen_seq 只向前推进；
- 多设备并发取 max。

thread_get 是读取动作；seen 由 thread_seen 显式确认。

这样 Agent 可以先读取、处理，再确认消费位置；人类客户端也可以在内容真正呈现后确认已读。

## 11. Role 加入与会话边界

manage 通过 ThreadRole 改变 Thread 的参与边界。

加入：

~~~
thread_role_set(Thread, Role, permission)
~~~

产生或更新 ThreadRole。

移除：

~~~
thread_role_remove(Thread, Role)
~~~

移除当前参与关系，同时撤销该 Thread 提供的 private Memory scoped access 和实时信号。

Root creator 在每个 descendant 中保持显式 manage ThreadRole，以统一治理、实时订阅与 seen_seq。

## 12. Child Thread 的参与边界

Child 使用独立 Role 集合。

创建 Child 的 actor 至少需要 parent write。

Child 的初始 Role：

- parent 中已有 Role 可以直接纳入；
- parent 外 Role 的纳入由 parent manage 授权。

Child 创建后继续加入 parent 外 Role时，actor 同时具备：

- Child manage；
- 直接 parent manage。

anchor 存在时，Child participant 可以通过 Child scope 读取该 anchor，即使其不是 parent participant。

该授权只覆盖 anchor 与 Child 自身 timeline。

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
→ poster seen_seq advances
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
→ actor target seen_seq advances
→ emit target thread_changed
~~~

### 14.3 Child

thread_create 可以创建 Root 或 Child。

Child：

- parent 必填；
- anchor 可选；
- 创建后 parent revision 推进；
- parent subscribers 收到 thread_changed；
- Child 从自己的 revision / head_seq 开始独立通讯。

## 15. 状态操作

Thread 核心语义能力：

- create：创建 Root / Child；
- get：读取当前 Thread 与 timeline 增量；
- list：发现可访问 Thread，并返回 revision / head_seq / seen_seq / unread_count；
- post：产生新 Memory；
- include：纳入已有 Memory；
- role set：加入 Role 或调整权限；
- role remove：移除 Role；
- status set：open / closed；
- seen：确认消费位置；
- realtime subscribe：接收 thread_changed。

协议层可以按现有 MCP / HTTP 能力组织这些语义，所有入口共享同一 service 规则。

## 16. 最小持久结构

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

head_seq 可由 next_seq 派生或由实现以等价方式维护，但对外语义保持一致。

### ThreadRole

~~~
thread_roles
- thread_id
- role_id
- permission
- seen_seq
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

## 17. 通讯不变量

实现必须保持：

1. 同一 Thread 的 seq 唯一且单调；
2. head_seq 与 timeline 最新 seq 一致；
3. seen_seq 单调且不超过 head_seq；
4. revision 单调递增；
5. 所有外部可见 Thread 变化最终反映到 revision；
6. timeline append 同时推进 head_seq 与 revision；
7. Memory update / delete 影响到的每个 Thread 都推进 revision；
8. thread_post 的 Memory create 与 timeline append 原子提交；
9. ThreadRole 是 scoped access、seen state 与实时订阅资格的统一事实源；
10. Child 有 parent，anchor 可选；
11. anchor 存在时属于直接 parent；
12. Root creator 在每个 descendant 中保持 manage ThreadRole；
13. private Thread 对非参与者保持不可见；
14. role removal 立即撤销 scoped read 与实时信号；
15. realtime signal 在 durable state commit 之后产生；
16. 信号丢失或重复后，客户端仍可通过 revision + head_seq + timeline 恢复一致状态。

## 18. 场景验收

### 私聊

~~~
T
Roles: A, B
Timeline: M1 → M2 → M3
~~~

双方实时接收 signal，离线后按 seen_seq / head_seq 恢复。

### 群聊 / 多人对话

~~~
T
Roles: A, B, C, D
Timeline: #1 ... #N
~~~

每个 Role 拥有独立 seen_seq 和 unread_count。

### 时间线消费

Agent 保存 last seq：

~~~
thread_get(after_seq=120)
~~~

读取 121..head，并在完成处理后 thread_seen(head)。

### 回复串

~~~
T0
└─ M8
   └─ T1
~~~

T1 的 anchor=M8，T1 participants 可以读取 M8 和 T1 timeline。

### 新话题

~~~
T0
└─ T1
~~~

T1 有 parent、无 anchor，形成父会话下独立话题。

### 并行协作

同一 parent 同时拥有多个 Child，每个 Child 独立 Roles、timeline、revision、seen state。

### 外部协作者

parent manage 将外部 Role 加入 Child。外部 Role 只获得 Child scope。

### 结果回主线

Child Memory 经 tree result promotion 进入 ancestor timeline，ancestor head_seq / revision 前进并实时通知其参与者。

### 编辑同步

M5 已存在于 T0，owner 更新 M5：

~~~
T0 head_seq 保持
T0 revision + 1
emit thread_changed
~~~

客户端收到 revision 变化并刷新当前状态。

### 断线恢复

客户端错过若干 realtime signal：

~~~
local revision=40, seq=18
server revision=47, head_seq=22
~~~

重新连接后通过 thread_list / thread_get 恢复到 revision 47、seq 22，再提交 seen。

## 19. Memory 原子前置

Thread post 以 Memory 作为唯一内容原子，因此 Memory 输入契约调整为：

- content 必填，最少 1 个字符；
- title 可选；
- tags 可选；
- description 可选；
- owner、visibility、checksum、soft delete、update 保持现有语义。

Thread post 与 standalone memory_put 复用同一个 transaction-safe Memory create primitive。

Task 对 Memory 的 live-reference 行为保持一致。

## 20. 审计结论

### 模型

Role / Memory / Thread 三个业务原子足以表达通讯与协作。ThreadRole、ThreadMemory 承担关系状态。

### 通讯

Timeline + seq 提供可靠有序消息流；Child Thread 提供回复串、新话题和并行会话。

### 实时

revision 负责所有 Thread 可见变化，head_seq 负责 timeline append，thread_changed 提供低延迟信号。实时链路与持久事实分离，支持断线恢复。

### 提醒

seen_seq 是必要的最小参与者通讯状态。它只表达消费位置，并直接推导 unread_count。

### 活引用

Memory edit/delete 通过 Thread revision fan-out 纳入实时同步，因此活引用与 Timeline 模型保持一致。

### 场景

私聊、群聊、多人对话、时间线、回复串、新话题、并行协作、外部协作者、结果回流、Agent 轮询、实时客户端均由同一套 Thread 通讯逻辑表达。
