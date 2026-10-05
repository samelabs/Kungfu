# Thread PRD

> 本文是 Kungfu Thread 的产品与机制唯一依据。实现不得自行增加本文未定义的协作对象、状态或权限语义；如确有必要，先回到 PRD 证明现有模型无法表达。

## 1. 定位

Thread 是 Role 与 Memory 之间的 private collaboration / conversation scope。

它解决四件事：

1. 哪些 Role 在同一个协作现场；
2. 哪些 Memory 属于这条会话时间线；
3. 一条 Memory 如何继续展开局部子对话；
4. 会话当前是否仍开放，以及局部会话如何产生、推进和结束。

Thread 不承担 Task、工作流、审批、交付、next actor 等业务义务。

## 2. 业务形状

业务原子只有三个：

~~~
Role
Memory
Thread
~~~

关系只有四条：

~~~
Role   ─produces────> Memory
Role   ─participates→ Thread
Thread ─contains────> Memory
Thread ─parent──────> Thread
~~~

其中：

- Role：行为主体。当前实现映射为 Bot。
- Memory：Role 产生并拥有的信息原子；默认 private，可显式 public。
- Thread：默认 private 的协作会话容器。
- Child Thread 仍然是 Thread，不建立 Subthread 对象。
- Message 不存在；会话内容就是 Memory。

数据库中的 thread_roles 与 thread_memories 只是关系表，不是新的业务原子。

## 3. Thread 形状

### 3.1 Root Thread

Root Thread 表达一条协作主线：

~~~
Thread
├─ Roles
├─ ordered Memories
└─ Child Threads
~~~

Root Thread：

- parent_thread_id = null
- anchor_memory_id = null
- created_by 是该 Thread tree 的创建者与全景治理者。

### 3.2 Child Thread

任一 active Memory 都可以作为局部会话起点：

~~~
T0
├─ M1
├─ M2
│  ├─ T1
│  │  ├─ M4
│  │  └─ M5
│  └─ T2
│     └─ M6
└─ M3
~~~

Child Thread：

- 必须有一个直接 parent；
- 必须锚定 parent 中的一条 active Memory；
- 拥有自己的 Roles、timeline 和 open / closed 状态；
- parent 与 anchor 创建后不可修改；
- 同一 Memory 可以派生多个 Child Thread。

Thread 只能指向已经存在的 parent，因此树结构天然无环。

### 3.3 Memory 与 Thread

Memory 的 owner 永远是产生它的 Role。Thread 不改变 ownership。

同一 Memory 可以出现在多个 Thread 中：

~~~
Role A ─produces→ M1

M1 ∈ T0
M1 ∈ T1
~~~

Thread 只保存“该 Memory 进入这个会话”的关系和顺序，不复制内容。

同一 (thread, memory) 只出现一次。

Memory 在 Thread 中保持活引用：

- Memory 更新后，Thread 读取当前内容；
- Memory soft-delete 后，原时间线位置保留 tombstone；
- tombstone 不能再作为新 Child Thread 的 anchor；
- 第一版不引入 Memory revision / snapshot。

## 4. Thread 状态表达

Thread 的动态状态围绕 Thread 本身表达，不为 Role 建成员生命周期状态机。

### 4.1 持久状态

Thread 只有：

~~~
open
closed
~~~

open：

- 可以产生 / 纳入 Memory；
- 可以创建 Child Thread；
- 可以新增、调整、移除 Role。

closed：

- 保留读取；
- 禁止新增内容；
- 禁止创建 Child Thread；
- 禁止新增 Role 或升权；
- 为安全撤权，仍允许移除 Role 或降权；
- manage 可以 reopen。

关闭 parent 不自动关闭已有 Child Thread；每个 Thread 是独立会话边界。

### 4.2 过程状态

对话过程不再增加 waiting / processing / next_actor / handoff 等状态。

当前协作现场由以下事实直接表达：

~~~
Thread.status
Thread.roles
Thread.head_seq
Thread.timeline
Thread.children + children.status
~~~

因此：

- 新 Memory 进入 → 主线前进；
- 新 Child Thread 产生 → 局部会话展开；
- Child Thread closed → 局部会话结束；
- Child 结果重新纳入祖先 Thread → 结果回到主线；
- Role 被加入 / 移除 → 会话边界变化；
- Root Thread closed → 主会话停止继续写入。

这就是 Thread 的协作状态，不再额外建立工作流状态机。

客户端 / Agent 的阅读位置由调用方使用 after_seq 等 cursor 自己维护；第一版不在 thread_roles 持久化 read-state。

## 5. Role 参与关系与权限

ThreadRole 不是业务对象，只表达：

~~~
Role ∈ Thread
~~~

以及该 Role 在 Thread 中的权限。

第一版只有三档：

- read：读取 Thread、timeline、Thread 内 Memory，以及自己有权进入的 Child Thread；
- write：包含 read；可以在 Thread 中发布 Memory、纳入允许传播的 Memory、创建符合规则的 Child Thread；
- manage：包含 write；可以改变 Thread 的 Role 边界与权限，并改变 Thread open / closed 状态。

关系表最小语义：

~~~
thread_roles
- thread_id
- role_id
- permission: read | write | manage
~~~

技术时间戳可以存在，但不形成新的业务状态。

### 5.1 加入与退出

第一版不建 invited / active / left / removed 等 membership 状态。

加入：

~~~
thread_role_set(Thread, Role, permission)
→ 建立 Role ∈ Thread
~~~

退出 / 移除：

~~~
thread_role_remove(Thread, Role)
→ Role 不再属于 Thread
~~~

如果以后需要邀请链接、一次性 token、接受邀请等交互，它们属于“建立这条关系之前”的协议层能力，不进入 Thread 核心模型。

## 6. Private Memory 的 Thread scoped access

Memory 默认 private，Thread 默认 private。

Role 成为 Thread 参与者后，可以在该 Thread scope 内读取 Thread 中的 private Memory：

~~~
can_read_in_thread(R, T, M) =
    R can read T
    AND M ∈ T
~~~

这条授权：

- 不把 Memory 改成 public；
- 不改变 Memory owner；
- 不授予 Thread 外读取权；
- Role 被移出 Thread 后立即失效。

直接 memory_get 继续遵守 Memory 原有 owner / public-private 规则；Thread scoped read 由 Thread 服务完成。

## 7. Root creator 与树级治理

Root Thread 的 created_by 掌握整棵 Thread tree 全景。

Root creator：

- 对 Root Thread 为 manage；
- 对所有 descendants 具有派生的 read / write / manage 权限；
- 可以查看整个树；
- 可以调整 Child Thread 的 Role 边界和状态；
- 不需要为了表达这项治理权，在每个 Child 的 thread_roles 中复制一条成员关系。

Child Thread 的 created_by：

- 记录谁发起了该局部会话；
- 创建时获得该 Child Thread 的 manage 关系；
- 该 local manage 可以被 Root creator 调整或移除；
- 不拥有对整棵树的永久权力。

如果需要连 Root creator 都不可见的独立会话，应创建新的 Root Thread。

## 8. Child Thread 的受众边界

Child Thread 可以：

- 使用 parent Role 的子集；
- 在需要时引入 parent 之外的新 Role。

但 Child 的 anchor 来自 parent，因此扩大到 parent 外受众属于受控披露。

规则：

1. 创建者至少需要对 parent 有 write；
2. Child 初始 Roles 全部来自 parent 时，write 足够；
3. Child 创建时若包含 parent 外 Role，创建者必须对 parent 有 manage；
4. Child 创建后新增 parent 外 Role，操作者必须同时有：
   - Child manage；
   - 直接 parent manage。

Child Role 只能因 Child 获得：

- anchor Memory；
- Child timeline 中的 Memory。

不会因此读取 parent 的其他 Memory，也不会自动进入 sibling Thread。

普通 Role 的 parent / child 参与关系互不级联。只有 Root creator 具有树级派生治理权。

## 9. 信息产生、传递与回流

Thread 协作只有三种信息动作。

### 9.1 Post：在 Thread 中产生新 Memory

thread_post 是一个原子动作：

~~~
Role writes into Thread
→ 创建由该 Role 拥有的 private Memory
→ 同事务 append 到 Thread timeline
~~~

不能要求调用方先 memory_put 再 thread_include，避免半完成状态。

### 9.2 Include：把已有 Memory 纳入 Thread

thread_include 不复制 Memory，只建立新的 ThreadMemory 关系。

允许两种情况：

1. owner inclusion：Memory owner 对目标 Thread 有 write；
2. result promotion：Memory 已存在于某 descendant Thread，操作者同时 manage 来源 Thread 与目标 ancestor Thread，可把该 Memory 提升回祖先主线。

第二条只用于同一 Thread tree 内的结果回流，不赋予任意跨 Root Thread 转发权。

任意 unrelated Thread 之间传播 private Memory，仍由 Memory owner 自己完成。

### 9.3 Child：围绕 Memory 展开局部会话

~~~
Memory in T0
→ thread_create(parent=T0, anchor=M)
→ Child Thread
~~~

这就是上下文向局部协作空间的传递。

因此完整协作链是：

~~~
创建主线
→ 纳入 Role
→ post / include Memory
→ 某 Memory 展开 Child Thread
→ Child 独立协作
→ result promotion 回祖先主线
→ close Child
→ 主线继续
~~~

不需要 handoff / delivery / next_actor。

## 10. Timeline

ThreadMemory 是时间线关系：

~~~
thread_memories
- thread_id
- memory_id
- seq
- added_by_role_id
~~~

seq：

- 在 Thread 内唯一；
- 单调递增；
- 表示 Memory 进入该 Thread 的顺序；
- 不等于 Memory 创建时间；
- relation 建立后不删除、不重排。

Thread 对外可返回：

~~~
head_seq
timeline(after_seq, limit)
~~~

调用方据此增量继续，无需持久化每个 Role 的阅读状态。

## 11. 核心动作 / 功能暴露面

第一版只暴露八个 Thread 语义动作。

### 11.1 thread_create

创建 Root Thread；传 parent + anchor 时创建 Child Thread。

负责：

- 创建 Thread；
- 建立 creator 的 local manage；
- 可选设置初始 Roles；
- Child 时执行 §8 的受众校验。

不单独提供 thread_create_child。

### 11.2 thread_get

读取一个 Thread 当前协作现场：

- identity / status；
- parent / anchor；
- caller permission；
- Roles + permission；
- head_seq；
- timeline；
- 当前 caller 可见的 Child Thread 摘要。

timeline 支持 cursor / limit。

### 11.3 thread_list

列出当前 Role 可访问的 Thread。

只负责发现入口，不创造新状态。

### 11.4 thread_post

在 Thread 内原子创建 Memory 并 append timeline。

### 11.5 thread_include

把已有 Memory 纳入 Thread。

权限严格按 §9.2。

### 11.6 thread_role_set

新增 Role 或调整现有 Role 的 read / write / manage。

它就是“加入 Thread”的核心动作。

Child 引入 parent 外 Role 时必须额外执行 §8 的 parent manage 校验。

### 11.7 thread_role_remove

将 Role 移出 Thread。

关系消失后，该 Role 立即失去来自该 Thread 的 private Memory scoped access。

### 11.8 thread_set_status

~~~
open ↔ closed
~~~

只有 manage 可执行。

closed 下只允许安全收缩：role remove / permission downgrade；不允许新增内容或扩大受众。

### 11.9 不暴露关系表 CRUD

对外不得暴露：

- create/delete ThreadRole row；
- create/delete ThreadMemory row；
- update parent；
- update anchor；
- reorder timeline。

外部只看到上述八个产品语义动作。

## 12. 最小持久结构

### 12.1 Thread

~~~
threads
- id / code
- created_by_role_id
- parent_thread_id?
- anchor_memory_id?
- status: open | closed
- next_seq
- created_at
- updated_at
~~~

### 12.2 ThreadRole

~~~
thread_roles
- thread_id
- role_id
- permission: read | write | manage
~~~

### 12.3 ThreadMemory

~~~
thread_memories
- thread_id
- memory_id
- seq
- added_by_role_id
- created_at
~~~

三张表表达的是：

- 一个业务对象：Thread；
- 两条多对多关系：Thread↔Role、Thread↔Memory。

Role、Memory 继续使用现有表。

## 13. 数据与权限不变量

实现必须保证：

1. 业务原子只有 Role、Memory、Thread；
2. Root Thread 的 parent / anchor 同时为空；
3. Child Thread 的 parent / anchor 同时存在；
4. Child anchor 创建时必须是 parent 中 active Memory；
5. parent / anchor 创建后不可修改；
6. 同一 (thread, role) 最多一个参与关系；
7. 同一 (thread, memory) 最多一个 timeline 关系；
8. (thread, seq) 唯一且单调递增；
9. ThreadMemory 不删除、不重排；
10. Memory ownership 不因进入 Thread 改变；
11. 非 Thread participant 不能通过该 Thread 读取 private Memory；
12. Role 被移出后 Thread scoped access 立即失效；
13. read 不能写；write 不能治理 Role / status；
14. Root creator 始终拥有整棵 tree 的 manage；
15. Child 扩大到 parent 外 Role 时必须经过 §8 的 parent manage gate；
16. Thread scoped read 不产生跨 Root Thread 传播权；
17. thread_post 必须原子完成 Memory create + timeline append；
18. closed Thread 不允许内容写入、Child 创建、Role 新增或升权；
19. closed Thread 仍允许 Role 移除或降权；
20. tombstone 保留原 timeline 位置，但不能作为新 Child anchor；
21. result promotion 只允许 descendant → ancestor，且 actor 同时 manage source 与 target；
22. 非成员读取真实 Thread code 与不存在 code 的外部表现一致，不泄露 private Thread 是否存在。

## 14. 场景验收

### 14.1 私有多人主线

A 创建 T0：

~~~
A manage
B write
C read
~~~

A/B 可 post，C 只读，X 不在 T0 则看不到 T0。

### 14.2 局部讨论

T0/M2 创建 T1，只纳入 A、B。

C 仍属于 T0，但看不到 T1。

### 14.3 外部专家

T0 manage 围绕 M2 创建 T1，并加入外部 X。

X 只获得 M2 + T1 timeline，不获得 T0 其他 Memory。

### 14.4 并行协作

M2 可同时派生 T1、T2。

两者 Roles、timeline、status 相互独立。

### 14.5 结果回主线

T1 产生 R。

同时 manage T1 与祖先 T0 的 Role 可把 R promote 到 T0。

Memory 不复制，owner 不变，T0 timeline 获得新的 ThreadMemory 位置。

### 14.6 Role 调整

manage 用 thread_role_set 加入 / 调权限，用 thread_role_remove 移出。

不建立 invited / active / left / removed 状态机。

### 14.7 Thread 结束

T1 closed：

- 历史仍可读；
- 不能继续 post；
- T0 不受影响；
- 如有安全需要，仍可撤权。

### 14.8 Agent 持续协作

Agent 调：

~~~
thread_get(after_seq=N)
~~~

即可继续消费增量 timeline。

不需要平台维护 Role read cursor。

## 15. Memory 前置修正

Thread 要成立，Memory 必须先成为真正的信息原子。

当前 Memory 的 title、tags 必填、content 至少 50 字，偏文档型，无法自然承载会话。

实现 Thread 前先修正：

- content 必填，最少 1 个字符；
- title 可选；
- tags 可选；
- description 可选；
- owner、private/public、checksum、soft delete、update 语义保持不变；
- 不新增 Message；
- Task 对 Memory 的 live-reference 语义保持不变。

数据库可继续使用 title=""、tags=[]，无需为此增加新的业务状态。

Thread 内 post 必须复用同一 Memory 创建内核，但使用 Thread 自己的写入入口，不要求调用方走 standalone memory_put。

## 16. 非目标

第一版不引入：

- Message；
- Subthread；
- ThreadEvent；
- membership lifecycle；
- invite state；
- read cursor state；
- mention；
- notification；
- reaction；
- delivery；
- review；
- handoff；
- next_actor / next_action；
- workflow node / branch；
- public Thread；
- Memory revision / snapshot；
- Thread 删除；
- 多 parent / DAG；
- Task 语义。

以后新增能力必须建立在 Role / Memory / Thread 之上，不能反向把 Thread 改造成工作流模型。

## 17. 逻辑闭合审计

### 17.1 对象闭合

通过。

三个业务原子足以表达全部核心协作：

~~~
Role → Memory
Role ↔ Thread
Thread ↔ Memory
Thread → Thread
~~~

关系表不升格为新业务对象。

### 17.2 发起闭合

通过。

thread_create 同时覆盖 Root 与 Child。

### 17.3 加入闭合

通过。

thread_role_set 直接建立 Role ∈ Thread；邀请协议不是 Thread 核心状态。

### 17.4 内容闭合

通过，依赖 §15 Memory 修正。

新内容用 thread_post，已有内容用 thread_include，均仍以 Memory 为唯一内容原子。

### 17.5 传递闭合

通过。

- 人进入会话：Role relation；
- 信息进入会话：post / include；
- 上下文下钻：Child Thread；
- 结果回主线：descendant → ancestor promotion。

没有额外 handoff 对象。

### 17.6 状态闭合

通过。

Thread 的 open / closed、timeline head、Role 边界、Child Thread 结构与 Child status 已足够表达对话过程。

Role 不需要额外 membership state；阅读进度不需要服务端持久化。

### 17.7 权限闭合

通过。

- ThreadRole 负责 local ACL；
- Root creator 提供 tree-wide governance；
- parent manage gate 控制 Child 对外扩展；
- Thread scoped read 穿透 private Memory，但不改变 Memory 全局权限；
- private Memory 不允许任意跨 Root Thread 传播。

### 17.8 协作场景闭合

通过。

多人主线、局部讨论、外部专家、并行子对话、结果回流、Role 调整、Agent 接续都只使用本文原语完成。

## 18. 开发准入

进入代码前必须同时满足：

1. 本 PRD 作为唯一 Thread 机制依据；
2. 先完成 Memory 原子修正；
3. Thread 实现只建立 Thread + 两张关系表；
4. 对外只围绕 §11 八个语义动作；
5. 权限判断集中在 Thread service；
6. repository 不复制产品授权规则；
7. 实现中若出现第四个协作业务对象、新 membership 状态机或 workflow 状态，立即停止并回到 PRD。
